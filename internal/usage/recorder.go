// Package usage records LLM token consumption asynchronously.
package usage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/store"
)

// Event is the input shape for Record. Token fields may be 0 if the upstream
// did not report them (some local providers like ollama don't always populate).
type Event struct {
	SessionID        string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMs       int64
}

// Recorder buffers events in a bounded channel and writes them to the store
// from a background goroutine. It is safe for concurrent use.
type Recorder struct {
	store   store.Store
	logger  *zap.Logger
	queue   chan Event
	doneCh  chan struct{}
	mu      sync.Mutex
	closed  bool
	closeOn sync.Once
}

// NewRecorder starts a background worker that drains the queue into store.
// queueSize bounds the in-memory buffer; when full, Record blocks (back-pressure).
func NewRecorder(s store.Store, logger *zap.Logger, queueSize int) *Recorder {
	if queueSize <= 0 {
		queueSize = 1000
	}
	r := &Recorder{
		store:  s,
		logger: logger,
		queue:  make(chan Event, queueSize),
		doneCh: make(chan struct{}),
	}
	go r.worker()
	return r
}

// Record enqueues an event. Blocks if the queue is full. Safe on a closed
// recorder: it will return an error instead of panicking.
func (r *Recorder) Record(e Event) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("usage: recorder is closed")
	}
	r.mu.Unlock()
	r.queue <- e
	return nil
}

// Close drains pending events and stops the worker. Blocks until done or the
// context is cancelled.
func (r *Recorder) Close(ctx context.Context) error {
	r.closeOn.Do(func() {
		r.mu.Lock()
		r.closed = true
		close(r.queue)
		r.mu.Unlock()
	})
	select {
	case <-r.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Recorder) worker() {
	defer close(r.doneCh)
	for e := range r.queue {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := r.store.RecordUsage(ctx, store.UsageEvent{
			SessionID:        e.SessionID,
			Provider:         e.Provider,
			Model:            e.Model,
			PromptTokens:     e.PromptTokens,
			CompletionTokens: e.CompletionTokens,
			TotalTokens:      e.TotalTokens,
			DurationMs:       e.DurationMs,
		})
		cancel()
		if err != nil {
			r.logger.Warn("usage: record failed",
				zap.String("session_id", e.SessionID),
				zap.String("provider", e.Provider),
				zap.String("model", e.Model),
				zap.Error(err))
		}
	}
}
