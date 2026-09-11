package feishu

import (
	"context"
	"errors"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"go.uber.org/zap"
)

// Handler processes an inbound message. Implementations reply via the Sender.
type Handler interface {
	Handle(ctx context.Context, in Inbound) error
}

// HandlerFunc adapts a plain function to a Handler.
type HandlerFunc func(ctx context.Context, in Inbound) error

// Handle implements the Handler interface.
func (f HandlerFunc) Handle(ctx context.Context, in Inbound) error { return f(ctx, in) }

// Mode selects how the bot receives events from Feishu.
type Mode string

const (
	// ModeWebSocket uses the WebSocket long-connection. It needs no public
	// URL and is the default.
	ModeWebSocket Mode = "websocket"
	// ModeCallback receives events over HTTP. It requires a publicly reachable
	// URL registered in the Feishu console.
	ModeCallback Mode = "callback"
)

// ParseMode normalizes a configured transport name. The empty string and the
// "ws" alias mean WebSocket; "http" and "webhook" are aliases for callback.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "ws", "websocket", "long_connection", "long-connection":
		return ModeWebSocket, nil
	case "callback", "http", "webhook":
		return ModeCallback, nil
	default:
		return "", fmt.Errorf("feishu: unknown transport mode %q (want websocket or callback)", s)
	}
}

// Config is what NewApp needs to run the Feishu bot transport.
type Config struct {
	AppID             string
	AppSecret         string
	Domain            string
	VerificationToken string
	EncryptKey        string
	// Mode selects WebSocket (default) or HTTP callback delivery.
	Mode Mode
	// CallbackAddr is the listen address for ModeCallback, e.g. "0.0.0.0:8080".
	CallbackAddr string
	// CallbackPath is the event path for ModeCallback. Defaults to
	// DefaultCallbackPath.
	CallbackPath string
	// Sender is the outbound sender. It must be set (the bot core injects a
	// real one wired to the lark client).
	Sender Sender
	Logger *zap.Logger
}

// App is the Feishu bot transport: it owns either the WebSocket
// long-connection client or the HTTP callback server, and forwards inbound
// messages to a Handler.
type App struct {
	cfg        Config
	logger     *zap.Logger
	handler    Handler
	dispatcher *dispatcher.EventDispatcher
	mode       Mode
	ws         *larkws.Client
	callback   *callbackServer
}

// NewApp builds the Feishu transport. AppID is required; the sender is the
// caller's responsibility so the transport stays network-only and testable.
// In callback mode the listen socket is bound here, so an unusable address
// fails fast rather than when Run is called.
func NewApp(cfg Config, handler Handler) (*App, error) {
	if cfg.AppID == "" {
		return nil, errors.New("feishu: app_id is required")
	}
	if handler == nil {
		return nil, errors.New("feishu: handler is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	mode, err := ParseMode(string(cfg.Mode))
	if err != nil {
		return nil, err
	}

	dis := newDispatcher(cfg, handler, logger)
	app := &App{
		cfg:        cfg,
		logger:     logger,
		handler:    handler,
		dispatcher: dis,
		mode:       mode,
	}

	switch mode {
	case ModeCallback:
		cb, err := newCallbackServer(cfg.CallbackAddr, cfg.CallbackPath, dis,
			cfg.VerificationToken, cfg.EncryptKey, logger)
		if err != nil {
			return nil, err
		}
		app.callback = cb
	default:
		domain := cfg.Domain
		if domain == "" {
			domain = lark.FeishuBaseUrl
		}
		app.ws = larkws.NewClient(cfg.AppID, cfg.AppSecret,
			larkws.WithEventHandler(dis),
			larkws.WithDomain(domain),
		)
	}
	return app, nil
}

// newDispatcher builds the event dispatcher shared by both transports, so the
// same normalization, logging and handler wiring applies either way.
func newDispatcher(cfg Config, handler Handler, logger *zap.Logger) *dispatcher.EventDispatcher {
	return dispatcher.NewEventDispatcher(cfg.VerificationToken, cfg.EncryptKey).
		OnP2MessageReceiveV1(
			func(ctx context.Context, ev *im.P2MessageReceiveV1) error {
				if ev == nil {
					logger.Warn("feishu: received nil message event")
					return nil
				}
				in := parseInboundMessage(ev)
				// Always log what we got at info level so a missing reply is
				// distinguishable from "event never arrived". Include enough
				// to see whether the payload was parsed (chat/open/type).
				logger.Info("feishu message received",
					zap.String("msg_type", in.MsgType),
					zap.String("open_id", in.OpenID),
					zap.String("chat_id", in.ChatID),
					zap.String("chat_type", in.ChatType),
					zap.Int("text_len", len(in.Text)),
					zap.Int("resources", len(in.Resources)),
					zap.Bool("mentioned", in.Mentioned),
				)
				if !in.Handled() {
					logger.Info("feishu: ignoring unsupported message",
						zap.String("type", in.MsgType))
					return nil
				}
				if err := handler.Handle(ctx, in); err != nil {
					logger.Error("feishu handler failed",
						zap.Error(err), zap.String("open_id", in.OpenID))
				}
				return nil
			})
}

// Mode reports the transport this App runs.
func (a *App) Mode() Mode { return a.mode }

// CallbackAddr reports the bound callback address ("" in WebSocket mode).
func (a *App) CallbackAddr() string {
	if a.callback == nil {
		return ""
	}
	return a.callback.Addr()
}

// Run blocks on the configured transport until ctx is done.
func (a *App) Run(ctx context.Context) error {
	if a.mode == ModeCallback {
		if a.callback == nil {
			return errors.New("feishu: callback transport not initialized")
		}
		return a.callback.Run(ctx)
	}
	if a.ws == nil {
		return errors.New("feishu: transport not initialized")
	}
	go func() { <-ctx.Done(); a.ws.Close() }()
	return a.ws.Start(ctx)
}

// RealSender builds a Sender wired to a real lark client using the given
// credentials. It returns a non-nil Sender ready for NewApp's Config.Sender.
func RealSender(appID, appSecret, domain string) (Sender, error) {
	create, patch, recall, err := RealSenderFuncs(appID, appSecret, domain)
	if err != nil {
		return nil, err
	}
	return NewLarkSenderWithCreate(create, WithPatcher(patch), WithRecaller(recall)), nil
}

// RealSenderFuncs returns the send primitives backed by a single real lark
// client, so callers can wrap the create step with their own resilience layer
// (retry, rate limiting, timeout) before handing it to NewLarkSenderWithCreate.
func RealSenderFuncs(appID, appSecret, domain string) (CreateMessageFn, PatchMessageFn, RecallMessageFn, error) {
	if appID == "" {
		return nil, nil, nil, errors.New("feishu: app_id is required")
	}
	if domain == "" {
		domain = lark.FeishuBaseUrl
	}
	opts := []lark.ClientOptionFunc{}
	if domain != lark.FeishuBaseUrl {
		opts = append(opts, lark.WithOpenBaseUrl(domain))
	}
	cli := lark.NewClient(appID, appSecret, opts...)
	return createViaLark(cli.Im.Message),
		patchViaLark(cli.Im.Message),
		recallViaLark(cli.Im.Message),
		nil
}
