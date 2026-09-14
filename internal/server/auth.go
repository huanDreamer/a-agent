package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/huan/huan-agent/internal/config"
)

// cookieSameSiteLax is the SameSite policy used for the session cookie.
const cookieSameSiteLax = protocol.CookieSameSiteLaxMode

// SessionCookieName is the cookie carrying the session token.
const SessionCookieName = "huan_admin_session"

// sessionTokenBytes is the entropy of a session token (256 bits).
const sessionTokenBytes = 32

// loginMaxFailures and loginWindow implement a crude per-process login
// throttle: after this many failures within the window, further attempts are
// rejected until it expires. It is deliberately simple — the admin API is
// single-user and normally bound to localhost.
const (
	loginMaxFailures = 8
	loginWindow      = 5 * time.Minute
)

// ErrNoPassword is returned when the admin password hash is not configured.
var ErrNoPassword = errors.New("server: admin password is not configured (run `huan-agent admin set-password`)")

// authenticator validates the admin password and manages sessions.
type authenticator struct {
	username string
	hash     []byte
	ttl      time.Duration
	logger   *zap.Logger

	// disabled makes every request pass. It is set when the operator runs a
	// single-user local console and does not want to log in; the server refuses
	// to combine it with a non-loopback bind.
	disabled bool

	// trustsLoopback makes every request *from this machine* pass, so a
	// deployment can require a password of remote callers only. See
	// config.AdminConfig.TrustLoopback for why that defaults to on and what it
	// gives up.
	trustsLoopback bool

	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
	failures []time.Time          // recent login failure timestamps
}

// newAuthenticator validates the configured credentials at construction time,
// so a misconfigured admin account fails fast instead of at first login.
//
// With requireLogin false there are no credentials to validate: a single-user
// local console has no account, and demanding a password hash would make a
// fresh install refuse to start for a password it will never ask for.
func newAuthenticator(admin config.AdminConfig, ttl time.Duration, logger *zap.Logger) (*authenticator, error) {
	username, passwordHash := admin.Username, admin.PasswordHash
	if strings.TrimSpace(username) == "" {
		username = "admin"
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	if !admin.RequireLogin {
		return &authenticator{
			username: username,
			ttl:      ttl,
			logger:   logger,
			sessions: make(map[string]time.Time),
			disabled: true,
		}, nil
	}

	if strings.TrimSpace(passwordHash) == "" {
		return nil, ErrNoPassword
	}
	// Fail fast on a malformed hash rather than at login time.
	if _, err := bcrypt.Cost([]byte(passwordHash)); err != nil {
		return nil, fmt.Errorf("server: admin password_hash is not a valid bcrypt hash: %w", err)
	}
	return &authenticator{
		username:       username,
		hash:           []byte(passwordHash),
		ttl:            ttl,
		logger:         logger,
		trustsLoopback: admin.TrustLoopback,
		sessions:       make(map[string]time.Time),
	}, nil
}

// isLoopbackPeer reports whether a connection came from this machine.
//
// It answers from the socket's peer address only, and never from a header:
// X-Forwarded-For (what Hertz's ClientIP reads first) is written by whoever is
// talking to us, so honouring it here would let any remote caller claim to be
// local simply by sending one. The consequence is the opposite of convenient
// and the right way round: a client behind a proxy looks like the proxy, which
// is exactly why config.AdminConfig.TrustLoopback has to be turned off there.
//
// An unknown address — no connection, a unix socket, anything unparsable — is
// not local. Hertz hands out a zero TCP address (0.0.0.0) when there is no
// connection, so that case lands here too.
func isLoopbackPeer(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	host := addr.String()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// Strip an IPv6 zone ("fe80::1%en0"), which ParseIP rejects.
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	// IsLoopback covers 127.0.0.0/8 and ::1, and To4 inside it also recognises
	// the IPv4-mapped form (::ffff:127.0.0.1) a dual-stack listener reports.
	return ip != nil && ip.IsLoopback()
}

// skipsPassword reports whether this caller is exempt from the password: login
// is off entirely, or it came from this machine and loopback is trusted.
func (a *authenticator) skipsPassword(addr net.Addr) bool {
	if a.disabled {
		return true
	}
	return a.trustsLoopback && isLoopbackPeer(addr)
}

// HashPassword produces a bcrypt hash suitable for config. It is exported so
// the CLI can generate hashes without importing bcrypt itself.
func HashPassword(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", errors.New("server: password must not be empty")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("server: hash password: %w", err)
	}
	return string(b), nil
}

// throttled reports whether too many recent login failures have accumulated.
func (a *authenticator) throttled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneFailuresLocked()
	return len(a.failures) >= loginMaxFailures
}

// pruneFailuresLocked drops failures older than the window. Caller holds mu.
func (a *authenticator) pruneFailuresLocked() {
	cutoff := time.Now().Add(-loginWindow)
	kept := a.failures[:0]
	for _, t := range a.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	a.failures = kept
}

// login verifies the password and, on success, returns a fresh session token.
func (a *authenticator) login(password string) (string, error) {
	if a.throttled() {
		return "", errors.New("too many failed attempts, try again later")
	}
	if err := bcrypt.CompareHashAndPassword(a.hash, []byte(password)); err != nil {
		a.mu.Lock()
		a.failures = append(a.failures, time.Now())
		a.mu.Unlock()
		return "", errors.New("invalid password")
	}

	token, err := newSessionToken()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.failures = nil
	a.sessions[token] = time.Now().Add(a.ttl)
	a.mu.Unlock()
	return token, nil
}

// newSessionToken returns a URL-safe random token.
func newSessionToken() (string, error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("server: generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// valid reports whether a session token is live. It also slides the expiry
// forward so an actively used session does not expire mid-session.
func (a *authenticator) valid(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, token)
		return false
	}
	a.sessions[token] = time.Now().Add(a.ttl)
	return true
}

// logout revokes a session token.
func (a *authenticator) logout(token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

// sessionCount reports the number of live sessions (used by tests).
func (a *authenticator) sessionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sessions)
}

// requireSession is Hertz middleware rejecting unauthenticated requests.
//
// It lets a caller through when this deployment has nothing to ask them:
// login disabled entirely, or — with trust_loopback — a request that arrived
// over the loopback interface.
func (a *authenticator) requireSession(ctx context.Context, c *app.RequestContext) {
	if a.skipsPassword(c.RemoteAddr()) {
		c.Next(ctx)
		return
	}
	token := string(c.Cookie(SessionCookieName))
	if !a.valid(token) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, map[string]string{
			"error": "unauthorized",
		})
		return
	}
	c.Next(ctx)
}

// setSessionCookie writes the session cookie. httpOnly is always on; Secure is
// on only when the request arrived over TLS, so plain-HTTP localhost use still
// works.
func setSessionCookie(c *app.RequestContext, token string, ttl time.Duration) {
	// Only mark the cookie Secure when the request actually arrived over TLS;
	// otherwise a local plain-HTTP admin would never receive its cookie.
	secure := strings.EqualFold(string(c.Request.Scheme()), "https")
	c.SetCookie(
		SessionCookieName,
		token,
		int(ttl.Seconds()),
		"/",
		"",
		cookieSameSiteLax,
		secure,
		true,
	)
}

// clearSessionCookie expires the session cookie.
func clearSessionCookie(c *app.RequestContext) {
	c.SetCookie(SessionCookieName, "", -1, "/", "", cookieSameSiteLax, false, true)
}

// constantTimeEqual compares two strings without leaking length via timing.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
