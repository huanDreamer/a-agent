package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/core/httpserverext"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"go.uber.org/zap"
)

// DefaultCallbackPath is the HTTP path that receives Feishu event callbacks.
const DefaultCallbackPath = "/feishu/event"

// callbackShutdownTimeout bounds the graceful HTTP shutdown.
const callbackShutdownTimeout = 5 * time.Second

// maxCallbackBodyBytes caps a callback request body. Feishu events are small;
// this only guards against memory abuse on a public endpoint.
const maxCallbackBodyBytes = 1 << 20

// callbackServer serves Feishu event callbacks over HTTP. It is the
// alternative to the WebSocket long-connection: the same EventDispatcher is
// driven by an http.Handler instead of a WS client, which requires a publicly
// reachable URL configured in the Feishu console.
type callbackServer struct {
	srv    *http.Server
	ln     net.Listener
	logger *zap.Logger
}

// callbackEnvelope is the subset of a callback body needed to authenticate the
// request before the SDK sees it.
type callbackEnvelope struct {
	// Encrypt is set when Feishu encrypts the payload; the token then lives
	// inside the ciphertext and cannot be read here.
	Encrypt string `json:"encrypt"`
	// Token is the schema 1.0 location.
	Token string `json:"token"`
	// Header carries the schema 2.0 location.
	Header *struct {
		Token string `json:"token"`
	} `json:"header"`
}

// extractCallbackToken returns the verification token carried by a plaintext
// callback body, and whether the body is encrypted.
func extractCallbackToken(body []byte) (token string, encrypted bool) {
	var env callbackEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", false
	}
	if env.Encrypt != "" {
		return "", true
	}
	if env.Header != nil && env.Header.Token != "" {
		return env.Header.Token, false
	}
	return env.Token, false
}

// newCallbackServer binds addr and wires the dispatcher onto path. It returns
// the server with its listener already bound, so Addr() is valid immediately
// and bind errors surface at construction time rather than on Run.
//
// Two authentication layers protect the endpoint:
//
//  1. When verificationToken is set, the request body's token must match. The
//     SDK only enforces this for the url_verification handshake, so a plain
//     event callback would otherwise be accepted from anyone.
//  2. When encryptKey is set, the SDK verifies Feishu's request signature
//     (which covers encrypted payloads, where layer 1 cannot read the token).
//
// With neither configured the endpoint is unauthenticated; a warning is logged.
func newCallbackServer(addr, path string, dis *dispatcher.EventDispatcher,
	verificationToken, encryptKey string, logger *zap.Logger) (*callbackServer, error) {
	if addr == "" {
		return nil, errors.New("feishu: callback listen address is required")
	}
	if dis == nil {
		return nil, errors.New("feishu: event dispatcher is required")
	}
	if path == "" {
		path = DefaultCallbackPath
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("feishu: listen on %s: %w", addr, err)
	}

	// Signature verification stays ON: a callback endpoint is public, so
	// Feishu's request signature must be validated (it is a no-op only when no
	// encrypt key is configured, which layer 1 covers instead).
	eventHandler := httpserverext.NewEventHandlerFunc(dis, larkevent.WithSkipSignVerify(false))

	guarded := func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxCallbackBodyBytes+1))
		if err != nil {
			http.Error(w, "cannot read body", http.StatusBadRequest)
			return
		}
		if len(body) > maxCallbackBodyBytes {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// The SDK needs to read the body itself, so restore it.
		r.Body = io.NopCloser(bytes.NewReader(body))

		if verificationToken != "" {
			tok, encrypted := extractCallbackToken(body)
			if !encrypted && tok != verificationToken {
				logger.Warn("feishu: rejecting callback with a bad verification token",
					zap.String("remote", r.RemoteAddr))
				http.Error(w, "bad verification token", http.StatusForbidden)
				return
			}
		}
		eventHandler(w, r)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, guarded)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	if verificationToken == "" && encryptKey == "" {
		logger.Warn("feishu: callback transport has no verification_token and no encrypt_key; " +
			"the endpoint will accept unauthenticated event payloads")
	}

	return &callbackServer{
		srv:    &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second},
		ln:     ln,
		logger: logger,
	}, nil
}

// Addr reports the actual bound address (useful when the config used port 0).
func (c *callbackServer) Addr() string {
	if c.ln == nil {
		return ""
	}
	return c.ln.Addr().String()
}

// Run serves callbacks until ctx is cancelled, then shuts down gracefully.
func (c *callbackServer) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		err := c.srv.Serve(c.ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	c.logger.Info("feishu callback server listening", zap.String("addr", c.Addr()))

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), callbackShutdownTimeout)
		defer cancel()
		if err := c.srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("feishu: callback shutdown: %w", err)
		}
		return nil
	}
}
