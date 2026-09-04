package context

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func msgs(n int) []*schema.Message {
	out := make([]*schema.Message, n)
	for i := 0; i < n; i++ {
		role := schema.User
		if i%2 == 1 {
			role = schema.Assistant
		}
		out[i] = &schema.Message{Role: role, Content: strings.Repeat("x", 20)}
	}
	return out
}

type fakeSummarizer struct{ got int }

func (f *fakeSummarizer) Summarize(ctx context.Context, msgs []*schema.Message) (string, error) {
	f.got = len(msgs)
	return "SUMMARY", nil
}

// chainEstimator returns a huge estimate so compression always triggers.
func chainEstimator(msgs []*schema.Message) int { return 1 << 20 }

func TestManagerDisabled(t *testing.T) {
	m, err := NewManager(Budget{}, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.ShouldCompress(msgs(500)) {
		t.Fatal("compression should be disabled with MaxTokens=0")
	}
	out, sum, err := m.Compress(context.Background(), msgs(500))
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if sum != "" || len(out) != 500 {
		t.Fatalf("disabled compress mutated window: sum=%q len=%d", sum, len(out))
	}
}

func TestCompressKeepsRecent(t *testing.T) {
	sm := &fakeSummarizer{}
	m, err := NewManager(Budget{MaxTokens: 100, KeepRecent: 3, Summarizer: sm}, nil, chainEstimator)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	src := msgs(10)
	out, sum, err := m.Compress(context.Background(), src)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if sum != "SUMMARY" {
		t.Fatalf("expected SUMMARY, got %q", sum)
	}
	if sm.got != 7 {
		t.Fatalf("summarizer should receive 7 older, got %d", sm.got)
	}
	// 1 summary system message + 3 recent.
	if len(out) != 4 {
		t.Fatalf("expected 4 messages after compress, got %d", len(out))
	}
	if out[0].Role != schema.System || !strings.Contains(out[0].Content, "SUMMARY") {
		t.Fatalf("first message should be the summary, got %+v", out[0])
	}
	if out[len(out)-1].Content != src[len(src)-1].Content {
		t.Fatal("last recent message should be preserved")
	}
}

func TestCompressNoSummarizer(t *testing.T) {
	m, _ := NewManager(Budget{MaxTokens: 100, KeepRecent: 2}, nil, chainEstimator)
	out, sum, err := m.Compress(context.Background(), msgs(8))
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if !strings.Contains(sum, "omitted") {
		t.Fatalf("expected passthrough placeholder, got %q", sum)
	}
	if len(out) != 3 { // summary + 2 recent
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
}

func TestCompressWithinBudgetNoop(t *testing.T) {
	m, _ := NewManager(Budget{MaxTokens: 10000, KeepRecent: 2, Summarizer: &fakeSummarizer{}}, nil, nil)
	src := msgs(5)
	out, sum, err := m.Compress(context.Background(), src)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if sum != "" || len(out) != 5 {
		t.Fatalf("should be unchanged: sum=%q len=%d", sum, len(out))
	}
}

func TestCompressRefusesToDropEverything(t *testing.T) {
	m, _ := NewManager(Budget{MaxTokens: 100, KeepRecent: 10, Summarizer: &fakeSummarizer{}}, nil, chainEstimator)
	out, _, err := m.Compress(context.Background(), msgs(5))
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("KeepRecent>=len should leave unchanged, got %d", len(out))
	}
}

func TestManagerValidation(t *testing.T) {
	if _, err := NewManager(Budget{MaxTokens: -1}, nil, nil); err == nil {
		t.Fatal("expected error for negative MaxTokens")
	}
	if _, err := NewManager(Budget{KeepRecent: -1}, nil, nil); err == nil {
		t.Fatal("expected error for negative KeepRecent")
	}
}

func TestShouldCompress(t *testing.T) {
	m, _ := NewManager(Budget{MaxTokens: 50}, nil, DefaultEstimator)
	// ~24 chars/message -> ~5 tokens each; 20 msgs = 100 tokens > 50.
	if !m.ShouldCompress(msgs(20)) {
		t.Fatal("expected ShouldCompress true on large window")
	}
	if m.ShouldCompress(msgs(2)) {
		t.Fatal("expected ShouldCompress false on small window")
	}
}

func TestDefaultEstimator(t *testing.T) {
	// each 20-char msg -> 20/4 + 4 = 9 tokens: 2 msgs = 18 tokens.
	if n := DefaultEstimator(msgs(2)); n != 18 {
		t.Fatalf("expected 18 tokens, got %d", n)
	}
}
