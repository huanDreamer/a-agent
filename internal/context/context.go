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
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// LLMSummarizer adapts a chat model into a Summarizer: it asks the model to
// compress the messages being rolled out of the window.
//
// It lives here rather than in each caller because there is exactly one useful
// instruction for this job, and two copies of it drift: the CLI's memory manager
// and a long turn's in-loop condenser must summarize the same way, or the same
// conversation is compressed differently depending on which surface read it.
type LLMSummarizer struct {
	// Model is the chat model used to summarize. It is deliberately not
	// necessarily the model answering the conversation: summarization is a
	// mechanical job and the configured default is a predictable choice for it.
	Model model.BaseChatModel
}

// Summarize compresses a block of messages into one short statement.
func (l LLMSummarizer) Summarize(ctx context.Context, msgs []*schema.Message) (string, error) {
	if l.Model == nil {
		return "", errors.New("context: no model available for summarization")
	}
	var b strings.Builder
	b.WriteString("请把以下对话压缩成一段简短的摘要，保留关键事实、决策和未完成事项，不要复述原话：\n\n")
	for _, m := range msgs {
		b.WriteString("[" + string(m.Role) + "]: " + m.Content + "\n")
	}
	out, err := l.Model.Generate(ctx, []*schema.Message{
		{Role: schema.User, Content: b.String()},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Content), nil
}

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

// DefaultEstimator approximates the token count of a message list.
//
// It replaced `len(Content)/4` — bytes over four — after measuring that estimate
// against a real long turn: across 383 steps of a session that mixed Chinese
// prose, Go source and tool output, the provider's own count was 1.95× the
// estimate (median). Two things were missing. A Chinese character is three bytes
// but roughly one token, so dividing bytes by four under-counts CJK by a factor
// of three; and tool-call arguments were not counted at all.
//
// Being wrong in that direction is the expensive one: the condenser fires late,
// the window runs at the provider's real limit, and the turn ends in a
// context-limit error instead of a summary. So the estimate is deliberately
// slightly pessimistic, and the schemas (which are resent every step and appear
// in no message) are added by the caller — see EstimateSchemas.
func DefaultEstimator(msgs []*schema.Message) int {
	var total int
	for _, m := range msgs {
		if m == nil {
			continue
		}
		total += EstimateText(m.Content)
		for _, tc := range m.ToolCalls {
			// The arguments are JSON the provider tokenizes like any other text.
			total += EstimateText(tc.Function.Arguments) + 8
		}
		if m.ToolCallID != "" {
			total += 4
		}
		// Role, name and the per-message scaffolding every chat template adds.
		total += 6
	}
	return total
}

// EstimateText approximates the tokens in one string.
//
// ASCII is about four characters to a token. A CJK character is one token (and
// three bytes, which is why the old byte count was wrong), and everything else
// non-ASCII — Cyrillic, accented Latin, emoji — is counted as half a token per
// rune, which is close for the scripts a model is likely to see.
func EstimateText(s string) int {
	if s == "" {
		return 0
	}
	var cjk, ascii, wide int
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			ascii++
		case isCJK(r):
			cjk++
		default:
			wide++
		}
	}
	return cjk + wide*2/4 + ascii/4 + 1
}

// isCJK reports whether a rune belongs to a script a Chinese/Japanese/Korean
// tokenizer emits one token per character for.
func isCJK(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F: // Hangul Jamo
		return true
	case r >= 0x2E80 && r <= 0x303E: // CJK radicals, Kangxi, punctuation
		return true
	case r >= 0x3041 && r <= 0x33FF: // kana, CJK compatibility
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK ext A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK unified
		return true
	case r >= 0xA000 && r <= 0xA4CF: // Yi
		return true
	case r >= 0xAC00 && r <= 0xD7A3: // Hangul syllables
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK compatibility ideographs
		return true
	case r >= 0xFE30 && r <= 0xFE4F: // CJK compatibility forms
		return true
	case r >= 0xFF00 && r <= 0xFF60: // fullwidth forms
		return true
	case r >= 0xFFE0 && r <= 0xFFE6:
		return true
	case r >= 0x20000 && r <= 0x3FFFD: // CJK ext B+
		return true
	}
	return false
}

// ToolSchema is one tool definition as the model receives it.
type ToolSchema struct {
	Name        string
	Description string
	// ParametersJSON is the tool's parameter schema, as the JSON text that is
	// sent. It is measured as text rather than by parsing it: eino's
	// ParamsOneOf keeps its schema in unexported fields and its jsonschema
	// marshaller drops most of a large schema, so a measurement taken through
	// them silently reports a fraction of the real cost.
	ParametersJSON string
}

// EstimateToolSchemas approximates what a turn's tool definitions cost.
//
// It is a separate function because the cost has a separate shape: the schemas
// are not messages, so the estimator above never sees them, yet they are resent
// on every single step — a thirty-tool turn carries several thousand tokens of
// invisible context, which is enough to matter when a window is being filled to
// a fraction of the model's limit.
func EstimateToolSchemas(tools []ToolSchema) int {
	if len(tools) == 0 {
		return 0
	}
	total := 0
	for _, t := range tools {
		total += EstimateText(t.Name) + EstimateText(t.Description) + EstimateText(t.ParametersJSON) + 12
	}
	return total
}

// Budget is the concrete compression manager.
//
// A Manager is safe for concurrent use and holds no per-call state. That is not
// decoration: one manager is shared by every conversation running on the same
// model (the web console builds a runner — and with it a condenser — per
// (provider, model)), so two browser sessions can compress at the same instant.
// Anything remembered between calls would be a data race and, worse, a leak of
// one conversation's behaviour into another's.
type Manager struct {
	budget   Budget
	estimate Estimator
	// logged fires the one-shot "compression is happening" line, at most once
	// per manager and safely from several goroutines.
	logged  sync.Once
	logOnce func(string)
}

// budgetOr returns the budget with its zero values resolved, without touching
// the receiver.
func (m *Manager) budgetOr() Budget {
	b := m.budget
	if b.KeepRecent <= 0 {
		b.KeepRecent = 1 // always keep at least the latest turn
	}
	return b
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
//
// A nil Manager reports false rather than panicking. That is not politeness: a
// nil *Manager stored in an interface is non-nil as an interface, so "no
// compression" arrives here as a call on a nil receiver, and the alternative to
// returning false is taking the whole turn down.
func (m *Manager) ShouldCompress(msgs []*schema.Message) bool {
	return m.ShouldCompressWith(msgs, 0)
}

// ShouldCompressWith is ShouldCompress including the tokens the window carries
// but the messages do not show: the tool schemas, which the provider counts on
// every step.
func (m *Manager) ShouldCompressWith(msgs []*schema.Message, overheadTokens int) bool {
	if m == nil {
		return false
	}
	if m.budget.MaxTokens <= 0 {
		return false
	}
	return m.estimate(msgs)+overheadTokens > m.budget.MaxTokens
}

// Pin says which messages survive a compression pass verbatim, beyond the
// KeepRecent tail that always survives.
//
// It exists because the messages that matter in a long turn are not the recent
// ones: the system prompt states the rules and the last user message states what
// was actually asked. Rolling either into a summary loses the one thing the
// model cannot reconstruct, so a caller that knows where they are can pin them.
type Pin struct {
	// Head is how many leading messages are kept verbatim — typically the
	// system prompt, or a system prompt plus whatever the caller prepended.
	Head int
	// LastUser keeps the most recent user message verbatim.
	LastUser bool
}

// Compress reduces a message list to fit under the budget. It returns the new
// message list and a combined long-term summary string ("" when no
// compression happened).
//
// Strategy (per PLAN.md): always keep the most recent KeepRecent turns; roll
// the older turns into one summary via the Summarizer. If no Summarizer is set,
// the older turns are dropped and only a short placeholder summary is used.
func (m *Manager) Compress(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, string, error) {
	return m.CompressWith(ctx, msgs, Pin{})
}

// CompressKeeping is CompressWith by position: pin the first head messages and,
// when asked, the most recent user message.
//
// It is the method chat.Condenser declares, which is what lets the runner depend
// on the capability rather than on this package: a deployment with compression
// switched off passes a nil Condenser instead of a manager that does nothing.
func (m *Manager) CompressKeeping(ctx context.Context, msgs []*schema.Message, head int, pinLastUser bool, overheadTokens int) ([]*schema.Message, string, error) {
	return m.compress(ctx, msgs, Pin{Head: head, LastUser: pinLastUser}, overheadTokens)
}

// CompressWith reduces msgs exactly like Compress, except that the messages a
// Pin names are kept verbatim instead of being rolled into the summary.
//
// Output order is: msgs[:Head], the summary system message, the pinned last user
// message (when it was in the foldable middle), then the most recent KeepRecent
// messages. Order is not cosmetic here: providers reject a tool result that
// precedes the assistant message which asked for it, so pinned and kept messages
// may only ever be reordered by removing what sits between them, never by
// swapping.
//
// The kept tail is widened when the KeepRecent boundary would otherwise cut a
// tool exchange in half, because the summary is a system message and cannot
// stand in as the assistant message a kept tool result answers. The result is
// that the returned window always satisfies what providers require: every
// `role: "tool"` message has the assistant message carrying its tool_call_id
// somewhere before it. The cost is that a boundary landing on a tool result can
// fold one exchange less than asked, and in the tightest case returns msgs
// unchanged — the caller's over-budget window is the better failure.
func (m *Manager) CompressWith(ctx context.Context, msgs []*schema.Message, pin Pin) ([]*schema.Message, string, error) {
	return m.compress(ctx, msgs, pin, 0)
}

// compress is CompressWith plus the invisible part of the window.
func (m *Manager) compress(ctx context.Context, msgs []*schema.Message, pin Pin, overheadTokens int) ([]*schema.Message, string, error) {
	// A nil receiver means "no compression configured", which is a supported
	// state rather than a bug: see ShouldCompress.
	if m == nil {
		return msgs, "", nil
	}
	if m.budget.MaxTokens <= 0 {
		// Compression disabled.
		return msgs, "", nil
	}
	if !m.ShouldCompressWith(msgs, overheadTokens) {
		return msgs, "", nil
	}
	// A local copy rather than a fix-up of the receiver: the manager is shared
	// across conversations, and writing to it here raced with every other
	// conversation compressing at the same time.
	budget := m.budgetOr()

	head := pin.Head
	if head < 0 {
		head = 0
	}
	if head > len(msgs) {
		head = len(msgs)
	}
	rest := msgs[head:]
	if budget.KeepRecent >= len(rest) {
		// Nothing worth dropping; return unchanged to avoid an empty window.
		return msgs, "", nil
	}

	keep := rest[len(rest)-budget.KeepRecent:]
	middle := rest[:len(rest)-budget.KeepRecent]

	// The split above is a plain slice boundary, and it can land in the middle
	// of a tool exchange: the assistant message that carried tool_calls falls
	// into the foldable middle and is rolled into the summary, while the tool
	// results it asked for stay in the kept tail. The window then opens with an
	// orphan "role: tool" message, which providers reject outright — DeepSeek
	// returns 400 "Messages with role 'tool' must be a response to a preceding
	// message with 'tool_calls'". Walk the boundary back until the kept window
	// still opens with the message that asked for what follows.
	for len(middle) > 0 && orphanedToolHead(msgs[:head], keep) {
		keep = append([]*schema.Message{middle[len(middle)-1]}, keep...)
		middle = middle[:len(middle)-1]
	}

	// Pull the pinned user message out of the foldable middle rather than out of
	// the kept tail: the tail is the most recent work, the pinned message is the
	// standing instruction, and only the middle is expendable. The copy keeps the
	// caller's slice untouched — the runner owns its history and reuses it.
	var pinned *schema.Message
	if pin.LastUser {
		for i := len(middle) - 1; i >= 0; i-- {
			if middle[i].Role != schema.User {
				continue
			}
			pinned = middle[i]
			trimmed := make([]*schema.Message, 0, len(middle)-1)
			trimmed = append(trimmed, middle[:i]...)
			trimmed = append(trimmed, middle[i+1:]...)
			middle = trimmed
			break
		}
	}

	// A middle emptied by pinning has nothing left to summarize, and a summary
	// message that summarizes nothing is noise in the window.
	var summary string
	if len(middle) > 0 {
		if budget.Summarizer != nil {
			s, err := budget.Summarizer.Summarize(ctx, middle)
			if err != nil {
				// Fall back to a placeholder so compression still bounds the window.
				summary = fmt.Sprintf("(Previous context summarized but summarization failed: %v)", err)
			} else if s != "" {
				summary = s
			}
		} else {
			summary = "(Previous messages omitted to stay within the token budget.)"
		}
	}

	if m.logOnce != nil {
		m.logged.Do(func() {
			m.logOnce(fmt.Sprintf("context compressed: %d older messages rolled into a summary (%d pinned)",
				len(middle), head+pinnedCount(pinned)))
		})
	}

	out := make([]*schema.Message, 0, head+2+len(keep))
	out = append(out, msgs[:head]...)
	if summary != "" {
		out = append(out, &schema.Message{Role: schema.System, Content: "Previous conversation summary:\n" + summary})
	}
	if pinned != nil {
		out = append(out, pinned)
	}
	out = append(out, keep...)
	return out, summary, nil
}

// orphanedToolHead reports whether the kept window opens with a tool result
// whose requesting assistant message is not among the messages that precede it.
//
// Only the head of the window needs checking: a tool result further in is
// preceded by whatever came with it, and the foldable middle is behind both.
func orphanedToolHead(prefix, keep []*schema.Message) bool {
	if len(keep) == 0 || keep[0].Role != schema.Tool {
		return false
	}
	want := keep[0].ToolCallID
	for _, m := range prefix {
		if m.Role != schema.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == want {
				return false
			}
		}
	}
	return true
}

// pinnedCount is the number of messages a pinned pointer stands for, for the
// log line: pinned is nil when nothing was pinned.
func pinnedCount(pinned *schema.Message) int {
	if pinned == nil {
		return 0
	}
	return 1
}
