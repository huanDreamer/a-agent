// Package config loads huan-agent configuration from YAML with env override.
package config

import (
	"errors"
	"fmt"
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

// AdminConfig configures admin authentication.
type AdminConfig struct {
	// Username is the single admin account (MVP: single-user password auth).
	Username string `mapstructure:"username" json:"username"`
	// PasswordHash is a bcrypt hash. Never commit a real hash; set it with
	// `huan-agent admin set-password` or the HUAN_ADMIN_PASSWORD_HASH env var.
	PasswordHash string `mapstructure:"password_hash" json:"password_hash"`
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
}

// LLMProvider is a single LLM backend entry.
type LLMProvider struct {
	APIKey  string `mapstructure:"api_key" json:"api_key"`
	BaseURL string `mapstructure:"base_url" json:"base_url"`
	Model   string `mapstructure:"model" json:"model"`
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
