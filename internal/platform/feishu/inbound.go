package feishu

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// ResourceKind classifies an inbound attachment.
type ResourceKind string

const (
	// ResourceImage is an inline or standalone image.
	ResourceImage ResourceKind = "image"
	// ResourceFile is a generic uploaded file.
	ResourceFile ResourceKind = "file"
	// ResourceAudio is a voice message.
	ResourceAudio ResourceKind = "audio"
	// ResourceMedia is a video (with optional cover image).
	ResourceMedia ResourceKind = "media"
	// ResourceSticker is a sticker/emoji message.
	ResourceSticker ResourceKind = "sticker"
)

// Resource is an attachment referenced by an inbound message.
//
// Key is the opaque handle to download the attachment through the
// message-resource API: it carries the `image_key` for images and the
// `file_key` for every other kind.
type Resource struct {
	Kind ResourceKind
	Key  string
	// Name is the original file name when Feishu provides one.
	Name string
	// Duration is the media length in milliseconds (audio/media only).
	Duration int
}

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
	// MsgType is the raw message type, e.g. "text", "image", "file", "post".
	MsgType string
	// Title is the rich-text (post) heading, when present.
	Title string
	// Resources are the attachments carried by the message, in order.
	Resources []Resource
	// Mentioned reports whether the bot was @-mentioned in the message.
	Mentioned bool
}

// IsText reports whether the inbound message is a plain text message.
func (in Inbound) IsText() bool { return in.MsgType == "text" }

// IsPost reports whether the inbound message is a rich-text (post) message.
func (in Inbound) IsPost() bool { return in.MsgType == "post" }

// HasResources reports whether the message carries any attachment.
func (in Inbound) HasResources() bool { return len(in.Resources) > 0 }

// Handled reports whether the message type carries something the agent can
// act on. Anything else (recall notices, reaction events, ...) is ignored.
func (in Inbound) Handled() bool {
	return in.IsText() || in.IsPost() || in.HasResources()
}

// PromptText renders the inbound message as text for the model: the raw text
// for text messages, a flattened rendering for rich-text (post) messages, and
// a bracketed placeholder for attachment-only messages. Local file paths are
// intentionally not included; the caller appends them after downloading.
func (in Inbound) PromptText() string {
	switch {
	case in.IsText():
		return strings.TrimSpace(in.Text)
	case in.IsPost():
		var b strings.Builder
		if t := strings.TrimSpace(in.Title); t != "" {
			b.WriteString(t)
			b.WriteString("\n")
		}
		b.WriteString(in.Text)
		return strings.TrimSpace(b.String())
	default:
		return in.describeResources()
	}
}

// describeResources renders a short human-readable description of the
// attachments, used when a message has no text body.
func (in Inbound) describeResources() string {
	if len(in.Resources) == 0 {
		return ""
	}
	parts := make([]string, 0, len(in.Resources))
	for _, r := range in.Resources {
		switch r.Kind {
		case ResourceImage:
			parts = append(parts, "[图片]")
		case ResourceFile:
			if r.Name != "" {
				parts = append(parts, fmt.Sprintf("[文件: %s]", r.Name))
			} else {
				parts = append(parts, "[文件]")
			}
		case ResourceAudio:
			parts = append(parts, fmt.Sprintf("[语音 %ds]", r.Duration/1000))
		case ResourceMedia:
			if r.Name != "" {
				parts = append(parts, fmt.Sprintf("[视频: %s]", r.Name))
			} else {
				parts = append(parts, "[视频]")
			}
		case ResourceSticker:
			parts = append(parts, "[表情]")
		default:
			parts = append(parts, "[附件]")
		}
	}
	return strings.Join(parts, " ")
}

// textContent is the decoded shape of a text message's content field.
type textContent struct {
	Text string `json:"text"`
}

// imageContent is the decoded shape of an image message's content field.
type imageContent struct {
	ImageKey string `json:"image_key"`
}

// fileContent is the decoded shape of file / audio / media / sticker content.
type fileContent struct {
	FileKey  string `json:"file_key"`
	FileName string `json:"file_name"`
	ImageKey string `json:"image_key"`
	Duration int    `json:"duration"`
}

// postElement is one inline element of a rich-text (post) message.
type postElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	Href     string `json:"href"`
	ImageKey string `json:"image_key"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	FileName string `json:"file_name"`
}

// postContent is the decoded shape of a rich-text (post) message's content.
type postContent struct {
	Title   string          `json:"title"`
	Content [][]postElement `json:"content"`
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
		in.Mentioned = len(m.Mentions) > 0
		if c := strval(m.Content); c != "" {
			in.decodeContent(c)
		}
	}
	if s := data.Sender; s != nil && s.SenderId != nil {
		in.OpenID = strval(s.SenderId.OpenId)
	}
	return in
}

// decodeContent fills Text/Title/Resources from the raw content JSON blob,
// according to the message type. Malformed content is ignored (best effort):
// the message is still delivered, just with no body.
func (in *Inbound) decodeContent(content string) {
	switch in.MsgType {
	case "text":
		if t, err := decodeTextContent(content); err == nil {
			in.Text = t
		}
	case "post":
		in.decodePost(content)
	case "image":
		var ic imageContent
		if err := json.Unmarshal([]byte(content), &ic); err == nil && ic.ImageKey != "" {
			in.Resources = append(in.Resources, Resource{Kind: ResourceImage, Key: ic.ImageKey})
		}
	case "file":
		if fc, ok := decodeFileContent(content); ok {
			in.Resources = append(in.Resources, Resource{
				Kind: ResourceFile, Key: fc.FileKey, Name: fc.FileName,
			})
		}
	case "audio":
		if fc, ok := decodeFileContent(content); ok {
			in.Resources = append(in.Resources, Resource{
				Kind: ResourceAudio, Key: fc.FileKey, Duration: fc.Duration,
			})
		}
	case "media":
		if fc, ok := decodeFileContent(content); ok {
			in.Resources = append(in.Resources, Resource{
				Kind: ResourceMedia, Key: fc.FileKey, Name: fc.FileName, Duration: fc.Duration,
			})
		}
	case "sticker":
		if fc, ok := decodeFileContent(content); ok {
			in.Resources = append(in.Resources, Resource{Kind: ResourceSticker, Key: fc.FileKey})
		}
	}
}

// decodeFileContent parses a file-like content blob, requiring a file key.
func decodeFileContent(content string) (fileContent, bool) {
	var fc fileContent
	if err := json.Unmarshal([]byte(content), &fc); err != nil {
		return fc, false
	}
	if fc.FileKey == "" {
		return fc, false
	}
	return fc, true
}

// decodePost flattens a rich-text message into plain text plus its inline
// images. Paragraphs are joined with newlines.
func (in *Inbound) decodePost(content string) {
	var pc postContent
	if err := json.Unmarshal([]byte(content), &pc); err != nil {
		return
	}
	in.Title = pc.Title

	paragraphs := make([]string, 0, len(pc.Content))
	for _, para := range pc.Content {
		var b strings.Builder
		for _, el := range para {
			switch el.Tag {
			case "text":
				b.WriteString(el.Text)
			case "a":
				if el.Href != "" && el.Text != "" {
					b.WriteString(fmt.Sprintf("[%s](%s)", el.Text, el.Href))
				} else {
					b.WriteString(el.Text)
				}
			case "at":
				if el.UserName != "" {
					b.WriteString("@" + el.UserName)
				} else {
					b.WriteString("@")
				}
			case "img":
				b.WriteString("[图片]")
				if el.ImageKey != "" {
					in.Resources = append(in.Resources, Resource{
						Kind: ResourceImage, Key: el.ImageKey,
					})
				}
			case "media":
				b.WriteString("[视频]")
				if el.FileName != "" {
					b.WriteString("(" + el.FileName + ")")
				}
			case "emotion":
				b.WriteString("[表情]")
			case "code_block":
				if el.Text != "" {
					b.WriteString("\n```\n" + el.Text + "\n```\n")
				}
			case "hr":
				b.WriteString("\n---\n")
			default:
				b.WriteString(el.Text)
			}
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			paragraphs = append(paragraphs, s)
		}
	}
	in.Text = strings.Join(paragraphs, "\n")
}

func strval(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
