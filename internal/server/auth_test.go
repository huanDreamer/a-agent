package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
)

// newGuardStore opens a throwaway store for the construction-time tests.
func newGuardStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestServer_RefusesLoginFreeAdminOnANonLoopbackHost(t *testing.T) {
	// The combination is dangerous: the agent's tools can execute commands, so a
	// password-less admin reachable from the network is a remote shell. A bad
	// config must fail loudly rather than start something exposed.
	st := newGuardStore(t)

	for _, host := range []string{"0.0.0.0", "192.168.1.10", "example.com", "::"} {
		t.Run("refuses "+host, func(t *testing.T) {
			_, err := New(Config{Host: host, Port: 0, Logger: zap.NewNop()}, st, nil,
				config.AdminConfig{RequireLogin: false})
			if err == nil {
				t.Fatal("want an error: a login-free admin on a public host is a remote shell")
			}
			if !strings.Contains(err.Error(), "login-free") {
				t.Errorf("the error should explain the problem, got: %v", err)
			}
		})
	}

	t.Run("allows an explicit override", func(t *testing.T) {
		// Behind a reverse proxy or a tunnel the operator may know better.
		if _, err := New(Config{Host: "0.0.0.0", Port: 0, Logger: zap.NewNop()}, st, nil,
			config.AdminConfig{RequireLogin: false, AllowInsecureBind: true}); err != nil {
			t.Fatalf("allow_insecure_bind should permit it: %v", err)
		}
	})

	t.Run("allows a password on a public host", func(t *testing.T) {
		hash, herr := HashPassword("hunter2hunter2")
		if herr != nil {
			t.Fatalf("hash: %v", herr)
		}
		if _, err := New(Config{Host: "0.0.0.0", Port: 0, Logger: zap.NewNop()}, st, nil,
			config.AdminConfig{RequireLogin: true, PasswordHash: hash}); err != nil {
			t.Fatalf("requiring login makes a public bind legitimate: %v", err)
		}
	})

	t.Run("allows login-free on loopback", func(t *testing.T) {
		// The intended single-user local setup.
		for _, host := range []string{"127.0.0.1", "localhost", ""} {
			if _, err := New(Config{Host: host, Port: 0, Logger: zap.NewNop()}, st, nil,
				config.AdminConfig{RequireLogin: false}); err != nil {
				t.Fatalf("host %q should be allowed: %v", host, err)
			}
		}
	})
}

func TestServer_LoginDisabledServesTheAPIToAnyone(t *testing.T) {
	// With login off the API answers without a session cookie. Asserted so the
	// behaviour is deliberate and visible rather than an accident of the
	// middleware, and so a future change that re-enables the check is noticed.
	st := newGuardStore(t)

	srv, err := New(Config{Host: "127.0.0.1", Port: 0, Logger: zap.NewNop()}, st, nil,
		config.AdminConfig{RequireLogin: false})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startHarness(t, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/api/meta")
	if err != nil {
		t.Fatalf("GET /api/meta: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a session", resp.StatusCode)
	}
}

func TestServer_LoginEnabledStillRejectsAnonymous(t *testing.T) {
	// The guard must not have broken the protected mode.
	st := newGuardStore(t)

	hash, err := HashPassword("hunter2hunter2")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	srv, err := New(Config{Host: "127.0.0.1", Port: 0, Logger: zap.NewNop()}, st, nil,
		config.AdminConfig{RequireLogin: true, PasswordHash: hash})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startHarness(t, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/api/meta")
	if err != nil {
		t.Fatalf("GET /api/meta: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when login is required", resp.StatusCode)
	}
}

func TestServer_LoginEndpointExplainsWhenDisabled(t *testing.T) {
	// Answering 401 would read as "wrong password" and send a caller hunting for
	// a password that does not exist.
	st := newGuardStore(t)
	srv, err := New(Config{Host: "127.0.0.1", Port: 0, Logger: zap.NewNop()}, st, nil,
		config.AdminConfig{RequireLogin: false})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startHarness(t, srv)

	body, _ := json.Marshal(map[string]string{"password": "anything"})
	resp, err := http.Post("http://"+srv.Addr()+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var msg struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(msg.Error, "disabled") {
		t.Errorf("the message should say login is disabled, got %q", msg.Error)
	}
}

func TestServer_MetaReportsLoginStateToTheUIBootstrap(t *testing.T) {
	// The UI needs to know whether to render a login form.
	st := newGuardStore(t)
	srv, err := New(Config{Host: "127.0.0.1", Port: 0, Logger: zap.NewNop()}, st, nil,
		config.AdminConfig{RequireLogin: false})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startHarness(t, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/api/me")
	if err != nil {
		t.Fatalf("GET /api/me: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no login to do)", resp.StatusCode)
	}
}
