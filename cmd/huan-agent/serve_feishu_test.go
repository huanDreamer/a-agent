package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/platform/feishu"
)

// fakeSender records the outbound calls made by the bot handler.
type fakeSender struct {
	texts   []sentText
	cards   []sentCard
	updates []sentCard
	recalls []string

	sendErr   error
	updateErr error
}

type sentText struct {
	chatID string
	text   string
}

type sentCard struct {
	chatID  string
	message string
	title   string
	body    string
}

func (f *fakeSender) ReplyText(_ context.Context, in feishu.Inbound, text string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.texts = append(f.texts, sentText{chatID: in.ChatID, text: text})
	return nil
}

func (f *fakeSender) ReplyCard(_ context.Context, in feishu.Inbound, title, markdown string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.cards = append(f.cards, sentCard{chatID: in.ChatID, title: title, body: markdown})
	return nil
}

func (f *fakeSender) SendText(_ context.Context, chatID, text string) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.texts = append(f.texts, sentText{chatID: chatID, text: text})
	return "om_text", nil
}

func (f *fakeSender) SendCard(_ context.Context, chatID, title, markdown string) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.cards = append(f.cards, sentCard{chatID: chatID, title: title, body: markdown})
	return "om_placeholder", nil
}

func (f *fakeSender) UpdateCard(_ context.Context, messageID, title, markdown string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, sentCard{message: messageID, title: title, body: markdown})
	return nil
}

func (f *fakeSender) Recall(_ context.Context, messageID string) error {
	f.recalls = append(f.recalls, messageID)
	return nil
}

// fakeDownloader records downloads and returns a deterministic path.
type fakeDownloader struct {
	paths []string
	err   error
	calls int
}

func (f *fakeDownloader) Download(_ context.Context, messageID string, r feishu.Resource) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	p := "/tmp/downloads/" + messageID + "_" + string(r.Kind) + ".bin"
	f.paths = append(f.paths, p)
	return p, nil
}

// newTestHandler builds a botHandler with only the fields these tests need.
func newTestHandler(sender feishu.Sender, dl feishu.ResourceDownloader) *botHandler {
	return &botHandler{
		cfg:        &config.Config{},
		logger:     zap.NewNop(),
		sender:     sender,
		downloader: dl,
		sessions:   map[string]*botSession{},
	}
}

func TestBuildPrompt_NoResources(t *testing.T) {
	h := newTestHandler(&fakeSender{}, &fakeDownloader{})
	in := feishu.Inbound{MsgType: "text", Text: "  hello  "}
	if got := h.buildPrompt(context.Background(), in); got != "hello" {
		t.Errorf("buildPrompt = %q, want hello", got)
	}
}

func TestBuildPrompt_NoDownloader(t *testing.T) {
	h := newTestHandler(&fakeSender{}, nil)
	in := feishu.Inbound{
		MsgType:   "image",
		Resources: []feishu.Resource{{Kind: feishu.ResourceImage, Key: "k"}},
	}
	// Without a downloader the attachment description is still surfaced.
	if got := h.buildPrompt(context.Background(), in); got != "[图片]" {
		t.Errorf("buildPrompt = %q, want [图片]", got)
	}
}

func TestBuildPrompt_AppendsDownloadedPaths(t *testing.T) {
	dl := &fakeDownloader{}
	h := newTestHandler(&fakeSender{}, dl)
	in := feishu.Inbound{
		MsgType:   "text",
		Text:      "看看这个文件",
		MessageID: "om_1",
		Resources: []feishu.Resource{{Kind: feishu.ResourceFile, Key: "fk", Name: "report.pdf"}},
	}
	got := h.buildPrompt(context.Background(), in)
	if !strings.Contains(got, "看看这个文件") {
		t.Errorf("prompt lost the user text: %q", got)
	}
	if !strings.Contains(got, "/tmp/downloads/om_1_file.bin") {
		t.Errorf("prompt missing the downloaded path: %q", got)
	}
	if !strings.Contains(got, "report.pdf") {
		t.Errorf("prompt missing the original file name: %q", got)
	}
	if dl.calls != 1 {
		t.Errorf("download calls = %d, want 1", dl.calls)
	}
}

func TestBuildPrompt_AttachmentOnlyMessage(t *testing.T) {
	h := newTestHandler(&fakeSender{}, &fakeDownloader{})
	in := feishu.Inbound{
		MsgType:   "image",
		MessageID: "om_2",
		Resources: []feishu.Resource{{Kind: feishu.ResourceImage, Key: "k"}},
	}
	got := h.buildPrompt(context.Background(), in)
	if !strings.Contains(got, "[图片]") || !strings.Contains(got, "/tmp/downloads/om_2_image.bin") {
		t.Errorf("buildPrompt = %q, want the image marker plus its path", got)
	}
}

func TestBuildPrompt_DownloadFailureDegrades(t *testing.T) {
	dl := &fakeDownloader{err: errors.New("network down")}
	h := newTestHandler(&fakeSender{}, dl)
	in := feishu.Inbound{
		MsgType:   "file",
		MessageID: "om_3",
		Resources: []feishu.Resource{{Kind: feishu.ResourceFile, Key: "k", Name: "a.pdf"}},
	}
	got := h.buildPrompt(context.Background(), in)
	if !strings.Contains(got, "下载失败") {
		t.Errorf("buildPrompt = %q, want a download-failure note", got)
	}
}

func TestFinish_UpdatesPlaceholder(t *testing.T) {
	s := &fakeSender{}
	h := newTestHandler(s, nil)

	if err := h.finish(context.Background(), feishu.Inbound{ChatID: "oc"}, "om_ph", "the answer"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(s.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(s.updates))
	}
	if s.updates[0].message != "om_ph" || s.updates[0].body != "the answer" {
		t.Errorf("unexpected update: %+v", s.updates[0])
	}
	if len(s.texts) != 0 || len(s.cards) != 0 {
		t.Error("finish should update in place, not send another message")
	}
}

func TestFinish_FallsBackToNewCardWhenUpdateFails(t *testing.T) {
	s := &fakeSender{updateErr: errors.New("cannot patch")}
	h := newTestHandler(s, nil)

	if err := h.finish(context.Background(), feishu.Inbound{ChatID: "oc"}, "om_ph", "answer"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(s.cards) != 1 {
		t.Fatalf("cards = %d, want 1 fallback card", len(s.cards))
	}
	if s.cards[0].body != "answer" {
		t.Errorf("fallback card body = %q, want answer", s.cards[0].body)
	}
}

func TestFinish_NoPlaceholderSendsText(t *testing.T) {
	s := &fakeSender{}
	h := newTestHandler(s, nil)

	if err := h.finish(context.Background(), feishu.Inbound{ChatID: "oc"}, "", "plain"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(s.texts) != 1 || s.texts[0].text != "plain" {
		t.Errorf("texts = %+v, want a single plain reply", s.texts)
	}
	if len(s.updates) != 0 {
		t.Error("no placeholder was sent, so nothing should be updated")
	}
}

func TestHandle_IgnoresUnsupportedMessage(t *testing.T) {
	s := &fakeSender{}
	h := newTestHandler(s, &fakeDownloader{})
	if err := h.Handle(context.Background(), feishu.Inbound{MsgType: "recall"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(s.texts) != 0 || len(s.cards) != 0 {
		t.Error("an unsupported message must not produce a reply")
	}
}

func TestHandle_ProviderCommand(t *testing.T) {
	s := &fakeSender{}
	h := newTestHandler(s, &fakeDownloader{})
	h.provider = "deepseek"
	h.model = "deepseek-chat"

	err := h.Handle(context.Background(), feishu.Inbound{
		MsgType: "text", Text: "/provider", ChatID: "oc_1", OpenID: "ou_1",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(s.texts) != 1 {
		t.Fatalf("texts = %d, want 1", len(s.texts))
	}
	if !strings.Contains(s.texts[0].text, "deepseek") {
		t.Errorf("reply = %q, want it to mention the provider", s.texts[0].text)
	}
}

func TestHandle_HelpCommand(t *testing.T) {
	s := &fakeSender{}
	h := newTestHandler(s, &fakeDownloader{})
	if err := h.Handle(context.Background(), feishu.Inbound{
		MsgType: "text", Text: "/help", ChatID: "oc_1",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(s.texts) != 1 || !strings.Contains(s.texts[0].text, "/reset") {
		t.Errorf("help reply = %+v, want the command list", s.texts)
	}
}

func TestThinkingPlaceholderConst(t *testing.T) {
	if !strings.Contains(thinkingPlaceholder, "思考") {
		t.Errorf("placeholder = %q, want a Chinese thinking hint", thinkingPlaceholder)
	}
}
