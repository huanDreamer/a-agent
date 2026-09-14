package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
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

// meBody is the boot probe's answer — the one shape the console branches on to
// choose between its shell and its login form.
type meBody struct {
	Authenticated bool   `json:"authenticated"`
	LoginRequired bool   `json:"login_required"`
	Username      string `json:"username"`
}

// meProbe asks GET /api/me (with client, or a plain one) and decodes it,
// requiring the 200 the console depends on: a 401 here would leave it unable to
// tell "a password is required" from "your session expired".
func meProbe(t *testing.T, base string, client *http.Client) meBody {
	t.Helper()
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Get(base + "/api/me")
	if err != nil {
		t.Fatalf("GET /api/me: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/me status = %d, want 200", resp.StatusCode)
	}
	var me meBody
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	return me
}

func TestServer_MetaReportsLoginStateToTheUIBootstrap(t *testing.T) {
	// The UI needs to know whether to render a login form, and it asks before it
	// has a session, so /api/me has to answer 200 whatever the caller's state.
	st := newGuardStore(t)

	t.Run("login-free deployment asks for no password", func(t *testing.T) {
		srv, err := New(Config{Host: "127.0.0.1", Port: 0, Logger: zap.NewNop()}, st, nil,
			config.AdminConfig{RequireLogin: false})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		startHarness(t, srv)

		me := meProbe(t, "http://"+srv.Addr(), nil)
		if me.LoginRequired {
			t.Error("login_required should be false when require_login is off")
		}
		if me.Authenticated {
			t.Error("a login-free deployment has no session to report as authenticated")
		}
	})

	t.Run("login-required deployment says so before it says no", func(t *testing.T) {
		hash, err := HashPassword(adminPassword)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		// TrustLoopback false, so the caller this test makes — from 127.0.0.1 —
		// is asked for the password like any other.
		srv, err := New(Config{Host: "127.0.0.1", Port: 0, Logger: zap.NewNop()}, st, nil,
			config.AdminConfig{Username: "root", PasswordHash: hash, RequireLogin: true})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		startHarness(t, srv)
		base := "http://" + srv.Addr()

		me := meProbe(t, base, nil)
		if !me.LoginRequired {
			t.Error("login_required should be true when require_login is on")
		}
		if me.Authenticated {
			t.Error("an anonymous caller must not be reported as authenticated")
		}

		// After logging in the probe is the console's source for the account name
		// it shows next to 退出登录 — which is the configured username, not the
		// default.
		client := newJar(t)
		body, _ := json.Marshal(map[string]string{"password": adminPassword})
		login, err := client.Post(base+"/api/login", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /api/login: %v", err)
		}
		defer login.Body.Close()
		if login.StatusCode != http.StatusOK {
			t.Fatalf("login status = %d, want 200", login.StatusCode)
		}

		me = meProbe(t, base, client)
		if !me.Authenticated || me.Username != "root" {
			t.Errorf("after login: authenticated=%v username=%q, want true/root", me.Authenticated, me.Username)
		}
		if !me.LoginRequired {
			t.Error("login_required is about the caller, not their session: this one is still asked")
		}
	})
}

func TestServer_TrustsLoopbackCallersWithoutAPassword(t *testing.T) {
	// admin.trust_loopback: the password is for whoever is *not* on this machine.
	// This test's client is on it, so the very requests that need a session in
	// the test above are served here with none.
	hash, err := HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	srv, err := New(Config{Host: "127.0.0.1", Port: 0, Logger: zap.NewNop()}, newGuardStore(t), nil,
		config.AdminConfig{PasswordHash: hash, RequireLogin: true, TrustLoopback: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startHarness(t, srv)
	base := "http://" + srv.Addr()

	me := meProbe(t, base, nil)
	if me.LoginRequired {
		t.Error("a loopback caller should not be told to log in when loopback is trusted")
	}
	if me.Authenticated {
		t.Error("skipping the password is not a session: authenticated must stay false")
	}

	resp, err := http.Get(base + "/api/meta")
	if err != nil {
		t.Fatalf("GET /api/meta: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a session: loopback is trusted", resp.StatusCode)
	}

	// The password still works for a client that logs in anyway, and the probe
	// keeps reporting the truth about the caller: this one is not asked.
	client := newJar(t)
	body, _ := json.Marshal(map[string]string{"password": adminPassword})
	login, err := client.Post(base+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/login: %v", err)
	}
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", login.StatusCode)
	}
	after := meProbe(t, base, client)
	if !after.Authenticated {
		t.Error("a successful login should be reported as authenticated wherever it came from")
	}
	if after.LoginRequired {
		t.Error("login_required describes the caller: a trusted loopback caller is still not asked")
	}
}

func TestIsLoopbackPeer(t *testing.T) {
	// The decision that makes trust_loopback safe to default on: it is read from
	// the socket's peer, never from a header. A remote client must not be able to
	// look local by sending X-Forwarded-For — which is exactly why ClientIP() is
	// not used here.
	cases := []struct {
		name string
		addr net.Addr
		want bool
	}{
		{"ipv4 loopback", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 51234}, true},
		{"another 127 address", &net.TCPAddr{IP: net.ParseIP("127.0.0.5"), Port: 1}, true},
		{"ipv6 loopback", &net.TCPAddr{IP: net.ParseIP("::1"), Port: 51234}, true},
		{"ipv4-mapped loopback", &net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.1"), Port: 1}, true},
		{"lan address", &net.TCPAddr{IP: net.ParseIP("192.168.1.10"), Port: 40000}, false},
		{"ipv4-mapped lan address", &net.TCPAddr{IP: net.ParseIP("::ffff:192.168.1.10"), Port: 1}, false},
		{"unspecified", &net.TCPAddr{IP: net.IPv4zero}, false},
		{"link-local ipv6", &net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "en0"}, false},
		{"unix socket", &net.UnixAddr{Name: "/tmp/admin.sock", Net: "unix"}, false},
		{"nothing at all", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLoopbackPeer(tc.addr); got != tc.want {
				t.Errorf("isLoopbackPeer(%v) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

func TestAuthenticator_SkipsPasswordForTheRightCallers(t *testing.T) {
	// The three postures checked on the decision itself rather than through a
	// server: login off asks nobody; login on with trust asks everyone but this
	// machine; login on without trust asks everybody.
	local := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	remote := &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 1234}

	open, err := newAuthenticator(config.AdminConfig{RequireLogin: false}, 0, zap.NewNop())
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}
	if !open.skipsPassword(remote) || !open.skipsPassword(local) {
		t.Error("with require_login off nobody is asked, local or not")
	}

	// Trusting loopback must not weaken construction: a password-less account is
	// still refused at startup, exactly as before.
	if _, err := newAuthenticator(
		config.AdminConfig{RequireLogin: true, TrustLoopback: true}, 0, zap.NewNop()); !errors.Is(err, ErrNoPassword) {
		t.Errorf("want ErrNoPassword for require_login without a hash, got %v", err)
	}

	hash, herr := HashPassword(adminPassword)
	if herr != nil {
		t.Fatalf("hash: %v", herr)
	}
	trusting, err := newAuthenticator(
		config.AdminConfig{PasswordHash: hash, RequireLogin: true, TrustLoopback: true}, 0, zap.NewNop())
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}
	if !trusting.skipsPassword(local) {
		t.Error("a loopback caller is not asked when loopback is trusted")
	}
	if trusting.skipsPassword(remote) {
		t.Error("a remote caller is still asked when loopback is trusted")
	}

	strict, err := newAuthenticator(config.AdminConfig{PasswordHash: hash, RequireLogin: true}, 0, zap.NewNop())
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}
	if strict.skipsPassword(local) {
		t.Error("with trust_loopback off, a local caller is asked like everyone else")
	}
}
