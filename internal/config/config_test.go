package config

import (
	"os"
	"path/filepath"
	"testing"
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
