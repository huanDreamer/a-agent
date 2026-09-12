package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/google/uuid"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
)

// defaultSystemPrompt is used when config does not override it.
const defaultSystemPrompt = "You are huan-agent, a helpful personal AI assistant. " +
	"Answer in the user's language, be concise, and use tools when they help."

// sseHeartbeat keeps an idle stream alive through proxies and lets the client
// notice a dead connection.
const sseHeartbeat = 15 * time.Second

// chatTracer is the tracing contract the chat layer needs. It is declared here
// (rather than importing the Langfuse client) so the server compiles and runs
// with tracing switched off, and so tests can substitute a double.
type chatTracer interface {
	chat.Tracer
	// Enabled reports whether tracing is active, for the admin UI.
	Enabled() bool
}

// nopTracer is the default when tracing is not configured.
type nopTracer struct{}

// StartTrace implements chatTracer.
func (nopTracer) StartTrace(context.Context, chat.TraceInfo) string { return "" }

// EndTrace implements chatTracer.
func (nopTracer) EndTrace(context.Context, string, any) {}

// StartSpan implements chatTracer.
func (nopTracer) StartSpan(context.Context, chat.SpanInfo) string { return "" }

// EndSpan implements chatTracer.
func (nopTracer) EndSpan(context.Context, string, any, string) {}

// StartGeneration implements chatTracer.
func (nopTracer) StartGeneration(context.Context, chat.GenInfo) string { return "" }

// EndGeneration implements chatTracer.
func (nopTracer) EndGeneration(context.Context, string, any, chat.Usage, string) {}

// Enabled implements chatTracer.
func (nopTracer) Enabled() bool { return false }

// ChatDeps is what the chat API needs from the application.
type ChatDeps struct {
	// Runner executes one conversational turn. Required to enable chat.
	Runner *chat.Runner
	// Builder turns a (provider, model) choice into a chat model. Optional: when
	// nil every conversation uses the default model the Runner was built with.
	Builder ModelBuilder
	// Tools is the registry the runner may call, exposed to the UI so a user can
	// see what the agent can do.
	Tools *tool.Registry
	// SystemPrompt overrides the default.
	SystemPrompt string
	// Workspace confines the chat to one directory. It is where uploaded
	// attachments are stored and read back from, so uploads are refused — with a
	// clear message — when it is nil or read-only rather than written somewhere
	// the agent's tools cannot reach.
	Workspace *workspace.Workspace
	// MaxAttachmentBytes overrides MaxAttachmentBytes for one upload. Zero uses
	// the default; a deployment with a smaller storage budget can lower it.
	MaxAttachmentBytes int64
	// MaxInlineImageBytes overrides MaxInlineImageBytes, the cap on an image that
	// is base64-inlined into a model request. Zero uses the default.
	MaxInlineImageBytes int64
	// Usage records token usage for the admin analytics. Optional: when nil the
	// conversation still works and simply is not counted.
	//
	// The web chat is the surface most turns go through, so leaving this unset
	// makes 统计监控 show nothing for the conversations a user actually has.
	Usage UsageRecorder
}

// UsageRecorder records one model call's token usage. It mirrors
// usage.Recorder without importing it, so the server keeps depending on a
// narrow interface rather than the whole package.
type UsageRecorder interface {
	Record(UsageEvent) error
}

// UsageEvent is one recorded model call.
type UsageEvent struct {
	SessionID        string
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMs       int64
}

// ModelBuilder resolves a per-session model choice.
type ModelBuilder interface {
	// Build returns a chat model for a provider and model name. An empty
	// provider selects the default.
	Build(provider, modelName string) (any, error)
	// Catalog lists the selectable providers.
	Catalog() []ModelChoice
}

// ModelChoice is one selectable provider/model pair.
type ModelChoice struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Default   bool   `json:"default"`
	HasAPIKey bool   `json:"has_api_key"`
}

// Setting up the chat routes is separate from the usage API so the feature can
// be disabled without touching the rest of the server.

// registerChatRoutes wires the chat endpoints. It is a no-op when chat is off.
func (s *Server) registerChatRoutes(authed *route.RouterGroup) {
	if s.chat.Runner == nil {
		return
	}
	authed.GET("/chat/models", s.handleChatModels)
	authed.GET("/chat/sessions", s.handleListSessions)
	authed.POST("/chat/sessions", s.handleCreateSession)
	authed.GET("/chat/sessions/:id", s.handleGetSession)
	authed.PATCH("/chat/sessions/:id", s.handlePatchSession)
	authed.DELETE("/chat/sessions/:id", s.handleDeleteSession)
	authed.POST("/chat/sessions/:id/clear", s.handleClearSession)
	// Attachment endpoints: an upload stores a file in the workspace and records
	// it as a media asset, and the download serves those bytes back so a
	// reloaded conversation can render the image that was sent with it.
	authed.POST("/chat/sessions/:id/attachments", s.handleUploadAttachment)
	authed.GET("/chat/attachments/:id", s.handleGetAttachment)
	// Streaming endpoint: Server-Sent Events, because the browser needs partial
	// output as the model produces it.
	authed.POST("/chat/sessions/:id/messages", s.handleSendMessage)
}

// maxAttachmentBytes returns the effective per-upload cap.
func (s *Server) maxAttachmentBytes() int64 {
	if v := s.chat.MaxAttachmentBytes; v > 0 {
		return v
	}
	return MaxAttachmentBytes
}

// maxInlineImageBytes returns the effective cap on an inlined image.
func (s *Server) maxInlineImageBytes() int64 {
	if v := s.chat.MaxInlineImageBytes; v > 0 {
		return v
	}
	return MaxInlineImageBytes
}

// handleChatModels lists the selectable providers and the available tools.
func (s *Server) handleChatModels(_ context.Context, c *app.RequestContext) {
	out := map[string]any{
		"system_prompt": s.chatPrompt(),
		"max_steps":     s.chatMaxSteps(),
	}
	if s.chat.Builder != nil {
		out["models"] = s.chat.Builder.Catalog()
	} else {
		out["models"] = []ModelChoice{}
	}
	out["tools"] = s.toolNames()
	c.JSON(http.StatusOK, out)
}

// toolNames lists the tools the agent may call.
func (s *Server) toolNames() []string {
	if s.chat.Tools == nil {
		return []string{}
	}
	return s.chat.Tools.Names()
}

// chatPrompt returns the effective system prompt.
func (s *Server) chatPrompt() string {
	if p := strings.TrimSpace(s.chat.SystemPrompt); p != "" {
		return p
	}
	return defaultSystemPrompt
}

// chatMaxSteps returns the effective per-turn step cap.
func (s *Server) chatMaxSteps() int {
	if s.cfg.ChatMaxSteps > 0 {
		return s.cfg.ChatMaxSteps
	}
	return chat.DefaultMaxSteps
}

// handleListSessions returns the caller's sessions, newest activity first.
func (s *Server) handleListSessions(ctx context.Context, c *app.RequestContext) {
	sessions, err := s.store.ListChatSessions(ctx, store.ChatSessionFilter{
		Limit: limitFromQuery(c, 100),
	})
	if err != nil {
		s.fail(c, "list chat sessions", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"sessions": sessions})
}

// handleCreateSession creates a conversation, applying the default model.
func (s *Server) handleCreateSession(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Title    string `json:"title"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := c.BindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		// An empty body is acceptable: it means "use the defaults".
		if strings.TrimSpace(string(c.Request.Body())) != "" {
			c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	}

	provider, model := s.resolveModelChoice(body.Provider, body.Model)
	sess := store.ChatSession{
		ID:       uuid.NewString(),
		Title:    strings.TrimSpace(body.Title),
		UserID:   s.auth.username,
		Provider: provider,
		Model:    model,
	}
	// An untitled session keeps an EMPTY title: the UI renders its own
	// "新对话" placeholder. Storing the placeholder instead would make it
	// indistinguishable from a user who deliberately renamed a session to that
	// text, and the auto-titler would then overwrite their choice.
	if err := s.store.CreateChatSession(ctx, sess); err != nil {
		s.fail(c, "create chat session", err)
		return
	}
	saved, err := s.store.GetChatSession(ctx, sess.ID)
	if err != nil {
		s.fail(c, "read back chat session", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"session": saved})
}

// resolveModelChoice fills an unset provider/model from the defaults.
func (s *Server) resolveModelChoice(provider, model string) (string, string) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if s.chat.Builder == nil {
		return provider, model
	}
	catalog := s.chat.Builder.Catalog()
	for _, m := range catalog {
		if m.Provider == provider && provider != "" {
			if model == "" {
				return provider, m.Model
			}
			return provider, model
		}
	}
	// Unknown or empty provider: fall back to the default entry.
	for _, m := range catalog {
		if m.Default {
			return m.Provider, m.Model
		}
	}
	if len(catalog) > 0 {
		return catalog[0].Provider, catalog[0].Model
	}
	return provider, model
}

// handleGetSession returns one session with its stored messages.
func (s *Server) handleGetSession(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	sess, err := s.store.GetChatSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if err != nil {
		s.fail(c, "get chat session", err)
		return
	}
	msgs, err := s.store.ListChatMessages(ctx, id, 0)
	if err != nil {
		s.fail(c, "list chat messages", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"session": sess, "messages": msgs})
}

// handlePatchSession renames a session or changes its model.
func (s *Server) handlePatchSession(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var body struct {
		Title    *string `json:"title"`
		Provider *string `json:"provider"`
		Model    *string `json:"model"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	patch := store.ChatSessionPatch{Title: body.Title}
	if body.Provider != nil || body.Model != nil {
		// Resolve the pair together so a provider change also picks that
		// provider's model instead of leaving a model from the old provider.
		cur, err := s.store.GetChatSession(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
				return
			}
			s.fail(c, "get chat session", err)
			return
		}
		provider, model := cur.Provider, cur.Model
		if body.Provider != nil {
			provider = *body.Provider
			model = "" // let the resolver choose this provider's default
		}
		if body.Model != nil {
			model = *body.Model
		}
		provider, model = s.resolveModelChoice(provider, model)
		patch.Provider, patch.Model = &provider, &model
	}

	if err := s.store.UpdateChatSession(ctx, id, patch); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		s.fail(c, "update chat session", err)
		return
	}
	sess, err := s.store.GetChatSession(ctx, id)
	if err != nil {
		s.fail(c, "read back chat session", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"session": sess})
}

// handleDeleteSession removes a session and its messages.
func (s *Server) handleDeleteSession(ctx context.Context, c *app.RequestContext) {
	if err := s.store.DeleteChatSession(ctx, c.Param("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		s.fail(c, "delete chat session", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// handleClearSession empties a session's history but keeps the session.
func (s *Server) handleClearSession(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if _, err := s.store.GetChatSession(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		s.fail(c, "get chat session", err)
		return
	}
	if err := s.store.DeleteChatMessages(ctx, id); err != nil {
		s.fail(c, "clear chat session", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// handleSendMessage runs one turn and streams events back as Server-Sent
// Events.
//
// SSE is used rather than a plain JSON response because the whole point is to
// show reasoning and tool calls as they happen; a buffered response would only
// appear once the turn finished.
func (s *Server) handleSendMessage(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var body struct {
		Content string `json:"content"`
		// Attachments are media asset ids uploaded for this session. Optional.
		Attachments []string `json:"attachments"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" && !hasAnyID(body.Attachments) {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "content is required"})
		return
	}

	sess, err := s.store.GetChatSession(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		s.fail(c, "get chat session", err)
		return
	}

	// Resolve the attachments before anything is persisted, so a rejected id
	// (unknown, or belonging to another session) leaves no half-sent message
	// behind. Only this turn's attachments are inlined: replaying stored images
	// on every later turn would re-send megabytes of base64 as prompt tokens for
	// a conversation that already contains the answer.
	inline, notes, assetIDs, err := s.loadTurnAttachments(ctx, sess, body.Attachments)
	if err != nil {
		s.answerStatusError(c, "load chat attachments", err)
		return
	}
	turnText := content
	if len(notes) > 0 {
		note := attachmentNote(notes)
		if turnText == "" {
			turnText = note
		} else {
			turnText += "\n\n" + note
		}
	}
	if turnText == "" {
		turnText = defaultAttachmentText
	}
	// What is persisted is the user's own text, not the note appended for the
	// model: a reloaded transcript should show the message the user wrote, with
	// its attachments rendered from the ids, rather than a system note that
	// looks like the user typed it.
	persisted := content
	if persisted == "" {
		persisted = defaultAttachmentText
	}

	// Build the model for this session's choice before opening the stream, so a
	// misconfiguration is reported as a normal JSON error instead of a broken
	// stream.
	runner, err := s.runnerFor(sess)
	if err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	history, err := s.buildHistory(ctx, sess, buildUserMessage(turnText, inline))
	if err != nil {
		s.fail(c, "build chat history", err)
		return
	}

	// Persist the user's message before running, so a crash mid-turn does not
	// lose it.
	if _, err := s.store.AppendChatMessage(ctx, sess.ID, store.ChatMessage{
		Role: store.RoleUser, Content: persisted, Attachments: attachmentsJSON(assetIDs),
	}); err != nil {
		s.fail(c, "persist user message", err)
		return
	}

	// The handler returns while the goroutine keeps writing, which is what makes
	// this a stream. The request context is tied to the connection, so a client
	// disconnect cancels the run.
	clientGone := ctx.Done()
	pr, pw := io.Pipe()
	c.Response.Header.Set("Content-Type", "text/event-stream")
	c.Response.Header.Set("Cache-Control", "no-cache")
	c.Response.Header.Set("Connection", "keep-alive")
	// Defeat proxy buffering, which would otherwise hold the stream until it
	// ended and defeat the purpose.
	c.Response.Header.Set("X-Accel-Buffering", "no")
	c.SetBodyStream(pr, -1)

	events := make(chan chat.Event, 64)
	done := make(chan *chat.Result, 1)

	go func() {
		defer close(done)
		res, runErr := runner.Run(ctx, chat.Request{
			Messages:  history,
			SessionID: sess.ID,
			UserID:    sess.UserID,
		}, func(e chat.Event) {
			select {
			case events <- e:
			case <-clientGone:
			}
		})
		if runErr != nil {
			// Run already emitted an error event; record it for the history.
			res = &chat.Result{Text: "", Usage: chat.Usage{}}
			select {
			case events <- chat.Event{Type: chat.EventError, Error: runErr.Error()}:
			case <-clientGone:
			}
		}
		done <- res
	}()

	go s.streamTurn(ctx, pw, sess, events, done, clientGone)
}

// streamTurn writes SSE frames until the turn finishes and persists the result.
func (s *Server) streamTurn(ctx context.Context, pw *io.PipeWriter, sess store.ChatSession,
	events <-chan chat.Event, done <-chan *chat.Result, clientGone <-chan struct{}) {

	// Closing the writer ends the response body; the client sees the stream end.
	defer func() { _ = pw.Close() }()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	var (
		answer    strings.Builder
		reasoning strings.Builder
		usage     chat.Usage
		toolRuns  []chat.ToolRun
		runErr    string
	)

	write := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		// SSE frame: "data: <json>\n\n".
		if _, err := fmt.Fprintf(pw, "data: %s\n\n", b); err != nil {
			return false
		}
		return true
	}

	flush := func() {
		// Persist what we have once the turn is over, even if the client
		// disconnected: the conversation should survive a closed tab.
		s.persistTurn(ctx, sess, answer.String(), reasoning.String(), toolRuns, usage, runErr)
	}

	for {
		select {
		case <-clientGone:
			// The browser went away: stop writing but let the run finish and
			// still persist the answer.
			flush()
			return

		case <-heartbeat.C:
			if _, err := fmt.Fprint(pw, ": ping\n\n"); err != nil {
				flush()
				return
			}

		case e, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			switch e.Type {
			case chat.EventTextDelta:
				answer.WriteString(e.Text)
			case chat.EventReasoningDelta:
				reasoning.WriteString(e.Text)
			case chat.EventToolCall:
				toolRuns = append(toolRuns, chat.ToolRun{
					ID: e.ToolCallID, Name: e.ToolName, Args: e.ToolArgs,
				})
			case chat.EventToolResult:
				for i := range toolRuns {
					if toolRuns[i].ID == e.ToolCallID {
						toolRuns[i].Result = e.ToolResult
						toolRuns[i].Err = e.ToolError
						toolRuns[i].DurationMs = e.DurationMs
						break
					}
				}
			case chat.EventUsage:
				if e.Usage != nil {
					usage = *e.Usage
				}
			case chat.EventDone:
				if e.Text != "" {
					answer.Reset()
					answer.WriteString(e.Text)
				}
			case chat.EventError:
				runErr = e.Error
			}
			if !write(e) {
				flush()
				return
			}

		case res := <-done:
			// Drain anything the runner queued before finishing.
			for {
				select {
				case e := <-events:
					switch e.Type {
					case chat.EventReasoningDelta:
						reasoning.WriteString(e.Text)
					case chat.EventDone:
						if e.Text != "" {
							answer.Reset()
							answer.WriteString(e.Text)
						}
					case chat.EventError:
						runErr = e.Error
					}
					_ = write(e)
					continue
				default:
				}
				break
			}
			if res != nil {
				if res.Text != "" {
					answer.Reset()
					answer.WriteString(res.Text)
				}
				if res.Reasoning != "" {
					reasoning.Reset()
					reasoning.WriteString(res.Reasoning)
				}
				if res.Usage.TotalTokens > 0 {
					usage = res.Usage
				}
				if len(res.Tools) > 0 {
					toolRuns = res.Tools
				}
			}
			flush()
			// A terminating frame tells the client the stream is complete even
			// if a proxy closes the connection without warning.
			_ = write(map[string]any{"type": "stream_end"})
			return
		}
	}
}

// persistTurn stores the assistant message (or the failure) for later reloads.
func (s *Server) persistTurn(ctx context.Context, sess store.ChatSession, answer, reasoning string,
	tools []chat.ToolRun, usage chat.Usage, runErr string) {

	// Use a fresh context: the request context is already cancelled when the
	// client disconnects, and the answer must still be saved.
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	usageJSON := ""
	if usage.TotalTokens > 0 || usage.PromptTokens > 0 {
		b, err := json.Marshal(map[string]int{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
			"duration_ms":       int(usage.DurationMs),
		})
		if err == nil {
			usageJSON = string(b)
		}
	}
	toolCallsJSON := ""
	if len(tools) > 0 {
		b, err := json.Marshal(tools)
		if err == nil {
			toolCallsJSON = string(b)
		}
	}

	// Record the turn's token usage, so the analytics count the conversations a
	// user actually has. Without this the web chat — the surface nearly every
	// turn goes through — is invisible in 统计监控, and the dashboard reads as
	// broken rather than empty.
	s.recordTurnUsage(sess, usage)

	// Record each tool invocation in the audit log as well, so the audit view
	// and the conversation agree.
	for _, t := range tools {
		if err := s.store.RecordInvocation(saveCtx, store.InvocationEvent{
			SessionID:  sess.ID,
			UserID:     sess.UserID,
			ToolName:   t.Name,
			Arguments:  t.Args,
			Result:     t.Result,
			Err:        t.Err,
			DurationMs: t.DurationMs,
		}); err != nil {
			s.logger.Warn("persist chat tool invocation failed", zapError(err))
		}
	}

	if answer == "" && runErr == "" {
		return
	}
	msg := store.ChatMessage{
		Role:      store.RoleAssistant,
		Content:   answer,
		Reasoning: reasoning,
		ToolCalls: toolCallsJSON,
		UsageJSON: usageJSON,
		Error:     runErr,
	}
	if _, err := s.store.AppendChatMessage(saveCtx, sess.ID, msg); err != nil {
		s.logger.Warn("persist chat answer failed", zapError(err))
		return
	}
	if err := s.store.TouchChatSession(saveCtx, sess.ID); err != nil {
		s.logger.Warn("touch chat session failed", zapError(err))
	}
	// Auto-title a session from its first user message, so the sidebar is
	// useful without requiring a manual rename.
	s.maybeTitle(saveCtx, sess)
}

// recordTurnUsage reports one turn's totals to the usage recorder.
//
// A turn can span several model calls (one per tool round trip) and the runner
// has already summed them; recording each step separately is not possible from
// here, and a single summed row per turn is what the analytics want.
//
// Failures are logged, never returned: usage accounting must not be able to
// fail a conversation that already succeeded.
func (s *Server) recordTurnUsage(sess store.ChatSession, usage chat.Usage) {
	if s.chat.Usage == nil {
		return
	}
	if usage.TotalTokens == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		// A provider that reported nothing. Recording a zero row would inflate
		// the call count without adding any information.
		return
	}
	// Attribute to the session's model when it has one, else the server default.
	provider, model := sess.Provider, sess.Model
	if provider == "" {
		provider = s.cfg.Provider
	}
	if model == "" {
		model = s.cfg.Model
	}
	if err := s.chat.Usage.Record(UsageEvent{
		SessionID:        sess.ID,
		Provider:         provider,
		Model:            model,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		DurationMs:       usage.DurationMs,
	}); err != nil {
		s.logger.Warn("record chat usage failed", zapError(err))
	}
}

// maybeTitle names an untitled session from its first exchange.
//
// It only fires while the title is empty, so a title the user chose is never
// overwritten — including one that happens to read "新对话".
func (s *Server) maybeTitle(ctx context.Context, sess store.ChatSession) {
	if strings.TrimSpace(sess.Title) != "" {
		return
	}
	msgs, err := s.store.ListChatMessages(ctx, sess.ID, 1)
	if err != nil || len(msgs) == 0 {
		return
	}
	title := strings.TrimSpace(msgs[0].Content)
	if title == "" {
		return
	}
	title = firstLine(title, 40)
	if err := s.store.UpdateChatSession(ctx, sess.ID, store.ChatSessionPatch{Title: &title}); err != nil {
		s.logger.Warn("auto-title chat session failed", zapError(err))
	}
}

// firstLine truncates to the first line and a rune-safe maximum length.
func firstLine(s string, max int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// utf8Start reports whether b begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
