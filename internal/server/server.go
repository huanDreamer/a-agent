// Package server implements the Phase 5 admin HTTP service: a small
// authenticated JSON API over the usage/audit data, a Prometheus exposition
// endpoint, and the embedded admin UI.
//
// The server is intentionally single-user: one admin account whose bcrypt hash
// lives in config, and opaque in-memory session tokens handed out as cookies.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/artifact"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/metrics"
	"github.com/huan/huan-agent/internal/pricing"
	"github.com/huan/huan-agent/internal/store"
)

// DefaultSessionTTL bounds a login when the config does not say otherwise.
const DefaultSessionTTL = 12 * time.Hour

// engineExitWait is how long the engine waits for in-flight requests to drain.
// It is deliberately short: the admin API serves fast queries, and a long wait
// only stalls shutdown while an idle keep-alive connection is held open.
const engineExitWait = 3 * time.Second

// shutdownTimeout bounds the whole graceful shutdown.
const shutdownTimeout = 5 * time.Second

// Config is what the admin server needs to run.
type Config struct {
	// Host and Port are the listen address.
	Host string
	Port int
	// SessionTTL is how long a login stays valid.
	SessionTTL time.Duration
	// MetricsPath is where the Prometheus exposition is served (default
	// "/metrics").
	MetricsPath string
	// MetricsEnable turns the exposition endpoint on.
	MetricsEnable bool
	// Metrics may be nil, in which case instrumentation is skipped.
	Metrics *metrics.Metrics
	// Tracer receives conversation traces. A nil value disables tracing.
	Tracer chatTracer
	// Traces reads traces back for the trace UI. Nil hides the endpoints.
	Traces TraceReader
	// Checkpoints configures the per-turn file checkpoints. Enable=false, or an
	// empty Dir, leaves the feature off and the endpoints unregistered.
	Checkpoints CheckpointSettings
	// Artifacts configures the store of what the agent produced. Enable=false
	// leaves save_artifact refusing every call and the endpoints answering
	// `enabled: false` rather than 404ing.
	Artifacts ArtifactSettings
	// Version is reported by /api/health and /api/meta.
	Version string
	// Provider and Model are the active LLM target, reported by /api/meta.
	Provider string
	Model    string
	// SkillsDir is the directory skills are loaded from, and StatePath is the
	// file recording which of them are disabled.
	SkillsDir string
	StatePath string
	// The three budget fields below are *defaults* from config.yaml, not the
	// budget a turn necessarily runs with: 设置 → 对话预算 may have stored an
	// override in the database, and effectiveBudget() merges the two per field.
	// They are named accordingly so nothing reads them as the live value and
	// quietly reintroduces a second answer to "how many steps may this turn take".

	// DefaultChatMaxSteps caps tool iterations per chat turn (0 = default).
	DefaultChatMaxSteps int
	// DefaultChatMaxTokens caps what one chat turn may spend, from the usage the
	// provider reports (0 = unlimited).
	DefaultChatMaxTokens int
	// DefaultChatTurnDeadline bounds one chat turn's wall-clock time (0 =
	// unlimited).
	DefaultChatTurnDeadline time.Duration
	// ContextMaxTokens is the in-turn history budget resolved for the default
	// model. It is reported (not applied) by the budget panel, which uses it to
	// warn that a raised step cap without compression is the combination that
	// turns a long task into a context-limit error. 0 means compression is off.
	ContextMaxTokens int
	// ContextAuto says the budget above was derived from the model's context
	// window rather than fixed in the config, and ContextModel names the model it
	// was derived for. The panel shows both: "91750" alone invites the reader to
	// go looking for a 91750 in their config file, which is not there.
	ContextAuto  bool
	ContextModel string
	// ChatHistoryLimit bounds how many stored messages are replayed.
	ChatHistoryLimit int
	// ChatEnable turns the web chat endpoints on.
	ChatEnable bool
	// Chat carries the chat wiring. A nil Runner disables the chat endpoints.
	Chat ChatDeps
	// OpenViking is the context-database integration (memory mirror + document
	// store). Nil is a supported state: every /api/openviking route then
	// answers `enable: false` rather than failing.
	OpenViking OpenVikingConsole
	// MCPDial overrides how MCP connections are opened. Nil uses the real
	// client; a test injects an in-process server so the whole MCP surface can
	// be exercised without spawning a process.
	MCPDial mcp.Dialer
	// FeishuEnabled reports whether the IM bot is configured.
	FeishuEnabled bool
	// Jobs supervises the background processes this process started. Nil is a
	// supported state: /api/jobs then answers enabled:false rather than failing,
	// which is what a deployment with background jobs turned off should see.
	Jobs   *jobs.Manager
	Logger *zap.Logger
}

// Server is the admin HTTP service.
type Server struct {
	cfg     Config
	logger  *zap.Logger
	store   store.Store
	auth    *authenticator
	pricing *pricing.Table

	skillState *skillState

	// chat holds the chat dependencies; Runner == nil disables the feature.
	chat        ChatDeps
	tracer      chatTracer
	traces      TraceReader
	runnerCache runnerCache

	// questions holds the ask_user questions currently waiting for an answer. It
	// is the join between a streaming turn and the separate request that submits
	// the answer, and it is per server rather than per turn because those two are
	// different requests.
	questions *questionHub

	// checkpointSettings is what the per-workspace checkpointers are built from.
	//
	// The checkpointers themselves are per workspace (see checkpointFor): this
	// process serves several, and one built at startup would resolve every path
	// through whichever was current then.
	checkpointSettings CheckpointSettings

	// artifacts is the store of the resources the agent produced: bytes on the
	// server's disk, indexed in the database, served over
	// /api/artifacts/files. Nil is the feature turned off, which every artifact
	// endpoint reports rather than failing.
	//
	// One store for the whole process, unlike the checkpointer: an artifact
	// belongs to a session, not to a workspace, so there is nothing
	// per-workspace to resolve.
	artifacts *artifact.Store

	// approvals holds the write/exec requests currently waiting for a decision.
	// It is the same shape as questions and exists for the same reason; the
	// difference is what an unanswered request means, which is decided by the
	// approver rather than here.
	approvals *approvalHub

	// turns holds the conversations with a turn in flight.
	//
	// A turn belongs to its conversation rather than to the request that started
	// it, which is what lets a browser detach — another page, another
	// conversation, a reload — and attach again to the same running answer.
	turns *turnHub

	// mcp owns the live MCP connections and the tools they expose. It is nil
	// when there is no tool registry to register into (chat disabled): servers
	// can still be stored and tested, but nothing is connected.
	mcp *mcp.Manager
	// mcpDial is how MCP connections are opened. Kept on the server (rather
	// than only inside the manager) so 测试连接 also works when no manager
	// exists, which is the case with chat.enable = false.
	mcpDial mcp.Dialer

	// jobs is the background-process supervisor this process owns. It is not
	// closed here: the command that built it owns its lifetime, because a job
	// outliving a shut-down admin engine would be exactly the orphan this
	// feature exists to prevent.
	jobs *jobs.Manager

	hertz  *server.Hertz
	ln     net.Listener
	addr   string
	startT time.Time
}

// New binds the listen address and wires all routes. Binding here means an
// unusable port fails at construction rather than on Start.
func New(cfg Config, st store.Store, table *pricing.Table, adminCfg config.AdminConfig) (*Server, error) {
	if st == nil {
		return nil, errors.New("server: store is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	if cfg.MetricsPath == "" {
		cfg.MetricsPath = "/metrics"
	}
	if cfg.Tracer == nil {
		cfg.Tracer = nopTracer{}
	}

	// A login-free admin is only defensible on loopback: the agent's tools read,
	// write and execute, so exposing this port without a password would hand
	// command execution to anyone who can reach it. Refuse that combination
	// rather than starting something dangerous because of a config oversight.
	if !adminCfg.RequireLogin && !adminCfg.AllowInsecureBind && !config.LoopbackHost(cfg.Host) {
		return nil, fmt.Errorf(
			"refusing to serve a login-free admin on %q: it would expose command execution "+
				"to the network. Bind 127.0.0.1, set admin.require_login: true, or set "+
				"admin.allow_insecure_bind: true if it sits behind another authenticating layer",
			cfg.Host)
	}

	auth, err := newAuthenticator(adminCfg, cfg.SessionTTL, logger)
	if err != nil {
		return nil, err
	}
	// `require_login: true` reads as "everyone needs the password", and with
	// trust_loopback on (the default) that is not what this process enforces.
	// Say what it does instead: it cannot be detected from the outside, and the
	// case where it matters — a proxy or tunnel on this host, whose clients all
	// arrive from 127.0.0.1 — is invisible in the config file.
	if auth.trustsLoopback && !auth.disabled {
		logger.Warn("admin login is not enforced for loopback clients",
			zap.String("host", cfg.Host),
			zap.String("hint", "requests from 127.0.0.1/::1 skip the password — which includes a "+
				"reverse proxy, SSH tunnel or container port-forward on this host, and every "+
				"caller when the bind itself is loopback; set admin.trust_loopback: false to ask "+
				"them all for it"))
	}

	s := &Server{
		cfg:                cfg,
		logger:             logger,
		store:              st,
		auth:               auth,
		pricing:            table,
		skillState:         newSkillState(cfg.SkillsDir, cfg.StatePath),
		chat:               cfg.Chat,
		tracer:             cfg.Tracer,
		traces:             cfg.Traces,
		jobs:               cfg.Jobs,
		questions:          newQuestionHub(),
		approvals:          newApprovalHub(),
		checkpointSettings: cfg.Checkpoints,
		turns:              newTurnHub(),
		startT:             time.Now(),
	}

	// The artifact store is built once for the process: it belongs to sessions
	// rather than to workspaces, so there is nothing per-workspace to resolve.
	// A root that was configured but cannot be created refuses startup, because
	// the feature was asked for and starting without it would leave save_artifact
	// failing for the life of the process (see newArtifactStore).
	artifactStore, err := newArtifactStore(cfg.Artifacts)
	if err != nil {
		return nil, err
	}
	s.artifacts = artifactStore
	if s.artifacts != nil {
		logger.Info("artifacts enabled",
			zapString("root", s.artifacts.Root()),
			zapBool("public_urls", cfg.Artifacts.PublicURLs),
			zapInt64("max_bytes", s.artifacts.MaxBytes()))
		if cfg.Artifacts.PublicURLs && !config.LoopbackHost(cfg.Host) {
			logger.Warn("artifact files are served without a login on a non-loopback bind",
				zapString("root", s.artifacts.Root()),
				zapString("hint", "tools.artifacts.public_urls=true serves whatever the model wrote to "+
					"anyone who can reach this port — sandboxed, but readable."))
		}
	}

	// Checkpoints are built per workspace (see checkpointFor) because this process
	// serves several: a single checkpointer would resolve every path through
	// whichever workspace happened to be current when it was made. What is settled
	// here is only whether the feature is on and where its directories live.
	if cfg.Checkpoints.Enable && cfg.Checkpoints.Dir != "" {
		s.logger.Info("checkpoints enabled",
			zapString("dir", cfg.Checkpoints.Dir),
			zapInt("keep_turns", cfg.Checkpoints.KeepTurns),
			zapInt("max_total_mb", cfg.Checkpoints.MaxTotalMB))
	}

	// The MCP runtime writes into the same registry chat turns read, so a server
	// added in 设置 → MCP is callable by the model on the very next message.
	// Without a registry (chat.enable = false) there is nothing to register
	// into, and the console says so rather than pretending to connect.
	s.mcpDial = cfg.MCPDial
	if cfg.Chat.Tools != nil {
		s.mcp = mcp.NewManager(cfg.Chat.Tools, mcp.ManagerOptions{
			Dial:          cfg.MCPDial,
			Logger:        logger,
			OnServerError: s.recordMCPError,
		})
	}
	// The 技能 tool is part of the chat's tool set, not of the skills page: it is
	// how the model reads an enabled skill's instructions. Registering it here
	// (rather than in the caller) keeps the tool and the skill state that backs
	// it on the same object.
	if err := s.registerSkillTool(); err != nil {
		logger.Warn("skill tool unavailable", zapError(err))
	}

	// Hertz logs every registered route at debug level, which drowns a normal
	// startup. Route its logger through zap at the configured level instead.
	silenceHertzLogger(logger)

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	// Bind first so a bad address or busy port surfaces now, not later.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("server: listen on %s: %w", addr, err)
	}
	s.ln = ln
	s.addr = ln.Addr().String()

	h := server.New(
		server.WithListener(ln),
		server.WithExitWaitTime(engineExitWait),
		// Attachment uploads are multipart bodies that can legitimately be far
		// larger than the engine's 4 MiB request cap, and the upload handler
		// enforces its own limit while it reads. Streaming the body — instead of
		// buffering and pre-parsing it — keeps that decision in the handler,
		// which can answer with a JSON 413 rather than the engine's generic
		// error, and keeps a large upload from being materialised whole in
		// memory before it is even looked at.
		//
		// A body that fits the cap is still buffered, so every other endpoint's
		// JSON body is read exactly as before.
		server.WithStreamBody(true),
		server.WithDisablePreParseMultipartForm(true),
	)
	s.hertz = h
	s.registerRoutes(h)
	return s, nil
}

// Addr reports the bound address (useful when the config used port 0).
func (s *Server) Addr() string { return s.addr }

// registerRoutes wires the API, the metrics endpoint and the embedded UI.
func (s *Server) registerRoutes(h *server.Hertz) {
	// Bound what the engine buffers for a request that is not an attachment
	// upload; see limitStreamedBody for why streaming the body needs it.
	h.Use(s.limitStreamedBody)

	if s.cfg.MetricsEnable {
		h.GET(s.cfg.MetricsPath, s.handleMetrics)
	}

	api := h.Group("/api")
	api.POST("/login", s.handleLogin)
	api.POST("/logout", s.handleLogout)
	api.GET("/me", s.handleMe)
	api.GET("/health", s.handleHealth)

	// Everything below requires a valid session.
	authed := api.Group("", s.auth.requireSession)
	authed.GET("/meta", s.handleMeta)
	authed.GET("/usage/summary", s.handleUsageSummary)
	authed.GET("/usage/by-model", s.handleUsageByModel)
	authed.GET("/usage/by-provider", s.handleUsageByProvider)
	authed.GET("/usage/by-user", s.handleUsageByUser)
	authed.GET("/usage/by-day", s.handleUsageByDay)
	authed.GET("/usage/recent", s.handleUsageRecent)
	authed.GET("/audit", s.handleAudit)
	authed.GET("/skills", s.handleSkills)
	authed.POST("/skills/:name", s.handleSetSkill)
	authed.GET("/skills/:name", s.handleSkillDetail)
	authed.PUT("/skills/:name", s.handleSaveSkill)
	authed.DELETE("/skills/:name", s.handleDeleteSkill)
	// The 写作助手 lives at /api/skills-draft rather than /api/skills/draft on
	// purpose: the latter would be matched by the `/skills/:name` route above,
	// and a skill literally named "draft" would then shadow the endpoint.
	authed.POST("/skills-draft", s.handleDraftSkill)
	s.registerProviderRoutes(authed)
	s.registerChatRoutes(authed)
	// The per-turn budget is read and written on its own pair of routes rather
	// than folded into /chat/models: the panel changes it while the chat page is
	// idle, and the composer only ever reads the catalog.
	s.registerBudgetRoutes(authed)
	// Registered after the chat routes: /workspaces/preview must not be matched
	// by /workspaces/:name, so the order inside that group keeps the literal
	// path first (see registerWorkspaceRoutes).
	s.registerWorkspaceRoutes(authed)
	s.registerFSRoutes(authed)
	s.registerTraceRoutes(authed)
	s.registerMCPRoutes(authed)
	s.registerOpenVikingRoutes(authed)
	s.registerJobRoutes(authed)
	// The artifact routes are registered before the UI: the file route is on the
	// open group and asks for a session itself, because whether it needs one is
	// artifacts.public_urls.
	s.registerArtifactRoutes(api, authed)

	s.registerUI(h)
}

// Start runs the server until ctx is cancelled, then shuts down gracefully.
//
// It drives Engine.Run directly rather than Hertz's Spin: Spin blocks on OS
// signals and ignores a context, which cannot be cancelled by a caller (or a
// test), so a cancelled context would hang forever.
func (s *Server) Start(ctx context.Context) error {
	runErr := make(chan error, 1)
	go func() { runErr <- s.hertz.Run() }()

	s.logger.Info("admin server listening",
		zapString("addr", s.addr),
		zapString("metrics_path", s.cfg.MetricsPath),
		zap.Bool("metrics", s.cfg.MetricsEnable),
	)

	select {
	case err := <-runErr:
		// The engine stopped on its own (bind failure or a panic in a handler).
		if err != nil {
			return fmt.Errorf("server: run: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	// Disconnect the MCP servers before the engine drains: a stdio server is a
	// child process, and leaving it running after the console exits would leak
	// one process per connection.
	if s.mcp != nil {
		s.mcp.Close()
	}

	// Give the drain slightly longer than the engine's own exit wait so the
	// engine's timeout (and its log) is what reports a stuck connection.
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout+time.Second)
	defer cancel()

	// Turns in flight are not requests, so the engine's drain does not wait for
	// them: they are cancelled and given a moment to store what they produced,
	// because the store is closed as soon as this returns.
	s.turns.shutdown(shutCtx)

	if err := s.hertz.Shutdown(shutCtx); err != nil && !isNotRunning(err) {
		return fmt.Errorf("server: shutdown: %w", err)
	}
	<-runErr
	s.logger.Info("admin server stopped")
	return nil
}

// silenceHertzLogger routes Hertz's own logging through zap and raises its
// level to warnings, so the route table is not dumped on every start. Hertz
// logs through a package-level logger, which is why this is a global setting.
func silenceHertzLogger(logger *zap.Logger) {
	hlog.SetLogger(&hertzZapLogger{logger: logger})
	hlog.SetLevel(hlog.LevelWarn)
}

// hertzZapLogger adapts a zap logger to Hertz's FullLogger interface. Hertz
// requires the full interface (Logger + FormatLogger + CtxLogger + Control) to
// accept a custom logger, so SetLevel/SetOutput are implemented as no-ops: the
// level is controlled by zap instead.
type hertzZapLogger struct{ logger *zap.Logger }

// SetLevel is a no-op: the zap logger's own level applies.
func (l *hertzZapLogger) SetLevel(hlog.Level) {}

// SetOutput is a no-op: output is owned by zap.
func (l *hertzZapLogger) SetOutput(io.Writer) {}

func (l *hertzZapLogger) Trace(v ...any) { l.logger.Debug(fmt.Sprint(v...)) }
func (l *hertzZapLogger) Debug(v ...any) { l.logger.Debug(fmt.Sprint(v...)) }
func (l *hertzZapLogger) Info(v ...any)  { l.logger.Info(fmt.Sprint(v...)) }
func (l *hertzZapLogger) Notice(v ...any) {
	l.logger.Info(fmt.Sprint(v...))
}
func (l *hertzZapLogger) Warn(v ...any)  { l.logger.Warn(fmt.Sprint(v...)) }
func (l *hertzZapLogger) Error(v ...any) { l.logger.Error(fmt.Sprint(v...)) }
func (l *hertzZapLogger) Fatal(v ...any) { l.logger.Error(fmt.Sprint(v...)) }

func (l *hertzZapLogger) CtxTracef(_ context.Context, f string, v ...any) {
	l.logger.Debug(fmt.Sprintf(f, v...))
}
func (l *hertzZapLogger) CtxDebugf(_ context.Context, f string, v ...any) {
	l.logger.Debug(fmt.Sprintf(f, v...))
}
func (l *hertzZapLogger) CtxInfof(_ context.Context, f string, v ...any) {
	l.logger.Info(fmt.Sprintf(f, v...))
}
func (l *hertzZapLogger) CtxNoticef(_ context.Context, f string, v ...any) {
	l.logger.Info(fmt.Sprintf(f, v...))
}
func (l *hertzZapLogger) CtxWarnf(_ context.Context, f string, v ...any) {
	l.logger.Warn(fmt.Sprintf(f, v...))
}
func (l *hertzZapLogger) CtxErrorf(_ context.Context, f string, v ...any) {
	l.logger.Error(fmt.Sprintf(f, v...))
}
func (l *hertzZapLogger) CtxFatalf(_ context.Context, f string, v ...any) {
	l.logger.Error(fmt.Sprintf(f, v...))
}

func (l *hertzZapLogger) Tracef(f string, v ...any) { l.logger.Debug(fmt.Sprintf(f, v...)) }
func (l *hertzZapLogger) Debugf(f string, v ...any) { l.logger.Debug(fmt.Sprintf(f, v...)) }
func (l *hertzZapLogger) Infof(f string, v ...any)  { l.logger.Info(fmt.Sprintf(f, v...)) }
func (l *hertzZapLogger) Noticef(f string, v ...any) {
	l.logger.Info(fmt.Sprintf(f, v...))
}
func (l *hertzZapLogger) Warnf(f string, v ...any)  { l.logger.Warn(fmt.Sprintf(f, v...)) }
func (l *hertzZapLogger) Errorf(f string, v ...any) { l.logger.Error(fmt.Sprintf(f, v...)) }
func (l *hertzZapLogger) Fatalf(f string, v ...any) { l.logger.Error(fmt.Sprintf(f, v...)) }

// isNotRunning reports whether Shutdown failed only because the engine had
// already stopped (e.g. it exited just before the context was cancelled).
// Hertz exports no sentinel for this, so the message is matched.
func isNotRunning(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not running")
}
