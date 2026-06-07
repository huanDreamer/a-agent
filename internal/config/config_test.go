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
