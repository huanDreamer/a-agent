package llm

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestToOpenAIMessages_TextStaysAString guards the shape every existing caller
// depends on: a message with no multimodal content is sent with a plain string
// body, not a one-element content array.
func TestToOpenAIMessages_TextStaysAString(t *testing.T) {
	m := &openAIModel{provider: Provider{Name: "mock", Model: "m"}}
	got := m.toOpenAIMessages([]*schema.Message{
		{Role: schema.System, Content: "be brief"},
		{Role: schema.User, Content: "hello"},
		{Role: schema.Assistant, Content: "hi", ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "clock", Arguments: "{}"},
		}}},
		{Role: schema.Tool, Content: "12:00", ToolCallID: "c1", ToolName: "clock"},
	})
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4", len(got))
	}
	for i, msg := range got {
		if len(msg.MultiContent) != 0 {
			t.Errorf("message %d was converted to multimodal content", i)
		}
	}
	if got[2].Content != "hi" || len(got[2].ToolCalls) != 1 || got[2].ToolCalls[0].Function.Name != "clock" {
		t.Errorf("assistant message lost its tool call: %+v", got[2])
	}
	if got[3].ToolCallID != "c1" || got[3].Role != "tool" {
		t.Errorf("tool result lost its call id: %+v", got[3])
	}
}

// TestToOpenAIMessages_ImageBecomesADataURL is the wire contract for an attached
// image: OpenAI-compatible providers take the bytes as an RFC-2397 data URL in a
// content array, and refuse a message that carries both Content and parts.
func TestToOpenAIMessages_ImageBecomesADataURL(t *testing.T) {
	m := &openAIModel{provider: Provider{Name: "mock", Model: "m"}}
	encoded := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	got := m.toOpenAIMessages([]*schema.Message{{
		Role:    schema.User,
		Content: "what is in this image?",
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "what is in this image?"},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{
					Base64Data: &encoded,
					MIMEType:   "image/png",
				},
				Detail: schema.ImageURLDetailHigh,
			}},
		},
	}})
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1", len(got))
	}
	if got[0].Content != "" {
		t.Errorf("Content = %q, want it cleared: the SDK rejects both fields at once", got[0].Content)
	}
	if len(got[0].MultiContent) != 2 {
		t.Fatalf("content parts = %d, want 2", len(got[0].MultiContent))
	}

	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"role":"user","content":[{"type":"text","text":"what is in this image?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + encoded + `","detail":"high"}}]}`
	if string(raw) != want {
		t.Errorf("wire form =\n%s\nwant\n%s", raw, want)
	}
}

// TestToOpenAIMessages_PartialParts covers the parts that cannot be rendered.
// A malformed content array fails the whole request, so an unusable part is
// dropped rather than sent.
func TestToOpenAIMessages_PartialParts(t *testing.T) {
	m := &openAIModel{provider: Provider{Name: "mock", Model: "m"}}
	remote := "https://example.com/cat.png"
	noMime := base64.StdEncoding.EncodeToString([]byte("bytes"))

	got := m.toOpenAIMessages([]*schema.Message{{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			// Audio cannot travel as chat content.
			{Type: schema.ChatMessagePartTypeAudioURL},
			// An image with neither bytes nor a URL is nothing to send.
			{Type: schema.ChatMessagePartTypeImageURL},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{}},
			// Inline bytes without a media type would produce an invalid
			// data URL.
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{Base64Data: &noMime},
			}},
			// An empty text part is dropped, and a real URL passes through.
			{Type: schema.ChatMessagePartTypeText, Text: ""},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &remote},
			}},
			{Type: schema.ChatMessagePartTypeText, Text: "and this"},
		},
	}})
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1", len(got))
	}
	if len(got[0].MultiContent) != 2 {
		t.Fatalf("content parts = %+v, want the two usable ones", got[0].MultiContent)
	}
	if got[0].MultiContent[0].ImageURL == nil || got[0].MultiContent[0].ImageURL.URL != remote {
		t.Errorf("remote image = %+v, want the URL passed through", got[0].MultiContent[0])
	}
	if got[0].MultiContent[1].Text != "and this" {
		t.Errorf("text part = %+v", got[0].MultiContent[1])
	}
	// A message whose parts are all unusable must stay a text message rather
	// than becoming an empty content array.
	none := m.toOpenAIMessages([]*schema.Message{{
		Role:    schema.User,
		Content: "text only",
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeAudioURL},
		},
	}})
	if len(none[0].MultiContent) != 0 || none[0].Content != "text only" {
		t.Errorf("message = %+v, want it left as plain text", none[0])
	}
}

// TestImagePartURL covers the two ways an image part can be addressed.
func TestImagePartURL(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("bytes"))
	remote := "https://example.com/cat.png"
	empty := ""
	blankMime := ""

	cases := []struct {
		name string
		img  *schema.MessageInputImage
		want string
	}{
		{"nil", nil, ""},
		{"base64", &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
			Base64Data: &encoded, MIMEType: "image/webp"}},
			"data:image/webp;base64," + encoded},
		{"url", &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{URL: &remote}}, remote},
		{"empty base64 falls back to the url", &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
			Base64Data: &empty, URL: &remote}}, remote},
		{"base64 without a mime type", &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
			Base64Data: &encoded, MIMEType: blankMime}}, ""},
		{"neither", &schema.MessageInputImage{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imagePartURL(tc.img); got != tc.want {
				t.Errorf("imagePartURL = %q, want %q", got, tc.want)
			}
		})
	}

	// The data URL must be parseable as one: a wrong prefix would be rejected
	// by a provider with an unhelpful message.
	if got := imagePartURL(cases[1].img); !strings.HasPrefix(got, "data:image/webp;base64,") {
		t.Errorf("data URL = %q", got)
	}
}
