package feishu

import (
	"context"
	"encoding/json"
	"fmt"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// messageCreator is the minimal lark API surface used to send a message. The
// SDK's *im.message type (unexported) satisfies it, so we take it by interface.
type messageCreator interface {
	Create(ctx context.Context, req *im.CreateMessageReq, options ...larkcore.RequestOptionFunc) (*im.CreateMessageResp, error)
}

// sendToLark adapts a real lark im message service to SendMessage.
func sendToLark(create messageCreator) SendMessage {
	return func(ctx context.Context, receiveID, receiveIDType, msgType, content string) error {
		req := im.NewCreateMessageReqBuilder().
			ReceiveIdType(receiveIDType).
			Body(im.NewCreateMessageReqBodyBuilder().
				ReceiveId(receiveID).
				MsgType(msgType).
				Content(content).
				Build()).
			Build()
		resp, err := create.Create(ctx, req)
		if err != nil {
			return err
		}
		if !resp.Success() {
			return fmt.Errorf("feishu: send message: code=%d msg=%s", resp.Code, resp.Msg)
		}
		return nil
	}
}

// Sender sends outbound messages back to Feishu.
type Sender interface {
	// ReplyText sends a plain-text reply to the inbound message's chat.
	ReplyText(ctx context.Context, in Inbound, text string) error
	// ReplyCard sends an interactive card (title + markdown) reply.
	ReplyCard(ctx context.Context, in Inbound, title, markdown string) error
}

// SendMessage is the low-level send primitive. It is injectable so tests do
// not need real Feishu credentials.
type SendMessage func(ctx context.Context, receiveID, receiveIDType, msgType, content string) error

// larkSender implements Sender over a SendMessage function.
type larkSender struct {
	send SendMessage
}

// NewLarkSender returns a Sender that uses the given send function.
func NewLarkSender(send SendMessage) Sender {
	return &larkSender{send: send}
}

type textReplyBody struct {
	Text string `json:"text"`
}

// ReplyText sends a text message to the inbound message's chat.
func (s *larkSender) ReplyText(ctx context.Context, in Inbound, text string) error {
	if s.send == nil {
		return fmt.Errorf("feishu: no sender configured")
	}
	body, err := json.Marshal(textReplyBody{Text: text})
	if err != nil {
		return fmt.Errorf("feishu: marshal text: %w", err)
	}
	return s.send(ctx, in.ChatID, "chat_id", "text", string(body))
}

// cardConfig marks the interactive card as wide-screen.
type cardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
}

type cardMedia struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type cardElement struct {
	Tag  string    `json:"tag"`
	Text cardMedia `json:"text"`
}

type cardHeader struct {
	Title cardMedia `json:"title"`
}

type cardBody struct {
	MsgType  string        `json:"msg_type"`
	Config   cardConfig    `json:"config"`
	Header   *cardHeader   `json:"header,omitempty"`
	Elements []cardElement `json:"elements,omitempty"`
}

// ReplyCard sends an interactive card with a plain-text title and markdown body.
func (s *larkSender) ReplyCard(ctx context.Context, in Inbound, title, markdown string) error {
	if s.send == nil {
		return fmt.Errorf("feishu: no sender configured")
	}
	card := cardBody{
		MsgType: "interactive",
		Config:  cardConfig{WideScreenMode: true},
		Header:  &cardHeader{Title: cardMedia{Tag: "plain_text", Content: title}},
		Elements: []cardElement{
			{Tag: "div", Text: cardMedia{Tag: "lark_md", Content: markdown}},
		},
	}
	body, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("feishu: marshal card: %w", err)
	}
	return s.send(ctx, in.ChatID, "chat_id", "interactive", string(body))
}
