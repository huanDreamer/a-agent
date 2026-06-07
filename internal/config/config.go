// Package config loads huan-agent configuration from YAML with env override.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
}

// DatabaseConfig configures the SQLite database.
type DatabaseConfig struct {
	Path string `mapstructure:"path" json:"path"`
}

// ServerConfig controls the admin HTTP server (placeholder for Phase 5).
type ServerConfig struct {
	Host string `mapstructure:"host" json:"host"`
	Port int    `mapstructure:"port" json:"port"`
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
	return cfg, nil
}

// SetDefaults registers the default values on a viper instance.
func SetDefaults(v *viper.Viper) {
	v.SetDefault("server.host", "127.0.0.1")
	v.SetDefault("server.port", 8080)
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "console")
	v.SetDefault("database.path", "./data/huan-agent.db")
}
