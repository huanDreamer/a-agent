package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c == nil {
		t.Fatal("Default() returned nil")
	}
	if c.Server.Host != "127.0.0.1" {
		t.Errorf("default server.host = %q, want 127.0.0.1", c.Server.Host)
	}
	if c.Server.Port != 8080 {
		t.Errorf("default server.port = %d, want 8080", c.Server.Port)
	}
	if c.Logging.Level != "info" {
		t.Errorf("default logging.level = %q, want info", c.Logging.Level)
	}
	if c.Logging.Format != "console" {
		t.Errorf("default logging.format = %q, want console", c.Logging.Format)
	}
	if c.Database.Path == "" {
		t.Error("default database.path should not be empty")
	}
}

func TestLoad_DefaultsWhenNoFile(t *testing.T) {
	// Save & restore env vars we touch.
	for _, k := range []string{"HUAN_SERVER_HOST", "HUAN_LOGGING_LEVEL", "HUAN_CONFIG"} {
		old, ok := os.LookupEnv(k)
		if ok {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
	}

	// Run in a temp dir so the default ./configs path is empty.
	tmp := t.TempDir()
	t.Chdir(tmp)

	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error = %v", err)
	}
	if c.Server.Port != 8080 {
		t.Errorf("default port = %d, want 8080", c.Server.Port)
	}
}

func TestLoad_ExplicitYAML(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  host: "0.0.0.0"
  port: 9090
logging:
  level: "debug"
  format: "json"
`
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if c.Server.Host != "0.0.0.0" {
		t.Errorf("host = %q, want 0.0.0.0", c.Server.Host)
	}
	if c.Server.Port != 9090 {
		t.Errorf("port = %d, want 9090", c.Server.Port)
	}
	if c.Logging.Level != "debug" {
		t.Errorf("level = %q, want debug", c.Logging.Level)
	}
	if c.Logging.Format != "json" {
		t.Errorf("format = %q, want json", c.Logging.Format)
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	// Clean up env after the test.
	for _, k := range []string{"HUAN_LOGGING_LEVEL", "HUAN_SERVER_PORT"} {
		old, ok := os.LookupEnv(k)
		if ok {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
	}

	// Run in a temp dir so we use defaults.
	t.Chdir(t.TempDir())

	t.Setenv("HUAN_LOGGING_LEVEL", "warn")
	t.Setenv("HUAN_SERVER_PORT", "12345")

	c, err := Load("")
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if c.Logging.Level != "warn" {
		t.Errorf("level = %q, want warn (env override)", c.Logging.Level)
	}
	if c.Server.Port != 12345 {
		t.Errorf("port = %d, want 12345 (env override)", c.Server.Port)
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	for _, k := range []string{"HUAN_LOGGING_LEVEL"} {
		old, ok := os.LookupEnv(k)
		if ok {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
	}

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("logging:\n  level: debug\n  format: console\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("HUAN_LOGGING_LEVEL", "error")

	c, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if c.Logging.Level != "error" {
		t.Errorf("level = %q, want error (env wins over yaml)", c.Logging.Level)
	}
}

func TestLoad_MissingExplicitPath(t *testing.T) {
	_, err := Load("/this/path/does/not/exist.yaml")
	if err == nil {
		t.Fatal("Load of missing explicit path should fail")
	}
}

func TestLoad_AgentAndMCP(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	yaml := `
agent:
  max_steps: 8
  allowed_tools: ["time", "calc"]
mcp:
  servers:
    - name: "fs"
      command: "/bin/echo"
      args: ["hello"]
      env: ["FOO=bar"]
skills:
  dir: "/tmp/skills"
`
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Agent.MaxSteps != 8 {
		t.Errorf("max_steps = %d", c.Agent.MaxSteps)
	}
	if len(c.Agent.AllowedTools) != 2 || c.Agent.AllowedTools[0] != "time" {
		t.Errorf("allowed_tools = %v", c.Agent.AllowedTools)
	}
	if len(c.MCP.Servers) != 1 {
		t.Fatalf("mcp.servers len = %d", len(c.MCP.Servers))
	}
	s := c.MCP.Servers[0]
	if s.Name != "fs" || s.Command != "/bin/echo" || len(s.Args) != 1 || s.Args[0] != "hello" || len(s.Env) != 1 {
		t.Errorf("mcp.servers[0] = %+v", s)
	}
	if c.Skills.Dir != "/tmp/skills" {
		t.Errorf("skills.dir = %q", c.Skills.Dir)
	}
}

func TestDefault_AgentMCPSkills(t *testing.T) {
	c := Default()
	if c.Agent.MaxSteps != 12 {
		t.Errorf("default agent.max_steps = %d, want 12", c.Agent.MaxSteps)
	}
	if c.Skills.Dir == "" {
		t.Error("default skills.dir should not be empty")
	}
	if len(c.MCP.Servers) != 0 {
		t.Errorf("default mcp.servers len = %d, want 0", len(c.MCP.Servers))
	}
}

func TestFeishuResolve_ActiveApp(t *testing.T) {
	c := FeishuConfig{
		Active: "prod",
		Apps: map[string]FeishuApp{
			"dev":  {AppID: "cli_dev", AppSecret: "s_dev"},
			"prod": {AppID: "cli_prod", AppSecret: "s_prod", Domain: "https://open.larksuite.com"},
		},
	}
	got := c.Resolve()
	if got.AppID != "cli_prod" || got.AppSecret != "s_prod" {
		t.Errorf("Resolve(active=prod) = %+v, want prod app", got)
	}
	if got.Domain != "https://open.larksuite.com" {
		t.Errorf("prod domain = %q", got.Domain)
	}
	if !c.Enabled() {
		t.Error("Enabled() should be true for a configured app")
	}
}

func TestFeishuResolve_LegacyFlat(t *testing.T) {
	c := FeishuConfig{AppID: "cli_flat", AppSecret: "s_flat"}
	if !c.Enabled() {
		t.Error("legacy flat app should be enabled")
	}
	if got := c.Resolve(); got.AppID != "cli_flat" || got.AppSecret != "s_flat" {
		t.Errorf("legacy Resolve = %+v", got)
	}
}

func TestFeishuResolve_FirstWhenUnset(t *testing.T) {
	c := FeishuConfig{Apps: map[string]FeishuApp{
		"a": {AppID: "cli_a"},
		"b": {AppID: "cli_b"},
	}}
	if !c.Enabled() {
		t.Error("should be enabled when apps present")
	}
	got := c.Resolve()
	if got.AppID != "cli_a" && got.AppID != "cli_b" {
		t.Errorf("expected first of apps, got %+v", got)
	}
}

func TestFeishuResolve_Disabled(t *testing.T) {
	// Empty config -> disabled, zero app.
	c := FeishuConfig{}
	if c.Enabled() {
		t.Error("empty feishu config should be disabled")
	}
	if got := c.Resolve(); got.AppID != "" {
		t.Errorf("empty Resolve = %+v, want zero", got)
	}
}

func TestLoad_EnvOverridesNestedProviderMap(t *testing.T) {
	// Regression: Viper's Unmarshal copies nested map values (llm.providers.*)
	// straight from the file, bypassing AutomaticEnv. A provider declared with
	// an empty api_key in YAML must still pick up the env override.
	const key = "HUAN_LLM_PROVIDERS_DEEPSEEK_API_KEY"
	old, ok := os.LookupEnv(key)
	if ok {
		t.Cleanup(func() { _ = os.Setenv(key, old) })
	} else {
		t.Cleanup(func() { _ = os.Unsetenv(key) })
	}
	_ = os.Setenv(key, "sk-env-secret")

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  port: 9099
llm:
  default_provider: "deepseek"
  providers:
    deepseek:
      api_key: ""          # empty in YAML -> env should fill it
      base_url: ""
    qwen:
      api_key: ""
`
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	p := c.LLM.Providers["deepseek"]
	if p.APIKey != "sk-env-secret" {
		t.Errorf("deepseek api_key = %q, want env value sk-env-secret (nested map env override failed)", p.APIKey)
	}
}

func TestFeishuConfig_RetryBaseDelay(t *testing.T) {
	tests := []struct {
		name string
		ms   int
		want time.Duration
	}{
		{"default when unset", 0, 300 * time.Millisecond},
		{"default when negative", -5, 300 * time.Millisecond},
		{"configured", 50, 50 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := FeishuConfig{RetryBaseDelayMS: tc.ms}
			if got := c.RetryBaseDelay(); got != tc.want {
				t.Errorf("RetryBaseDelay() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFeishuConfig_SendTimeout(t *testing.T) {
	if got := (FeishuConfig{}).SendTimeout(); got != 0 {
		t.Errorf("unset SendTimeout() = %v, want 0 (disabled)", got)
	}
	c := FeishuConfig{SendTimeoutMS: 250}
	if got := c.SendTimeout(); got != 250*time.Millisecond {
		t.Errorf("SendTimeout() = %v, want 250ms", got)
	}
}

func TestFeishuConfig_MaxDownloadBytes(t *testing.T) {
	tests := []struct {
		name string
		mb   int
		want int64
	}{
		{"default when unset", 0, int64(DefaultFeishuMaxDownloadMB) << 20},
		{"default when negative", -1, int64(DefaultFeishuMaxDownloadMB) << 20},
		{"configured", 5, 5 << 20},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := FeishuConfig{MaxDownloadMB: tc.mb}
			if got := c.MaxDownloadBytes(); got != tc.want {
				t.Errorf("MaxDownloadBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFeishuConfig_ResolveDownloadDir(t *testing.T) {
	t.Run("explicit dir wins", func(t *testing.T) {
		c := FeishuConfig{DownloadDir: "/tmp/custom-dl"}
		if got := c.ResolveDownloadDir(); got != "/tmp/custom-dl" {
			t.Errorf("ResolveDownloadDir() = %q, want the configured dir", got)
		}
	})
	t.Run("whitespace is treated as unset", func(t *testing.T) {
		c := FeishuConfig{DownloadDir: "   "}
		got := c.ResolveDownloadDir()
		if got == "" || got == "   " {
			t.Errorf("ResolveDownloadDir() = %q, want a temp-dir fallback", got)
		}
		if !strings.HasSuffix(got, DefaultFeishuDownloadSubdir) {
			t.Errorf("ResolveDownloadDir() = %q, want it to end with %q", got, DefaultFeishuDownloadSubdir)
		}
	})
	t.Run("empty falls back under the temp dir", func(t *testing.T) {
		got := (FeishuConfig{}).ResolveDownloadDir()
		want := filepath.Join(os.TempDir(), DefaultFeishuDownloadSubdir)
		if got != want {
			t.Errorf("ResolveDownloadDir() = %q, want %q", got, want)
		}
	})
}

func TestLoad_FeishuBehaviorDefaults(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Feishu.Thinking {
		t.Error("feishu.thinking should default to true")
	}
	if c.Feishu.RetryAttempts != 3 {
		t.Errorf("retry_attempts = %d, want 3", c.Feishu.RetryAttempts)
	}
	if c.Feishu.RetryBaseDelayMS != 300 {
		t.Errorf("retry_base_delay_ms = %d, want 300", c.Feishu.RetryBaseDelayMS)
	}
	if c.Feishu.SendTimeoutMS != 15000 {
		t.Errorf("send_timeout_ms = %d, want 15000", c.Feishu.SendTimeoutMS)
	}
	if c.Feishu.RateLimitPerSec != 5.0 {
		t.Errorf("rate_limit_per_sec = %v, want 5.0", c.Feishu.RateLimitPerSec)
	}
	if c.Feishu.RateLimitBurst != 10 {
		t.Errorf("rate_limit_burst = %d, want 10", c.Feishu.RateLimitBurst)
	}
	if c.Feishu.MaxDownloadMB != DefaultFeishuMaxDownloadMB {
		t.Errorf("max_download_mb = %d, want %d", c.Feishu.MaxDownloadMB, DefaultFeishuMaxDownloadMB)
	}
}

func TestLoad_FeishuBehaviorFromYAML(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	yaml := `
feishu:
  thinking: false
  retry_attempts: 1
  retry_base_delay_ms: 10
  send_timeout_ms: 500
  rate_limit_per_sec: 0
  rate_limit_burst: 2
  download_dir: "/data/dl"
  max_download_mb: 4
  apps:
    prod:
      app_id: "cli_p"
      app_secret: "s"
      transport: "callback"
      callback_addr: "127.0.0.1:9999"
      callback_path: "/hook"
  active: "prod"
`
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Feishu.Thinking {
		t.Error("thinking should be false")
	}
	if c.Feishu.RetryAttempts != 1 {
		t.Errorf("retry_attempts = %d, want 1", c.Feishu.RetryAttempts)
	}
	if got := c.Feishu.SendTimeout(); got != 500*time.Millisecond {
		t.Errorf("SendTimeout() = %v, want 500ms", got)
	}
	if got := c.Feishu.MaxDownloadBytes(); got != 4<<20 {
		t.Errorf("MaxDownloadBytes() = %d, want 4MiB", got)
	}
	if got := c.Feishu.ResolveDownloadDir(); got != "/data/dl" {
		t.Errorf("ResolveDownloadDir() = %q, want /data/dl", got)
	}

	app := c.Feishu.Resolve()
	if app.AppID != "cli_p" {
		t.Errorf("active app = %+v, want the prod entry", app)
	}
	if app.Transport != "callback" || app.CallbackAddr != "127.0.0.1:9999" || app.CallbackPath != "/hook" {
		t.Errorf("transport fields not loaded: %+v", app)
	}
}
