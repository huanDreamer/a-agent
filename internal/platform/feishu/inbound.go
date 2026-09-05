package feishu

import (
	"encoding/json"

	v1 "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Inbound is a normalized inbound message from Feishu.
type Inbound struct {
	// OpenID is the sender's open_id within this app.
	OpenID string
	// ChatID is the chat (p2p or group) the message arrived in.
	ChatID string
	// ChatType is "p2p" or "group".
	ChatType string
	// MessageID is the Feishu message id (used to reply/reference).
	MessageID string
	// Text is the decoded text for a text message ("" otherwise).
	Text string
	// MsgType is the raw message type, e.g. "text".
	MsgType string
}

// IsText reports whether the inbound message is a text message.
func (in Inbound) IsText() bool { return in.MsgType == "text" }

// textContent is the decoded shape of a text message's content field.
type textContent struct {
	Text string `json:"text"`
}

// decodeTextContent extracts the "text" field from a content JSON blob.
func decodeTextContent(content string) (string, error) {
	var tc textContent
	if err := json.Unmarshal([]byte(content), &tc); err != nil {
		return "", err
	}
	return tc.Text, nil
}

// parseInboundMessage converts a raw SDK event into a normalized Inbound.
func parseInboundMessage(ev *v1.P2MessageReceiveV1) Inbound {
	var in Inbound
	if ev == nil || ev.Event == nil {
		return in
	}
	data := ev.Event
	if m := data.Message; m != nil {
		in.MessageID = strval(m.MessageId)
		in.ChatID = strval(m.ChatId)
		in.ChatType = strval(m.ChatType)
		in.MsgType = strval(m.MessageType)
	}
	if s := data.Sender; s != nil && s.SenderId != nil {
		in.OpenID = strval(s.SenderId.OpenId)
	}
	if in.MsgType == "text" && data.Message != nil {
		if t, err := decodeTextContent(strval(data.Message.Content)); err == nil {
			in.Text = t
		}
	}
	return in
}

func strval(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
