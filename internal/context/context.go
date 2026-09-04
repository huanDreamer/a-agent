// Package context manages the LLM context window for huan-agent: it estimates
// the token cost of a message list, enforces a configurable token budget, and
// compresses a conversation that grows too large by summarizing older turns
// into a compact roll-up (a "summary system message") while keeping the most
// recent turns verbatim.
//
// The MVP uses a lightweight heuristic token estimate (≈ chars/4). Swapping in
// a provider-accurate tokenizer is a local change confined to estimate().
package context

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// Budget configures the context-window management.
type Budget struct {
	// MaxTokens is the hard ceiling for the assembled message list. Once the
	// estimated tokens exceed this, compression kicks in. 0 disables the
	// budget (no auto-compression).
	MaxTokens int
	// KeepRecent is the number of most recent messages always retained verbatim
	// after a compression pass.
	KeepRecent int
	// Summarizer is used to turn a block of older messages into a summary. If
	// nil, compression uses a no-op passthrough of the dropped messages instead.
	Summarizer Summarizer
}

// Validate reports invalid budget settings.
func (b Budget) Validate() error {
	if b.MaxTokens < 0 {
		return errors.New("context: MaxTokens must be >= 0")
	}
	if b.KeepRecent < 0 {
		return errors.New("context: KeepRecent must be >= 0")
	}
	return nil
}

// Summarizer reduces a set of messages into a single compressed statement.
type Summarizer interface {
	Summarize(ctx context.Context, messages []*schema.Message) (string, error)
}

// Estimator estimates the token count of a message list.
type Estimator func(msgs []*schema.Message) int

// DefaultEstimator approximates tokens as characters/4 (typical for English
// and reasonable for code). CJK text is denser, so this over-counts slightly,
// which errs toward earlier compression — an acceptable trade-off.
func DefaultEstimator(msgs []*schema.Message) int {
	var total int
	for _, m := range msgs {
		total += len(m.Content)/4 + 4 // +4 header/role overhead
	}
	return total
}

// Budget is the concrete compression manager.
type Manager struct {
	budget    Budget
	estimate  Estimator
	lastState state
	logOnce   func(string)
}

type state struct {
	compressed bool
}

// NewManager builds a context manager. summarizer may be nil (fall back to a
// passthrough); estimator may be nil (use DefaultEstimator).
func NewManager(budget Budget, summarizer Summarizer, estimator Estimator) (*Manager, error) {
	if err := budget.Validate(); err != nil {
		return nil, err
	}
	if estimator == nil {
		estimator = DefaultEstimator
	}
	return &Manager{
		budget:   budget,
		estimate: estimator,
	}, nil
}

// SetLog sets an optional callback invoked once when compression first fires.
func (m *Manager) SetLog(fn func(string)) { m.logOnce = fn }

// ShouldCompress reports whether the assembled window exceeds the budget.
func (m *Manager) ShouldCompress(msgs []*schema.Message) bool {
	return m.budget.MaxTokens > 0 && m.estimate(msgs) > m.budget.MaxTokens
}

// Compress reduces a message list to fit under the budget. It returns the new
// message list and a combined long-term summary string ("" when no
// compression happened).
//
// Strategy (per PLAN.md): always keep the most recent KeepRecent turns; roll
// the older turns into one summary via the Summarizer. If no Summarizer is set,
// the older turns are dropped and only a short placeholder summary is used.
func (m *Manager) Compress(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, string, error) {
	if m.budget.MaxTokens <= 0 {
		// Compression disabled.
		return msgs, "", nil
	}
	if !m.ShouldCompress(msgs) {
		return msgs, "", nil
	}
	if m.budget.KeepRecent <= 0 {
		m.budget.KeepRecent = 1 // always keep at least the latest turn
	}
	if m.budget.KeepRecent >= len(msgs) {
		// Nothing worth dropping; return unchanged to avoid an empty window.
		return msgs, "", nil
	}

	keep := msgs[len(msgs)-m.budget.KeepRecent:]
	older := msgs[:len(msgs)-m.budget.KeepRecent]

	var summary string
	if m.budget.Summarizer != nil {
		s, err := m.budget.Summarizer.Summarize(ctx, older)
		if err != nil {
			// Fall back to a placeholder so compression still bounds the window.
			summary = fmt.Sprintf("(Previous context summarized but summarization failed: %v)", err)
		} else if s != "" {
			summary = s
		}
	} else {
		summary = "(Previous messages omitted to stay within the token budget.)"
	}

	if m.logOnce != nil && !m.lastState.compressed {
		m.logOnce(fmt.Sprintf("context compressed: %d older turns rolled into a summary", len(older)))
	}
	m.lastState.compressed = true

	var out []*schema.Message
	if summary != "" {
		out = append(out, &schema.Message{Role: schema.System, Content: "Previous conversation summary:\n" + summary})
	}
	out = append(out, keep...)
	return out, summary, nil
}
