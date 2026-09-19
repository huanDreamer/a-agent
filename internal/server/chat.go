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

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/google/uuid"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/prompt"
	"github.com/huan/huan-agent/internal/retry"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
	"github.com/huan/huan-agent/internal/workspaces"
)

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
	// ToolsFor, when set, returns the registry for one turn, overriding Tools.
	// It is what makes the tool set follow the conversation's workspace: the
	// caller closes over the workspace layer and reads the turn's scope from the
	// context (chat.ScopeFrom).
	//
	// The server resolves it once per turn and hands the result to the runner, so
	// a turn cannot half-run against one workspace and half against another.
	ToolsFor func(ctx context.Context) (*tool.Registry, error)
	// Workspaces is the multi-workspace layer, when the deployment has one. Nil
	// disables the sidebar's workspace folders, and conversations keep running in
	// whatever Tools/Workspace were wired with.
	Workspaces WorkspaceService
	// Condenser bounds the in-loop history of a long turn. Nil means the history
	// is sent whole, which is what an unconfigured context.max_tokens gets.
	//
	// It is supplied by the caller rather than derived here so this package does
	// not have to know which model summarizes: the summarizer is a deployment
	// choice, and the runner only needs the capability.
	Condenser chat.Condenser
	// CondenserFor, when set, builds the condenser for one conversation's model,
	// overriding Condenser. It is what makes the in-loop window follow the
	// model's own context window: a conversation on a 200k model and one on a 32k
	// model get different budgets, and switching model in the composer switches
	// the budget with it.
	//
	// A nil result means "compression off" rather than an error, so a caller that
	// cannot resolve a window does not have to fail the turn over it.
	CondenserFor func(cm model.BaseChatModel, provider, name string) (chat.Condenser, error)
	// Guard tunes the in-turn loop guard: the thresholds at which the model is
	// steered for repeating itself or for reading without ever acting, and the
	// points at which the turn is ended instead. The zero value is "on, with the
	// defaults" — see chat.GuardConfig.
	Guard chat.GuardConfig
	// ToolResultMaxChars bounds one tool result as the model sees it, which is
	// the knob with the most leverage over how often the window has to be
	// compressed. 0 uses the runner's default; negative leaves it unbounded.
	ToolResultMaxChars int
	// SystemPrompt overrides the default.
	SystemPrompt string
	// Workspace confines the chat to one directory. It is where uploaded
	// attachments are stored and read back from, so uploads are refused — with a
	// clear message — when it is nil or read-only rather than written somewhere
	// the agent's tools cannot reach.
	//
	// With Workspaces set this is the *fallback*: an upload is stored in the
	// conversation's own workspace, and this is what a deployment without the
	// workspace layer (or a session whose workspace cannot be resolved) uses.
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
	// StepRetry is how one step of a turn is retried when its model call fails
	// mid-stream — the failure the llm layer cannot take back, because it has
	// already handed the caller half an answer. The zero value means no retrying.
	StepRetry retry.Policy
	// PlanEnable installs the per-turn task plan: the plan_* tools write to it,
	// the console renders it above the composer, and a resumed turn reads it to
	// know what is already done.
	PlanEnable bool
	// PlanMaxTasks bounds how many tasks one plan may hold (0 = default).
	PlanMaxTasks int
	// AskTimeout bounds how long an ask_user question waits for an answer. Zero
	// uses DefaultAskTimeout.
	//
	// It is a deployment setting rather than a constant because the right value
	// is about people, not about the code: a solo operator on the same machine
	// wants a couple of minutes, and a shared console wants something closer to
	// an hour. The wait is bounded again by the turn's context — a stop, a closed
	// tab or a shutdown ends it — and it counts against the turn's wall-clock
	// deadline, so this must stay well under chat.turn_deadline_seconds.
	AskTimeout time.Duration
	// Checkpoints configures the file-level checkpoints the console keeps for a
	// turn. Enabled is what turns the feature on; the rest are its knobs.
	Checkpoints CheckpointSettings
	// MaxParallel is how many parallel-safe tool calls from one model reply may
	// run at once. Zero means one at a time.
	MaxParallel int
	// Subagents lists the runs a conversation delegated, for the header. Nil means
	// this deployment has no subagents and the endpoint is absent.
	Subagents SubagentTracker
	// SubagentMaxConcurrent is the process-wide bound the header reports, so a
	// reader can see why three spawned subagents are running two at a time.
	SubagentMaxConcurrent int
	// ApprovalTimeout bounds how long one write/exec approval waits for a
	// decision. Zero uses DefaultApprovalTimeout.
	//
	// It is shorter than AskTimeout on purpose: a question is the model gathering
	// information, while an approval is a decision about an action that is
	// otherwise blocked, so leaving it hanging holds a turn and a half-finished
	// tool call for no benefit. What happens at zero differs too: a question that
	// times out is an answer the model handles, and an approval that times out is
	// a refusal.
	ApprovalTimeout time.Duration
}

// CheckpointSettings are the checkpoint knobs the console needs.
//
// It is a struct here rather than a config type so the server keeps depending on
// what it uses: the console only ever reads these four fields, and a caller
// wiring it in a test does not have to build a whole configuration.
type CheckpointSettings struct {
	Enable     bool
	Dir        string
	KeepTurns  int
	MaxTotalMB int
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

// ModelBuilder resolves a per-session model choice and describes what is
// selectable.
//
// Both methods take a context because the catalog lives in the database: the
// console reads it on every 设置 and 对话 render, and a shut-down or timed-out
// request must not leave a query running.
type ModelBuilder interface {
	// Build returns a chat model for a provider and model name. An empty
	// provider selects the default (see CatalogModelBuilder.defaultTarget).
	Build(ctx context.Context, provider, modelName string) (any, error)
	// Catalog lists the selectable models and providers in one snapshot, so the
	// chat picker and 设置 cannot disagree about what exists.
	Catalog(ctx context.Context) ModelCatalog
}

// ModelChoice is one selectable provider/model pair.
//
// It carries the display name and the model's declared capabilities so a UI can
// group and label without a second request, and so the same entry renders
// identically in the composer and in 设置.
type ModelChoice struct {
	// Provider is the provider id (the store's primary key / the config key).
	Provider string `json:"provider"`
	// ProviderName is the human-readable name, falling back to the id. Two
	// providers may share a display name, so the id stays authoritative.
	ProviderName string `json:"provider_name"`
	// Model is the model id sent to the provider.
	Model string `json:"model"`
	// DisplayName is what the operator sees; the model id when unnamed.
	DisplayName string `json:"display_name"`
	// Capabilities are the model's stored capabilities.
	Capabilities []string `json:"capabilities"`
	// CapabilitiesSource says where that set came from: the provider's listing,
	// the model's own answer about itself, the model-name heuristics, or the
	// operator. The distinction decides how much the UI should hedge — "由模型名
	// 推断，请自行确认" is right for the heuristics and wrong for a value the
	// provider published.
	CapabilitiesSource string `json:"capabilities_source,omitempty"`
	// ContextWindow is the model's context window in tokens, 0 when nobody has
	// said, and ContextWindowSource says who did (see store.ModelWindowSource*).
	// The settings screen shows both: a number the provider published and one the
	// model guessed are different facts.
	ContextWindow       int    `json:"context_window"`
	ContextWindowSource string `json:"context_window_source,omitempty"`
	// ChatCapable reports whether the model declares the chat capability.
	//
	// ChatCapable=false is possible and meaningful: when a provider has no
	// chat-capable model at all its models are still offered, marked, rather
	// than hidden — a wrong inference must not make a provider vanish from the
	// chat. The UI uses this flag to say so.
	ChatCapable bool `json:"chat_capable"`
	// Default marks the model a new conversation starts on. It is set on at most
	// one entry, and on none when nothing is usable.
	Default bool `json:"default"`
	// HasAPIKey reports whether the provider has usable credentials, so the UI
	// can warn before a call fails with 401. A provider without a key is still
	// listed: the operator may be about to add one.
	HasAPIKey bool `json:"has_api_key"`
}

// ProviderChoice describes one provider as the model surfaces show it: how many
// models it has, when its list was last fetched, and whether that list is stale.
//
// It exists so 模型管理 can display counts and freshness from the same catalog
// endpoint the chat selector reads, instead of each surface deriving its own.
type ProviderChoice struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"`
	// Enabled is false for a provider the operator switched off: it contributes
	// no models to the catalog.
	Enabled   bool `json:"enabled"`
	HasAPIKey bool `json:"has_api_key"`
	// ModelCount is how many models are stored for this provider (all of them,
	// enabled or not) — what 模型管理 lists.
	ModelCount int `json:"model_count"`
	// EnabledModelCount is how many of them are enabled.
	EnabledModelCount int `json:"enabled_model_count"`
	// ChatModelCount is how many enabled models declare the chat capability —
	// how many the composer offers.
	ChatModelCount int `json:"chat_model_count"`
	// LastFetchedAt is when the provider's model list was last read from
	// {base_url}/models, or nil when it never was.
	LastFetchedAt *time.Time `json:"last_fetched_at"`
	// Stale reports that the list is missing or older than the configured TTL,
	// i.e. that it is worth refetching.
	Stale bool `json:"stale"`
	// LastError is the most recent connection failure, empty when the last
	// attempt worked.
	LastError string `json:"last_error"`
}

// ModelCatalog is one snapshot of what is selectable.
type ModelCatalog struct {
	// Models is every offered model, ordered by provider name then model name.
	Models []ModelChoice
	// Providers describes every known provider, enabled or not, in the same
	// order. Only an enabled provider contributes models.
	Providers []ProviderChoice
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
	// Sending a message starts a turn and returns immediately: the turn belongs
	// to the conversation rather than to this request, so the answer is read
	// from the attach endpoint below. That split is what lets a reader come back
	// to a running turn — another page, another conversation, a reload — and
	// still see it, instead of losing the answer because a connection went away.
	authed.POST("/chat/sessions/:id/messages", s.handleSendMessage)
	// Attaching streams a conversation's running turn: the events so far,
	// replayed, and then the rest of them as they happen. Server-Sent Events,
	// because the browser needs partial output as the model produces it.
	authed.GET("/chat/sessions/:id/turn", s.handleAttachTurn)
	// Stopping ends the turn for everyone attached to it.
	authed.POST("/chat/sessions/:id/turn/stop", s.handleStopTurn)
	// Resuming starts a new turn from where the last one died: the previous
	// turn's stored steps and the current plan are folded into the context, so
	// the model continues instead of starting the task over. It answers the same
	// shape as the message endpoint, and the reader then attaches to the turn in
	// the usual way.
	authed.POST("/chat/sessions/:id/resume", s.handleResumeTurn)
	// The answer to an ask_user card arrives on its own request: the attach
	// stream above is one-way, so a browser cannot send a second body on it. The
	// question id joins the two.
	authed.POST("/chat/sessions/:id/questions/:qid/answer", s.handleAnswerQuestion)
	// A write or exec waiting on a decision is decided the same way and for the
	// same reason: the attach stream is one-way, so the decision arrives on its
	// own request and the request id joins the two.
	authed.POST("/chat/sessions/:id/approvals/:aid", s.handleDecideApproval)

	// Checkpoints: what a turn changed, and the way back. Registered only when the
	// feature is on — a console offering a rollback button that always fails is
	// worse than one that does not offer it.
	s.registerCheckpointRoutes(authed)

	// Subagents: what this conversation delegated, for the header chip.
	s.registerSubagentRoutes(authed)
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

// handleChatModels lists the selectable models, the providers behind them and
// the available tools.
//
// It is the single model contract of the console: 对话's composer builds its
// grouped selector from `models`, and 设置's catalog table and 模型管理 read the
// same arrays. Anything a model surface displays therefore comes from here
// rather than from a second derivation that could disagree.
func (s *Server) handleChatModels(ctx context.Context, c *app.RequestContext) {
	// The budget comes from the same resolver a turn uses (config.yaml merged
	// with whatever 设置 → 对话预算 stored), so what the composer shows and what
	// the next turn actually runs cannot disagree.
	budget := s.effectiveBudget(ctx)
	out := map[string]any{
		"system_prompt": s.chatPrompt(),
		"max_steps":     budget.maxSteps,
		// The other two budgets travel with the step cap: the console shows what
		// a turn may spend, and a user who just watched a turn stop on its
		// budget should be able to see which budget that was.
		"turn_max_tokens":       budget.maxTokens,
		"turn_deadline_seconds": budget.deadlineSeconds(),
	}
	if s.chat.Builder != nil {
		cat := s.chat.Builder.Catalog(ctx)
		out["models"] = orEmptyChoices(cat.Models)
		out["providers"] = orEmptyProviders(cat.Providers)
	} else {
		out["models"] = []ModelChoice{}
		out["providers"] = []ProviderChoice{}
	}
	out["tools"] = s.toolNames()
	c.JSON(http.StatusOK, out)
}

// orEmptyChoices guarantees a JSON array rather than null, so a client can
// iterate the field without a null check.
func orEmptyChoices(in []ModelChoice) []ModelChoice {
	if in == nil {
		return []ModelChoice{}
	}
	return in
}

// orEmptyProviders is orEmptyChoices for the provider summaries.
func orEmptyProviders(in []ProviderChoice) []ProviderChoice {
	if in == nil {
		return []ProviderChoice{}
	}
	return in
}

// toolNames lists the tools the agent may call.
func (s *Server) toolNames() []string {
	if s.chat.Tools == nil {
		return []string{}
	}
	return s.chat.Tools.Names()
}

// chatPrompt returns the effective system prompt: the operator's override when
// they set one, otherwise the console's default (the general-purpose prompt plus
// the web section). The precedence is prompt.Effective's, so it is the same rule
// the Feishu bot applies.
func (s *Server) chatPrompt() string {
	return prompt.Effective(s.chat.SystemPrompt, prompt.SurfaceWeb)
}

// The three budget accessors that used to live here are gone: the per-turn
// budget now has exactly one resolver, effectiveBudget (budget.go), because a
// second one that only read config.yaml would ignore what 设置 → 对话预算 stored
// and let the console show one number while a turn ran another.

// handleListSessions returns the caller's sessions, newest activity first.
func (s *Server) handleListSessions(ctx context.Context, c *app.RequestContext) {
	sessions, err := s.store.ListChatSessions(ctx, store.ChatSessionFilter{
		Limit:     limitFromQuery(c, 100),
		Workspace: c.Query("workspace"),
	})
	if err != nil {
		s.fail(c, "list chat sessions", err)
		return
	}
	// Every row carries the workspace it belongs to, because the sidebar groups
	// by it (the store joins the binding in, so this is one query). A conversation
	// whose binding is somehow missing is filled in with the fallback here rather
	// than left blank, since a blank one has no folder to appear in.
	if s.chat.Workspaces != nil {
		fallback := ""
		for i := range sessions {
			if sessions[i].Workspace != "" {
				continue
			}
			if fallback == "" {
				def, derr := s.chat.Workspaces.Default(ctx)
				if derr != nil {
					break
				}
				fallback = def.Name
			}
			sessions[i].Workspace = fallback
		}
	}
	c.JSON(http.StatusOK, map[string]any{"sessions": s.withStreaming(sessions)})
}

// handleCreateSession creates a conversation, applying the default model.
func (s *Server) handleCreateSession(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Title     string `json:"title"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Workspace string `json:"workspace"`
	}
	if err := c.BindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		// An empty body is acceptable: it means "use the defaults".
		if strings.TrimSpace(string(c.Request.Body())) != "" {
			c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	}

	// Every conversation belongs to a workspace, so which one is decided here
	// rather than on the first message: the sidebar shows the conversation under
	// its workspace immediately, and there is no window in which it belongs
	// nowhere. An explicit choice wins; otherwise it starts where the last
	// conversation was.
	workspaceName, werr := s.sessionWorkspaceForCreate(ctx, body.Workspace)
	if werr != nil {
		s.workspaceError(c, "resolve workspace for new session", werr)
		return
	}

	provider, model := s.resolveModelChoice(ctx, body.Provider, body.Model)
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
	if workspaceName != "" {
		if err := s.store.SetWorkspaceBinding(ctx, workspaces.WebScope(sess.ID), workspaceName); err != nil {
			s.fail(c, "bind chat session workspace", err)
			return
		}
	}
	saved, err := s.store.GetChatSession(ctx, sess.ID)
	if err != nil {
		s.fail(c, "read back chat session", err)
		return
	}
	if workspaceName != "" {
		saved.Workspace = workspaceName
	}
	c.JSON(http.StatusOK, map[string]any{"session": saved})
}

// sessionWorkspaceForCreate decides which workspace a new conversation starts in.
//
// An empty name means "wherever the last conversation was" (the manager's
// default), which is what the sidebar's own 新建对话 button relies on. A name that
// does not exist is refused with the list of what does, rather than silently
// creating the conversation somewhere else.
func (s *Server) sessionWorkspaceForCreate(ctx context.Context, name string) (string, error) {
	if s.chat.Workspaces == nil {
		// No workspace layer: the conversation runs in whatever single directory
		// the deployment configured, which is the pre-workspace behaviour.
		return "", nil
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		spec, err := s.chat.Workspaces.Get(ctx, trimmed)
		if err != nil {
			return "", err
		}
		return spec.Name, nil
	}
	spec, err := s.chat.Workspaces.Default(ctx)
	if err != nil {
		return "", err
	}
	return spec.Name, nil
}

// resolveModelChoice fills an unset provider/model from the defaults.
func (s *Server) resolveModelChoice(ctx context.Context, provider, model string) (string, string) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if s.chat.Builder == nil {
		return provider, model
	}
	catalog := s.chat.Builder.Catalog(ctx).Models

	// 1. An explicit provider wins. A missing model takes that provider's first.
	if provider != "" {
		for _, m := range catalog {
			if m.Provider == provider {
				if model == "" {
					return provider, m.Model
				}
				return provider, model
			}
		}
	}

	// 2. Nothing chosen: reuse what the last conversation used, so a new
	// conversation starts where the user left off rather than snapping back to
	// the configured default. This is the same rule the workspace already
	// follows, and it is resolved here rather than in the browser so it holds
	// whichever device or tab opens the console.
	if provider == "" && model == "" && s.store != nil {
		if p, m, ok := s.store.LatestSessionModel(ctx); ok {
			if chosen, usable := modelChoiceFrom(catalog, p, m); usable {
				return chosen.Provider, chosen.Model
			}
			// The remembered model is gone (deleted, or its provider disabled).
			// Falling through to the default is better than resurrecting a
			// choice that can no longer run.
		}
	}

	// 3. The catalog's default entry, else the first one.
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

// modelChoiceFrom finds a catalog entry matching a remembered provider/model.
// A remembered provider with no model recorded falls back to that provider's
// first offered model. Both halves must be offered and enabled, otherwise the
// choice is unusable and the caller should move on to the default.
func modelChoiceFrom(catalog []ModelChoice, provider, model string) (ModelChoice, bool) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" {
		return ModelChoice{}, false
	}
	for _, m := range catalog {
		if m.Provider != provider {
			continue
		}
		if model == "" || m.Model == model {
			return m, true
		}
	}
	return ModelChoice{}, false
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
	// The conversation's workspace travels with the conversation, and it has to be
	// the same value the turn's tools will use rather than a client-side guess.
	// The console no longer renders it (the sidebar's folder is where that fact
	// lives), but the reply is the contract other clients read.
	body := map[string]any{
		"session":  sessionView{ChatSession: sess, Streaming: s.turns.running(sess.ID)},
		"messages": msgs,
		// The header's numbers (轮数 / 模型调用 / 工具调用 / token) are aggregated
		// here rather than in the browser: they have to survive a reload, and two
		// clients must not be able to disagree about them.
		"stats": s.sessionStatsFor(ctx, sess.ID, msgs),
		// The task plan travels with the conversation for the same reason: the
		// board above the composer is conversation state, and a reloaded page has
		// to show it without waiting for the next turn to touch it. Null means
		// "this conversation has no plan", which the console renders as no board
		// rather than as an empty one.
		"plan": s.sessionPlan(ctx, sess.ID),
	}
	if spec, ok := s.sessionWorkspace(ctx, sess); ok {
		body["workspace"] = spec
	}
	c.JSON(http.StatusOK, body)
}

// sessionWorkspace returns the workspace a conversation is in.
//
// The second return is false when the deployment has no workspace layer, which
// is a supported configuration (the conversation then runs in whatever single
// workspace it was wired with). A failure to read the selection is logged and
// treated the same way: it is presentation and audit metadata, and must not turn
// a readable conversation into an error.
func (s *Server) sessionWorkspace(ctx context.Context, sess store.ChatSession) (workspaces.Spec, bool) {
	if s.chat.Workspaces == nil {
		return workspaces.Spec{}, false
	}
	spec, err := s.chat.Workspaces.Active(ctx, workspaces.WebScope(sess.ID))
	if err != nil {
		s.logger.Warn("chat: read session workspace failed",
			zapString("session", sess.ID), zapError(err))
		return workspaces.Spec{}, false
	}
	return spec, true
}

// sessionWorkspaceName is sessionWorkspace reduced to the name the audit log
// records, with "" when there is nothing to record.
func (s *Server) sessionWorkspaceName(ctx context.Context, sess store.ChatSession) string {
	spec, ok := s.sessionWorkspace(ctx, sess)
	if !ok {
		return ""
	}
	return spec.Name
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
		provider, model = s.resolveModelChoice(ctx, provider, model)
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
	// The plan goes with the history. It is the one piece of turn state that
	// outlives its turn, and leaving it behind would show a board of tasks for a
	// conversation that no longer has the messages explaining them — and offer
	// 继续执行 for work whose context was just deleted.
	if err := s.store.DeleteChatPlan(ctx, id); err != nil {
		s.fail(c, "clear chat plan", err)
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

	history, err := s.buildHistory(ctx, sess, buildUserMessage(turnText, inline))
	if err != nil {
		s.fail(c, "build chat history", err)
		return
	}

	s.launchTurn(ctx, c, sess, store.ChatMessage{
		Role: store.RoleUser, Content: persisted, Attachments: attachmentsJSON(assetIDs),
	}, history, true)
}

// launchTurn puts one turn in flight for a conversation and answers the request
// that asked for it, on a history the caller has already assembled.
//
// It is shared by sending a message and by resuming a dead turn, because
// everything after "the history is ready" is identical for the two and the
// difference between them is exactly the history. Keeping it in one place is
// what makes the resume path inherit the budget resolution, the workspace
// binding, the one-turn-per-conversation rule and the accepted-response shape
// without a second copy that can drift from them.
//
// newTask says which of the two this is: a message from the user starts a new
// piece of work (so a finished plan is cleared), while a resume continues the one
// that is already in progress (so its plan is the whole point).
func (s *Server) launchTurn(ctx context.Context, c *app.RequestContext, sess store.ChatSession,
	userMsg store.ChatMessage, history []*schema.Message, newTask bool) {

	// Refuse before anything is written. A message that is going to be refused
	// must not be left in the conversation: the reader would see their own text
	// followed by no answer and no explanation, and the client is already
	// offering it back for a retry.
	if s.turns.running(sess.ID) {
		c.JSON(http.StatusConflict, map[string]string{
			"error": "这个对话已有一轮正在生成，请等它结束或先停止它",
		})
		return
	}

	// The model for this session's choice, resolved before anything is written:
	// a misconfigured model is a normal JSON error rather than a stored message
	// nobody will ever answer.
	runner, err := s.runnerFor(ctx, sess)
	if err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Persist the user's message before running, so a crash mid-turn does not
	// lose it.
	if _, err := s.store.AppendChatMessage(ctx, sess.ID, userMsg); err != nil {
		s.fail(c, "persist user message", err)
		return
	}

	// This turn's budget, resolved once, from config.yaml merged with whatever
	// 设置 → 对话预算 stored. Reading it here rather than at startup is what makes
	// a change in the console apply to the next message instead of the next
	// process; reading it once is what keeps a change made mid-turn from moving
	// the ceiling of a turn already in flight.
	budget := s.effectiveBudget(ctx)

	// Bump the conversation before the turn runs, not only when it is stored.
	//
	// The sidebar sorts by "last activity", and a conversation that is being
	// answered right now is the most active thing there is. Without this, a
	// reload would sort a running turn below older ones — and auto-select
	// something else, which is precisely when a reader cannot find the answer
	// they were watching.
	if err := s.store.TouchChatSession(ctx, sess.ID); err != nil {
		s.logger.Warn("chat: touch session on turn start failed",
			zapString("session", sess.ID), zapError(err))
	}

	workspace := s.sessionWorkspaceName(ctx, sess)
	live := s.startTurn(turnRun{
		session:   sess,
		workspace: workspace,
		runner:    runner,
		history:   history,
		scope:     workspaces.WebScope(sess.ID),
		newTask:   newTask,
		maxSteps:  budget.maxSteps,
		maxTokens: budget.maxTokens,
		deadline:  budget.deadline,
	})
	if live == nil {
		// One turn per conversation: the reader has a single composer, and two
		// answers interleaved into one transcript could not be told apart. The
		// check above already answered this case; this is the race it cannot
		// close, and losing here means the user message above is already stored.
		c.JSON(http.StatusConflict, map[string]string{
			"error": "这个对话已有一轮正在生成，请等它结束或先停止它",
		})
		return
	}

	// The turn is now the conversation's, not this request's: it keeps running
	// whether or not the caller stays attached. The browser attaches to it with
	// GET /chat/sessions/{id}/turn — and re-attaches after a reload, another
	// page, or another conversation, and sees the same stream from the start.
	c.JSON(http.StatusAccepted, map[string]any{
		"turn": map[string]any{
			"session_id": sess.ID,
			"streaming":  true,
			"started_at": live.startedAt.UTC().Format(time.RFC3339Nano),
		},
	})
}

// handleAttachTurn streams a conversation's running turn to whoever asks.
//
// This is the endpoint the console reads its answers from. It replays the turn
// from its first event and then follows it live, so attaching late — after
// switching pages, switching conversations, or reloading the browser — shows
// the same thing as having been there from the start: the answer so far, and
// then the rest of it as it arrives.
//
// It answers a plain JSON `{"streaming": false}` when nothing is running, rather
// than holding a connection open: the conversation's stored messages are then
// the whole truth, and the console has already loaded them.
func (s *Server) handleAttachTurn(ctx context.Context, c *app.RequestContext) {
	live := s.turns.find(c.Param("id"))
	if live == nil {
		c.JSON(http.StatusOK, map[string]any{"streaming": false})
		return
	}

	pr, pw := io.Pipe()
	c.Response.Header.Set("Content-Type", "text/event-stream")
	c.Response.Header.Set("Cache-Control", "no-cache")
	c.Response.Header.Set("Connection", "keep-alive")
	// Defeat proxy buffering, which would otherwise hold the stream until it
	// ended and defeat the purpose.
	c.Response.Header.Set("X-Accel-Buffering", "no")
	c.SetBodyStream(pr, -1)

	go s.streamLiveTurn(ctx, pw, live)
}

// handleStopTurn ends a conversation's running turn.
//
// Stopping is a request of its own so that "the reader stopped it" is a fact
// about the turn rather than about one connection: the run is cancelled, what
// it produced is kept, and every attached reader sees it end.
func (s *Server) handleStopTurn(ctx context.Context, c *app.RequestContext) {
	stopped := s.turns.stop(c.Param("id"))
	c.JSON(http.StatusOK, map[string]any{"ok": true, "stopped": stopped})
}

// streamLiveTurn writes one attached reader's copy of a live turn.
//
// Nothing here owns the turn: the writer may go away at any moment (a closed
// tab, a reload, a scroll to another conversation) and the turn carries on. The
// reader's cursor is local, so a slow one falls behind and then catches up
// rather than losing events, and the heartbeat keeps an idle stream alive
// through proxies during the long silent stretches a slow model produces.
func (s *Server) streamLiveTurn(ctx context.Context, pw *io.PipeWriter, live *liveTurn) {
	defer func() { _ = pw.Close() }()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	write := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		// SSE frame: "data: <json>\n\n".
		if _, err := fmt.Fprintf(pw, "data: %s\n\n", b); err != nil {
			return false
		}
		return true
	}

	watcher, replayed, cursor, done := live.subscribe()
	defer live.unsubscribe(watcher)

	send := func(events []chat.Event) bool {
		for _, e := range events {
			if !write(e) {
				return false
			}
		}
		return true
	}

	if !send(replayed) {
		return
	}

	for {
		if done {
			// A terminating frame tells the client the turn is complete even if
			// a proxy closes the connection without warning. The turn has been
			// persisted by now, so the reader can reload and find the message.
			write(map[string]any{"type": "stream_end"})
			return
		}
		select {
		case <-watcher.notify:
		case <-heartbeat.C:
			if _, err := fmt.Fprint(pw, ": ping\n\n"); err != nil {
				return
			}
		case <-ctx.Done():
			// This reader is gone. The turn is not.
			return
		}
		next, advanced, finished := live.since(cursor)
		if !send(next) {
			return
		}
		cursor, done = advanced, finished
	}
}

// persistTurn stores the assistant message (or the failure) for later reloads.
//
// workspace names the workspace the turn ran in, for the audit rows: a tool path
// is workspace-relative, so "write_file src/main.go" looks identical in every
// workspace and the audit would otherwise be unable to say which project a
// change landed in.
func (s *Server) persistTurn(ctx context.Context, sess store.ChatSession, workspace string,
	sum turnSummary, runErr, traceID string) {

	// Use a fresh context: the request context is already cancelled when the
	// client disconnects, and the answer must still be saved.
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	usageJSON := ""
	if sum.usage.TotalTokens > 0 || sum.usage.PromptTokens > 0 {
		b, err := json.Marshal(map[string]int{
			"prompt_tokens":     sum.usage.PromptTokens,
			"completion_tokens": sum.usage.CompletionTokens,
			"total_tokens":      sum.usage.TotalTokens,
			"duration_ms":       int(sum.usage.DurationMs),
		})
		if err == nil {
			usageJSON = string(b)
		}
	}
	// The flat list stays as well: the audit rows, the session statistics and
	// older clients read it, and it is what a tool-call count is derived from.
	toolCallsJSON := ""
	if len(sum.tools) > 0 {
		b, err := json.Marshal(sum.tools)
		if err == nil {
			toolCallsJSON = string(b)
		}
	}
	// The same calls, kept next to the reasoning that asked for them. This is
	// what the console renders: a flat list beside one block of reasoning cannot
	// show which thought belongs to which action, however it is styled.
	stepsJSON := ""
	if len(sum.plan) > 0 {
		b, err := json.Marshal(sum.plan)
		if err == nil {
			stepsJSON = string(b)
		}
	}

	// Record the turn's token usage, so the analytics count the conversations a
	// user actually has. Without this the web chat — the surface nearly every
	// turn goes through — is invisible in 统计监控, and the dashboard reads as
	// broken rather than empty.
	s.recordTurnUsage(sess, sum.usage)

	// Record each tool invocation in the audit log as well, so the audit view
	// and the conversation agree.
	for _, t := range sum.tools {
		if err := s.store.RecordInvocation(saveCtx, store.InvocationEvent{
			SessionID:  sess.ID,
			UserID:     sess.UserID,
			ToolName:   t.Name,
			Arguments:  t.Args,
			Result:     t.Result,
			Err:        t.Err,
			Workspace:  workspace,
			DurationMs: t.DurationMs,
		}); err != nil {
			s.logger.Warn("persist chat tool invocation failed", zapError(err))
		}
	}

	answer := sum.answer
	if answer == "" && runErr == "" {
		// The turn ran and produced nothing at all — no text, no failure the
		// runner could name. Writing no row would leave the conversation holding
		// the user's message and no trace of what became of it, which is exactly
		// what "the answer disappeared" looks like from the outside. So the turn
		// is recorded as the failure it was.
		runErr = "本轮没有产生任何输出"
	}
	msg := store.ChatMessage{
		Role:      store.RoleAssistant,
		Content:   answer,
		Reasoning: sum.reasoning,
		ToolCalls: toolCallsJSON,
		Steps:     stepsJSON,
		UsageJSON: usageJSON,
		Error:     runErr,
		// Why the turn ended, when it was not the model's own answer. Empty is
		// the ordinary case, and the UI renders a set value as a badge on the
		// message rather than leaving the reader to find the sentence inside it.
		StopReason: sum.stopReason,
		// The link from this answer to the trace that produced it. Empty when
		// tracing is off, which the UI renders as "no link" rather than a link
		// that leads nowhere.
		TraceID: traceID,
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
