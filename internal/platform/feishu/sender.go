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

// messagePatcher is the minimal lark API surface used to update a card message.
type messagePatcher interface {
	Patch(ctx context.Context, req *im.PatchMessageReq, options ...larkcore.RequestOptionFunc) (*im.PatchMessageResp, error)
}

// messageRecaller is the minimal lark API surface used to delete a message.
type messageRecaller interface {
	Delete(ctx context.Context, req *im.DeleteMessageReq, options ...larkcore.RequestOptionFunc) (*im.DeleteMessageResp, error)
}

// CreateMessageFn creates a message and returns its message id. The id is
// needed to later update (patch) or recall the message.
type CreateMessageFn func(ctx context.Context, receiveID, receiveIDType, msgType, content string) (string, error)

// SendMessage is the error-only send primitive. It is injectable so tests do
// not need real Feishu credentials.
type SendMessage func(ctx context.Context, receiveID, receiveIDType, msgType, content string) error

// PatchMessageFn replaces the content of an existing card message.
type PatchMessageFn func(ctx context.Context, messageID, content string) error

// RecallMessageFn deletes an existing message.
type RecallMessageFn func(ctx context.Context, messageID string) error

// createViaLark adapts a real lark im message service to CreateMessageFn.
func createViaLark(create messageCreator) CreateMessageFn {
	return func(ctx context.Context, receiveID, receiveIDType, msgType, content string) (string, error) {
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
			return "", err
		}
		if !resp.Success() {
			return "", fmt.Errorf("feishu: send message: code=%d msg=%s", resp.Code, resp.Msg)
		}
		if resp.Data == nil || resp.Data.MessageId == nil {
			return "", nil
		}
		return *resp.Data.MessageId, nil
	}
}

// sendToLark adapts a real lark im message service to SendMessage, discarding
// the created message id.
func sendToLark(create messageCreator) SendMessage {
	fn := createViaLark(create)
	return func(ctx context.Context, receiveID, receiveIDType, msgType, content string) error {
		_, err := fn(ctx, receiveID, receiveIDType, msgType, content)
		return err
	}
}

// patchViaLark adapts a real lark im message service to PatchMessageFn.
func patchViaLark(patch messagePatcher) PatchMessageFn {
	return func(ctx context.Context, messageID, content string) error {
		req := im.NewPatchMessageReqBuilder().
			MessageId(messageID).
			Body(im.NewPatchMessageReqBodyBuilder().Content(content).Build()).
			Build()
		resp, err := patch.Patch(ctx, req)
		if err != nil {
			return err
		}
		if !resp.Success() {
			return fmt.Errorf("feishu: patch message: code=%d msg=%s", resp.Code, resp.Msg)
		}
		return nil
	}
}

// recallViaLark adapts a real lark im message service to RecallMessageFn.
func recallViaLark(recall messageRecaller) RecallMessageFn {
	return func(ctx context.Context, messageID string) error {
		req := im.NewDeleteMessageReqBuilder().MessageId(messageID).Build()
		resp, err := recall.Delete(ctx, req)
		if err != nil {
			return err
		}
		if !resp.Success() {
			return fmt.Errorf("feishu: recall message: code=%d msg=%s", resp.Code, resp.Msg)
		}
		return nil
	}
}

// Sender sends outbound messages back to Feishu.
type Sender interface {
	// ReplyText sends a plain-text message to the inbound message's chat.
	ReplyText(ctx context.Context, in Inbound, text string) error
	// ReplyCard sends an interactive card (title + markdown) to the inbound
	// message's chat.
	ReplyCard(ctx context.Context, in Inbound, title, markdown string) error
	// SendText sends a text message to a chat and returns its message id.
	SendText(ctx context.Context, chatID, text string) (string, error)
	// SendCard sends an interactive card built from markdown to a chat and
	// returns its message id.
	SendCard(ctx context.Context, chatID, title, markdown string) (string, error)
	// UpdateCard replaces the content of a previously sent card message. It is
	// how a placeholder ("thinking…") is turned into the final answer.
	UpdateCard(ctx context.Context, messageID, title, markdown string) error
	// Recall deletes a previously sent message (best effort).
	Recall(ctx context.Context, messageID string) error
}

// larkSender implements Sender over injectable primitives.
type larkSender struct {
	create CreateMessageFn
	patch  PatchMessageFn
	recall RecallMessageFn
}

// NewLarkSender returns a Sender that uses the given error-only send function.
// The returned sender cannot update or recall messages.
func NewLarkSender(send SendMessage) Sender {
	return &larkSender{create: func(ctx context.Context, receiveID, receiveIDType, msgType, content string) (string, error) {
		return "", send(ctx, receiveID, receiveIDType, msgType, content)
	}}
}

// NewLarkSenderWithCreate returns a Sender over the id-returning create
// primitive, optionally with update and recall support.
func NewLarkSenderWithCreate(create CreateMessageFn, opts ...SenderOption) Sender {
	s := &larkSender{create: create}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SenderOption configures a Sender built by NewLarkSenderWithCreate.
type SenderOption func(*larkSender)

// WithPatcher enables UpdateCard.
func WithPatcher(patch PatchMessageFn) SenderOption {
	return func(s *larkSender) { s.patch = patch }
}

// WithRecaller enables Recall.
func WithRecaller(recall RecallMessageFn) SenderOption {
	return func(s *larkSender) { s.recall = recall }
}

// textPayload is the JSON body of a text message.
type textPayload struct {
	Text string `json:"text"`
}

// cardConfig marks the interactive card as wide-screen.
type cardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
}

// cardHeader is the card's title bar.
type cardHeader struct {
	Title CardText `json:"title"`
}

// cardBody is the JSON body of an interactive card message.
type cardBody struct {
	Config   cardConfig    `json:"config"`
	Header   *cardHeader   `json:"header,omitempty"`
	Elements []CardElement `json:"elements,omitempty"`
}

// buildText encodes a text message payload.
func buildText(text string) (string, error) {
	b, err := json.Marshal(textPayload{Text: text})
	if err != nil {
		return "", fmt.Errorf("feishu: marshal text: %w", err)
	}
	return string(b), nil
}

// buildCard encodes an interactive card payload. When title is empty it is
// derived from the markdown; the markdown is converted into rich card elements
// (code blocks, dividers, chunked paragraphs).
func buildCard(title, markdown string) (string, error) {
	if title == "" {
		title = CardTitleFromMarkdown(markdown, 40)
	}
	card := cardBody{
		Config:   cardConfig{WideScreenMode: true},
		Header:   &cardHeader{Title: CardText{Tag: "plain_text", Content: title}},
		Elements: MarkdownToCardElements(markdown),
	}
	b, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("feishu: marshal card: %w", err)
	}
	return string(b), nil
}

// requireCreate returns an error when no create primitive is configured.
func (s *larkSender) requireCreate() error {
	if s.create == nil {
		return fmt.Errorf("feishu: no sender configured")
	}
	return nil
}

// SendText sends a text message to a chat and returns its message id.
func (s *larkSender) SendText(ctx context.Context, chatID, text string) (string, error) {
	if err := s.requireCreate(); err != nil {
		return "", err
	}
	body, err := buildText(text)
	if err != nil {
		return "", err
	}
	return s.create(ctx, chatID, "chat_id", "text", body)
}

// SendCard sends an interactive card to a chat and returns its message id.
func (s *larkSender) SendCard(ctx context.Context, chatID, title, markdown string) (string, error) {
	if err := s.requireCreate(); err != nil {
		return "", err
	}
	body, err := buildCard(title, markdown)
	if err != nil {
		return "", err
	}
	return s.create(ctx, chatID, "chat_id", "interactive", body)
}

// UpdateCard replaces the content of a previously sent card message.
func (s *larkSender) UpdateCard(ctx context.Context, messageID, title, markdown string) error {
	if messageID == "" {
		return fmt.Errorf("feishu: message_id is required to update a card")
	}
	if s.patch == nil {
		return fmt.Errorf("feishu: sender does not support updating cards")
	}
	body, err := buildCard(title, markdown)
	if err != nil {
		return err
	}
	return s.patch(ctx, messageID, body)
}

// Recall deletes a previously sent message.
func (s *larkSender) Recall(ctx context.Context, messageID string) error {
	if messageID == "" {
		return fmt.Errorf("feishu: message_id is required to recall a message")
	}
	if s.recall == nil {
		return fmt.Errorf("feishu: sender does not support recalling messages")
	}
	return s.recall(ctx, messageID)
}

// ReplyText sends a text message to the inbound message's chat.
func (s *larkSender) ReplyText(ctx context.Context, in Inbound, text string) error {
	_, err := s.SendText(ctx, in.ChatID, text)
	return err
}

// ReplyCard sends an interactive card to the inbound message's chat.
func (s *larkSender) ReplyCard(ctx context.Context, in Inbound, title, markdown string) error {
	_, err := s.SendCard(ctx, in.ChatID, title, markdown)
	return err
}
