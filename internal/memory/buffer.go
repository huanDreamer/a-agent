package memory

import (
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Buffer is the short-term, in-memory conversation window. It keeps only the
// most recent Cap turns (user/assistant/tool messages) and drops the oldest
// once the cap is exceeded. This is the "always keep the last K turns" layer
// of memory; everything older is owned by the long-term Store.
//
// Buffer is safe for concurrent use.
type Buffer struct {
	mu    sync.RWMutex
	cap   int
	items []*schema.Message
}

// NewBuffer returns a short-term buffer holding up to cap messages. A cap <= 0
// means unbounded (kept for convenience; the REPL sets a concrete budget).
func NewBuffer(cap int) *Buffer {
	return &Buffer{cap: cap}
}

// Append adds a message, evicting the oldest once the capacity is exceeded.
func (b *Buffer) Append(m *schema.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = append(b.items, m)
	if b.cap > 0 && len(b.items) > b.cap {
		// Drop from the front, keeping the most recent b.cap.
		over := len(b.items) - b.cap
		b.items = append(b.items[:0:0], b.items[over:]...)
	}
}

// AppendMany adds a batch of messages as a single operation.
func (b *Buffer) AppendMany(ms []*schema.Message) {
	for _, m := range ms {
		b.Append(m)
	}
}

// List returns a copy of the buffered messages, oldest first.
func (b *Buffer) List() []*schema.Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*schema.Message, len(b.items))
	copy(out, b.items)
	return out
}

// Len returns the number of buffered messages.
func (b *Buffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.items)
}

// Cap returns the configured capacity (0 = unbounded).
func (b *Buffer) Cap() int { return b.cap }

// Reset drops all buffered messages.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = b.items[:0]
}
