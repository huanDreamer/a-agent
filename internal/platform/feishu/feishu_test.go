package feishu

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"go.uber.org/zap"
)

func strp(s string) *string { return &s }

func TestParseInboundMessage(t *testing.T) {
	t.Run("text message", func(t *testing.T) {
		ev := &im.P2MessageReceiveV1{
			Event: &im.P2MessageReceiveV1Data{
				Message: &im.EventMessage{
					MessageId:   strp("om_1"),
					ChatId:      strp("oc_1"),
					ChatType:    strp("p2p"),
					MessageType: strp("text"),
					Content:     strp(`{"text":"hello world"}`),
				},
				Sender: &im.EventSender{
					SenderId: &im.UserId{OpenId: strp("ou_123")},
				},
			},
		}
		in := parseInboundMessage(ev)
		if in.OpenID != "ou_123" {
			t.Errorf("OpenID = %q, want ou_123", in.OpenID)
		}
		if in.ChatID != "oc_1" {
			t.Errorf("ChatID = %q, want oc_1", in.ChatID)
		}
		if in.MsgType != "text" || !in.IsText() {
			t.Errorf("IsText = %v, MsgType=%q", in.IsText(), in.MsgType)
		}
		if in.Text != "hello world" {
			t.Errorf("Text = %q, want hello world", in.Text)
		}
	})

	t.Run("non-text and nil event", func(t *testing.T) {
		if in := parseInboundMessage(nil); !reflect.DeepEqual(in, Inbound{}) {
			t.Errorf("nil event should yield zero Inbound, got %+v", in)
		}
		ev := &im.P2MessageReceiveV1{Event: &im.P2MessageReceiveV1Data{
			Message: &im.EventMessage{MessageType: strp("image"), Content: strp(`{"img":"x"}`)},
		}}
		in := parseInboundMessage(ev)
		if in.IsText() {
			t.Error("image msg reported as text")
		}
		if in.Text != "" {
			t.Errorf("non-text Text = %q, want empty", in.Text)
		}
	})
}

func TestDecodeTextContent(t *testing.T) {
	tests := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"plain", `{"text":"hi"}`, "hi", false},
		{"empty", `{"text":""}`, "", false},
		{"bad json", `not-json`, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeTextContent(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsText(t *testing.T) {
	if !(Inbound{MsgType: "text"}).IsText() {
		t.Error("text type should be text")
	}
	if (Inbound{MsgType: "post"}).IsText() {
		t.Error("post type should not be text")
	}
}

// fakeSend records calls and returns a preset error.
type fakeSend struct {
	mu    sync.Mutex
	calls []sendCall
	err   error
}

type sendCall struct{ receiveID, receiveIDType, msgType, content string }

func (f *fakeSend) send(_ context.Context, receiveID, receiveIDType, msgType, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sendCall{receiveID, receiveIDType, msgType, content})
	return f.err
}

func TestReplyText(t *testing.T) {
	fs := &fakeSend{}
	s := NewLarkSender(fs.send)
	in := Inbound{ChatID: "oc_x"}
	err := s.ReplyText(context.Background(), in, "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(fs.calls))
	}
	c := fs.calls[0]
	if c.receiveID != "oc_x" || c.receiveIDType != "chat_id" || c.msgType != "text" {
		t.Errorf("unexpected send: %+v", c)
	}
	if !strings.Contains(c.content, "\"text\":\"hello\"") {
		t.Errorf("content missing text: %s", c.content)
	}
}

func TestReplyCard(t *testing.T) {
	fs := &fakeSend{}
	s := NewLarkSender(fs.send)
	err := s.ReplyCard(context.Background(), Inbound{ChatID: "oc_y"}, "Title", "**md**")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := fs.calls[0]
	if c.msgType != "interactive" {
		t.Errorf("msgType = %q, want interactive", c.msgType)
	}
	if !strings.Contains(c.content, "Title") || !strings.Contains(c.content, "**md**") {
		t.Errorf("card content missing title/body: %s", c.content)
	}
}

func TestReplyErrors(t *testing.T) {
	// nil sender callbacks are rejected.
	bad := &larkSender{create: nil}
	if err := bad.ReplyText(context.Background(), Inbound{}, "x"); err == nil {
		t.Error("expected error when send is nil")
	}
	if err := bad.ReplyCard(context.Background(), Inbound{}, "t", "m"); err == nil {
		t.Error("expected error when send is nil")
	}

	// Propagated send error.
	sentErr := errors.New("boom")
	fs := &fakeSend{err: sentErr}
	s := NewLarkSender(fs.send)
	if err := s.ReplyText(context.Background(), Inbound{}, "x"); !errors.Is(err, sentErr) {
		t.Errorf("want boom, got %v", err)
	}
	if err := s.ReplyCard(context.Background(), Inbound{}, "t", "m"); !errors.Is(err, sentErr) {
		t.Errorf("card want boom, got %v", err)
	}
}

// fakeCreator implements messageCreator.
type fakeCreator struct {
	mu    sync.Mutex
	calls int
	resp  *im.CreateMessageResp
	err   error
}

func (f *fakeCreator) Create(_ context.Context, _ *im.CreateMessageReq, _ ...larkcore.RequestOptionFunc) (*im.CreateMessageResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func TestSendToLark(t *testing.T) {
	// Success.
	ok := &fakeCreator{resp: &im.CreateMessageResp{}}
	send := sendToLark(ok)
	if err := send(context.Background(), "oc", "chat_id", "text", `{"text":"hi"}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok.calls != 1 {
		t.Fatalf("want 1 call, got %d", ok.calls)
	}

	// Non-success resp -> error.
	if sendToLark(&fakeCreator{resp: &im.CreateMessageResp{CodeError: larkcore.CodeError{Code: 5, Msg: "nope"}}})(context.Background(), "oc", "chat_id", "text", "{}") == nil {
		t.Error("expected error for code != 0")
	}

	// Network error propagated.
	if err := sendToLark(&fakeCreator{err: errors.New("net")})(context.Background(), "oc", "chat_id", "text", "{}"); err == nil {
		t.Error("expected network error")
	}
}

func TestHandlerFunc(t *testing.T) {
	called := false
	h := HandlerFunc(func(_ context.Context, in Inbound) error {
		called = true
		if in.OpenID != "ou_1" {
			t.Errorf("OpenID = %q", in.OpenID)
		}
		return nil
	})
	if err := h.Handle(context.Background(), Inbound{OpenID: "ou_1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !called {
		t.Error("handler func was not called")
	}
}

func TestNewApp(t *testing.T) {
	handler := HandlerFunc(func(context.Context, Inbound) error { return nil })

	if _, err := NewApp(Config{}, handler); err == nil {
		t.Error("empty app_id should error")
	}
	if _, err := NewApp(Config{AppID: "cli_x"}, nil); err == nil {
		t.Error("nil handler should error")
	}
	// Defaults: no logger uses a nop logger, empty domain uses the default URL.
	app, err := NewApp(Config{AppID: "cli_x"}, handler)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if app == nil || app.logger == nil || app.ws == nil {
		t.Error("expected built app with logger and websocket client")
	}
	// With explicit logger and domain.
	logger := zap.NewNop()
	app2, err := NewApp(Config{
		AppID: "cli_y", Domain: "https://open.larksuite.com", Logger: logger,
	}, handler)
	if err != nil {
		t.Fatalf("NewApp custom: %v", err)
	}
	if app2.logger != logger {
		t.Error("custom logger not used")
	}

	// Running a nil (uninitialized) transport errors.
	if err := (&App{}).Run(context.Background()); err == nil {
		t.Error("Run on nil transport should error")
	}
}

func TestRealSender(t *testing.T) {
	if _, err := RealSender("", "sec", ""); err == nil {
		t.Error("empty app_id should error")
	}
	s, err := RealSender("cli_x", "sec", "")
	if err != nil {
		t.Fatalf("RealSender: %v", err)
	}
	if s == nil {
		t.Fatal("missing sender")
	}
	// Custom domain path.
	if _, err := RealSender("cli_x", "sec", "https://open.larksuite.com"); err != nil {
		t.Fatalf("custom domain: %v", err)
	}
}
