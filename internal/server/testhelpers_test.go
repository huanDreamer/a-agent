package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/pricing"
	"github.com/huan/huan-agent/internal/store"
)

// buildServer constructs a Server on an ephemeral port with one admin account.
// skillsDir and statePath drive the skills API; seed may be nil.
func buildServer(t *testing.T, skillsDir, statePath string, seed func(store.Store)) (*Server, store.Store) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if seed != nil {
		seed(st)
	}

	hash, err := HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	srv, err := New(Config{
		Host:          "127.0.0.1",
		Port:          0, // ephemeral
		MetricsEnable: true,
		Version:       "test-version",
		Provider:      "deepseek",
		Model:         "deepseek-chat",
		SkillsDir:     skillsDir,
		StatePath:     statePath,
		Logger:        zap.NewNop(),
	}, st, pricing.NewTable(map[string]pricing.Rate{
		"deepseek/deepseek-chat": {PromptPer1K: 0.001, CompletionPer1K: 0.002},
	}, pricing.Rate{}), config.AdminConfig{
		Username:     "admin",
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv, st
}

// startHarness runs the server and waits until it accepts connections.
func startHarness(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down")
		}
	})
	waitReady(t, srv.Addr())
}

// waitReady blocks until the server accepts connections.
func waitReady(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", addr)
}

// newJar returns an HTTP client that stores cookies.
func newJar(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	// Keep-alive sockets would keep the engine draining until its exit timeout,
	// so close them before the harness tears the server down.
	t.Cleanup(client.CloseIdleConnections)
	return client
}

// mustParseURL parses a URL or fails the test.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}

// compareBcrypt verifies a password against a hash.
func compareBcrypt(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// writeFile writes a file, creating parent directories.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// requireStatus asserts the response status and closes the body.
func requireStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, want, buf.String())
	}
}

// buildOpts configures the shared test server.
type buildOpts struct {
	seed func(store.Store)
	chat ChatDeps
}

// buildServerWith constructs a Server with optional chat wiring.
func buildServerWith(t *testing.T, opts buildOpts) (*Server, store.Store) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if opts.seed != nil {
		opts.seed(st)
	}

	hash, err := HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	srv, err := New(Config{
		Host:          "127.0.0.1",
		Port:          0,
		MetricsEnable: true,
		Version:       "test-version",
		Provider:      "deepseek",
		Model:         "deepseek-chat",
		ChatMaxSteps:  4,
		Chat:          opts.chat,
		Logger:        zap.NewNop(),
	}, st, pricing.NewTable(map[string]pricing.Rate{
		"deepseek/deepseek-chat": {PromptPer1K: 0.001, CompletionPer1K: 0.002},
	}, pricing.Rate{}), config.AdminConfig{
		Username:     "admin",
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv, st
}

// patchJSON issues a PATCH with a JSON body.
func (h *harness) patchJSON(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPatch, h.base+path, &buf)
	if err != nil {
		t.Fatalf("new PATCH request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	return resp
}

// deleteJSON issues a DELETE.
func (h *harness) deleteJSON(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, h.base+path, nil)
	if err != nil {
		t.Fatalf("new DELETE request: %v", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	return resp
}

// stubTool is a minimal tool.Tool for the chat tests.
type stubTool struct {
	name string
	desc string
	run  func(ctx context.Context, args string) (string, error)
}

// Info implements tool.Tool.
func (s *stubTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name, Desc: s.desc}, nil
}

// InvokableRun implements tool.Tool.
func (s *stubTool) InvokableRun(ctx context.Context, args string, _ ...einotool.Option) (string, error) {
	return s.run(ctx, args)
}

// decode unmarshals a response body and closes it.
func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
