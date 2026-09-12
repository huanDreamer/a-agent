package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
)

// runnerFor returns a Runner bound to the session's chosen model.
//
// A Runner holds its model, so a per-session model choice means a per-session
// Runner. They are cheap (struct + options) and cached, so switching models is
// not a rebuild on every message.
func (s *Server) runnerFor(ctx context.Context, sess store.ChatSession) (*chat.Runner, error) {
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

	built, err := s.chat.Builder.Build(ctx, sess.Provider, sess.Model)
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
//
// Attachment bytes are not replayed either. A stored user message carries only
// its own text — the asset ids in its attachments column are for rendering the
// conversation, not for the model — so an image is inlined exactly once, on the
// turn it was sent. Re-inlining it here would re-upload the same base64 payload
// on every later turn of the conversation.
func (s *Server) buildHistory(ctx context.Context, sess store.ChatSession, newUser *schema.Message) ([]*schema.Message, error) {
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

	if newUser != nil {
		msgs = append(msgs, newUser)
	}
	return msgs, nil
}

// sessionModelTarget returns the (provider, model) a session's turns run on.
//
// It follows the same fallbacks the usage attribution uses, so the model a turn
// is billed to and the model whose capabilities decide how an attachment is sent
// cannot disagree.
func (s *Server) sessionModelTarget(sess store.ChatSession) (string, string) {
	provider, model := sess.Provider, sess.Model
	if provider == "" {
		provider = s.cfg.Provider
	}
	if model == "" {
		model = s.cfg.Model
	}
	return provider, model
}

// sessionSupportsVision reports whether the model this session runs on declares
// the vision capability.
//
// Capabilities are a hint the operator confirms: the providers' /models endpoint
// does not report them, so an unknown model is treated as text-only. That is the
// safe direction — sending image parts to a model that cannot accept them fails
// the whole turn, while a text-only turn that points at the file still works,
// because the agent can look at it with describe_image.
func (s *Server) sessionSupportsVision(ctx context.Context, sess store.ChatSession) bool {
	provider, model := s.sessionModelTarget(sess)
	if provider == "" || model == "" {
		return false
	}
	m, err := s.store.GetModel(ctx, provider, model)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.Warn("chat: read model capabilities failed",
				zapString("provider", provider), zapString("model", model), zapError(err))
		}
		return false
	}
	return m.Has(store.CapVision)
}

// chatHistoryLimit returns the configured replay limit.
func (s *Server) chatHistoryLimit() int {
	if s.cfg.ChatHistoryLimit > 0 {
		return s.cfg.ChatHistoryLimit
	}
	return config.DefaultChatHistoryLimit
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
