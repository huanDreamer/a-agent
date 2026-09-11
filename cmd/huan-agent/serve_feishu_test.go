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

	sendErr error
	// cardErr fails only card sends, so the plain-text fallback stays testable.
	cardErr error
	// cardFailN fails the first N card sends, to exercise the fallback ladder.
	cardFailN int
	cardSends int
	updateErr error
}

type sentText struct {
	chatID string
	text   string
}

type sentCard struct {
	chatID   string
	message  string
	title    string
	body     string
	elements []feishu.CardElement
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
	if err := firstErr(f.sendErr, f.cardErr); err != nil {
		return "", err
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

func (f *fakeSender) SendCardElements(_ context.Context, chatID, title string, elements []feishu.CardElement) (string, error) {
	f.cardSends++
	if f.cardFailN > 0 && f.cardSends <= f.cardFailN {
		return "", errors.New("platform rejected the card")
	}
	if err := firstErr(f.sendErr, f.cardErr); err != nil {
		return "", err
	}
	f.cards = append(f.cards, sentCard{
		chatID: chatID, title: title, body: renderElements(elements), elements: elements,
	})
	return "om_" + itoa(len(f.cards)), nil
}

func (f *fakeSender) UpdateCardElements(_ context.Context, messageID, title string, elements []feishu.CardElement) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, sentCard{
		message: messageID, title: title, body: renderElements(elements), elements: elements,
	})
	return nil
}

func (f *fakeSender) Recall(_ context.Context, messageID string) error {
	f.recalls = append(f.recalls, messageID)
	return nil
}

// renderElements flattens card elements into a searchable string so tests can
// assert on the delivered content.
func renderElements(els []feishu.CardElement) string {
	var b strings.Builder
	for _, el := range els {
		if el.Text != nil {
			b.WriteString(el.Text.Content)
			b.WriteString("\n")
		}
		for _, c := range el.Columns {
			b.WriteString(c.DisplayName)
			b.WriteString(" ")
		}
		for _, row := range el.Rows {
			for _, v := range row {
				b.WriteString(v)
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// itoa is a tiny local helper to avoid an extra import.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
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
	if s.updates[0].message != "om_ph" {
		t.Errorf("updated message = %q, want om_ph", s.updates[0].message)
	}
	if !strings.Contains(s.updates[0].body, "the answer") {
		t.Errorf("updated card lost the answer: %q", s.updates[0].body)
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
	if !strings.Contains(s.cards[0].body, "answer") {
		t.Errorf("fallback card body = %q, want it to contain the answer", s.cards[0].body)
	}
}

func TestFinish_FallsBackToTextWhenSendFails(t *testing.T) {
	s := &fakeSender{cardErr: errors.New("cannot create")}
	h := newTestHandler(s, nil)

	if err := h.finish(context.Background(), feishu.Inbound{ChatID: "oc"}, "", "plain"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(s.cards) != 0 {
		t.Errorf("cards = %d, want 0 when sending cards fails", len(s.cards))
	}
	if len(s.texts) != 1 || s.texts[0].text != "plain" {
		t.Errorf("texts = %+v, want the plain-text fallback to deliver the answer", s.texts)
	}
}

func TestFinish_NoPlaceholderSendsCard(t *testing.T) {
	s := &fakeSender{}
	h := newTestHandler(s, nil)

	if err := h.finish(context.Background(), feishu.Inbound{ChatID: "oc"}, "", "plain"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(s.cards) != 1 {
		t.Fatalf("cards = %+v, want one card", s.cards)
	}
	if !strings.Contains(s.cards[0].body, "plain") {
		t.Errorf("card body = %q, want it to contain the answer", s.cards[0].body)
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

// firstErr returns the first non-nil error.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func TestFinish_SplitsLargeTableAcrossMessages(t *testing.T) {
	s := &fakeSender{}
	h := newTestHandler(s, nil)

	// A table long enough to exceed the default per-message row limit.
	var b strings.Builder
	b.WriteString("数据如下：\n\n| n | v |\n| --- | --- |")
	for i := 0; i < 45; i++ {
		b.WriteString("\n| ")
		b.WriteString(itoa(i))
		b.WriteString(" | x |")
	}

	if err := h.finish(context.Background(), feishu.Inbound{ChatID: "oc"}, "om_ph", b.String()); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// The placeholder carries the first chunk...
	if len(s.updates) != 1 {
		t.Fatalf("updates = %d, want 1 (the placeholder)", len(s.updates))
	}
	// ...and the remaining chunks travel as additional messages.
	if len(s.cards) == 0 {
		t.Fatal("a large table should be split into additional messages")
	}

	// No row may be lost across the placeholder and the extra messages.
	seen := map[string]int{}
	collect := func(els []feishu.CardElement) {
		for _, el := range els {
			for _, row := range el.Rows {
				seen[row["col_0"]]++
			}
		}
	}
	collect(s.updates[0].elements)
	for _, c := range s.cards {
		collect(c.elements)
	}
	for i := 0; i < 45; i++ {
		if seen[itoa(i)] != 1 {
			t.Errorf("row %d delivered %d times, want exactly 1", i, seen[itoa(i)])
		}
	}
	// The prose must accompany the first chunk.
	if !strings.Contains(s.updates[0].body, "数据如下") {
		t.Errorf("the first message lost the surrounding prose: %q", s.updates[0].body)
	}
}

func TestFinish_SmallTableStaysOneMessage(t *testing.T) {
	s := &fakeSender{}
	h := newTestHandler(s, nil)

	md := "| a | b |\n| --- | --- |\n| 1 | 2 |"
	if err := h.finish(context.Background(), feishu.Inbound{ChatID: "oc"}, "om_ph", md); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(s.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(s.updates))
	}
	if len(s.cards) != 0 {
		t.Errorf("cards = %d, want 0 (a small table fits one message)", len(s.cards))
	}
	// It must be delivered as a native table element.
	found := false
	for _, el := range s.updates[0].elements {
		if el.Tag == "table" {
			found = true
		}
	}
	if !found {
		t.Errorf("the table was not rendered natively: %+v", s.updates[0].elements)
	}
}

func TestFinish_RetriesWithoutNativeTablesWhenRejected(t *testing.T) {
	// The platform rejects the first card (the one carrying a native table);
	// the compat rendering must then be attempted.
	s := &fakeSender{cardFailN: 1}
	h := newTestHandler(s, nil)

	md := "| a | b |\n| --- | --- |\n| 1 | 2 |"
	if err := h.finish(context.Background(), feishu.Inbound{ChatID: "oc"}, "", md); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(s.cards) != 1 {
		t.Fatalf("cards = %d, want 1 successful card after the retry", len(s.cards))
	}
	// The delivered card must not contain a native table element.
	for _, el := range s.cards[0].elements {
		if el.Tag == "table" {
			t.Error("the compat fallback must not emit a native table")
		}
	}
	if !strings.Contains(s.cards[0].body, "| a |") {
		t.Errorf("the table was lost in the compat rendering: %q", s.cards[0].body)
	}
}

func TestFinish_PlainTextWhenEveryCardIsRejected(t *testing.T) {
	s := &fakeSender{cardErr: errors.New("cards unsupported")}
	h := newTestHandler(s, nil)

	if err := h.finish(context.Background(), feishu.Inbound{ChatID: "oc"}, "", "hello"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(s.texts) != 1 || s.texts[0].text != "hello" {
		t.Errorf("texts = %+v, want the plain-text last resort", s.texts)
	}
}

func TestMarkdownToCardMessagesCompat_NoNativeTables(t *testing.T) {
	md := "说明\n\n| 产品 | 数量 |\n| --- | --- |\n| 甲 | 1 |"
	msgs := feishu.MarkdownToCardMessagesCompat(md, "T")
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (compat never splits)", len(msgs))
	}
	for _, el := range msgs[0].Elements {
		if el.Tag == "table" {
			t.Error("compat mode must not emit native table elements")
		}
	}
	if !strings.Contains(renderElements(msgs[0].Elements), "产品") {
		t.Error("the table content was lost")
	}
}
