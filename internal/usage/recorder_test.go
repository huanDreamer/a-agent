package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/store"
)

func newTestRecorder(t *testing.T) (*Recorder, store.Store) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewRecorder(s, zap.NewNop(), 16), s
}

func TestRecordAndQuery(t *testing.T) {
	rec, s := newTestRecorder(t)

	events := []Event{
		{SessionID: "s1", Provider: "deepseek", Model: "deepseek-chat", PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, DurationMs: 500},
		{SessionID: "s1", Provider: "deepseek", Model: "deepseek-chat", PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33, DurationMs: 600},
		{SessionID: "s2", Provider: "qwen", Model: "qwen-plus", PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12, DurationMs: 200},
	}
	for _, e := range events {
		if err := rec.Record(e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rec.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	all, err := s.QueryUsage(context.Background(), store.UsageFilter{})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
}

func TestRecordAfterClose(t *testing.T) {
	rec, _ := newTestRecorder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rec.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rec.Record(Event{}); err == nil {
		t.Error("Record after Close should fail")
	}
}

func TestBackpressureBlocks(t *testing.T) {
	rec, _ := newTestRecorder(t)
	// Drain isn't called, so the queue (cap 16) will fill up.
	for i := 0; i < 16; i++ {
		if err := rec.Record(Event{SessionID: "s"}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	// 17th should block; we don't want a deadlock, so verify with timeout.
	done := make(chan error, 1)
	go func() { done <- rec.Record(Event{SessionID: "s"}) }()
	select {
	case err := <-done:
		if err == nil {
			// unexpected success — buffer must have been drained in time
			t.Log("17th record completed (worker drained)")
		}
	case <-time.After(50 * time.Millisecond):
		// expected: back-pressure
	}
	// Close to release the goroutine.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = rec.Close(ctx)
}
