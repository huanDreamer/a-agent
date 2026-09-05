package feishu

import (
	"context"
	"errors"

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

// Config is what NewApp needs to run the Feishu bot transport.
type Config struct {
	AppID             string
	AppSecret         string
	Domain            string
	VerificationToken string
	EncryptKey        string
	// Sender is the outbound sender. It must be set (the bot core injects a
	// real one wired to the lark client).
	Sender Sender
	Logger *zap.Logger
}

// App is the Feishu bot transport: it owns the WebSocket long-connection
// client and forwards inbound messages to a Handler.
type App struct {
	ws      *larkws.Client
	logger  *zap.Logger
	handler Handler
}

// NewApp builds the Feishu transport. AppID is required; the sender is the
// caller's responsibility so the transport stays network-only and testable.
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

	dis := dispatcher.NewEventDispatcher(cfg.VerificationToken, cfg.EncryptKey).
		OnP2MessageReceiveV1(
			func(ctx context.Context, ev *im.P2MessageReceiveV1) error {
				in := parseInboundMessage(ev)
				if !in.IsText() {
					logger.Debug("ignoring non-text message", zap.String("type", in.MsgType))
					return nil
				}
				if err := handler.Handle(ctx, in); err != nil {
					logger.Error("feishu handler failed",
						zap.Error(err), zap.String("open_id", in.OpenID))
				}
				return nil
			})

	domain := cfg.Domain
	if domain == "" {
		domain = lark.FeishuBaseUrl
	}
	ws := larkws.NewClient(cfg.AppID, cfg.AppSecret,
		larkws.WithEventHandler(dis),
		larkws.WithDomain(domain),
	)
	return &App{ws: ws, logger: logger, handler: handler}, nil
}

// RealSender builds a Sender wired to a real lark client using the given
// credentials. It returns a non-nil Sender ready for NewApp's Config.Sender.
func RealSender(appID, appSecret, domain string) (Sender, error) {
	if appID == "" {
		return nil, errors.New("feishu: app_id is required")
	}
	if domain == "" {
		domain = lark.FeishuBaseUrl
	}
	opts := []lark.ClientOptionFunc{}
	if domain != lark.FeishuBaseUrl && domain != "" {
		opts = append(opts, lark.WithOpenBaseUrl(domain))
	}
	cli := lark.NewClient(appID, appSecret, opts...)
	return &larkSender{send: sendToLark(cli.Im.Message)}, nil
}

// Run blocks on the Feishu WebSocket long-connection until ctx is done.
func (a *App) Run(ctx context.Context) error {
	if a.ws == nil {
		return errors.New("feishu: transport not initialized")
	}
	go func() { <-ctx.Done(); a.ws.Close() }()
	return a.ws.Start(ctx)
}
