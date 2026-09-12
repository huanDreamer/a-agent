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
}

// ChatConfig configures the web chat feature.
type ChatConfig struct {
	// Enable turns the web chat on. It is off by default because it lets the
	// admin UI spend LLM tokens.
	Enable bool `mapstructure:"enable" json:"enable"`
	// MaxSteps caps tool-calling iterations per turn.
	MaxSteps int `mapstructure:"max_steps" json:"max_steps"`
	// HistoryLimit bounds how many stored messages are replayed to the model.
	HistoryLimit int `mapstructure:"history_limit" json:"history_limit"`
	// SystemPrompt overrides the default system prompt.
	SystemPrompt string `mapstructure:"system_prompt" json:"system_prompt"`
}

// DefaultChatHistoryLimit bounds the replayed conversation when unset.
const DefaultChatHistoryLimit = 40

// HistoryLimitOr returns the configured history limit or the default.
func (c ChatConfig) HistoryLimitOr() int {
	if c.HistoryLimit <= 0 {
		return DefaultChatHistoryLimit
	}
	return c.HistoryLimit
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
	// MaxSteps caps model→tool→model iterations. 0 means default (12);
	// the agent package clamps the upper bound to 25.
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

// MCPServer describes a single stdio MCP server. Env entries follow
// the "KEY=value" shape and are merged onto os.Environ() at spawn.
type MCPServer struct {
	Name    string   `mapstructure:"name" json:"name"`
	Command string   `mapstructure:"command" json:"command"`
	Args    []string `mapstructure:"args" json:"args"`
	Env     []string `mapstructure:"env" json:"env"`
}

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
	Workspace string `mapstructure:"workspace" json:"workspace"`
	// ReadOnly forbids writing files and running commands, leaving the agent
	// able to read and search but not change anything.
	ReadOnly bool `mapstructure:"read_only" json:"read_only"`
	// EnableBash turns on the shell tool.
	EnableBash bool `mapstructure:"enable_bash" json:"enable_bash"`
	// BashTimeoutSeconds bounds one command. 0 uses the tool default (120s).
	BashTimeoutSeconds int `mapstructure:"bash_timeout_seconds" json:"bash_timeout_seconds"`
	// MaxReadKB caps one file read. 0 uses the default (512 KiB).
	MaxReadKB int `mapstructure:"max_read_kb" json:"max_read_kb"`
	// MaxWriteMB caps one file write. 0 uses the default (4 MiB).
	MaxWriteMB int `mapstructure:"max_write_mb" json:"max_write_mb"`
	// MaxListEntries caps a listing or glob result. 0 uses the default (500).
	MaxListEntries int `mapstructure:"max_list_entries" json:"max_list_entries"`
	// DenyPatterns are RE2 regexes that refuse a matching command. A speed bump
	// for obviously destructive commands, NOT a security boundary: an LLM can
	// trivially write an equivalent command that does not match.
	DenyPatterns []string `mapstructure:"deny_patterns" json:"deny_patterns"`
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

// BashTimeout returns the configured command timeout.
func (c ToolsConfig) BashTimeout() time.Duration {
	if c.BashTimeoutSeconds <= 0 {
		return 120 * time.Second
	}
	return time.Duration(c.BashTimeoutSeconds) * time.Second
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
	// MaxTokens is the hard ceiling for the assembled window. 0 disables
	// auto-compression.
	MaxTokens int `mapstructure:"max_tokens" json:"max_tokens"`
	// KeepRecent is the number of most recent messages retained verbatim
	// after a compression pass.
	KeepRecent int `mapstructure:"keep_recent" json:"keep_recent"`
	// Summarize hooks the LLM itself to summarize the rolled-up older turns.
	// When true, a summarizer is wired into the agent loop (if a model is
	// available). Otherwise older turns are dropped with a placeholder.
	Summarize bool `mapstructure:"summarize" json:"summarize"`
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
		Context: ContextConfig{
			MaxTokens:  0, // 0 = disabled (no auto-compression) by default
			KeepRecent: 10,
			Summarize:  true,
		},
		Feishu: FeishuConfig{
			Apps: make(map[string]FeishuApp), // empty = bot disabled until configured
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
	v.SetDefault("chat.enable", true)
	v.SetDefault("chat.max_steps", 12)
	v.SetDefault("chat.history_limit", DefaultChatHistoryLimit)
	v.SetDefault("chat.system_prompt", "")
	v.SetDefault("langfuse.enable", false)
	v.SetDefault("langfuse.host", "")
	v.SetDefault("langfuse.public_key", "")
	v.SetDefault("langfuse.secret_key", "")
	v.SetDefault("langfuse.environment", "production")
	v.SetDefault("langfuse.release", "")
	v.SetDefault("tools.workspace", "")
	v.SetDefault("tools.read_only", false)
	v.SetDefault("tools.enable_bash", true)
	v.SetDefault("tools.bash_timeout_seconds", 120)
	v.SetDefault("tools.max_read_kb", 512)
	v.SetDefault("tools.max_write_mb", 4)
	v.SetDefault("tools.max_list_entries", 500)
	v.SetDefault("admin.require_login", false)
	v.SetDefault("admin.allow_insecure_bind", false)
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
}
