package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// recordingHandler captures the inbound messages it receives.
type recordingHandler struct {
	mu   sync.Mutex
	got  []Inbound
	err  error
	done chan struct{}
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{done: make(chan struct{}, 8)}
}

func (r *recordingHandler) Handle(_ context.Context, in Inbound) error {
	r.mu.Lock()
	r.got = append(r.got, in)
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
	return r.err
}

func (r *recordingHandler) messages() []Inbound {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Inbound(nil), r.got...)
}

// waitFor blocks until the handler was called or the deadline passes.
func (r *recordingHandler) waitFor(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not called")
	}
}

// callbackEventPayload builds a schema-2.0 im.message.receive_v1 payload.
func callbackEventPayload(token, text string) []byte {
	body := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id":    "ev_1",
			"event_type":  "im.message.receive_v1",
			"create_time": "1700000000000",
			"token":       token,
			"app_id":      "cli_test",
			"tenant_key":  "tenant",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id":   map[string]any{"open_id": "ou_cb"},
				"sender_type": "user",
			},
			"message": map[string]any{
				"message_id":   "om_cb",
				"chat_id":      "oc_cb",
				"chat_type":    "p2p",
				"message_type": "text",
				"content":      fmt.Sprintf(`{"text":%q}`, text),
			},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func TestCallbackServer_ReceivesEvent(t *testing.T) {
	const token = "test-verification-token"
	handler := newRecordingHandler()

	app, err := NewApp(Config{
		AppID:             "cli_test",
		AppSecret:         "secret",
		Mode:              ModeCallback,
		CallbackAddr:      "127.0.0.1:0",
		VerificationToken: token,
		Logger:            zap.NewNop(),
	}, handler)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if app.Mode() != ModeCallback {
		t.Fatalf("Mode = %q, want callback", app.Mode())
	}
	addr := app.CallbackAddr()
	if addr == "" {
		t.Fatal("CallbackAddr is empty; listener was not bound")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }()

	url := "http://" + addr + DefaultCallbackPath
	resp, err := http.Post(url, "application/json", bytes.NewReader(callbackEventPayload(token, "hello callback")))
	if err != nil {
		t.Fatalf("POST event: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	handler.waitFor(t)
	got := handler.messages()
	if len(got) != 1 {
		t.Fatalf("handler saw %d messages, want 1", len(got))
	}
	if got[0].Text != "hello callback" {
		t.Errorf("Text = %q, want %q", got[0].Text, "hello callback")
	}
	if got[0].OpenID != "ou_cb" || got[0].ChatID != "oc_cb" {
		t.Errorf("routing fields wrong: %+v", got[0])
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run returned %v, want nil after graceful shutdown", err)
		}
	case <-time.After(6 * time.Second):
		t.Error("Run did not return after context cancellation")
	}
}

func TestCallbackServer_Healthz(t *testing.T) {
	handler := newRecordingHandler()
	app, err := NewApp(Config{
		AppID: "cli_test", AppSecret: "s",
		Mode: ModeCallback, CallbackAddr: "127.0.0.1:0",
		Logger: zap.NewNop(),
	}, handler)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(ctx) }()

	resp, err := http.Get("http://" + app.CallbackAddr() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCallbackServer_RejectsWrongToken(t *testing.T) {
	handler := newRecordingHandler()
	app, err := NewApp(Config{
		AppID: "cli_test", AppSecret: "s",
		Mode: ModeCallback, CallbackAddr: "127.0.0.1:0",
		VerificationToken: "expected-token",
		Logger:            zap.NewNop(),
	}, handler)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(ctx) }()

	resp, err := http.Post("http://"+app.CallbackAddr()+DefaultCallbackPath,
		"application/json", bytes.NewReader(callbackEventPayload("wrong-token", "nope")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The handler must never see a request carrying the wrong token.
	select {
	case <-handler.done:
		t.Error("handler was called for a request with a bad verification token")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestCallbackServer_CustomPath(t *testing.T) {
	handler := newRecordingHandler()
	app, err := NewApp(Config{
		AppID: "cli_test", AppSecret: "s",
		Mode: ModeCallback, CallbackAddr: "127.0.0.1:0",
		CallbackPath: "/custom/hook",
		Logger:       zap.NewNop(),
	}, handler)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(ctx) }()

	resp, err := http.Post("http://"+app.CallbackAddr()+"/custom/hook",
		"application/json", bytes.NewReader(callbackEventPayload("", "custom path")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	handler.waitFor(t)

	if got := handler.messages(); len(got) != 1 || got[0].Text != "custom path" {
		t.Errorf("handler did not receive the event on the custom path: %+v", got)
	}
}

func TestNewApp_CallbackRequiresAddr(t *testing.T) {
	_, err := NewApp(Config{
		AppID: "cli_test", AppSecret: "s",
		Mode:   ModeCallback, // no CallbackAddr
		Logger: zap.NewNop(),
	}, newRecordingHandler())
	if err == nil {
		t.Fatal("want error when callback mode has no listen address")
	}
	if !strings.Contains(err.Error(), "callback listen address") {
		t.Errorf("err = %v, want it to mention the listen address", err)
	}
}

func TestNewApp_WebSocketNeedsNoAddr(t *testing.T) {
	app, err := NewApp(Config{AppID: "cli_test", AppSecret: "s", Logger: zap.NewNop()}, newRecordingHandler())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if app.Mode() != ModeWebSocket {
		t.Errorf("Mode = %q, want websocket", app.Mode())
	}
	if app.CallbackAddr() != "" {
		t.Errorf("CallbackAddr = %q, want empty in websocket mode", app.CallbackAddr())
	}
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeWebSocket, false},
		{"websocket", ModeWebSocket, false},
		{"ws", ModeWebSocket, false},
		{"  WebSocket ", ModeWebSocket, false},
		{"long_connection", ModeWebSocket, false},
		{"callback", ModeCallback, false},
		{"http", ModeCallback, false},
		{"webhook", ModeCallback, false},
		{"CALLBACK", ModeCallback, false},
		{"carrier-pigeon", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseMode(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ParseMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
