package claudecode

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/claudehook"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/retry"
	"github.com/huan/huan-agent/internal/store"
)

// reloadThrottle bounds how often the settings file is stat-ed.
//
// The file is checked before every dispatch and on every status read, and a
// stat is cheap — but "cheap" times every tool call of an agent that is reading
// five hundred files is not free, and no editor writes more than once a second.
const reloadThrottle = 2 * time.Second

// maxSessionContexts bounds the per-conversation context a SessionStart hook
// injected. A long-lived server serves many conversations and most of them are
// never opened again; the map would otherwise be a slow leak of strings.
const maxSessionContexts = 256

// Options wires a Service.
type Options struct {
	// Config is the claudecode section of config.yaml: the startup default of
	// the mode, the settings path, and the hook switches.
	Config config.ClaudeCodeConfig
	// Store persists the mode the console switched to. Nil means the mode is
	// whatever the config says and the switch cannot be flipped — which is what
	// a test or a tool that only reads the settings file wants.
	Store store.Store
	// Retry is the backoff every model this mode builds is given, so a
	// conversation on the compatibility endpoint retries exactly as a native one
	// does.
	Retry retry.Policy
	// Logger receives the warnings this mode produces. Nil is a no-op logger.
	Logger *zap.Logger
	// MCP runs mcp_tool hooks. Nil records them as skipped rather than silently
	// doing nothing.
	MCP claudehook.MCPCaller
	// Dir is the working directory command hooks run in, when a caller does not
	// name one per dispatch.
	Dir string
	// Now is injectable for tests. Nil means time.Now.
	Now func() time.Time
}

// Service is the compatibility mode: the settings file, the switch, the hook
// engine, and the provider this mode contributes to the model catalog.
//
// One per process. It is safe for concurrent use: a turn dispatches hooks from
// several goroutines at once (parallel tool calls), and the console reads the
// status while they run.
type Service struct {
	cfg    config.ClaudeCodeConfig
	st     store.Store
	retry  retry.Policy
	logger *zap.Logger
	now    func() time.Time
	// dir is the working directory command hooks run in, and the value exported
	// as CLAUDE_PROJECT_DIR. It is the deployment's workspace, not the process's
	// own directory: a hook that resolves a path relative to it must land in the
	// tree the agent is working in.
	dir string
	mcp claudehook.MCPCaller

	mu        sync.RWMutex
	settings  Settings
	compat    bool
	compatSrc string
	engine    *claudehook.Engine
	checkedAt time.Time

	// sessions is the context SessionStart hooks injected, per conversation,
	// until it is consumed or forgotten.
	smu      sync.Mutex
	sessions map[string]sessionContext

	// models caches what this mode has built, keyed by the endpoint and model it
	// was built from. It is dropped on every reload: the token lives in the
	// settings file, so a reload is the only thing that can change it, and a
	// cache that survived one would keep signing with the old credential.
	mmu    sync.Mutex
	models map[string]model.BaseChatModel
}

// sessionContext is one conversation's injected context, plus the id of the
// prompt it is serving.
//
// The two live together because they share a lifetime and a key: both are facts
// about "the turn this conversation is on", both are dropped by /clear, and
// keeping them in one map means one eviction rule rather than two that can
// disagree.
type sessionContext struct {
	lines []string
	// promptID is the id of the user prompt currently being processed, reported
	// to hooks as prompt_id (see Service.SetPromptID).
	promptID string
	at       time.Time
}

// NewService builds the mode from the config, the settings file and the stored
// switch, in that order of precedence: the console's switch wins over
// config.yaml, which wins over nothing at all.
func NewService(ctx context.Context, opts Options) *Service {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Service{
		cfg:      opts.Config,
		st:       opts.Store,
		retry:    opts.Retry,
		logger:   logger,
		now:      now,
		dir:      opts.Dir,
		mcp:      opts.MCP,
		sessions: map[string]sessionContext{},
		models:   map[string]model.BaseChatModel{},
	}
	s.compat = opts.Config.Enable
	s.compatSrc = "config"
	if opts.Store != nil {
		mode, ok, err := opts.Store.GetClaudeCodeMode(ctx)
		switch {
		case err != nil:
			logger.Warn("claudecode: reading the stored mode failed; using the config value",
				zap.Error(err))
		case ok:
			s.compat = mode == store.ClaudeCodeModeCompat
			s.compatSrc = "console"
		}
	}
	s.settings = Load(s.settingsPath())
	s.engine = s.newEngine()
	// Say what was found at startup, and say it plainly when the mode is on but
	// cannot run: that combination is a switch the operator believes in.
	if m := s.settings.Model(); s.compat {
		if m.Ready {
			logger.Info("claudecode compatibility mode on",
				zap.String("provider", ProviderID),
				zap.String("model", m.EffectiveModel),
				zap.String("base_url", m.BaseURL),
				zap.String("settings", s.settings.Path))
		} else {
			logger.Warn("claudecode compatibility mode is on but cannot run",
				zap.String("settings", s.settings.Path),
				zap.String("problem", m.Problem))
		}
	}
	if s.engine != nil {
		logger.Info("claudecode hooks loaded",
			zap.String("settings", s.settings.Path),
			zap.Int("configured_events", len(s.settings.Hooks.Groups)),
		)
	}
	return s
}

// settingsPath resolves the file to read.
func (s *Service) settingsPath() string {
	if p := strings.TrimSpace(s.cfg.SettingsPath); p != "" {
		return expandHome(p)
	}
	return DefaultSettingsPath()
}

// expandHome expands a leading ~ in a configured path.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	return home + strings.TrimPrefix(path, "~")
}

// Compat reports whether the mode is on.
//
// A nil Service reports "off". The guard is not decoration: this type is handed
// out as an interface (the catalog's ExtraProvider), and a typed nil in an
// interface is not a nil interface — so without it, a caller that passed a nil
// *Service would be told the provider exists and then dereference nothing.
func (s *Service) Compat() bool {
	if s == nil {
		return false
	}
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.compat
}

// Mode is the console-facing name of the current state, "claudecode" or
// "native".
func (s *Service) Mode() string {
	if s.Compat() {
		return store.ClaudeCodeModeCompat
	}
	return store.ClaudeCodeModeNative
}

// Source says where the mode came from: "console" when the switch was flipped
// here, "config" when it is still config.yaml's value.
func (s *Service) Source() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.compatSrc
}

// Settings returns the last read of the settings file, re-reading it first when
// the throttle allows and the file has changed on disk.
func (s *Service) Settings() Settings {
	if s == nil {
		return Settings{}
	}
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

// Reload re-reads the settings file and rebuilds everything derived from it.
//
// It is what 设置 → ClaudeCode's 重新读取 button calls, and it is also what the
// mtime check calls when the file changed under a running process: editing
// settings.json must take effect on the next message, not the next restart.
func (s *Service) Reload() Settings {
	path := s.settingsPath()
	next := Load(path)

	s.mu.Lock()
	previous := s.settings
	s.settings = next
	engine := s.newEngineLocked()
	s.engine = engine
	s.checkedAt = s.now()
	s.mu.Unlock()

	// The credential may have changed with the file, so nothing built from the
	// old one may be reused.
	s.mmu.Lock()
	s.models = map[string]model.BaseChatModel{}
	s.mmu.Unlock()

	if previous.Path != next.Path || previous.MTime != next.MTime {
		s.logger.Info("claudecode settings reloaded",
			zap.String("settings", next.Path),
			zap.Time("mtime", next.MTime),
			zap.Int("events", len(next.Hooks.Groups)),
			zap.String("error", next.Err))
	}
	return next
}

// maybeReload re-reads the file when its mtime moved, at most every
// reloadThrottle.
func (s *Service) maybeReload() {
	s.mu.RLock()
	due := s.now().Sub(s.checkedAt) >= reloadThrottle
	s.mu.RUnlock()
	if !due {
		return
	}

	path := s.settingsPath()
	info, err := os.Stat(path)
	s.mu.Lock()
	s.checkedAt = s.now()
	changed := false
	switch {
	case err != nil && s.settings.Found:
		// The file was removed: reloading turns that into the "找不到文件" state
		// the console renders, instead of keeping a configuration whose source is
		// gone.
		changed = true
	case err == nil && (info.ModTime() != s.settings.MTime || !s.settings.Found):
		changed = true
	}
	s.mu.Unlock()
	if changed {
		s.Reload()
	}
}

// SetCompat turns the mode on or off and stores the choice.
//
// Turning it on does not require the settings file to be usable: the switch is
// the operator's, and a file that cannot be read is reported as the reason the
// conversations still run natively (see Status), which is a better answer than
// a switch that refuses to move.
func (s *Service) SetCompat(ctx context.Context, on bool) error {
	if s.st != nil {
		mode := store.ClaudeCodeModeNative
		if on {
			mode = store.ClaudeCodeModeCompat
		}
		if err := s.st.SetClaudeCodeMode(ctx, mode); err != nil {
			return fmt.Errorf("claudecode: store the mode: %w", err)
		}
	}

	s.mu.Lock()
	s.compat = on
	s.compatSrc = "console"
	engine := s.newEngineLocked()
	s.engine = engine
	s.mu.Unlock()

	s.mmu.Lock()
	s.models = map[string]model.BaseChatModel{}
	s.mmu.Unlock()

	s.logger.Info("claudecode compatibility mode switched",
		zap.Bool("on", on),
		zap.String("settings", s.Settings().Path),
		zap.Bool("hooks", s.HooksActive()))
	return nil
}

// Engine returns the live hook engine. Nil means hooks are off.
func (s *Service) Engine() *claudehook.Engine {
	if s == nil {
		return nil
	}
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.engine
}

// SetMCPCaller installs the runner mcp_tool hooks use, and rebuilds the engine
// so it takes effect now.
//
// It is set after construction rather than passed to it because its only real
// implementation is the MCP runtime, which needs the tool registry the server
// builds — and that registry is built from the chat wiring this mode is itself
// part of. Passing it later, with an explicit rebuild, is what keeps the two
// halves from having to be constructed in one order.
func (s *Service) SetMCPCaller(caller claudehook.MCPCaller) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mcp = caller
	s.engine = s.newEngineLocked()
}

// newEngine builds the engine under the write lock's assumptions.
func (s *Service) newEngine() *claudehook.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newEngineLocked()
}

// newEngineLocked builds the hook engine for the current settings and mode.
//
// It returns nil rather than an empty engine when hooks are off: a nil engine
// is a fact the caller can show ("hooks 关闭"), while an empty engine makes
// "no hook matched" and "hooks are off" indistinguishable in the console.
func (s *Service) newEngineLocked() *claudehook.Engine {
	if !s.compat || !s.cfg.HooksOr() || s.settings.DisableAllHooks {
		return nil
	}
	if len(s.settings.Hooks.Groups) == 0 {
		return nil
	}
	return claudehook.NewEngine(s.settings.Hooks, claudehook.Options{
		Logger:     s.logger,
		Dir:        s.dir,
		MaxTimeout: s.cfg.HookTimeout(),
		MCP:        s.mcp,
		Env:        s.hookEnvLocked(),
		Now:        s.now,
	})
}

// hookEnvLocked is the environment command hooks see, appended to the inherited
// one.
//
// CLAUDE_PROJECT_DIR is what makes a hook written for Claude Code work here
// unchanged: hook scripts resolve their own paths from it, and a script that
// found it empty would silently read the wrong tree. CLAUDE_EFFORT carries the
// effort level the settings file configured, which the reference documents as
// being exported to hooks.
func (s *Service) hookEnvLocked() []string {
	env := []string{"CLAUDE_PROJECT_DIR=" + s.dir}
	if effort := s.settings.Get(EnvEffort); effort != "" {
		env = append(env, "CLAUDE_EFFORT="+effort)
	}
	return env
}

// HooksActive reports whether hook handlers actually run.
func (s *Service) HooksActive() bool { return s.Engine() != nil }

// Model returns the model configuration this mode runs on, and whether it is
// usable.
func (s *Service) Model() (ModelConfig, bool) {
	m := s.Settings().Model()
	// A model this mode cannot run is not a model to offer. The provider stays
	// out of the catalog in that case, so the composer cannot select a pair that
	// would fail on the first message.
	return m, m.Ready
}

// Dispatch runs the hooks configured for one event.
//
// It is a no-op when the mode is off, when hooks are switched off, when the
// settings file disabled them all, or when no handler is configured for the
// event — and it always answers with Continue=true in that case, because the
// zero value of a verdict means "stop everything" and "nothing was configured"
// must never be read as that.
func (s *Service) Dispatch(ctx context.Context, in claudehook.Input) claudehook.Verdict {
	stop := claudehook.Verdict{Continue: true}
	engine := s.Engine()
	if engine == nil {
		return stop
	}
	v := engine.Dispatch(ctx, in)
	if !v.Ran {
		v.Continue = true
	}
	return v
}

// Recent returns the most recent hook executions, newest first.
func (s *Service) Recent(limit int) []claudehook.Record {
	engine := s.Engine()
	if engine == nil {
		return nil
	}
	return engine.Recent(limit)
}

// --- per-conversation context injected by SessionStart ----------------------

// AddSessionContext records context a SessionStart hook injected, to be handed
// to the model on this conversation's next turn.
//
// It is kept in memory rather than in the database on purpose: it is derived
// from a file that is re-read constantly, so a stored copy would be a second,
// staler answer to the same question. The cost is that a restart re-runs the
// hook on the next SessionStart, which is exactly what Claude Code does.
func (s *Service) AddSessionContext(session string, lines []string) {
	if session == "" || len(lines) == 0 {
		return
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]sessionContext{}
	}
	cur := s.sessions[session]
	cur.lines = append(cur.lines, lines...)
	cur.at = s.now()
	// A map that only ever grows is a leak; the oldest entry is dropped rather
	// than the newest refused, so the conversation being used right now keeps
	// its context.
	if len(s.sessions) > maxSessionContexts {
		oldestKey, oldestAt := "", time.Time{}
		for k, v := range s.sessions {
			if oldestAt.IsZero() || v.at.Before(oldestAt) {
				oldestKey, oldestAt = k, v.at
			}
		}
		if oldestKey != "" && oldestKey != session {
			delete(s.sessions, oldestKey)
		}
	}
	s.sessions[session] = cur
}

// SetPromptID records the id of the prompt a conversation is about to process.
//
// Claude Code stamps every hook payload of one turn with the same prompt_id, and
// a handler that logs or correlates by it (the reference's own summary.jq reads
// the field) would otherwise see nothing. It is stored next to the injected
// context because both describe the current turn and are forgotten together.
func (s *Service) SetPromptID(session, promptID string) {
	if s == nil || session == "" || promptID == "" {
		return
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]sessionContext{}
	}
	cur := s.sessions[session]
	cur.promptID = promptID
	cur.at = s.now()
	s.sessions[session] = cur
}

// PromptID returns the id of the prompt a conversation is currently processing,
// empty when none was recorded.
func (s *Service) PromptID(session string) string {
	if s == nil {
		return ""
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.sessions[session].promptID
}

// SessionContext returns the context injected for one conversation.
func (s *Service) SessionContext(session string) []string {
	s.smu.Lock()
	defer s.smu.Unlock()
	return append([]string(nil), s.sessions[session].lines...)
}

// ForgetSession drops a conversation's injected context, which is what /clear
// means: the conversation that asked for it is gone.
func (s *Service) ForgetSession(session string) {
	s.smu.Lock()
	defer s.smu.Unlock()
	delete(s.sessions, session)
}

// --- the provider this mode contributes to the model catalog ---------------

// ID implements the catalog's extra-provider contract.
func (s *Service) ID() string { return ProviderID }

// Row returns this mode's provider row, or false when the mode cannot run.
//
// It is a store.Provider rather than something new because every surface that
// reads a provider — the catalog, the composer's picker, the health warnings —
// already knows how to read one; the row is simply not stored anywhere.
func (s *Service) Row(ctx context.Context) (store.Provider, bool) {
	if s == nil || !s.Compat() {
		return store.Provider{}, false
	}
	m, ok := s.Model()
	if !ok {
		return store.Provider{}, false
	}
	return store.Provider{
		ID:         ProviderID,
		Name:       ProviderName,
		BaseURL:    m.BaseURL,
		Kind:       llm.KindAnthropicMessages,
		Source:     store.SourceConfig,
		Enabled:    true,
		HasAPIKey:  m.HasToken,
		APIKeyHint: m.MaskedToken,
	}, true
}

// Models returns the models of this mode, as the catalog's model rows.
//
// Four rows rather than one: the settings file names up to four tiers, and a
// conversation that wants the cheap one should be able to pick it. They are all
// the same endpoint and credential, which is why they are one provider.
func (s *Service) Models(ctx context.Context) []store.Model {
	if s == nil || !s.Compat() {
		return nil
	}
	m, ok := s.Model()
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []store.Model
	add := func(name, display string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, store.Model{
			ProviderID:  ProviderID,
			ModelID:     name,
			DisplayName: display,
			// Chat and tools: the endpoint is an Anthropic Messages API and the
			// agent's whole value is calling tools through it. Vision is left
			// unclaimed — Claude Code's own env block does not say, and claiming
			// it would send image parts to a model that may reject them.
			Capabilities: store.Capabilities{store.CapChat, store.CapTools},
			Enabled:      true,
			Source:       string(store.SourceConfig),
		})
	}
	add(m.EffectiveModel, m.EffectiveModel)
	add(m.OpusModel, m.OpusModel)
	add(m.SonnetModel, m.SonnetModel)
	add(m.HaikuModel, m.HaikuModel)
	return out
}

// Build constructs a chat model for one of this mode's models.
func (s *Service) Build(ctx context.Context, modelName string) (model.BaseChatModel, error) {
	if s == nil {
		return nil, fmt.Errorf("claudecode: 兼容模式未启用（没有 ClaudeCode 服务）")
	}
	m, ok := s.Model()
	if !ok {
		return nil, fmt.Errorf("claudecode: 兼容模式不可用：%s", m.Problem)
	}
	name := strings.TrimSpace(modelName)
	if name == "" {
		name = m.EffectiveModel
	}

	key := m.BaseURL + "\x00" + name + "\x00" + m.AuthStyle
	s.mmu.Lock()
	if built, ok := s.models[key]; ok {
		s.mmu.Unlock()
		return built, nil
	}
	s.mmu.Unlock()

	p := m.LLMProvider()
	p.Model = name
	built, err := llm.New(p, llm.WithRetry(s.retry, s.logger))
	if err != nil {
		return nil, fmt.Errorf("claudecode: build %s/%s: %w", ProviderID, name, err)
	}
	s.mmu.Lock()
	if s.models == nil {
		s.models = map[string]model.BaseChatModel{}
	}
	s.models[key] = built
	s.mmu.Unlock()
	return built, nil
}

// Target is the (provider, model) a turn runs on in this mode.
func (s *Service) Target() (string, string, bool) {
	if s == nil || !s.Compat() {
		return "", "", false
	}
	m, ok := s.Model()
	if !ok {
		return "", "", false
	}
	return ProviderID, m.EffectiveModel, true
}
