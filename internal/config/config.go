// Package config loads huan-agent configuration from YAML with env override.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/huan/huan-agent/internal/context"
	"github.com/huan/huan-agent/internal/retry"
)

// EnvPrefix is prepended to environment variables that override config keys.
// For example, "HUAN_LOGGING_LEVEL" overrides "logging.level".
const EnvPrefix = "HUAN"

// Config is the root configuration tree.
type Config struct {
	Server   ServerConfig   `mapstructure:"server" json:"server"`
	Logging  LoggingConfig  `mapstructure:"logging" json:"logging"`
	LLM      LLMConfig      `mapstructure:"llm" json:"llm"`
	Database DatabaseConfig `mapstructure:"database" json:"database"`
	Agent    AgentConfig    `mapstructure:"agent" json:"agent"`
	MCP      MCPConfig      `mapstructure:"mcp" json:"mcp"`
	Skills   SkillsConfig   `mapstructure:"skills" json:"skills"`
	Memory   MemoryConfig   `mapstructure:"memory" json:"memory"`
	Context  ContextConfig  `mapstructure:"context" json:"context"`
	Feishu   FeishuConfig   `mapstructure:"feishu" json:"feishu"`
	Pricing  PricingConfig  `mapstructure:"pricing" json:"pricing"`
	Admin    AdminConfig    `mapstructure:"admin" json:"admin"`
	Langfuse LangfuseConfig `mapstructure:"langfuse" json:"langfuse"`
	Chat     ChatConfig     `mapstructure:"chat" json:"chat"`
	Tools    ToolsConfig    `mapstructure:"tools" json:"tools"`
	Run      RunConfig      `mapstructure:"run" json:"run"`
	Subagent SubagentConfig `mapstructure:"subagent" json:"subagent"`
	// OpenViking is the context database (long-term memory + documents) the
	// agent mirrors into. Off unless configured; see ApplyOpenVikingMCP.
	OpenViking OpenVikingConfig `mapstructure:"openviking" json:"openviking"`
}

// RunConfig configures the one-shot `huan-agent run` command: the entry point a
// git hook, a CI job or cron uses.
//
// Everything here has a "the command still works unconfigured" default, because
// a one-shot run is meant to be callable from a script without a config edit.
type RunConfig struct {
	// TimeoutSeconds bounds one run's wall-clock time. 0 (the default) means
	// unlimited, matching chat.turn_deadline_seconds: a pipeline that wants a
	// bound passes --timeout or sets this.
	TimeoutSeconds int `mapstructure:"timeout_seconds" json:"timeout_seconds"`
	// MaxSteps caps the tool-calling iterations. 0 means "inherit
	// chat.max_steps", which is what keeps the one-shot command and the
	// interactive one bounded the same way.
	MaxSteps int `mapstructure:"max_steps" json:"max_steps"`
	// DefaultOutput is the output format when --output is not passed: "text"
	// (the answer alone on stdout) or "json" (one object).
	DefaultOutput string `mapstructure:"default_output" json:"default_output"`
}

// The accepted values of run.default_output and of `run --output`.
const (
	RunOutputText = "text"
	RunOutputJSON = "json"
)

// Timeout returns the one-shot wall-clock budget. Zero means unlimited, which is
// the zero value's meaning everywhere else in this file.
func (c RunConfig) Timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// MaxStepsOr falls back to the interactive step cap.
//
// The fallback is to chat.max_steps rather than to a constant of its own so the
// two ways of running one turn cannot drift: raising the interactive cap also
// raises the one-shot cap, which is what "the same agent, without a terminal"
// should mean.
func (c RunConfig) MaxStepsOr(chatMax int) int {
	if c.MaxSteps > 0 {
		return c.MaxSteps
	}
	return chatMax
}

// OutputOr returns the configured default output format, defaulting to text.
//
// An unparseable value falls back to the default rather than being refused: the
// flag overrides it per invocation, and failing to start a pipeline over a
// display preference is the wrong trade.
func (c RunConfig) OutputOr() string {
	if strings.EqualFold(strings.TrimSpace(c.DefaultOutput), RunOutputJSON) {
		return RunOutputJSON
	}
	return RunOutputText
}

// ChatConfig configures the web chat feature.
type ChatConfig struct {
	// Enable turns the web chat on. It is off by default because it lets the
	// admin UI spend LLM tokens.
	Enable bool `mapstructure:"enable" json:"enable"`
	// MaxSteps caps tool-calling iterations per turn. 0 uses the runner default
	// (12). A value above chat.MaxStepsCeiling is refused rather than clamped:
	// a mistyped 1200 is a configuration error worth seeing once, at startup,
	// instead of a turn that quietly ran hundreds of steps.
	MaxSteps int `mapstructure:"max_steps" json:"max_steps"`
	// TurnMaxTokens caps what one turn may spend, summed from the usage the
	// provider reports. 0 (the default) means unlimited.
	//
	// It is the other half of the step cap: forty steps over long tool output
	// cost far more than two hundred cheap ones, and a step count cannot tell
	// them apart.
	TurnMaxTokens int `mapstructure:"turn_max_tokens" json:"turn_max_tokens"`
	// TurnDeadlineSeconds bounds one turn's wall-clock time. 0 (the default)
	// means unlimited.
	//
	// It catches the failure neither of the other two can see: a loop that is
	// neither many steps nor many tokens but slow — a build that never finishes,
	// a tool waiting out a network timeout.
	TurnDeadlineSeconds int `mapstructure:"turn_deadline_seconds" json:"turn_deadline_seconds"`
	// HistoryLimit bounds how many stored messages are replayed to the model.
	HistoryLimit int `mapstructure:"history_limit" json:"history_limit"`
	// SystemPrompt overrides the default system prompt.
	SystemPrompt string `mapstructure:"system_prompt" json:"system_prompt"`
	// AskUserTimeoutSeconds bounds how long the model's ask_user question waits
	// for an answer in the web console. 0 (the default) uses
	// DefaultChatAskUserTimeoutSeconds.
	//
	// It has to stay well below turn_deadline_seconds: waiting happens *inside*
	// the turn, so a limit larger than the deadline leaves the model with no
	// budget left to act on the answer it just received.
	AskUserTimeoutSeconds int `mapstructure:"ask_user_timeout_seconds" json:"ask_user_timeout_seconds"`
	// StepRetry is how one *step* of a turn is retried when its model call
	// fails — including when the stream died after the model had already started
	// answering, which the llm layer deliberately does not retry (see
	// llm.retry) because replaying it would show the reader the beginning of the
	// answer twice.
	StepRetry RetryConfig `mapstructure:"step_retry" json:"step_retry"`
	// Plan configures the task plan the model maintains through the plan_*
	// tools, and the 任务看板 that renders it above the composer.
	Plan PlanConfig `mapstructure:"plan" json:"plan"`
	// Guard bounds the two ways a long turn wastes itself without failing: a
	// model that keeps re-running the same call, and a model that keeps reading
	// without ever acting on what it read. Neither is an error the provider or
	// the step cap can see, and both end in a turn that spent its whole budget
	// exploring.
	//
	// It steers before it stops: the model is told what it is repeating, which is
	// usually enough, and the turn is ended only if it keeps going. That order
	// matters — a guard that kills a productive turn is worse than the loop it
	// was watching for.
	Guard GuardConfig `mapstructure:"guard" json:"guard"`
}

// Guard defaults. They are named here as well as in internal/chat because the
// two sides have to agree and neither may import the other: the runner must not
// depend on the config tree, and the config tree should not depend on the chat
// runtime. A test asserts the two sets are equal.
const (
	// DefaultGuardRepeatNudge / DefaultGuardRepeatStop are the identical-call
	// thresholds.
	DefaultGuardRepeatNudge = 3
	DefaultGuardRepeatStop  = 5
	// DefaultGuardIdleNudgeSteps / DefaultGuardIdleStopSteps are the
	// no-progress thresholds, in steps.
	DefaultGuardIdleNudgeSteps = 25
	DefaultGuardIdleStopSteps  = 75
	// DefaultGuardMaxSteers caps the steering messages one turn may carry.
	DefaultGuardMaxSteers = 6
	// DefaultGuardRereadNudge is how many times one file may be read the same way.
	DefaultGuardRereadNudge = 5
)

// GuardConfig tunes the loop guard. Every field has a working default, so a
// deployment only has to say what it wants to change — except Enable, which is
// a bool and therefore defaults to "off" unless a default sets it (both Default()
// and SetDefaults do).
type GuardConfig struct {
	// Enable turns the guard off. Off means a turn may burn its entire step
	// budget reading the same file, which is what a deployment that would rather
	// trust the model can choose.
	Enable bool `mapstructure:"enable" json:"enable"`
	// RepeatNudge is how many identical tool calls (same tool, same arguments)
	// earn a steering message. 0 uses the default (3).
	RepeatNudge int `mapstructure:"repeat_nudge" json:"repeat_nudge"`
	// RepeatStop is how many identical calls end the turn. 0 uses the default
	// (5). It must be above RepeatNudge: a stop the model was never warned about
	// is a guard that failed rather than one that fired.
	RepeatStop int `mapstructure:"repeat_stop" json:"repeat_stop"`
	// IdleNudgeSteps is how many consecutive steps may run tools that change
	// nothing — reads, searches, and commands that only look — before the model
	// is told to start acting or answer. 0 uses the default (25).
	IdleNudgeSteps int `mapstructure:"idle_nudge_steps" json:"idle_nudge_steps"`
	// IdleStopSteps is how many such steps end the turn. 0 uses the default (75).
	// A read-only task that legitimately needs longer says so in its answer; the
	// guard's steering message asks for exactly that.
	IdleStopSteps int `mapstructure:"idle_stop_steps" json:"idle_stop_steps"`
	// MaxSteers caps how many steering messages one turn may carry. A model that
	// ignores three of them is not going to read the fourth, and every steer is
	// a message every later step pays for again. 0 uses the default (6).
	MaxSteers int `mapstructure:"max_steers" json:"max_steers"`
	// RereadNudge is how many times one file may be read *the same way* (the same
	// path with the same offset/limit, the same grep pattern, the same sed range)
	// before the model is told it has been there already. Reading a large file in
	// consecutive slices does not count: the count is per (file, part). 0 uses the
	// default (5).
	//
	// It exists as a knob because a deployment may legitimately re-read one file
	// often; the alternative to raising it is turning the whole guard off.
	RereadNudge int `mapstructure:"reread_nudge" json:"reread_nudge"`
}

// PlanConfig configures the visible task plan.
type PlanConfig struct {
	// Enable registers the plan_* tools for the web console and lets the turn
	// publish plan updates. Turning it off removes the tools entirely rather
	// than describing them in the prompt and refusing to run them.
	Enable bool `mapstructure:"enable" json:"enable"`
	// MaxTasks bounds how many tasks one plan may hold. A plan longer than this
	// is not a plan but a transcript, and a board nobody can read is worse than
	// a refused tool call.
	MaxTasks int `mapstructure:"max_tasks" json:"max_tasks"`
}

// DefaultPlanMaxTasks bounds a plan when the config does not say.
const DefaultPlanMaxTasks = 50

// MaxTasksOr returns the configured plan size limit or the default.
func (c PlanConfig) MaxTasksOr() int {
	if c.MaxTasks <= 0 {
		return DefaultPlanMaxTasks
	}
	return c.MaxTasks
}

// RetryConfig is the shared shape of every retry policy in this config tree: the
// model-call one (llm.retry) and the step one (chat.step_retry).
//
// It is one struct rather than two identical ones because the two policies differ
// only in their numbers, and a reader comparing them should be comparing values,
// not field names.
type RetryConfig struct {
	// Enable turns retrying off entirely. It defaults to true: a long turn that
	// dies on one dropped connection is the failure this exists to remove, and
	// the cost of being wrong is one extra attempt after a backoff.
	Enable bool `mapstructure:"enable" json:"enable"`
	// MaxAttempts is the total number of attempts *including* the first.
	MaxAttempts int `mapstructure:"max_attempts" json:"max_attempts"`
	// BaseDelayMS is how long the first retry waits.
	BaseDelayMS int `mapstructure:"base_delay_ms" json:"base_delay_ms"`
	// MaxDelayMS caps the computed backoff.
	MaxDelayMS int `mapstructure:"max_delay_ms" json:"max_delay_ms"`
	// Multiplier grows the delay before each further attempt.
	Multiplier float64 `mapstructure:"multiplier" json:"multiplier"`
	// Jitter randomises each delay by +/-25%, so several turns that failed on the
	// same outage do not retry in lockstep. Defaults to true.
	Jitter *bool `mapstructure:"jitter" json:"jitter"`
}

// Policy renders the config as the backoff the retry package runs.
//
// A disabled retry becomes a policy of one attempt — "no retrying" is expressed
// as an attempt count rather than as a flag, so there is exactly one place that
// decides whether a second call happens.
func (c RetryConfig) Policy() retry.Policy {
	if !c.Enable {
		return retry.Policy{MaxAttempts: 1}
	}
	attempts := c.MaxAttempts
	if attempts <= 0 {
		// An enabled block with no count gets the default rather than becoming
		// "retry zero times": a configuration that says "retry" and silently does
		// not is the kind of thing nobody reads back. Saying one attempt on
		// purpose is what enable: false is for.
		attempts = retry.DefaultMaxAttempts
	}
	jitter := true
	if c.Jitter != nil {
		jitter = *c.Jitter
	}
	return retry.Policy{
		MaxAttempts: attempts,
		BaseDelay:   time.Duration(c.BaseDelayMS) * time.Millisecond,
		MaxDelay:    time.Duration(c.MaxDelayMS) * time.Millisecond,
		Multiplier:  c.Multiplier,
		Jitter:      jitter,
	}
}

// RetryPolicy is the policy for one model call.
func (c LLMConfig) RetryPolicy() retry.Policy { return c.Retry.Policy() }

// StepRetryPolicy is the policy for one step of a turn.
func (c ChatConfig) StepRetryPolicy() retry.Policy { return c.StepRetry.Policy() }

// DefaultChatHistoryLimit bounds the replayed conversation when unset.
const DefaultChatHistoryLimit = 40

// DefaultChatAskUserTimeoutSeconds is how long an ask_user card waits for an
// answer when the config does not say. Ten minutes lets someone step away from
// the desk without losing the turn; a longer default would only park a goroutine
// for a conversation nobody is watching.
const DefaultChatAskUserTimeoutSeconds = 600

// HistoryLimitOr returns the configured history limit or the default.
func (c ChatConfig) HistoryLimitOr() int {
	if c.HistoryLimit <= 0 {
		return DefaultChatHistoryLimit
	}
	return c.HistoryLimit
}

// TurnDeadline returns the configured per-turn wall-clock budget, or 0 for
// unlimited.
func (c ChatConfig) TurnDeadline() time.Duration {
	if c.TurnDeadlineSeconds <= 0 {
		return 0
	}
	return time.Duration(c.TurnDeadlineSeconds) * time.Second
}

// AskUserTimeout returns how long one ask_user question waits for an answer.
func (c ChatConfig) AskUserTimeout() time.Duration {
	secs := c.AskUserTimeoutSeconds
	if secs <= 0 {
		secs = DefaultChatAskUserTimeoutSeconds
	}
	return time.Duration(secs) * time.Second
}

// DatabaseConfig configures the SQLite database.
type DatabaseConfig struct {
	Path string `mapstructure:"path" json:"path"`
}

// ServerConfig configures the admin HTTP server (Phase 5).
type ServerConfig struct {
	// Host is the listen address.
	Host string `mapstructure:"host" json:"host"`
	// Port is the listen port.
	Port int `mapstructure:"port" json:"port"`
	// Enable turns the admin server on. It is off by default because it
	// exposes usage data and must be deliberately started.
	Enable bool `mapstructure:"enable" json:"enable"`
	// SessionTTLMinutes bounds how long a login lasts.
	SessionTTLMinutes int `mapstructure:"session_ttl_minutes" json:"session_ttl_minutes"`
	// MetricsPath is where the Prometheus exposition is served.
	MetricsPath string `mapstructure:"metrics_path" json:"metrics_path"`
	// MetricsEnable turns the /metrics endpoint on.
	MetricsEnable bool `mapstructure:"metrics_enable" json:"metrics_enable"`
}

// SessionTTL returns how long an admin login stays valid.
func (c ServerConfig) SessionTTL() time.Duration {
	mins := c.SessionTTLMinutes
	if mins <= 0 {
		mins = DefaultSessionTTLMinutes
	}
	return time.Duration(mins) * time.Minute
}

// DefaultSessionTTLMinutes bounds an admin login when unset.
const DefaultSessionTTLMinutes = 720

// PricingConfig configures cost estimation for LLM usage.
type PricingConfig struct {
	// Fallback is used when no entry in Models matches a call.
	Fallback PricingRate `mapstructure:"fallback" json:"fallback"`
	// Models maps a key to a rate. Keys may be "provider/model", "model" or
	// "provider"; the most specific match wins (Phase 5 price table).
	Models map[string]PricingRate `mapstructure:"models" json:"models"`
}

// PricingRate is the price of 1000 tokens in USD.
type PricingRate struct {
	PromptPer1K     float64 `mapstructure:"prompt_per_1k" json:"prompt_per_1k"`
	CompletionPer1K float64 `mapstructure:"completion_per_1k" json:"completion_per_1k"`
}

// LangfuseConfig configures Langfuse tracing.
type LangfuseConfig struct {
	// Enable turns tracing on. Tracing also requires Host and the API keys.
	Enable bool `mapstructure:"enable" json:"enable"`
	// Host is the Langfuse base URL, e.g. https://cloud.langfuse.com.
	Host string `mapstructure:"host" json:"host"`
	// PublicKey and SecretKey are the project API keys. Never commit these.
	PublicKey string `mapstructure:"public_key" json:"public_key"`
	SecretKey string `mapstructure:"secret_key" json:"secret_key"`
	// Environment and Release label every trace.
	Environment string `mapstructure:"environment" json:"environment"`
	Release     string `mapstructure:"release" json:"release"`
}

// AdminConfig configures admin authentication.
type AdminConfig struct {
	// Username is the single admin account (MVP: single-user password auth).
	Username string `mapstructure:"username" json:"username"`
	// PasswordHash is a bcrypt hash. Never commit a real hash; set it with
	// `huan-agent admin set-password` or the HUAN_ADMIN_PASSWORD_HASH env var.
	PasswordHash string `mapstructure:"password_hash" json:"password_hash"`
	// RequireLogin asks for the password before the admin API is usable.
	//
	// It defaults to FALSE because the admin UI is a single-user local console:
	// requiring a password you have to type on every restart is friction with no
	// benefit when the only way in is loopback.
	//
	// That reasoning holds only on loopback. The agent's tools can read, write
	// and execute, so a login-free admin reachable from another host would hand
	// shell access to anyone who can reach the port. Serve therefore REFUSES to
	// start with login disabled on a non-loopback host unless AllowInsecureBind
	// says otherwise.
	RequireLogin bool `mapstructure:"require_login" json:"require_login"`
	// TrustLoopback treats a request that arrives over the loopback interface as
	// already authenticated, so the password is asked of remote callers and of
	// nobody on this machine. It does nothing while RequireLogin is false: there
	// is no password to skip.
	//
	// It defaults to TRUE, which is the same judgement RequireLogin's default
	// makes — the admin API is a single-user console, and a caller already on
	// this machine is not what the password is for. What it gives up is
	// protection against other users and processes on the same host.
	//
	// The caveat is that "127.0.0.1" is also what a reverse proxy, an SSH tunnel
	// or a container port-forward looks like from the inside. Those clients are
	// local as far as the socket is concerned, so they skip the password too; set
	// this to false when the console is reached through one of them, or
	// RequireLogin protects nothing but the direct network path.
	TrustLoopback bool `mapstructure:"trust_loopback" json:"trust_loopback"`
	// AllowInsecureBind permits a login-free admin on a non-loopback address.
	// Only set it behind another authenticating layer (a reverse proxy, a VPN,
	// or an SSH tunnel): on its own it exposes command execution.
	AllowInsecureBind bool `mapstructure:"allow_insecure_bind" json:"allow_insecure_bind"`
}

// LoopbackHost reports whether host binds only to this machine. A blank host,
// "localhost", and any 127.x/::1 address count as loopback; "0.0.0.0" and "::"
// do not, because they listen on every interface.
func LoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" || strings.EqualFold(h, "localhost") {
		return true
	}
	if h == "::1" || strings.EqualFold(h, "[::1]") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	// An unresolvable name is treated as non-loopback: assuming the safe answer
	// is wrong here, since being wrong exposes command execution.
	return false
}

// LoggingConfig controls zap logger behavior.
type LoggingConfig struct {
	Level  string `mapstructure:"level" json:"level"`   // debug|info|warn|error
	Format string `mapstructure:"format" json:"format"` // json|console
}

// LLMConfig is a placeholder populated in Phase 1.
type LLMConfig struct {
	DefaultProvider string                 `mapstructure:"default_provider" json:"default_provider"`
	Providers       map[string]LLMProvider `mapstructure:"providers" json:"providers"`
	// AutoRefreshModels refreshes, at startup, the model list of every enabled
	// provider whose cached list is stale. It runs in the background, so a slow
	// provider delays nothing; a failure is recorded on the provider (last_error)
	// and never stops the pass or the server.
	AutoRefreshModels bool `mapstructure:"auto_refresh_models" json:"auto_refresh_models"`
	// ModelsCacheTTLHours is how long a fetched model list stays fresh. Beyond
	// it the list is "stale": the startup pass refetches it, and the console
	// offers a refresh. Zero uses DefaultModelsCacheTTLHours.
	ModelsCacheTTLHours int `mapstructure:"models_cache_ttl_hours" json:"models_cache_ttl_hours"`
	// Retry is how a single model call is retried when it fails for a reason
	// another attempt could survive (a dropped connection, a 429, a 5xx). A 400
	// or a 401 is never retried: see llm.IsRetryable.
	//
	// It applies to every caller of a model in this process — the web chat, the
	// Feishu bot, the CLI REPL, the context condenser, the title generator —
	// because being interrupted mid-task is not specific to one surface.
	Retry RetryConfig `mapstructure:"retry" json:"retry"`
}

// DefaultModelsCacheTTLHours is how long a fetched model list is trusted when
// the config does not say otherwise.
//
// A model list changes rarely (a provider adds a model), so refetching it more
// often than daily is traffic for nothing; a day also means a console that is
// opened every morning picks up a new model within one start.
const DefaultModelsCacheTTLHours = 24

// ModelsCacheTTL returns how long a fetched model list is considered fresh.
func (c LLMConfig) ModelsCacheTTL() time.Duration {
	hours := c.ModelsCacheTTLHours
	if hours <= 0 {
		hours = DefaultModelsCacheTTLHours
	}
	return time.Duration(hours) * time.Hour
}

// LLMProvider is a single LLM backend entry.
type LLMProvider struct {
	APIKey  string `mapstructure:"api_key" json:"api_key"`
	BaseURL string `mapstructure:"base_url" json:"base_url"`
	Model   string `mapstructure:"model" json:"model"`
	// DisableUsageRequest suppresses stream_options.include_usage, which
	// streaming needs in order to report token usage. Set it for a provider that
	// rejects that OpenAI extension; the cost is that streamed calls then report
	// no token counts.
	DisableUsageRequest bool `mapstructure:"disable_usage_request" json:"disable_usage_request"`
}

// AgentConfig configures the ReAct agent loop. Empty values fall back
// to internal defaults at construction time.
type AgentConfig struct {
	// MaxSteps caps model→tool→model iterations. 0 means default (12); the
	// agent package clamps the upper bound to agent.MaxStepsCeiling (200), which
	// is deliberately lower than chat.MaxStepsCeiling because the agent carries
	// a CLI's synchronous caller rather than one streaming turn.
	MaxSteps int `mapstructure:"max_steps" json:"max_steps"`
	// AllowedTools optionally restricts the registry to a subset. When
	// empty, all registered tools are exposed to the LLM.
	AllowedTools []string `mapstructure:"allowed_tools" json:"allowed_tools"`
}

// MCPConfig groups the MCP servers the agent should spawn at startup.
// Each entry mirrors mcp.ServerSpec; the package-level Servers list is
// keyed by display name for YAML readability.
type MCPConfig struct {
	Servers []MCPServer `mapstructure:"servers" json:"servers"`
}

// MCPServer describes a single MCP server.
//
// A stdio server (the default, and what every entry written before the console
// grew an MCP tab is) spawns Command with Args and Env. An sse or http server
// dials URL with Headers instead. The entries declared here are mirrored into
// the database at startup so the console can list them; they are re-synced on
// every start, which is why their console rows are read-only apart from
// enabled.
type MCPServer struct {
	Name string `mapstructure:"name" json:"name"`
	// Transport is "stdio" (default), "sse" or "http".
	Transport string   `mapstructure:"transport" json:"transport"`
	Command   string   `mapstructure:"command" json:"command"`
	Args      []string `mapstructure:"args" json:"args"`
	// Env entries follow the "KEY=value" shape and are merged onto
	// os.Environ() at spawn.
	Env []string `mapstructure:"env" json:"env"`
	// URL is the base URL of an sse / http server.
	URL string `mapstructure:"url" json:"url"`
	// Headers are "Name: value" lines sent with every request to an sse / http
	// server, which is where a bearer token goes.
	Headers []string `mapstructure:"headers" json:"headers"`
	// Enabled defaults to true when absent. A disabled entry is kept in the
	// console's list (so it can be turned back on) but never connected.
	Enabled *bool `mapstructure:"enabled" json:"enabled,omitempty"`
}

// IsEnabled reports whether the server should be connected, treating an absent
// flag as enabled: a config entry that says nothing about being off is on,
// which is what it meant before the flag existed.
func (m MCPServer) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// EnabledValue returns the flag as a plain bool for storage.
func (m MCPServer) EnabledValue() bool { return m.IsEnabled() }

// ToolsConfig configures the filesystem and command tools that let the agent
// actually work on a codebase.
//
// SECURITY: these tools let an LLM read and write real files and run real
// commands. They are confined to Workspace, but a command's confinement is its
// working directory, not a guarantee about what it can reach. Anyone who can
// send the bot a message can therefore drive them — see docs/tools.md.
type ToolsConfig struct {
	// Workspace is the directory the file and command tools are confined to.
	// Relative paths resolve against the process working directory. Empty
	// still enables the tools, rooted at the process working directory.
	//
	// It is the root of the built-in "default" workspace: the one a scope that
	// has selected nothing falls back to, which is why an untouched deployment
	// behaves exactly as it did before named workspaces existed.
	Workspace string `mapstructure:"workspace" json:"workspace"`
	// WorkspacesDir is where named workspaces live: a workspace called "blog"
	// is the directory <WorkspacesDir>/blog. Empty uses a "workspaces"
	// directory beside the database file (see WorkspacesDirOrDefault).
	//
	// It is deliberately separate from Workspace: Workspace points the default
	// at a project that already exists, while this one is a container the agent
	// creates directories inside, by name.
	WorkspacesDir string `mapstructure:"workspaces_dir" json:"workspaces_dir"`
	// ReadOnly forbids writing files and running commands, leaving the agent
	// able to read and search but not change anything.
	ReadOnly bool `mapstructure:"read_only" json:"read_only"`
	// EnableBash turns on the shell tool.
	EnableBash bool `mapstructure:"enable_bash" json:"enable_bash"`
	// BashTimeoutSeconds bounds one command. 0 uses the tool default (120s).
	BashTimeoutSeconds int `mapstructure:"bash_timeout_seconds" json:"bash_timeout_seconds"`
	// BashMaxTimeoutSeconds is the ceiling a single call may raise its own
	// timeout to with timeout_ms: a long build or test run may ask for more
	// time, but not for an unbounded amount. 0 uses the default (900s).
	BashMaxTimeoutSeconds int `mapstructure:"bash_max_timeout_seconds" json:"bash_max_timeout_seconds"`
	// EnableBackground turns on the background-process tools (bash_background,
	// bash_jobs, bash_output, bash_stop): dev servers, watch builds, resident
	// APIs and databases. It only matters with EnableBash on, because starting
	// one is still running a command.
	EnableBackground bool `mapstructure:"enable_background" json:"enable_background"`
	// BackgroundDir is where background job logs are written. Empty puts a
	// "jobs" directory beside the database file (see JobsDirOrDefault).
	BackgroundDir string `mapstructure:"background_dir" json:"background_dir"`
	// BackgroundMaxJobs caps how many background jobs may run at once. 0 uses
	// the default (8).
	BackgroundMaxJobs int `mapstructure:"background_max_jobs" json:"background_max_jobs"`
	// BackgroundLogMaxMB caps one job's log file. 0 uses the default (8 MiB).
	BackgroundLogMaxMB int `mapstructure:"background_log_max_mb" json:"background_log_max_mb"`
	// BackgroundWindowKB caps the in-memory tail of one job's output that a
	// reader is served from. 0 uses the default (256 KiB).
	BackgroundWindowKB int `mapstructure:"background_window_kb" json:"background_window_kb"`
	// BackgroundStopGraceSeconds is how long a stopping job gets between SIGTERM
	// and SIGKILL. 0 uses the default (5s).
	BackgroundStopGraceSeconds int `mapstructure:"background_stop_grace_seconds" json:"background_stop_grace_seconds"`
	// MaxReadKB caps one file read. 0 uses the default (512 KiB).
	MaxReadKB int `mapstructure:"max_read_kb" json:"max_read_kb"`
	// MaxWriteMB caps one file write. 0 uses the default (4 MiB).
	MaxWriteMB int `mapstructure:"max_write_mb" json:"max_write_mb"`
	// MaxListEntries caps a listing or glob result. 0 uses the default (500).
	MaxListEntries int `mapstructure:"max_list_entries" json:"max_list_entries"`
	// MaxParallel is how many parallel-safe tool calls from one model reply may
	// run at once. 0 or 1 means one at a time, which is what every deployment got
	// before this option existed.
	//
	// Only tools that declared themselves ParallelSafe may overlap at all, and
	// writes and commands are barriers, so this is a bound on I/O concurrency
	// rather than on what the model can do at once.
	MaxParallel int `mapstructure:"max_parallel" json:"max_parallel"`
	// DenyPatterns are RE2 regexes that refuse a matching command. A speed bump
	// for obviously destructive commands, NOT a security boundary: an LLM can
	// trivially write an equivalent command that does not match.
	DenyPatterns []string `mapstructure:"deny_patterns" json:"deny_patterns"`
	// LSP configures the language servers behind the code-intelligence tools
	// (diagnostics, go-to-definition, references, workspace symbols) and the
	// diagnostics that are attached to what the editing tools return.
	LSP LSPConfig `mapstructure:"lsp" json:"lsp"`
	// Approval gates write and exec tool calls behind a decision by a person.
	Approval ApprovalConfig `mapstructure:"approval" json:"approval"`
	// Web configures reading pages from the internet.
	Web WebConfig `mapstructure:"web" json:"web"`
	// Checkpoint keeps the pre-image of every file a turn changes, so a turn can
	// be undone without git.
	Checkpoint CheckpointConfig `mapstructure:"checkpoint" json:"checkpoint"`
	// Artifacts configures where the resources the agent produces (pages,
	// documents, images) are stored and who may fetch them.
	Artifacts ArtifactsConfig `mapstructure:"artifacts" json:"artifacts"`
}

// WebConfig configures the fetch_url tool.
type WebConfig struct {
	// Enable registers fetch_url.
	Enable bool `mapstructure:"enable" json:"enable"`
	// AllowPrivate permits loopback, private and link-local addresses.
	//
	// Off by default, and the default is the security-relevant choice: the
	// addresses it permits include the cloud metadata endpoint (169.254.169.254,
	// which hands out instance credentials) and whatever else is listening on the
	// host — including this agent's own console and its MCP servers.
	AllowPrivate bool `mapstructure:"allow_private" json:"allow_private"`
	// TimeoutSeconds bounds one request.
	TimeoutSeconds int `mapstructure:"timeout_seconds" json:"timeout_seconds"`
	// MaxKB bounds the response body.
	MaxKB int `mapstructure:"max_kb" json:"max_kb"`
	// MaxChars bounds how much of one page a single call returns.
	MaxChars int `mapstructure:"max_chars" json:"max_chars"`
	// CacheTTLSeconds bounds the in-process cache. 0 disables it.
	CacheTTLSeconds int `mapstructure:"cache_ttl_seconds" json:"cache_ttl_seconds"`
	// UserAgent identifies this client.
	UserAgent string `mapstructure:"user_agent" json:"user_agent"`
}

// Defaults for the web section.
const (
	DefaultWebTimeoutSeconds = 20
	DefaultWebMaxKB          = 2048
	DefaultWebMaxChars       = 20000
	DefaultWebCacheTTL       = 300
)

// Timeout returns the request timeout.
func (c WebConfig) Timeout() time.Duration {
	secs := c.TimeoutSeconds
	if secs <= 0 {
		secs = DefaultWebTimeoutSeconds
	}
	return time.Duration(secs) * time.Second
}

// MaxBytes returns the body limit.
func (c WebConfig) MaxBytes() int64 {
	kb := c.MaxKB
	if kb <= 0 {
		kb = DefaultWebMaxKB
	}
	return int64(kb) << 10
}

// MaxCharsOr returns the per-call window.
func (c WebConfig) MaxCharsOr() int {
	if c.MaxChars > 0 {
		return c.MaxChars
	}
	return DefaultWebMaxChars
}

// CacheTTL returns the cache lifetime; a negative configured value disables it.
func (c WebConfig) CacheTTL() time.Duration {
	if c.CacheTTLSeconds < 0 {
		return 0
	}
	secs := c.CacheTTLSeconds
	if secs == 0 {
		secs = DefaultWebCacheTTL
	}
	return time.Duration(secs) * time.Second
}

// SubagentConfig configures the subagents a turn may spawn.
//
// Everything here is a bound on cost rather than a feature switch, because the
// feature is the risk: a subagent is a nested model call sequence that the parent
// pays for, and an unbounded one is a budget landmine rather than a convenience.
type SubagentConfig struct {
	// Enable registers spawn_agent. Off means the tool is not on the menu at all.
	Enable bool `mapstructure:"enable" json:"enable"`
	// MaxSteps bounds a nested run's iterations. 0 — the default — means "as many
	// as the turn that spawned it", so a subagent inherits the conversation's own
	// budget (chat.max_steps, including a console override) instead of a number
	// chosen here.
	//
	// It is deliberately the same budget rather than a smaller one. A subagent's
	// job is reconnaissance or a self-contained sub-task, and both need room: the
	// fixed 8 this used to default to left two real reconnaissance spawns
	// truncated mid-sentence, after which the parent did the work itself — the
	// delegation cost a call and saved nothing.
	//
	// A positive value caps every nested run at that number instead. It is a way
	// to make subagents cheaper, not a way to make them work.
	MaxSteps int `mapstructure:"max_steps" json:"max_steps"`
	// MaxConcurrent bounds how many subagents run at once across the whole
	// process. 0 uses the default (2).
	//
	// Process-wide on purpose: "spawn three explorations" is the intended use, and
	// four parents each spawning four is how a fan-out becomes a bill.
	MaxConcurrent int `mapstructure:"max_concurrent" json:"max_concurrent"`
	// MaxReportChars truncates a report. 0 uses the default (8000).
	MaxReportChars int `mapstructure:"max_report_chars" json:"max_report_chars"`
}

// Defaults for the subagent section.
const (
	DefaultSubagentMaxSteps       = 8
	DefaultSubagentMaxConcurrent  = 2
	DefaultSubagentMaxReportChars = 8000
)

// MaxStepsOr returns the nested step cap: the configured one, or inherit when
// the configuration says nothing.
//
// inherit is what the caller can offer instead — the parent turn's budget for a
// spawn inside a turn, or the deployment's chat cap for a caller that has no turn
// in hand. A zero inherit falls back to DefaultSubagentMaxSteps, so a nested run
// is never open-ended by accident.
func (c SubagentConfig) MaxStepsOr(inherit int) int {
	if c.MaxSteps > 0 {
		return c.MaxSteps
	}
	if inherit > 0 {
		return inherit
	}
	return DefaultSubagentMaxSteps
}

// MaxConcurrentOr returns the process-wide concurrency bound.
func (c SubagentConfig) MaxConcurrentOr() int {
	if c.MaxConcurrent > 0 {
		return c.MaxConcurrent
	}
	return DefaultSubagentMaxConcurrent
}

// MaxReportCharsOr returns the report truncation limit.
func (c SubagentConfig) MaxReportCharsOr() int {
	if c.MaxReportChars > 0 {
		return c.MaxReportChars
	}
	return DefaultSubagentMaxReportChars
}

// CheckpointConfig configures the file-level checkpoints.
type CheckpointConfig struct {
	// Enable turns checkpointing on. It defaults to on: the cost is a copy of
	// each file a turn touches, and the benefit is that a wrong turn is one click
	// away from being undone rather than one `git` command the model had to
	// remember to run first.
	Enable bool `mapstructure:"enable" json:"enable"`
	// Dir is where checkpoints are written. Empty puts a "checkpoints" directory
	// beside the database file (see CheckpointDirOrDefault).
	Dir string `mapstructure:"dir" json:"dir"`
	// KeepTurns bounds how many turns per conversation are kept. 0 uses the
	// default (20).
	KeepTurns int `mapstructure:"keep_turns" json:"keep_turns"`
	// MaxTotalMB bounds the whole checkpoint directory. 0 uses the default (512).
	MaxTotalMB int `mapstructure:"max_total_mb" json:"max_total_mb"`
}

// DefaultCheckpointSubdir is where checkpoints are written, relative to the
// database file, when tools.checkpoint.dir is empty.
const DefaultCheckpointSubdir = "checkpoints"

// Defaults for the checkpoint section.
const (
	DefaultCheckpointKeepTurns  = 20
	DefaultCheckpointMaxTotalMB = 512
)

// CheckpointDirOrDefault resolves where checkpoints are written.
//
// Beside the database for the same reason job logs are: a checkpoint written into
// the workspace would show up in the model's own greps and in the user's git
// status, and it is the deployment's data rather than the project's.
func (c ToolsConfig) CheckpointDirOrDefault(databasePath string) (string, bool) {
	if dir := strings.TrimSpace(c.Checkpoint.Dir); dir != "" {
		return dir, true
	}
	return dirBesideDatabase(databasePath, DefaultCheckpointSubdir), true
}

// KeepTurnsOr returns the per-conversation turn cap.
func (c CheckpointConfig) KeepTurnsOr() int {
	if c.KeepTurns > 0 {
		return c.KeepTurns
	}
	return DefaultCheckpointKeepTurns
}

// MaxTotalMBOr returns the directory size cap.
func (c CheckpointConfig) MaxTotalMBOr() int {
	if c.MaxTotalMB > 0 {
		return c.MaxTotalMB
	}
	return DefaultCheckpointMaxTotalMB
}

// ArtifactsConfig configures the artifact store: the resources the agent makes
// that are not code and not project documentation — a generated page, a report,
// a screenshot — written to the server's disk and served back over a URL.
//
// It is deliberately separate from the workspace. An artifact is not a file the
// agent edited in the project; it is a deliverable, and the console has to be able
// to fetch it later. Putting it under the workspace would also make it show up in
// the model's own greps and in the user's git status, which is exactly what it is
// not.
type ArtifactsConfig struct {
	// Enable registers save_artifact and serves the artifact routes. It defaults
	// to on: the store is one directory beside the database, the tool is bounded
	// by MaxBytes, and a deployment that cannot store an artifact cannot show the
	// user the page it just wrote.
	Enable bool `mapstructure:"enable" json:"enable"`
	// Root is the directory artifacts are written to. Empty puts an "artifacts"
	// directory beside the database file (see ArtifactsRootOrDefault).
	Root string `mapstructure:"root" json:"root"`
	// PublicURLs serves artifact files without a console login, which is what a
	// link shared with somebody who has no account needs.
	//
	// Off by default, and the default is the safe one: artifact bytes are
	// whatever the model wrote and they are served from the console's own origin,
	// so an unauthenticated artifact URL is stored XSS against whoever opens it.
	// On is a deliberate choice by an operator who knows the store holds nothing
	// private and wants the link to work for outsiders.
	PublicURLs bool `mapstructure:"public_urls" json:"public_urls"`
	// MaxBytes caps one artifact. 0 uses the default (32 MiB).
	MaxBytes int64 `mapstructure:"max_bytes" json:"max_bytes"`
}

// DefaultArtifactsSubdir is where artifacts are written, relative to the database
// file, when tools.artifacts.root is empty.
const DefaultArtifactsSubdir = "artifacts"

// ArtifactsRootOrDefault resolves where artifacts are written.
//
// Beside the database for the same reason job logs and checkpoints are: the store
// is the deployment's data, not the project's.
func (c ToolsConfig) ArtifactsRootOrDefault(databasePath string) (string, bool) {
	if root := strings.TrimSpace(c.Artifacts.Root); root != "" {
		return root, true
	}
	return dirBesideDatabase(databasePath, DefaultArtifactsSubdir), true
}

// ApprovalConfig configures the approval gate.
//
// The default is off, and that is a deliberate choice rather than an unfinished
// one: turning a gate on by default would take write access away from the
// deployments nobody is watching (a CI run, the bot, a cron job), which is a
// worse surprise than not having a gate. Turning it on is a decision an operator
// makes once, in the knowledge that some surfaces then lose their write tools.
type ApprovalConfig struct {
	// Mode selects what needs approval: "off" (the default), "writes",
	// "writes+exec" or "all".
	Mode string `mapstructure:"mode" json:"mode"`
	// Allow is a list of RE2 patterns matched against a request's summary; a match
	// means the call runs without asking. Like tools.deny_patterns it is a
	// convenience, not a boundary — but here the default is to ask, so a missed
	// pattern costs a prompt instead of a disaster.
	Allow []string `mapstructure:"allow" json:"allow"`
	// TimeoutSeconds bounds one request. 0 uses the default (300). A timeout is a
	// refusal: the gate holds when nobody is watching, which is when it matters.
	TimeoutSeconds int `mapstructure:"timeout_seconds" json:"timeout_seconds"`
}

// DefaultApprovalTimeout is how long one request waits for a decision.
//
// Five minutes is shorter than the ask_user timeout on purpose: a question is the
// model gathering information, while an approval is a decision about an action
// that is otherwise blocked — leaving that hanging for ten minutes holds a turn
// and a half-finished tool call for no benefit.
const DefaultApprovalTimeout = 5 * time.Minute

// ModeOr returns the configured mode, defaulting to off.
func (c ApprovalConfig) ModeOr() string {
	if strings.TrimSpace(c.Mode) == "" {
		return "off"
	}
	return strings.ToLower(strings.TrimSpace(c.Mode))
}

// Timeout returns how long one request waits.
func (c ApprovalConfig) Timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return DefaultApprovalTimeout
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// LSPConfig configures code intelligence.
//
// Every field has a default that makes the feature work unconfigured on a Go
// repository, and every failure it can produce is non-fatal by design: a language
// server is an external binary that may not be installed, and the agent has to
// keep working without one.
type LSPConfig struct {
	// Enable turns the whole capability on. With it off, no process is started,
	// no code-intelligence tool is registered, and the editing tools return
	// exactly what they returned before this feature existed.
	Enable bool `mapstructure:"enable" json:"enable"`
	// AttachDiagnostics appends the diagnostics for a file to the result of the
	// write or edit that changed it. This is the half of the feature that
	// shortens the fix-a-type-error loop, so it defaults on.
	AttachDiagnostics bool `mapstructure:"attach_diagnostics" json:"attach_diagnostics"`
	// DiagnosticsWaitMS bounds how long an edit waits for the server to catch up.
	// The edit itself never depends on it: on timeout the result says the
	// diagnostics were not ready. 0 uses the default (2000).
	DiagnosticsWaitMS int `mapstructure:"diagnostics_wait_ms" json:"diagnostics_wait_ms"`
	// IdleTimeoutSeconds reaps a language server that has not been used for this
	// long. 0 uses the default (600). A negative value disables reaping, which is
	// only sensible for a short-lived process.
	IdleTimeoutSeconds int `mapstructure:"idle_timeout_seconds" json:"idle_timeout_seconds"`
	// MaxDiagnostics caps how many diagnostics are attached to an edit result.
	// The total is always reported, so truncation is never silent. 0 uses the
	// default (20).
	MaxDiagnostics int `mapstructure:"max_diagnostics" json:"max_diagnostics"`
	// Servers declares the language servers. Empty uses the built-in table
	// (gopls for Go), which is what makes `tools.lsp.enable: true` do the obvious
	// thing on a Go repository.
	Servers []LSPServerConfig `mapstructure:"servers" json:"servers"`
}

// LSPServerConfig is one language server.
type LSPServerConfig struct {
	Name    string   `mapstructure:"name" json:"name"`
	Command string   `mapstructure:"command" json:"command"`
	Args    []string `mapstructure:"args" json:"args"`
	Env     []string `mapstructure:"env" json:"env"`
	// Languages are the file extensions or language ids this server handles
	// (".go", "go", "typescript"). Both spellings work.
	Languages []string `mapstructure:"languages" json:"languages"`
	// RootMarkers are the files whose nearest ancestor becomes the server's root
	// ("go.mod", "package.json"). This decides which project gets indexed.
	RootMarkers []string `mapstructure:"root_markers" json:"root_markers"`
	// InitOptions is passed through as initializationOptions.
	InitOptions map[string]any `mapstructure:"init_options" json:"init_options"`
	// Enabled defaults to true when absent.
	Enabled *bool `mapstructure:"enabled" json:"enabled,omitempty"`
}

// Defaults for the LSP section.
const (
	DefaultLSPDiagnosticsWaitMS = 2000
	DefaultLSPIdleTimeoutSecs   = 600
	DefaultLSPMaxDiagnostics    = 20
	// DisableLSPReaping is the IdleTimeoutSeconds value that turns reaping off.
	DisableLSPReaping = -1
)

// DiagnosticsWait returns how long an edit waits for diagnostics.
func (c LSPConfig) DiagnosticsWait() time.Duration {
	ms := c.DiagnosticsWaitMS
	if ms <= 0 {
		ms = DefaultLSPDiagnosticsWaitMS
	}
	return time.Duration(ms) * time.Millisecond
}

// IdleTimeout returns how long a server may sit unused, or 0 for "never reap".
func (c LSPConfig) IdleTimeout() time.Duration {
	if c.IdleTimeoutSeconds == DisableLSPReaping {
		return 0
	}
	secs := c.IdleTimeoutSeconds
	if secs <= 0 {
		secs = DefaultLSPIdleTimeoutSecs
	}
	return time.Duration(secs) * time.Second
}

// MaxDiagnosticsOr returns the attachment cap.
func (c LSPConfig) MaxDiagnosticsOr() int {
	if c.MaxDiagnostics > 0 {
		return c.MaxDiagnostics
	}
	return DefaultLSPMaxDiagnostics
}

// DefaultMaxParallel is how many parallel-safe tool calls run at once when the
// deployment does not say otherwise.
//
// Four is a guess with a reason: a step that reads five files is the common case
// this exists for, and four overlapping reads already collapse the latency to one
// read plus a little. Higher would mostly open more file descriptors at once.
const DefaultMaxParallel = 4

// MaxParallelOr returns the configured bound, or the default.
func (c ToolsConfig) MaxParallelOr() int {
	if c.MaxParallel > 0 {
		return c.MaxParallel
	}
	return DefaultMaxParallel
}

// DefaultDenyPatterns are refuse-on-match patterns for commands that are almost
// never what a coding agent means and are catastrophic when they are.
var DefaultDenyPatterns = []string{
	`rm\s+(-[a-zA-Z]+\s+)*/\s*$`,
	`mkfs(\.|\s)`,
	`dd\s+[^|]*of=/dev/(disk|sd|nvme|hd)`,
	`>\s*/dev/(disk|sd|nvme|hd)`,
	`:\(\)\s*\{.*\};\s*:`,
	`chmod\s+-R\s+777\s+/\s*$`,
	// Anchored to a command position, so "echo reboot-status" or a commit
	// message mentioning a reboot is not refused while `reboot` is.
	`(^|[;&|]\s*)(sudo\s+)?(shutdown|reboot|halt|poweroff)\b`,
}

// WorkspaceOrDefault returns the configured workspace root, or the process
// working directory when unset. A second return of false means no root could
// be determined.
func (c ToolsConfig) WorkspaceOrDefault() (string, bool) {
	if strings.TrimSpace(c.Workspace) != "" {
		return c.Workspace, true
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	return wd, true
}

// DefaultWorkspacesSubdir is the directory named workspaces are created under,
// relative to the database file, when tools.workspaces_dir is empty.
const DefaultWorkspacesSubdir = "workspaces"

// DefaultJobsSubdir is the directory background job logs are written to,
// relative to the database file, when tools.background_dir is empty.
const DefaultJobsSubdir = "jobs"

// dirBesideDatabase resolves a directory next to the database file.
//
// The rule is literally "beside the database file": for `database.path: huan.db`
// that is the current directory, and only an empty path falls back to ./data,
// which is where the default database path points anyway. Keeping data the agent
// produces next to the data it belongs to is what stops a deployment's
// directories from depending on wherever the process happened to be started.
func dirBesideDatabase(databasePath, subdir string) string {
	if strings.TrimSpace(databasePath) == "" {
		databasePath = filepath.Join("data", "huan-agent.db")
	}
	base := filepath.Dir(databasePath)
	if strings.TrimSpace(base) == "" {
		base = "data"
	}
	return filepath.Join(base, subdir)
}

// JobsDirOrDefault resolves where background job logs are written.
//
// A job's log is the durable record of what a process printed, so it lives with
// the deployment's data rather than in the workspace it happened to run in: a log
// dropped into a repository would show up in the model's own greps and in the
// user's git status.
func (c ToolsConfig) JobsDirOrDefault(databasePath string) (string, bool) {
	if dir := strings.TrimSpace(c.BackgroundDir); dir != "" {
		return dir, true
	}
	return dirBesideDatabase(databasePath, DefaultJobsSubdir), true
}

// BackgroundStopGrace returns the SIGTERM-to-SIGKILL grace for a stopping job.
func (c ToolsConfig) BackgroundStopGrace() time.Duration {
	if c.BackgroundStopGraceSeconds <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.BackgroundStopGraceSeconds) * time.Second
}

// WorkspacesDirOrDefault resolves where named workspaces are created.
//
// A configured value is used as given, relative to the process working
// directory; otherwise it is the DefaultWorkspacesSubdir beside the database
// file (see dirBesideDatabase). A second return of false means no directory
// could be determined.
func (c ToolsConfig) WorkspacesDirOrDefault(databasePath string) (string, bool) {
	if dir := strings.TrimSpace(c.WorkspacesDir); dir != "" {
		return dir, true
	}
	return dirBesideDatabase(databasePath, DefaultWorkspacesSubdir), true
}

// BashTimeout returns the configured command timeout.
func (c ToolsConfig) BashTimeout() time.Duration {
	if c.BashTimeoutSeconds <= 0 {
		return 120 * time.Second
	}
	return time.Duration(c.BashTimeoutSeconds) * time.Second
}

// DefaultBashMaxTimeoutSeconds is the ceiling for a per-call bash timeout when
// the configuration does not set one. It mirrors builtin.DefaultBashMaxTimeout —
// fifteen minutes, which covers a cold dependency install or a full test suite
// without leaving a wedged command holding a turn open for an hour.
const DefaultBashMaxTimeoutSeconds = 900

// BashMaxTimeout returns the ceiling a single command may ask for with
// timeout_ms.
//
// A ceiling below the configured default is raised to it rather than honoured:
// capping the default would make the ordinary case unreachable, and an operator
// who wants a shorter default can lower bash_timeout_seconds itself.
func (c ToolsConfig) BashMaxTimeout() time.Duration {
	def := c.BashTimeout()
	if c.BashMaxTimeoutSeconds <= 0 {
		max := time.Duration(DefaultBashMaxTimeoutSeconds) * time.Second
		if max < def {
			return def
		}
		return max
	}
	if max := time.Duration(c.BashMaxTimeoutSeconds) * time.Second; max > def {
		return max
	}
	return def
}

// Limits converts the size settings into limits, applying defaults for
// anything unset.
func (c ToolsConfig) Limits() (maxRead, maxWrite int64, maxList int) {
	maxRead = int64(c.MaxReadKB) << 10
	if maxRead <= 0 {
		maxRead = 512 << 10
	}
	maxWrite = int64(c.MaxWriteMB) << 20
	if maxWrite <= 0 {
		maxWrite = 4 << 20
	}
	maxList = c.MaxListEntries
	if maxList <= 0 {
		maxList = 500
	}
	return maxRead, maxWrite, maxList
}

// DenyOrDefault returns the configured deny patterns, falling back to the
// built-in set when none are configured.
func (c ToolsConfig) DenyOrDefault() []string {
	if len(c.DenyPatterns) > 0 {
		return c.DenyPatterns
	}
	return DefaultDenyPatterns
}

// SkillsConfig configures the on-disk skill loader.
type SkillsConfig struct {
	// Dir is the directory scanned for *.md skill files. Empty disables
	// skill loading entirely.
	Dir string `mapstructure:"dir" json:"dir"`
}

// MemoryConfig configures short/long-term memory.
type MemoryConfig struct {
	// Dir is the root directory where long-term memory files (JSONL) are stored.
	Dir string `mapstructure:"dir" json:"dir"`
	// Enable turns memory on/off. When false the chat loop keeps no memory.
	Enable bool `mapstructure:"enable" json:"enable"`
	// MaxTurns is the short-term buffer cap (most recent N turns kept in memory).
	MaxTurns int `mapstructure:"max_turns" json:"max_turns"`
}

// ContextConfig configures the LLM context-window budget and compression.
type ContextConfig struct {
	// MaxTokens is the ceiling for the assembled window.
	//
	// The three cases matter, because the default is the interesting one:
	//
	//   - 0 (default): derive the ceiling from the model's own context window —
	//     window × WindowRatio − ReserveOutputTokens, with the window taken from
	//     ModelWindows, the built-in table, or DefaultWindow. This is what a
	//     deployment wants when one console serves several models: 60000 throws
	//     away most of a 200k window and overflows a 32k one.
	//   - > 0: a fixed ceiling, exactly as configured. It wins over everything
	//     above, because an operator who wrote a number meant it.
	//   - < 0: compression off. The whole history is resent every step, which is
	//     only affordable for short turns.
	MaxTokens int `mapstructure:"max_tokens" json:"max_tokens"`
	// WindowRatio is how much of the model's window the history may fill when
	// MaxTokens is 0. 0 uses the built-in default (0.7). The remainder absorbs
	// the system prompt, the tool schemas, and the error in the token estimate.
	WindowRatio float64 `mapstructure:"window_ratio" json:"window_ratio"`
	// ReserveOutputTokens is held back from the window for the model's own
	// output — a reasoning model spends it before writing a word of the answer.
	// 0 uses the built-in default; a negative value reserves nothing.
	ReserveOutputTokens int `mapstructure:"reserve_output_tokens" json:"reserve_output_tokens"`
	// DefaultWindow is the context window assumed for a model the built-in table
	// does not know. 0 uses the built-in default (128k). Guessing a mainstream
	// size is the right failure: too large costs a retryable provider error,
	// too small costs money on every step of every turn.
	DefaultWindow int `mapstructure:"default_window" json:"default_window"`
	// ModelWindows overrides the window for models the table gets wrong — a
	// gateway that truncates earlier than the vendor does, a private deployment
	// with a smaller limit. A key matches as a case-insensitive substring of the
	// model id (the longest matching key wins), so "deepseek" covers
	// deepseek/deepseek-v4.1-flash without naming it.
	ModelWindows map[string]int `mapstructure:"model_windows" json:"model_windows"`
	// ToolResultMaxChars bounds one tool result *as the model sees it*.
	//
	// It is the other half of the window budget: a single command that dumps
	// 128 KiB used to arrive in the window whole, which fires a compression
	// every couple of steps and folds away the work in progress — the failure
	// this bound exists to prevent. The result stored for the conversation and
	// shown in the console is never truncated; only the copy replayed to the
	// model is. 0 uses the built-in default; a negative value does not bound it.
	ToolResultMaxChars int `mapstructure:"tool_result_max_chars" json:"tool_result_max_chars"`
	// KeepRecent is the number of most recent messages retained verbatim
	// after a compression pass.
	KeepRecent int `mapstructure:"keep_recent" json:"keep_recent"`
	// Summarize hooks the LLM itself to summarize the rolled-up older turns.
	// When true, a summarizer is wired into the agent loop (if a model is
	// available). Otherwise older turns are dropped with a placeholder.
	Summarize bool `mapstructure:"summarize" json:"summarize"`
}

// WindowSpecFor turns the section into the resolver the runner uses.
//
// It exists so the three ways of saying "how big is the window" travel together:
// a caller that resolved the cap itself from cfg.Context.MaxTokens would ignore
// the ratio, the reserve, and the per-model overrides — which is precisely the
// bug that made a fixed 60000 the answer for every model.
func (c ContextConfig) WindowSpecFor() context.WindowSpec {
	return context.WindowSpec{
		MaxTokens: c.MaxTokens,
		Ratio:     c.WindowRatio,
		Reserve:   c.ReserveOutputTokens,
		Default:   c.DefaultWindow,
		Overrides: c.ModelWindows,
	}
}

// DefaultToolResultMaxChars is the bound on one tool result as the model sees
// it, mirroring context.DefaultToolResultMaxChars (see the guard constants above
// for why it is spelled twice).
func DefaultToolResultMaxChars() int { return context.DefaultToolResultMaxChars }

// DefaultGuardConfig is the guard configuration a deployment gets when it says
// nothing.
func DefaultGuardConfig() GuardConfig {
	return GuardConfig{
		Enable:         true,
		RepeatNudge:    DefaultGuardRepeatNudge,
		RepeatStop:     DefaultGuardRepeatStop,
		IdleNudgeSteps: DefaultGuardIdleNudgeSteps,
		IdleStopSteps:  DefaultGuardIdleStopSteps,
		MaxSteers:      DefaultGuardMaxSteers,
		RereadNudge:    DefaultGuardRereadNudge,
	}
}

// FixedCap is the explicit ceiling, or 0 when the ceiling is derived from the
// model. Callers that have no model to resolve against — the CLI's memory
// window, which compresses a conversation rather than a turn — use it directly.
func (c ContextConfig) FixedCap() int {
	if c.MaxTokens <= 0 {
		return 0
	}
	return c.MaxTokens
}

// FeishuConfig configures the Feishu (Lark) IM bot integration. Multiple
// apps can be declared under Apps (keyed by name) and the active one picked
// via Active (mirroring llm.providers / llm.default_provider). The legacy
// single-app fields (AppID/AppSecret/...) are still honoured when no app is
// selected. Leave the active app's AppID empty to disable the bot.
type FeishuConfig struct {
	// Active selects which named app from Apps is used. Empty falls back to
	// the legacy AppID/AppSecret (or the first entry if the legacy fields are
	// also empty and len(Apps) > 0).
	Active string `mapstructure:"active" json:"active"`
	// Apps maps a display name to a Feishu app.
	Apps map[string]FeishuApp `mapstructure:"apps" json:"apps"`

	// Thinking sends a "thinking…" card immediately and replaces it with the
	// answer once the model finishes, so the user always gets feedback.
	Thinking bool `mapstructure:"thinking" json:"thinking"`
	// RetryAttempts is the total number of outbound send attempts (>=1).
	RetryAttempts int `mapstructure:"retry_attempts" json:"retry_attempts"`
	// RetryBaseDelayMS is the first backoff delay in milliseconds.
	RetryBaseDelayMS int `mapstructure:"retry_base_delay_ms" json:"retry_base_delay_ms"`
	// SendTimeoutMS bounds a single outbound send attempt.
	SendTimeoutMS int `mapstructure:"send_timeout_ms" json:"send_timeout_ms"`
	// RateLimitPerSec caps outbound sends per second (0 = unlimited).
	RateLimitPerSec float64 `mapstructure:"rate_limit_per_sec" json:"rate_limit_per_sec"`
	// RateLimitBurst is the token-bucket burst size.
	RateLimitBurst int `mapstructure:"rate_limit_burst" json:"rate_limit_burst"`
	// DownloadDir stores inbound attachments. Empty uses a directory under the
	// system temp dir.
	DownloadDir string `mapstructure:"download_dir" json:"download_dir"`
	// MaxDownloadMB caps a single downloaded attachment.
	MaxDownloadMB int `mapstructure:"max_download_mb" json:"max_download_mb"`
	// EnableTools gives the bot the file, search and command tools, confined to
	// the workspace the sender has selected (see 设置 → 工作区).
	//
	// SECURITY: with this on, anyone who can send the bot a message can read,
	// write and execute inside a workspace, and a command can still reach
	// absolute paths elsewhere on the machine. That is the same exposure the web
	// console has; the difference is who can reach it. Turn it off to keep the
	// bot conversational.
	EnableTools bool `mapstructure:"enable_tools" json:"enable_tools"`

	// Legacy single-app fields (still honoured when Active is empty).
	AppID             string `mapstructure:"app_id" json:"app_id"`
	AppSecret         string `mapstructure:"app_secret" json:"app_secret"`
	Domain            string `mapstructure:"domain" json:"domain"`
	VerificationToken string `mapstructure:"verification_token" json:"verification_token"`
	EncryptKey        string `mapstructure:"encrypt_key" json:"encrypt_key"`
}

// FeishuApp is a single Feishu application entry.
type FeishuApp struct {
	// AppID is the Feishu app's client id ("cli_xxx"). Empty disables it.
	AppID string `mapstructure:"app_id" json:"app_id"`
	// AppSecret is the Feishu app secret. Never commit real values.
	AppSecret string `mapstructure:"app_secret" json:"app_secret"`
	// Domain overrides the Feishu API domain. Empty uses the SDK default.
	Domain string `mapstructure:"domain" json:"domain"`
	// VerifyToken and EncryptKey are only needed for HTTP event/card
	// callbacks; WebSocket long-connection does not require them.
	VerificationToken string `mapstructure:"verification_token" json:"verification_token"`
	EncryptKey        string `mapstructure:"encrypt_key" json:"encrypt_key"`
	// Transport selects how events arrive: "websocket" (default) or
	// "callback". Aliases: ws / http / webhook.
	Transport string `mapstructure:"transport" json:"transport"`
	// CallbackAddr is the listen address for the callback transport.
	CallbackAddr string `mapstructure:"callback_addr" json:"callback_addr"`
	// CallbackPath is the event path for the callback transport.
	CallbackPath string `mapstructure:"callback_path" json:"callback_path"`
}

// DefaultFeishuDownloadSubdir is appended to the system temp dir when no
// download directory is configured.
const DefaultFeishuDownloadSubdir = "huan-agent-downloads"

// DefaultFeishuMaxDownloadMB caps a single downloaded attachment when unset.
const DefaultFeishuMaxDownloadMB = 32

// RetryBaseDelay returns the configured first backoff delay.
func (c FeishuConfig) RetryBaseDelay() time.Duration {
	if c.RetryBaseDelayMS <= 0 {
		return 300 * time.Millisecond
	}
	return time.Duration(c.RetryBaseDelayMS) * time.Millisecond
}

// SendTimeout returns the per-attempt send timeout (0 disables it).
func (c FeishuConfig) SendTimeout() time.Duration {
	if c.SendTimeoutMS <= 0 {
		return 0
	}
	return time.Duration(c.SendTimeoutMS) * time.Millisecond
}

// MaxDownloadBytes returns the per-attachment download cap.
func (c FeishuConfig) MaxDownloadBytes() int64 {
	mb := c.MaxDownloadMB
	if mb <= 0 {
		mb = DefaultFeishuMaxDownloadMB
	}
	return int64(mb) << 20
}

// ResolveDownloadDir returns the directory inbound attachments are stored in,
// falling back to a subdirectory of the system temp dir.
func (c FeishuConfig) ResolveDownloadDir() string {
	if strings.TrimSpace(c.DownloadDir) != "" {
		return c.DownloadDir
	}
	return filepath.Join(os.TempDir(), DefaultFeishuDownloadSubdir)
}

// Resolve returns the active Feishu app. When Active names an entry in Apps it
// is returned; otherwise it falls back to the legacy single-app fields.
func (c FeishuConfig) Resolve() FeishuApp {
	if c.Active != "" {
		if a, ok := c.Apps[c.Active]; ok {
			return a
		}
	}
	// Legacy flat fields (or first app if no flat values and Active empty).
	if a, ok := c.Apps[c.Active]; ok && c.Active != "" {
		return a
	}
	if c.AppID != "" || c.AppSecret != "" {
		return FeishuApp{
			AppID:             c.AppID,
			AppSecret:         c.AppSecret,
			Domain:            c.Domain,
			VerificationToken: c.VerificationToken,
			EncryptKey:        c.EncryptKey,
		}
	}
	// No explicit selection and no flat fields: pick the first entry.
	for _, a := range c.Apps {
		return a
	}
	return FeishuApp{}
}

// Enabled reports whether the active Feishu app has an AppID set.
func (c FeishuConfig) Enabled() bool { return c.Resolve().AppID != "" }

// Default returns the default configuration.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "127.0.0.1",
			Port: 8080,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "console",
		},
		LLM: LLMConfig{
			DefaultProvider: "",
			Providers:       map[string]LLMProvider{},
			// Refreshing is on by default: without it a provider added in the
			// console has an empty model list until someone clicks 刷新模型, and
			// the chat selector has nothing to offer.
			AutoRefreshModels:   true,
			ModelsCacheTTLHours: DefaultModelsCacheTTLHours,
		},
		Database: DatabaseConfig{
			Path: "./data/huan-agent.db",
		},
		Agent: AgentConfig{
			MaxSteps: 12,
		},
		MCP: MCPConfig{
			Servers: nil,
		},
		Skills: SkillsConfig{
			Dir: "./configs/skills",
		},
		Memory: MemoryConfig{
			Dir:      "./data/memory",
			Enable:   true,
			MaxTurns: 20,
		},
		Chat: ChatConfig{
			// Only the guard is defaulted here. The rest of chat.* is read
			// through viper (SetDefaults), and a caller that builds a Config with
			// Default() and no file must still get the guard: its Enable is a bool,
			// so its zero value is "off", which is the failure the guard exists to
			// catch.
			Guard: DefaultGuardConfig(),
		},
		Context: ContextConfig{
			// 0 = derive the ceiling from the model's own window. A fixed number
			// cannot be right for a console that serves several models, which is
			// why the default is now the model's answer rather than a guess.
			MaxTokens:  0,
			KeepRecent: 12, // the same 12 SetDefaults uses, so both paths agree
			Summarize:  true,
		},
		Feishu: FeishuConfig{
			Apps: make(map[string]FeishuApp), // empty = bot disabled until configured
		},
		OpenViking: OpenVikingConfig{
			// Off, and pointed at the address a local `ov` listens on, so
			// turning it on needs one flag rather than a host to look up.
			Enable:         false,
			BaseURL:        DefaultOpenVikingBaseURL,
			TimeoutSeconds: DefaultOpenVikingTimeoutSeconds,
			Memory: OpenVikingMemoryConfig{
				Enable:        true,
				Commit:        true,
				FlushEvery:    DefaultOpenVikingFlushEvery,
				RecallEnable:  true,
				RecallLimit:   DefaultOpenVikingRecallLimit,
				JournalEnable: true,
			},
			Documents: OpenVikingDocumentsConfig{
				Enable:        true,
				SyncWorkspace: true,
				SyncOnExit:    true,
				MaxFileKB:     DefaultOpenVikingMaxFileKB,
				BinaryMode:    DefaultOpenVikingBinaryMode,
				StatePath:     DefaultOpenVikingStatePath,
			},
			MCP: OpenVikingMCPConfig{Register: true, Name: DefaultOpenVikingMCPName},
		},
	}
}

// Load reads configuration from the given path. If path is empty, it tries
// the well-known locations. Environment variables with prefix HUAN_ override
// any value loaded from the file.
//
// Resolution order (highest priority first):
//  1. Environment variables (HUAN_<SECTION>_<KEY>)
//  2. Config file (--config flag or well-known paths)
//
// If no config file is found and the explicit path is empty, defaults are
// returned. If the explicit path is set but missing, an error is returned.
func Load(explicitPath string) (*Config, error) {
	v := viper.NewWithOptions(viper.KeyDelimiter("."))
	SetDefaults(v)

	// 1. Apply explicit --config path
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("config file not found: %s", explicitPath)
			}
			return nil, fmt.Errorf("stat config file: %w", err)
		}
		v.SetConfigFile(explicitPath)
	} else {
		// 2. Try well-known paths
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		if home, err := os.UserHomeDir(); err == nil {
			v.AddConfigPath(filepath.Join(home, ".config", "huan-agent"))
		}
	}

	// Env override: HUAN_LOGGING_LEVEL -> logging.level
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var nfErr viper.ConfigFileNotFoundError
		if errors.As(err, &nfErr) {
			// No config file found anywhere — use defaults.
			cfg := Default()
			if err := v.Unmarshal(cfg); err != nil {
				return nil, fmt.Errorf("unmarshal config: %w", err)
			}
			cfg.ApplyOpenVikingMCP()
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Viper's Unmarshal copies nested map entries straight from the config
	// file, so a provider's env override (e.g. HUAN_LLM_PROVIDERS_DEEPSEEK_API_KEY)
	// is NOT applied to map contents here. Re-read each provider field via
	// GetString, which does consult AutomaticEnv, so a non-empty env value
	// wins even when the YAML field is empty.
	for name, p := range cfg.LLM.Providers {
		// Apply env override to api_key / base_url / model when the config
		// file value is empty (or the field is absent). GetString consults
		// AutomaticEnv, so a non-empty env var wins over an empty YAML value.
		if s := v.GetString("llm.providers." + name + ".api_key"); s != "" {
			p.APIKey = s
		}
		if s := v.GetString("llm.providers." + name + ".base_url"); s != "" {
			p.BaseURL = s
		}
		if s := v.GetString("llm.providers." + name + ".model"); s != "" {
			p.Model = s
		}
		cfg.LLM.Providers[name] = p
	}

	// Register OpenViking's own MCP server so the model gets its tools in the
	// CLI, the Feishu bot and the console alike.
	cfg.ApplyOpenVikingMCP()

	return cfg, nil
}

// SetDefaults registers the default values on a viper instance.
func SetDefaults(v *viper.Viper) {
	v.SetDefault("server.host", "127.0.0.1")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.enable", false)
	v.SetDefault("server.session_ttl_minutes", 720)
	v.SetDefault("server.metrics_path", "/metrics")
	v.SetDefault("server.metrics_enable", true)
	v.SetDefault("admin.username", "admin")
	v.SetDefault("admin.password_hash", "")
	// Fallback price (USD per 1000 tokens) for providers without an entry.
	v.SetDefault("pricing.fallback.prompt_per_1k", 0.0)
	v.SetDefault("pricing.fallback.completion_per_1k", 0.0)
	// Model-list caching for the console's 模型管理 panel: refresh the enabled
	// providers' lists at startup when they are older than the TTL.
	v.SetDefault("llm.auto_refresh_models", true)
	v.SetDefault("llm.models_cache_ttl_hours", DefaultModelsCacheTTLHours)
	// Retrying a failed model call: 3 attempts (the first try plus two retries),
	// 800ms before the first retry, doubling to a 30s cap, jittered. The numbers
	// are deliberately modest — a failure that survives three spaced attempts is
	// usually not a blip, and an unbounded retry loop on a paid endpoint is worse
	// than an honest error.
	v.SetDefault("llm.retry.enable", true)
	v.SetDefault("llm.retry.max_attempts", retry.DefaultMaxAttempts)
	v.SetDefault("llm.retry.base_delay_ms", retry.DefaultBaseDelay.Milliseconds())
	v.SetDefault("llm.retry.max_delay_ms", retry.DefaultMaxDelay.Milliseconds())
	v.SetDefault("llm.retry.multiplier", retry.DefaultMultiplier)
	v.SetDefault("llm.retry.jitter", true)
	v.SetDefault("chat.enable", true)
	v.SetDefault("chat.max_steps", 12)
	v.SetDefault("chat.turn_max_tokens", 0)
	v.SetDefault("chat.turn_deadline_seconds", 0)
	v.SetDefault("chat.history_limit", DefaultChatHistoryLimit)
	v.SetDefault("chat.system_prompt", "")
	// One step's own retry, for the failure the llm layer cannot take back: the
	// stream died after it had already delivered text, so the step is re-run from
	// the same history and the client is told to drop the half-answer it received.
	// The wait is longer than the model-call one because the step re-runs real
	// work (a tool round trip is about to follow it).
	v.SetDefault("chat.step_retry.enable", true)
	v.SetDefault("chat.step_retry.max_attempts", 3)
	v.SetDefault("chat.step_retry.base_delay_ms", 1500)
	v.SetDefault("chat.step_retry.max_delay_ms", 20000)
	v.SetDefault("chat.step_retry.multiplier", retry.DefaultMultiplier)
	v.SetDefault("chat.step_retry.jitter", true)
	// The task plan behind 任务看板. On by default: it is what makes a long turn
	// legible while it runs and resumable after it dies.
	v.SetDefault("chat.plan.enable", true)
	v.SetDefault("chat.plan.max_tasks", DefaultPlanMaxTasks)
	// The loop guard. On by default: the failure it catches (a turn that spends
	// its whole step budget re-reading and re-deciding) leaves no other trace
	// than a bill, and its steering thresholds are set where a normal turn never
	// reaches them.
	v.SetDefault("chat.guard.enable", true)
	v.SetDefault("chat.guard.repeat_nudge", DefaultGuardRepeatNudge)
	v.SetDefault("chat.guard.repeat_stop", DefaultGuardRepeatStop)
	v.SetDefault("chat.guard.idle_nudge_steps", DefaultGuardIdleNudgeSteps)
	v.SetDefault("chat.guard.idle_stop_steps", DefaultGuardIdleStopSteps)
	v.SetDefault("chat.guard.max_steers", DefaultGuardMaxSteers)
	v.SetDefault("chat.guard.reread_nudge", DefaultGuardRereadNudge)
	// Context window management. These defaults live here as well as in Default()
	// because a config file that has a `context:` section at all overrides the
	// whole struct: without them, omitting one key would silently turn that
	// setting to zero — and a keep_recent of 0 compresses a long turn down to a
	// single kept message.
	v.SetDefault("context.max_tokens", 0)
	v.SetDefault("context.keep_recent", 12)
	v.SetDefault("context.summarize", true)
	v.SetDefault("context.window_ratio", context.DefaultWindowRatio)
	v.SetDefault("context.reserve_output_tokens", context.DefaultReserveOutputTokens)
	v.SetDefault("context.default_window", context.DefaultModelWindow)
	v.SetDefault("context.tool_result_max_chars", context.DefaultToolResultMaxChars)
	// One-shot runs (`huan-agent run`) are callable from a script with no
	// config at all: no timeout unless asked for, the interactive step cap, and
	// the answer alone on stdout.
	v.SetDefault("run.timeout_seconds", 0)
	v.SetDefault("run.max_steps", 0)
	v.SetDefault("run.default_output", RunOutputText)
	v.SetDefault("langfuse.enable", false)
	v.SetDefault("langfuse.host", "")
	v.SetDefault("langfuse.public_key", "")
	v.SetDefault("langfuse.secret_key", "")
	v.SetDefault("langfuse.environment", "production")
	v.SetDefault("langfuse.release", "")
	v.SetDefault("tools.workspace", "")
	v.SetDefault("tools.workspaces_dir", "")
	v.SetDefault("tools.read_only", false)
	v.SetDefault("tools.enable_bash", true)
	v.SetDefault("tools.bash_timeout_seconds", 120)
	v.SetDefault("tools.bash_max_timeout_seconds", DefaultBashMaxTimeoutSeconds)
	v.SetDefault("tools.enable_background", true)
	v.SetDefault("tools.background_dir", "")
	v.SetDefault("tools.background_max_jobs", 8)
	v.SetDefault("tools.background_log_max_mb", 8)
	v.SetDefault("tools.background_window_kb", 256)
	v.SetDefault("tools.background_stop_grace_seconds", 5)
	v.SetDefault("tools.max_read_kb", 512)
	v.SetDefault("tools.max_write_mb", 4)
	v.SetDefault("tools.max_list_entries", 500)
	v.SetDefault("tools.max_parallel", DefaultMaxParallel)
	// Code intelligence. `enable: true` registers the tools when a language
	// server covers the workspace, and starts one lazily on first use — so a
	// deployment that never asks a semantic question never pays for a server.
	v.SetDefault("tools.lsp.enable", true)
	v.SetDefault("tools.lsp.attach_diagnostics", true)
	v.SetDefault("tools.lsp.diagnostics_wait_ms", DefaultLSPDiagnosticsWaitMS)
	v.SetDefault("tools.lsp.idle_timeout_seconds", DefaultLSPIdleTimeoutSecs)
	v.SetDefault("tools.lsp.max_diagnostics", DefaultLSPMaxDiagnostics)
	// The approval gate is off by default: see ApprovalConfig.
	v.SetDefault("tools.approval.mode", "off")
	v.SetDefault("tools.approval.allow", []string{})
	v.SetDefault("tools.approval.timeout_seconds", int(DefaultApprovalTimeout.Seconds()))
	// Checkpoints default on: a wrong turn being one click from undone is worth a
	// copy of the files it touched.
	v.SetDefault("tools.checkpoint.enable", true)
	v.SetDefault("tools.checkpoint.dir", "")
	v.SetDefault("tools.checkpoint.keep_turns", DefaultCheckpointKeepTurns)
	v.SetDefault("tools.checkpoint.max_total_mb", DefaultCheckpointMaxTotalMB)
	// Artifacts default on: the agent writing a page for the user to look at is
	// the point of the feature, and the store is one bounded directory. The
	// public-URL switch is off, because that one is a security decision.
	v.SetDefault("tools.artifacts.enable", true)
	v.SetDefault("tools.artifacts.root", "")
	v.SetDefault("tools.artifacts.public_urls", false)
	v.SetDefault("tools.artifacts.max_bytes", 0)
	// Subagents are on by default (the tool is useful and bounded); the bounds are
	// what make that safe.
	v.SetDefault("tools.web.enable", true)
	v.SetDefault("tools.web.allow_private", false)
	v.SetDefault("tools.web.timeout_seconds", DefaultWebTimeoutSeconds)
	v.SetDefault("tools.web.max_kb", DefaultWebMaxKB)
	v.SetDefault("tools.web.max_chars", DefaultWebMaxChars)
	v.SetDefault("tools.web.cache_ttl_seconds", DefaultWebCacheTTL)
	v.SetDefault("subagent.enable", true)
	v.SetDefault("subagent.max_steps", DefaultSubagentMaxSteps)
	v.SetDefault("subagent.max_concurrent", DefaultSubagentMaxConcurrent)
	v.SetDefault("subagent.max_report_chars", DefaultSubagentMaxReportChars)
	v.SetDefault("admin.require_login", false)
	v.SetDefault("admin.allow_insecure_bind", false)
	// Loopback callers are not asked for the password, so a deployment that
	// turns require_login on still opens for whoever is at the machine.
	v.SetDefault("admin.trust_loopback", true)
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "console")
	v.SetDefault("database.path", "./data/huan-agent.db")
	v.SetDefault("agent.max_steps", 12)
	v.SetDefault("skills.dir", "./configs/skills")
	v.SetDefault("feishu.active", "")
	v.SetDefault("feishu.app_id", "")
	v.SetDefault("feishu.app_secret", "")
	v.SetDefault("feishu.domain", "")
	v.SetDefault("feishu.thinking", true)
	v.SetDefault("feishu.retry_attempts", 3)
	v.SetDefault("feishu.retry_base_delay_ms", 300)
	v.SetDefault("feishu.send_timeout_ms", 15000)
	v.SetDefault("feishu.rate_limit_per_sec", 5.0)
	v.SetDefault("feishu.rate_limit_burst", 10)
	v.SetDefault("feishu.download_dir", "")
	v.SetDefault("feishu.max_download_mb", DefaultFeishuMaxDownloadMB)
	v.SetDefault("feishu.enable_tools", true)
	// OpenViking (context database). enable stays false: a machine without the
	// server running must behave exactly as it did before this section existed.
	v.SetDefault("openviking.enable", false)
	v.SetDefault("openviking.base_url", DefaultOpenVikingBaseURL)
	v.SetDefault("openviking.api_key", "")
	v.SetDefault("openviking.account", "default")
	v.SetDefault("openviking.user", "default")
	v.SetDefault("openviking.timeout_seconds", DefaultOpenVikingTimeoutSeconds)
	v.SetDefault("openviking.memory.enable", true)
	v.SetDefault("openviking.memory.commit", true)
	v.SetDefault("openviking.memory.flush_every", DefaultOpenVikingFlushEvery)
	v.SetDefault("openviking.memory.recall_enable", true)
	v.SetDefault("openviking.memory.recall_limit", DefaultOpenVikingRecallLimit)
	v.SetDefault("openviking.memory.recall_target", "")
	v.SetDefault("openviking.memory.journal_enable", true)
	v.SetDefault("openviking.memory.session_prefix", DefaultOpenVikingSessionPrefix)
	v.SetDefault("openviking.documents.enable", true)
	v.SetDefault("openviking.documents.root_uri", "")
	v.SetDefault("openviking.documents.sync_workspace", true)
	v.SetDefault("openviking.documents.workspace_dir", "")
	v.SetDefault("openviking.documents.max_file_kb", DefaultOpenVikingMaxFileKB)
	v.SetDefault("openviking.documents.wait_index", false)
	v.SetDefault("openviking.documents.binary_mode", DefaultOpenVikingBinaryMode)
	v.SetDefault("openviking.documents.sync_interval_seconds", 0)
	v.SetDefault("openviking.documents.sync_on_exit", true)
	v.SetDefault("openviking.documents.state_path", DefaultOpenVikingStatePath)
	v.SetDefault("openviking.mcp.register", true)
	v.SetDefault("openviking.mcp.name", DefaultOpenVikingMCPName)
}
