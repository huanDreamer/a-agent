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

	"github.com/huan/huan-agent/internal/config"
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
	// Version is reported by /api/health and /api/meta.
	Version string
	// Provider and Model are the active LLM target, reported by /api/meta.
	Provider string
	Model    string
	// SkillsDir is the directory skills are loaded from, and StatePath is the
	// file recording which of them are disabled.
	SkillsDir string
	StatePath string
	// ChatMaxSteps caps tool iterations per chat turn (0 = default).
	ChatMaxSteps int
	// ChatHistoryLimit bounds how many stored messages are replayed.
	ChatHistoryLimit int
	// ChatEnable turns the web chat endpoints on.
	ChatEnable bool
	// Chat carries the chat wiring. A nil Runner disables the chat endpoints.
	Chat ChatDeps
	// FeishuEnabled reports whether the IM bot is configured.
	FeishuEnabled bool
	Logger        *zap.Logger
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

	auth, err := newAuthenticator(adminCfg.Username, adminCfg.PasswordHash, cfg.SessionTTL, logger)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:        cfg,
		logger:     logger,
		store:      st,
		auth:       auth,
		pricing:    table,
		skillState: newSkillState(cfg.SkillsDir, cfg.StatePath),
		chat:       cfg.Chat,
		tracer:     cfg.Tracer,
		traces:     cfg.Traces,
		startT:     time.Now(),
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

	h := server.New(server.WithListener(ln), server.WithExitWaitTime(engineExitWait))
	s.hertz = h
	s.registerRoutes(h)
	return s, nil
}

// Addr reports the bound address (useful when the config used port 0).
func (s *Server) Addr() string { return s.addr }

// registerRoutes wires the API, the metrics endpoint and the embedded UI.
func (s *Server) registerRoutes(h *server.Hertz) {
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
	s.registerChatRoutes(authed)
	s.registerTraceRoutes(authed)

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

	// Give the drain slightly longer than the engine's own exit wait so the
	// engine's timeout (and its log) is what reports a stuck connection.
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout+time.Second)
	defer cancel()
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
