package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/huan/huan-agent/internal/retry"
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

func TestDefaultDenyPatterns_CompileAndMatch(t *testing.T) {
	// A pattern that does not compile would either panic at runtime or silently
	// never match, so both are worth asserting.
	for _, p := range DefaultDenyPatterns {
		if _, err := regexp.Compile(p); err != nil {
			t.Errorf("deny pattern %q does not compile: %v", p, err)
		}
	}

	denied := []string{
		"rm -rf /",
		"rm -fr /",
		"sudo rm -rf /",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda",
		"cat /dev/urandom > /dev/sda",
		":(){ :|:& };:",
		"chmod -R 777 /",
		"shutdown -h now",
		"reboot",
	}
	for _, cmd := range denied {
		t.Run("denies "+cmd, func(t *testing.T) {
			if !DeniedByDefault(cmd) {
				t.Errorf("%q should be refused", cmd)
			}
		})
	}

	// Ordinary coding commands must NOT be caught, or the tool becomes useless.
	allowed := []string{
		"go test ./...",
		"git status",
		"rm -rf ./build",
		"rm -rf node_modules",
		"mkdir -p a/b",
		"ls -la /tmp",
		// A mention of a dangerous word is not the word as a command: refusing
		// these would make the tool maddening to use.
		"echo reboot-status",
		"git commit -m \"fix reboot handling\"",
		"grep -rn shutdown ./internal",
		"go build -o /dev/null ./...",
	}
	for _, cmd := range allowed {
		t.Run("allows "+cmd, func(t *testing.T) {
			if DeniedByDefault(cmd) {
				t.Errorf("%q should be allowed", cmd)
			}
		})
	}
}

// DeniedByDefault reports whether any built-in pattern matches cmd.
func DeniedByDefault(cmd string) bool {
	for _, p := range DefaultDenyPatterns {
		if re, err := regexp.Compile(p); err == nil && re.MatchString(cmd) {
			return true
		}
	}
	return false
}

func TestToolsConfig_WorkspaceOrDefault(t *testing.T) {
	t.Run("explicit workspace wins", func(t *testing.T) {
		c := ToolsConfig{Workspace: "/tmp/project"}
		got, ok := c.WorkspaceOrDefault()
		if !ok || got != "/tmp/project" {
			t.Errorf("got (%q, %v), want (/tmp/project, true)", got, ok)
		}
	})
	t.Run("blank falls back to the process working directory", func(t *testing.T) {
		for _, blank := range []string{"", "   ", "\t"} {
			got, ok := (ToolsConfig{Workspace: blank}).WorkspaceOrDefault()
			if !ok {
				t.Fatalf("blank %q should still resolve", blank)
			}
			wd, err := os.Getwd()
			if err != nil {
				t.Skipf("Getwd: %v", err)
			}
			if got != wd {
				t.Errorf("got %q, want %q", got, wd)
			}
		}
	})
}

func TestToolsConfig_BashMaxTimeout(t *testing.T) {
	cases := []struct {
		name string
		cfg  ToolsConfig
		want time.Duration
	}{
		{"default ceiling", ToolsConfig{}, 900 * time.Second},
		{"explicit ceiling", ToolsConfig{BashTimeoutSeconds: 60, BashMaxTimeoutSeconds: 3600}, time.Hour},
		// A ceiling under the default would make the default unreachable, so it
		// is raised to it rather than honoured.
		{"ceiling under the default", ToolsConfig{BashTimeoutSeconds: 300, BashMaxTimeoutSeconds: 10}, 300 * time.Second},
		{"default timeout, default ceiling", ToolsConfig{BashTimeoutSeconds: 0}, 900 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.BashMaxTimeout(); got != tc.want {
				t.Errorf("BashMaxTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestChatConfig_TurnBudgets(t *testing.T) {
	// Unset means unlimited for both, which is what an existing deployment keeps
	// after upgrading: the step cap was and remains the only default bound.
	if got := (ChatConfig{}).TurnDeadline(); got != 0 {
		t.Errorf("TurnDeadline() = %s, want 0 (unlimited)", got)
	}
	if got := (ChatConfig{TurnDeadlineSeconds: -5}).TurnDeadline(); got != 0 {
		t.Errorf("negative TurnDeadline() = %s, want 0", got)
	}
	if got := (ChatConfig{TurnDeadlineSeconds: 1800}).TurnDeadline(); got != 30*time.Minute {
		t.Errorf("TurnDeadline() = %s, want 30m", got)
	}
}

func TestToolsConfig_BashTimeout(t *testing.T) {
	if got := (ToolsConfig{}).BashTimeout(); got != 120*time.Second {
		t.Errorf("default = %v, want 2m", got)
	}
	if got := (ToolsConfig{BashTimeoutSeconds: -5}).BashTimeout(); got != 120*time.Second {
		t.Errorf("negative = %v, want the default", got)
	}
	if got := (ToolsConfig{BashTimeoutSeconds: 30}).BashTimeout(); got != 30*time.Second {
		t.Errorf("configured = %v, want 30s", got)
	}
}

func TestToolsConfig_Limits(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		r, w, l := (ToolsConfig{}).Limits()
		if r != 512<<10 {
			t.Errorf("maxRead = %d, want %d", r, 512<<10)
		}
		if w != 4<<20 {
			t.Errorf("maxWrite = %d, want %d", w, 4<<20)
		}
		if l != 500 {
			t.Errorf("maxList = %d, want 500", l)
		}
	})
	t.Run("configured", func(t *testing.T) {
		r, w, l := (ToolsConfig{MaxReadKB: 64, MaxWriteMB: 2, MaxListEntries: 10}).Limits()
		if r != 64<<10 || w != 2<<20 || l != 10 {
			t.Errorf("got (%d, %d, %d), want (65536, 2097152, 10)", r, w, l)
		}
	})
	t.Run("non-positive falls back to defaults", func(t *testing.T) {
		r, w, l := (ToolsConfig{MaxReadKB: -1, MaxWriteMB: 0, MaxListEntries: -3}).Limits()
		if r != 512<<10 || w != 4<<20 || l != 500 {
			t.Errorf("got (%d, %d, %d), want the defaults", r, w, l)
		}
	})
}

func TestToolsConfig_DenyOrDefault(t *testing.T) {
	if got := (ToolsConfig{}).DenyOrDefault(); len(got) != len(DefaultDenyPatterns) {
		t.Errorf("unconfigured = %d patterns, want the %d built-ins", len(got), len(DefaultDenyPatterns))
	}
	custom := []string{`rm\s+important`}
	got := (ToolsConfig{DenyPatterns: custom}).DenyOrDefault()
	if len(got) != 1 || got[0] != custom[0] {
		t.Errorf("configured patterns should replace the defaults, got %v", got)
	}
}

func TestConfig_ToolsDefaultsAndParsing(t *testing.T) {
	// The documented defaults must survive a load that says nothing about tools,
	// otherwise a fresh install behaves differently from the example config.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "llm:\n  default_provider: \"ollama\"\n  providers:\n    ollama:\n      base_url: \"http://localhost:11434/v1\"\n      model: \"llama3.2\"\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Tools.EnableBash {
		t.Error("enable_bash should default to true")
	}
	if cfg.Tools.ReadOnly {
		t.Error("read_only should default to false")
	}
	if cfg.Tools.BashTimeout() != 120*time.Second {
		t.Errorf("bash timeout = %v, want 2m", cfg.Tools.BashTimeout())
	}
	if !cfg.Tools.EnableBackground {
		t.Error("enable_background should default to true")
	}
	if got := cfg.Tools.BackgroundMaxJobs; got != 8 {
		t.Errorf("background_max_jobs = %d, want 8", got)
	}
	if got := cfg.Tools.BackgroundLogMaxMB; got != 8 {
		t.Errorf("background_log_max_mb = %d, want 8", got)
	}
	if got := cfg.Tools.BackgroundWindowKB; got != 256 {
		t.Errorf("background_window_kb = %d, want 256", got)
	}
	if cfg.Tools.BackgroundStopGrace() != 5*time.Second {
		t.Errorf("stop grace = %v, want 5s", cfg.Tools.BackgroundStopGrace())
	}

	// And an explicit section must be honoured.
	yaml2 := yaml + "tools:\n  workspace: \"/tmp/ws\"\n  read_only: true\n  enable_bash: false\n  bash_timeout_seconds: 5\n  max_read_kb: 8\n  max_write_mb: 1\n  max_list_entries: 3\n  deny_patterns: [\"boom\"]\n" +
		"  enable_background: false\n  background_dir: \"/tmp/jobs\"\n  background_max_jobs: 2\n" +
		"  background_log_max_mb: 1\n  background_window_kb: 64\n  background_stop_grace_seconds: 1\n"
	path2 := filepath.Join(dir, "config2.yaml")
	if err := os.WriteFile(path2, []byte(yaml2), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Tools.Workspace != "/tmp/ws" || !cfg2.Tools.ReadOnly || cfg2.Tools.EnableBash {
		t.Errorf("tools section not parsed: %+v", cfg2.Tools)
	}
	if cfg2.Tools.BashTimeout() != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", cfg2.Tools.BashTimeout())
	}
	r, w, l := cfg2.Tools.Limits()
	if r != 8<<10 || w != 1<<20 || l != 3 {
		t.Errorf("limits = (%d,%d,%d), want (8192,1048576,3)", r, w, l)
	}
	if got := cfg2.Tools.DenyOrDefault(); len(got) != 1 || got[0] != "boom" {
		t.Errorf("deny = %v, want [boom]", got)
	}
	if cfg2.Tools.EnableBackground {
		t.Error("enable_background: false was not parsed")
	}
	if cfg2.Tools.BackgroundDir != "/tmp/jobs" || cfg2.Tools.BackgroundMaxJobs != 2 ||
		cfg2.Tools.BackgroundLogMaxMB != 1 || cfg2.Tools.BackgroundWindowKB != 64 {
		t.Errorf("background settings not parsed: %+v", cfg2.Tools)
	}
	if cfg2.Tools.BackgroundStopGrace() != time.Second {
		t.Errorf("stop grace = %v, want 1s", cfg2.Tools.BackgroundStopGrace())
	}
}

// TestToolsConfig_JobsDirOrDefault covers where job logs are written: beside the
// database by default, because a log dropped into a workspace would show up in
// the model's own greps and in the user's git status.
func TestToolsConfig_JobsDirOrDefault(t *testing.T) {
	cases := []struct {
		name string
		cfg  ToolsConfig
		db   string
		want string
	}{
		{"beside the database", ToolsConfig{}, "/var/db/huan.db", filepath.Join("/var/db", DefaultJobsSubdir)},
		{"a bare database file", ToolsConfig{}, "huan.db", DefaultJobsSubdir},
		{"no database path", ToolsConfig{}, "", filepath.Join("data", DefaultJobsSubdir)},
		{"configured", ToolsConfig{BackgroundDir: "/tmp/logs"}, "/var/db/huan.db", "/tmp/logs"},
		{"blank is unset", ToolsConfig{BackgroundDir: "  "}, "/var/db/huan.db", filepath.Join("/var/db", DefaultJobsSubdir)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.cfg.JobsDirOrDefault(tc.db)
			if !ok {
				t.Fatal("no directory was resolved")
			}
			if got != tc.want {
				t.Errorf("JobsDirOrDefault(%q) = %q, want %q", tc.db, got, tc.want)
			}
		})
	}
}

func TestLoopbackHost(t *testing.T) {
	// Erring toward "not loopback" is the safe direction: a wrong "yes" would
	// expose command execution to the network.
	loopback := []string{"", "   ", "localhost", "LOCALHOST", "127.0.0.1", "127.0.0.53", "::1", "[::1]"}
	for _, h := range loopback {
		if !LoopbackHost(h) {
			t.Errorf("LoopbackHost(%q) = false, want true", h)
		}
	}
	exposed := []string{"0.0.0.0", "::", "[::]", "192.168.1.10", "10.0.0.5", "example.com", "huan.local", "8.8.8.8"}
	for _, h := range exposed {
		if LoopbackHost(h) {
			t.Errorf("LoopbackHost(%q) = true, want false", h)
		}
	}
}

func TestAdminConfig_LoginDefaults(t *testing.T) {
	// A fresh install is a single-user local console: no password prompt.
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("llm:\n  default_provider: \"x\"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin.RequireLogin {
		t.Error("require_login should default to false")
	}
	if cfg.Admin.AllowInsecureBind {
		t.Error("allow_insecure_bind should default to false")
	}
	// Trusting loopback is what makes "require_login: true" mean "a password for
	// everyone who is not at this machine"; the console on the desktop it runs on
	// must not be asked.
	if !cfg.Admin.TrustLoopback {
		t.Error("trust_loopback should default to true")
	}
}

func TestAdminConfig_TrustLoopbackCanBeTurnedOff(t *testing.T) {
	// The deployment this matters for: a reverse proxy or an SSH tunnel on the
	// same host, whose clients all arrive from 127.0.0.1. There the password has
	// to apply to loopback too, so the flag has to reach the struct from the file
	// (and, from there, the server).
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("admin:\n  require_login: true\n  trust_loopback: false\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin.TrustLoopback {
		t.Error("trust_loopback: false in the file should stay false")
	}
	if !cfg.Admin.RequireLogin {
		t.Error("require_login: true in the file should stay true")
	}
}

func TestLLMConfig_ModelRefreshDefaults(t *testing.T) {
	// The defaults come from Default() and from Viper's SetDefaults; both paths
	// are what a fresh install without a config file goes through.
	if c := Default(); !c.LLM.AutoRefreshModels {
		t.Error("auto_refresh_models should default to true: without it a provider added in the console keeps an empty model list")
	}
	if got := Default().LLM.ModelsCacheTTL(); got != DefaultModelsCacheTTLHours*time.Hour {
		t.Errorf("default TTL = %v, want %dh", got, DefaultModelsCacheTTLHours)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("llm:\n  default_provider: \"x\"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.LLM.AutoRefreshModels {
		t.Error("auto_refresh_models must default to true even when the file does not mention it")
	}
	if got := cfg.LLM.ModelsCacheTTL(); got != 24*time.Hour {
		t.Errorf("TTL = %v, want 24h", got)
	}
}

func TestLLMConfig_ModelRefreshOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := "llm:\n  auto_refresh_models: false\n  models_cache_ttl_hours: 6\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.AutoRefreshModels {
		t.Error("auto_refresh_models: false must turn the startup pass off")
	}
	if got := cfg.LLM.ModelsCacheTTL(); got != 6*time.Hour {
		t.Errorf("TTL = %v, want 6h", got)
	}

	// A zero or missing value falls back to the default rather than meaning
	// "always stale", which would refetch on every start.
	if got := (LLMConfig{}).ModelsCacheTTL(); got != DefaultModelsCacheTTLHours*time.Hour {
		t.Errorf("unset TTL = %v, want the default", got)
	}
}

// TestToolsConfig_WorkspacesDirOrDefault covers where named workspaces are
// created: the default has to sit beside the database rather than in whatever
// directory the process happened to be started from, because a workspace that
// moved when the service was launched from a different cwd would be a support
// case nobody could explain.
func TestToolsConfig_WorkspacesDirOrDefault(t *testing.T) {
	t.Run("configured value wins", func(t *testing.T) {
		c := ToolsConfig{WorkspacesDir: "/tmp/ws"}
		got, ok := c.WorkspacesDirOrDefault("/var/db/huan.db")
		if !ok || got != "/tmp/ws" {
			t.Errorf("got (%q, %v), want (/tmp/ws, true)", got, ok)
		}
	})
	t.Run("default sits beside the database", func(t *testing.T) {
		for _, tc := range []struct{ db, want string }{
			{"/var/db/huan.db", "/var/db/" + DefaultWorkspacesSubdir},
			{"data/huan.db", filepath.Join("data", DefaultWorkspacesSubdir)},
			// A bare filename is a database in the current directory, so its
			// workspaces directory is there too — the rule is "beside the
			// database file", not "under ./data".
			{"huan.db", DefaultWorkspacesSubdir},
			{"", filepath.Join("data", DefaultWorkspacesSubdir)},
		} {
			got, ok := (ToolsConfig{}).WorkspacesDirOrDefault(tc.db)
			if !ok || got != tc.want {
				t.Errorf("db %q: got (%q, %v), want (%q, true)", tc.db, got, ok, tc.want)
			}
		}
	})
	t.Run("blank is treated as unset", func(t *testing.T) {
		for _, blank := range []string{"", "   "} {
			got, _ := (ToolsConfig{WorkspacesDir: blank}).WorkspacesDirOrDefault("/var/db/huan.db")
			if got != "/var/db/"+DefaultWorkspacesSubdir {
				t.Errorf("blank %q resolved to %q", blank, got)
			}
		}
	})
}

// TestFeishu_EnableTools pins a security-relevant default rather than leaving it
// to whatever a zero value means: the bot's tool access must be on for the
// feature to work out of the box, and switchable off with one key.
func TestFeishu_EnableTools(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(cfgFile, []byte("feishu:\n  app_id: \"cli_x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Feishu.EnableTools {
		t.Error("feishu.enable_tools defaults to false, so the bot would have no tools")
	}

	if err := os.WriteFile(cfgFile, []byte("feishu:\n  app_id: \"cli_x\"\n  enable_tools: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Feishu.EnableTools {
		t.Error("feishu.enable_tools: false did not turn the tools off")
	}
}

// TestTools_WorkspacesDirYAML checks the new key is actually wired to the config
// file (and not only to the struct), since a typo here would silently fall back
// to the default directory.
func TestTools_WorkspacesDirYAML(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	yaml := "tools:\n  workspaces_dir: \"/tmp/named-areas\"\n"
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.WorkspacesDir != "/tmp/named-areas" {
		t.Errorf("workspaces_dir = %q, want /tmp/named-areas", cfg.Tools.WorkspacesDir)
	}
	if got, _ := cfg.Tools.WorkspacesDirOrDefault("/var/db/huan.db"); got != "/tmp/named-areas" {
		t.Errorf("resolved to %q, want the configured value", got)
	}
}

func TestRetryConfig_Policy(t *testing.T) {
	// A disabled block means one attempt, which is exactly the behaviour of a
	// build without retrying: "off" is expressed as an attempt count so there is
	// one place that decides whether a second call happens.
	off := RetryConfig{Enable: false, MaxAttempts: 5}
	if got := off.Policy().Attempts(); got != 1 {
		t.Errorf("disabled Policy().Attempts() = %d, want 1", got)
	}

	on := RetryConfig{
		Enable:      true,
		MaxAttempts: 4,
		BaseDelayMS: 250,
		MaxDelayMS:  5000,
		Multiplier:  3,
	}
	p := on.Policy()
	if p.Attempts() != 4 {
		t.Errorf("Attempts() = %d, want 4", p.Attempts())
	}
	if p.BaseDelay != 250*time.Millisecond || p.MaxDelay != 5*time.Second || p.Multiplier != 3 {
		t.Errorf("Policy() = %+v, want the configured numbers", p)
	}
	// Jitter unset means on: without it, several turns that failed on the same
	// outage retry in lockstep and hit it again together.
	if !p.Jitter {
		t.Error("Jitter defaults to off, want on")
	}
	noJitter := false
	if got := (RetryConfig{Enable: true, Jitter: &noJitter}).Policy(); got.Jitter {
		t.Error("an explicit jitter: false did not turn it off")
	}
	// A zero MaxAttempts with retrying enabled resolves through the policy's own
	// defaults rather than becoming "no retrying" by accident.
	if got := (RetryConfig{Enable: true}).Policy().Attempts(); got != retry.DefaultMaxAttempts {
		t.Errorf("Attempts() with no number = %d, want the default %d", got, retry.DefaultMaxAttempts)
	}
}

func TestPlanConfig_MaxTasksOr(t *testing.T) {
	if got := (PlanConfig{}).MaxTasksOr(); got != DefaultPlanMaxTasks {
		t.Errorf("default = %d, want %d", got, DefaultPlanMaxTasks)
	}
	if got := (PlanConfig{MaxTasks: -3}).MaxTasksOr(); got != DefaultPlanMaxTasks {
		t.Errorf("negative = %d, want the default", got)
	}
	if got := (PlanConfig{MaxTasks: 8}).MaxTasksOr(); got != 8 {
		t.Errorf("configured = %d, want 8", got)
	}
}

func TestLoad_RetryAndPlanDefaultsAreOn(t *testing.T) {
	// These defaults are the feature: a deployment that never configures them
	// must still survive a dropped connection and still show its plan. This is
	// the one place that says so.
	tmp := t.TempDir()
	t.Chdir(tmp)
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error = %v", err)
	}
	if got := c.LLM.RetryPolicy().Attempts(); got <= 1 {
		t.Errorf("llm.retry attempts = %d, want retrying on by default", got)
	}
	if got := c.Chat.StepRetryPolicy().Attempts(); got <= 1 {
		t.Errorf("chat.step_retry attempts = %d, want retrying on by default", got)
	}
	if !c.Chat.Plan.Enable {
		t.Error("chat.plan.enable defaults to off, want on")
	}
	if got := c.Chat.Plan.MaxTasksOr(); got != DefaultPlanMaxTasks {
		t.Errorf("plan max tasks = %d, want %d", got, DefaultPlanMaxTasks)
	}
}
