package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	gctx "github.com/huan/huan-agent/internal/context"
	"github.com/huan/huan-agent/internal/memory"
)

// sessionMemory bundles the Phase 3 memory + context machinery for a single
// chat session. It owns the long-term JSONL store (namespaced by session),
// a short-term buffer, and a context-window manager with an optional
// LLM-backed summarizer.
type sessionMemory struct {
	store        memory.Store
	buffer       *memory.Buffer
	manager      *gctx.Manager
	ns           string
	enable       bool
	persist      bool
	logger       *zap.Logger
	cm           model.BaseChatModel
	systemPrompt string
}

// newSessionMemory builds the memory+context wiring. When memory is disabled
// (cfg.Memory.Enable == false) all methods are safe no-ops.
func newSessionMemory(cfg *config.Config, cm model.BaseChatModel, systemPrompt, ns string, logger *zap.Logger) (*sessionMemory, error) {
	m := &sessionMemory{
		buffer:       memory.NewBuffer(cfg.Memory.MaxTurns),
		ns:           ns,
		enable:       cfg.Memory.Enable,
		logger:       logger,
		cm:           cm,
		systemPrompt: systemPrompt,
	}
	if !m.enable {
		return m, nil
	}
	// Long-term JSONL store.
	if cfg.Memory.Dir != "" {
		st, err := memory.NewStore(cfg.Memory.Dir)
		if err != nil {
			return nil, fmt.Errorf("open memory store: %w", err)
		}
		m.store = st
		m.persist = true
	}
	// Context manager (auto-compression disabled when MaxTokens == 0).
	var summarizer gctx.Summarizer
	if cfg.Context.Summarize && cm != nil {
		summarizer = llmSummarizer{cm: cm}
	}
	mgr, err := gctx.NewManager(gctx.Budget{
		MaxTokens:  cfg.Context.MaxTokens,
		KeepRecent: cfg.Context.KeepRecent,
		Summarizer: summarizer,
	}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("init context manager: %w", err)
	}
	m.manager = mgr
	mgr.SetLog(func(s string) { fmt.Fprintln(os.Stderr, "["+s+"]") })
	return m, nil
}

// llmSummarizer adapts a chat model into a gctx.Summarizer.
type llmSummarizer struct{ cm model.BaseChatModel }

func (l llmSummarizer) Summarize(ctx context.Context, msgs []*schema.Message) (string, error) {
	if l.cm == nil {
		return "", errors.New("no model available for summarization")
	}
	var b strings.Builder
	b.WriteString("请把以下对话压缩成一段简短的摘要，保留关键事实、决策和未完成事项，不要复述原话：\n\n")
	for _, m := range msgs {
		b.WriteString("[" + string(m.Role) + "]: " + m.Content + "\n")
	}
	out, err := l.cm.Generate(ctx, []*schema.Message{
		{Role: schema.User, Content: b.String()},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Content), nil
}

// addUserMessage records a user message to short (buffer) + long-term (store)
// memory and returns it for inclusion in the prompt window.
func (m *sessionMemory) addUserMessage(line string) *schema.Message {
	msg := &schema.Message{Role: schema.User, Content: line}
	if !m.enable {
		return msg
	}
	m.buffer.Append(msg)
	if m.persist && m.store != nil {
		_ = m.store.Append(context.Background(), m.ns, memory.NewEntry(memory.KindUser, line))
	}
	return msg
}

// addAssistantMessage records the assistant reply.
func (m *sessionMemory) addAssistantMessage(out *schema.Message) *schema.Message {
	if !m.enable || out == nil {
		return out
	}
	m.buffer.Append(out)
	if m.persist && m.store != nil {
		_ = m.store.Append(context.Background(), m.ns, memory.NewEntry(memory.KindAssistant, out.Content))
	}
	return out
}

// addFact records a durable fact (used by /remember).
func (m *sessionMemory) addFact(key, value string) error {
	if !m.enable || m.store == nil {
		return errors.New("memory disabled")
	}
	return m.store.AddFact(context.Background(), m.ns, memory.Fact{Key: key, Value: value})
}

// recallFacts searches persisted facts (used by /remember <query>).
func (m *sessionMemory) recallFacts(query string, limit int) ([]memory.Fact, error) {
	if !m.enable || m.store == nil {
		return nil, errors.New("memory disabled")
	}
	return m.store.SearchFacts(context.Background(), query, limit)
}

// history returns the current prompt window: the optional system message
// followed by the buffered turns, with compression applied if over budget.
func (m *sessionMemory) history() ([]*schema.Message, error) {
	if !m.enable {
		return nil, nil
	}
	msgs := m.buffer.List()
	if m.systemPrompt != "" {
		pre := []*schema.Message{{Role: schema.System, Content: m.systemPrompt}}
		pre = append(pre, msgs...)
		msgs = pre
	}
	if m.manager == nil {
		return msgs, nil
	}
	out, _, err := m.manager.Compress(context.Background(), msgs)
	if err != nil {
		return msgs, err
	}
	return out, nil
}

// close releases the long-term store.
func (m *sessionMemory) close() {
	if m.store != nil {
		_ = m.store.Close()
	}
}
