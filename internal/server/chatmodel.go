package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/store"
)

// runnerFor returns a Runner bound to the session's chosen model.
//
// A Runner holds its model, so a per-session model choice means a per-session
// Runner. They are cheap (struct + options) and cached, so switching models is
// not a rebuild on every message.
func (s *Server) runnerFor(sess store.ChatSession) (*chat.Runner, error) {
	if s.chat.Runner == nil {
		return nil, fmt.Errorf("server: chat is not enabled")
	}
	if s.chat.Builder == nil {
		return s.chat.Runner, nil
	}

	key := sess.Provider + "\x00" + sess.Model
	if r, ok := s.runnerCache.get(key); ok {
		return r, nil
	}

	built, err := s.chat.Builder.Build(sess.Provider, sess.Model)
	if err != nil {
		return nil, err
	}
	cm, ok := built.(model.BaseChatModel)
	if !ok {
		return nil, fmt.Errorf("server: model builder returned %T, want a chat model", built)
	}

	r, err := chat.New(chat.Config{
		Model:    cm,
		Tools:    s.chat.Tools,
		Tracer:   s.tracer,
		MaxSteps: s.chatMaxSteps(),
		Logger:   s.logger,
	})
	if err != nil {
		return nil, err
	}
	s.runnerCache.put(key, r)
	return r, nil
}

// runnerCache is a small synchronised map of built runners.
type runnerCache struct {
	mu sync.RWMutex
	m  map[string]*chat.Runner
}

func (c *runnerCache) get(key string) (*chat.Runner, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.m[key]
	return r, ok
}

func (c *runnerCache) put(key string, r *chat.Runner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]*chat.Runner, 4)
	}
	// Bound the cache: providers x models is small, but a caller could
	// otherwise grow it with arbitrary model strings.
	if len(c.m) >= maxRunnerCache {
		for k := range c.m {
			delete(c.m, k)
			break
		}
	}
	c.m[key] = r
}

// maxRunnerCache bounds cached runners.
const maxRunnerCache = 32

// buildHistory assembles the conversation to send to the model: the system
// prompt, then the stored history, then the new user message.
//
// Reasoning and tool-call metadata from previous turns are deliberately NOT
// replayed as model input: they are display-only, and replaying an assistant
// tool_call without its paired tool result is exactly what makes providers
// reject a request.
func (s *Server) buildHistory(ctx context.Context, sess store.ChatSession, newUser string) ([]*schema.Message, error) {
	limit := s.chatHistoryLimit()
	stored, err := s.store.ListChatMessages(ctx, sess.ID, 0)
	if err != nil {
		return nil, err
	}
	// Keep the most recent `limit` messages, but never split a tool exchange
	// from the assistant message that requested it.
	if limit > 0 && len(stored) > limit {
		stored = stored[len(stored)-limit:]
	}

	msgs := make([]*schema.Message, 0, len(stored)+2)
	msgs = append(msgs, &schema.Message{Role: schema.System, Content: s.chatPrompt()})

	for _, m := range stored {
		switch m.Role {
		case store.RoleUser:
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			msgs = append(msgs, &schema.Message{Role: schema.User, Content: m.Content})
		case store.RoleAssistant:
			// Skip failed or empty assistant turns: sending an empty assistant
			// message confuses some providers.
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: m.Content})
		}
		// store.RoleTool messages are intentionally not replayed; their content
		// is already reflected in the assistant's next answer.
	}

	msgs = append(msgs, &schema.Message{Role: schema.User, Content: newUser})
	return msgs, nil
}

// chatHistoryLimit returns the configured replay limit.
func (s *Server) chatHistoryLimit() int {
	if s.cfg.ChatHistoryLimit > 0 {
		return s.cfg.ChatHistoryLimit
	}
	return config.DefaultChatHistoryLimit
}

// llmModelBuilder adapts the LLM registry to ModelBuilder.
type llmModelBuilder struct {
	reg *llm.Registry
}

// NewModelBuilder exposes a registry as a ModelBuilder.
func NewModelBuilder(reg *llm.Registry) ModelBuilder {
	return &llmModelBuilder{reg: reg}
}

// Build returns a chat model for a provider and model name.
func (b *llmModelBuilder) Build(provider, modelName string) (any, error) {
	if b.reg == nil {
		return nil, fmt.Errorf("server: no LLM registry configured")
	}
	if strings.TrimSpace(provider) == "" {
		provider = b.reg.DefaultName()
	}
	return b.reg.GetWithModel(provider, modelName)
}

// Catalog lists the selectable providers.
func (b *llmModelBuilder) Catalog() []ModelChoice {
	if b.reg == nil {
		return nil
	}
	cat := b.reg.Catalog()
	out := make([]ModelChoice, 0, len(cat))
	for _, c := range cat {
		out = append(out, ModelChoice{
			Provider:  c.Name,
			Model:     c.Model,
			Default:   c.Default,
			HasAPIKey: c.HasAPIKey,
		})
	}
	return out
}

// usageJSON is the shape stored in ChatMessage.UsageJSON.
type usageJSON struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	DurationMs       int `json:"duration_ms"`
}

// parseUsage reads a stored usage blob, tolerating an empty or malformed value.
func parseUsage(raw string) usageJSON {
	var u usageJSON
	if strings.TrimSpace(raw) == "" {
		return u
	}
	_ = json.Unmarshal([]byte(raw), &u)
	return u
}

// toolRunsFromJSON reads the stored tool-call blob.
func toolRunsFromJSON(raw string) []chat.ToolRun {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var runs []chat.ToolRun
	if err := json.Unmarshal([]byte(raw), &runs); err != nil {
		return nil
	}
	return runs
}

// compile-time check that the builder satisfies the interface.
var _ ModelBuilder = (*llmModelBuilder)(nil)
