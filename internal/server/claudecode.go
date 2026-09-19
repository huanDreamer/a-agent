package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/claudecode"
	"github.com/huan/huan-agent/internal/claudehook"
	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/workspaces"
)

// ClaudeCode 兼容模式 (设置 → ClaudeCode)
// ======================================
//
// The mode is owned by internal/claudecode; this file is the HTTP surface and
// the four dispatch points. Neither half can answer the other's questions:
// internal/claudecode knows what ~/.claude/settings.json says and nothing about
// conversations, and this file knows which conversation is doing what and
// nothing about hook matchers.
//
// Where the five events fire, which is the contract the panel shows:
//
//	SessionStart      a conversation is created (startup), resumed (resume) or
//	                  cleared (clear)
//	UserPromptSubmit  before a submitted message is persisted — it can refuse it
//	PreToolUse        before every tool call — it can refuse it and rewrite args
//	PostToolUse       after every tool call — it can rewrite the result
//	Stop              when a turn is about to answer — it can make it continue
//
// The first two are dispatched from the handlers below, the last three from the
// turn runner through the chat.Hooks seam (see hookAdapter).

// claudeCodeRoutes registers the mode's endpoints.
func (s *Server) registerClaudeCodeRoutes(authed *route.RouterGroup) {
	// Read and switch the mode. Both answer the same payload the panel renders,
	// so a flip and a read cannot show two different states.
	authed.GET("/claudecode", s.handleClaudeCodeStatus)
	authed.POST("/claudecode/mode", s.handleClaudeCodeMode)
	authed.POST("/claudecode/reload", s.handleClaudeCodeReload)
	// The hook run log, on its own route because the panel refreshes it on its
	// own: a hook fires on every tool call, and re-reading the whole status to
	// see one line would rebuild the whole payload each time.
	authed.GET("/claudecode/events", s.handleClaudeCodeEvents)
}

// claudeCodeStatus assembles the panel payload, or nil when the feature is off.
func (s *Server) claudeCodeStatus() *claudecode.Status {
	if s.claudeCode == nil {
		return nil
	}
	st := s.claudeCode.Status(claudeCodeHookLogLimit)
	// What the mode is *not* using: this deployment's own resolved target. It is
	// read here rather than in the package because it comes from the catalog,
	// which internal/claudecode is only a contributor to.
	st.Native = &claudecode.NativeTarget{Provider: s.cfg.Provider, Model: s.cfg.Model}
	return &st
}

// claudeCodeHookLogLimit bounds the run log the status payload carries.
const claudeCodeHookLogLimit = 50

// handleClaudeCodeStatus answers GET /api/claudecode.
func (s *Server) handleClaudeCodeStatus(ctx context.Context, c *app.RequestContext) {
	st := s.claudeCodeStatus()
	if st == nil {
		// The feature is switched off in config: the panel renders "未启用" from
		// `available:false` rather than from a 404, which would be
		// indistinguishable from a broken server.
		c.JSON(http.StatusOK, map[string]any{
			"mode":           store.ClaudeCodeModeNative,
			"compat":         false,
			"available":      false,
			"provider_id":    claudecode.ProviderID,
			"settings_error": "本进程未启用 ClaudeCode 兼容模式（claudecode 配置段）",
			"hook_log":       []any{},
		})
		return
	}
	c.JSON(http.StatusOK, st)
}

// handleClaudeCodeMode answers POST /api/claudecode/mode.
//
// The body accepts either spelling — `{"compat": true}` or `{"mode":
// "claudecode"}` — because the panel and a script are both plausible callers and
// one of them will get it wrong. An unparseable body is refused rather than
// guessed at: flipping the model of every conversation on a malformed request
// would be the worst possible reading.
func (s *Server) handleClaudeCodeMode(ctx context.Context, c *app.RequestContext) {
	if s.claudeCode == nil {
		c.JSON(http.StatusConflict, map[string]string{
			"error": "本进程未启用 ClaudeCode 兼容模式（claudecode 配置段）",
		})
		return
	}
	var body struct {
		Compat *bool   `json:"compat"`
		Mode   *string `json:"mode"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	var on bool
	switch {
	case body.Compat != nil:
		on = *body.Compat
	case body.Mode != nil:
		switch strings.ToLower(strings.TrimSpace(*body.Mode)) {
		case store.ClaudeCodeModeCompat, "on", "true":
			on = true
		case store.ClaudeCodeModeNative, "off", "false":
			on = false
		default:
			c.JSON(http.StatusBadRequest, map[string]string{
				"error": "mode 只能是 " + store.ClaudeCodeModeNative + " 或 " + store.ClaudeCodeModeCompat,
			})
			return
		}
	default:
		c.JSON(http.StatusBadRequest, map[string]string{"error": "需要 compat 或 mode 字段"})
		return
	}

	if err := s.claudeCode.SetCompat(ctx, on); err != nil {
		s.fail(c, "switch ClaudeCode mode", err)
		return
	}

	// A runner is built per (provider, model) and cached, so a mode change must
	// drop them: a cached runner holds the model it was built with, and keeping
	// one would leave an already-used model answering on the old target until the
	// process restarted. This is the same reason 对话预算 resets the cache.
	s.runnerCache.reset()

	st := s.claudeCodeStatus()
	c.JSON(http.StatusOK, st)
}

// handleClaudeCodeReload answers POST /api/claudecode/reload: re-read
// settings.json now, rather than waiting for the mtime check.
func (s *Server) handleClaudeCodeReload(ctx context.Context, c *app.RequestContext) {
	if s.claudeCode == nil {
		c.JSON(http.StatusConflict, map[string]string{
			"error": "本进程未启用 ClaudeCode 兼容模式（claudecode 配置段）",
		})
		return
	}
	s.claudeCode.Reload()
	// The default model may have changed with the file, and cached runners carry
	// the model they were built with.
	s.runnerCache.reset()
	c.JSON(http.StatusOK, s.claudeCodeStatus())
}

// handleClaudeCodeEvents answers GET /api/claudecode/events.
//
// It is the same records the status payload carries, on their own route so the
// panel can refresh just the log while a turn is running.
func (s *Server) handleClaudeCodeEvents(ctx context.Context, c *app.RequestContext) {
	limit := 50
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	events := []claudehook.Record{}
	if s.claudeCode != nil {
		if recent := s.claudeCode.Recent(limit); recent != nil {
			events = recent
		}
	}
	c.JSON(http.StatusOK, map[string]any{"events": events})
}

// --- the two events the request handlers own --------------------------------

// sessionStartHooks runs SessionStart for a conversation and applies what the
// hooks returned.
//
// Applied effects, and the reason each is a real effect rather than a log line:
//
//   - sessionTitle renames the conversation, which is visible in the sidebar —
//     the same thing /rename does in Claude Code;
//   - additionalContext and initialUserMessage are kept for the conversation's
//     next turn, which is the documented injection point (before the first
//     prompt);
//   - plain stdout (no JSON) is context too: SessionStart is one of the events
//     whose text output reaches the model.
//
// A user-set title is never overwritten: the panel passes the current title, and
// a non-empty one wins. Claude Code's own reference calls this out, and losing a
// rename to a hook would be the same bug here.
func (s *Server) sessionStartHooks(ctx context.Context, sess store.ChatSession, source string) {
	if s.claudeCode == nil {
		return
	}
	verdict := s.claudeCode.Dispatch(ctx, claudehook.Input{
		SessionID:      sess.ID,
		Cwd:            s.sessionWorkspaceRoot(ctx, sess),
		HookEventName:  "SessionStart",
		Source:         source,
		Model:          sess.Model,
		PermissionMode: s.permissionMode(),
		Effort:         s.effort(),
		// §8: a hook that returns sessionTitle should check this first, so it does
		// not overwrite a title the user set. An untitled conversation sends
		// nothing, which is what §8's "若已设置会话标题则为其值" means.
		Extra: sessionTitleExtra(sess.Title),
	})
	if !verdict.Ran {
		return
	}

	if title := strings.TrimSpace(verdict.SessionTitle); title != "" && strings.TrimSpace(sess.Title) == "" {
		if err := s.store.UpdateChatSession(ctx, sess.ID, store.ChatSessionPatch{Title: &title}); err != nil {
			s.logger.Warn("claudecode: applying the SessionStart hook title failed",
				zapString("session", sess.ID), zapError(err))
		}
	}

	var injected []string
	if msg := strings.TrimSpace(verdict.InitialUserMessage); msg != "" {
		injected = append(injected, msg)
	}
	injected = append(injected, verdict.AdditionalContext...)
	s.claudeCode.AddSessionContext(sess.ID, injected)

	for _, msg := range verdict.SystemMessages {
		// The console has no toast lane for a hook's systemMessage; the run log
		// in 设置 → ClaudeCode is where it is read, and the log line is what makes
		// it findable in the server's own log when it is not.
		s.logger.Info("claudecode hook message",
			zapString("event", "SessionStart"), zapString("session", sess.ID), zapString("message", msg))
	}
	if verdict.Failures != nil {
		s.logger.Warn("claudecode SessionStart hook reported failures",
			zapString("session", sess.ID), zapString("failures", strings.Join(verdict.Failures, "; ")))
	}
}

// sessionTitleExtra is the SessionStart payload's session_title, or no extra field
// at all when the conversation has no title.
func sessionTitleExtra(title string) map[string]any {
	if strings.TrimSpace(title) == "" {
		return nil
	}
	return map[string]any{"session_title": strings.TrimSpace(title)}
}

// userPromptSubmitHooks runs UserPromptSubmit for one message.
//
// It returns whether the message is refused and why. Refusing is a documented
// capability of this event (exit 2 clears the prompt), and it is the one hook
// decision that has to be honoured *before* anything is persisted: a message
// that is blocked and stored anyway would be a prompt the user can see, the
// model never answers, and nothing explains.
func (s *Server) userPromptSubmitHooks(ctx context.Context, sess store.ChatSession,
	prompt, promptID string) (string, bool, []string) {

	if s.claudeCode == nil {
		return "", false, nil
	}
	verdict := s.claudeCode.Dispatch(ctx, claudehook.Input{
		SessionID:      sess.ID,
		Cwd:            s.sessionWorkspaceRoot(ctx, sess),
		HookEventName:  "UserPromptSubmit",
		Prompt:         prompt,
		PromptID:       promptID,
		Model:          sess.Model,
		PermissionMode: s.permissionMode(),
		Effort:         s.effort(),
	})
	if !verdict.Ran {
		return "", false, nil
	}
	for _, msg := range verdict.SystemMessages {
		s.logger.Info("claudecode hook message",
			zapString("event", "UserPromptSubmit"), zapString("session", sess.ID), zapString("message", msg))
	}
	if verdict.Block {
		reason := strings.TrimSpace(verdict.BlockReason)
		if reason == "" {
			reason = "这条消息被 UserPromptSubmit hook 阻止（hook 未给出原因）"
		}
		return reason, true, nil
	}
	if !verdict.Continue {
		// continue:false ends processing entirely, which for this event means the
		// prompt is not sent. It is reported with stopReason when there is one,
		// because a refusal with no reason is the worst thing to show a user.
		reason := strings.TrimSpace(verdict.StopReason)
		if reason == "" {
			reason = "UserPromptSubmit hook 返回 continue:false，已停止处理这条消息"
		}
		return reason, true, nil
	}
	return "", false, verdict.AdditionalContext
}

// --- the runner's view ------------------------------------------------------

// hookAdapter implements chat.Hooks for one conversation.
//
// It is built per conversation rather than once per process because two of the
// payload fields are properties of the conversation: cwd (where a command hook
// runs, and what CLAUDE_PROJECT_DIR points at) and the model it runs on. A
// process-wide adapter would hand every hook the same directory, which is wrong
// in exactly the deployment that needs hooks most — several workspaces.
type hookAdapter struct {
	svc       *claudecode.Service
	sessionID string
	cwd       string
	model     string
	// jobs lists the background processes this conversation started, for the Stop
	// payload's background_tasks. It is a narrow interface so the hook layer does
	// not depend on the supervisor.
	jobs backgroundJobs
	// promptID is this turn's prompt id, read from the mode at each dispatch
	// rather than captured at construction: a runner is cached per (provider,
	// model) and reused across turns, so a value fixed here would be the first
	// turn's id forever. It is what lets a handler join the events of one turn —
	// §10 shows the same prompt_id on UserPromptSubmit, PreToolUse, PostToolUse
	// and Stop.
	promptID string
	// permissionMode and effort are the same for every turn of this process; they
	// are captured here so the payload is assembled in one place.
	permissionMode string
	effort         *claudehook.Effort
}

// chatHooksFor returns the dispatcher for one conversation, or nil when the mode
// is off or has no hooks — in which case the runner does no hook work at all.
func (s *Server) chatHooksFor(ctx context.Context, sess store.ChatSession) chat.Hooks {
	if s.claudeCode == nil || !s.claudeCode.HooksActive() {
		return nil
	}
	_, model, _ := s.claudeCode.Target()
	if model == "" {
		model = sess.Model
	}
	return &hookAdapter{
		svc:       s.claudeCode,
		sessionID: sess.ID,
		cwd:       s.sessionWorkspaceRoot(ctx, sess),
		model:     model,
		jobs:      jobRegistryFor(s.jobs),
		// The prompt id is deliberately not captured here: a runner is cached per
		// (provider, model) and reused across turns, so a value read once would be
		// the first turn's id for every later one. input() reads it per dispatch.
		permissionMode: s.permissionMode(),
		effort:         s.effort(),
	}
}

// backgroundJobs is the slice of the job supervisor the Stop payload needs.
type backgroundJobs interface {
	List() []jobs.Job
}

// jobRegistryFor adapts the supervisor, tolerating a deployment without one.
func jobRegistryFor(m *jobs.Manager) backgroundJobs {
	if m == nil {
		return nil
	}
	return m
}

// backgroundTasks renders the running jobs of one conversation the way §8's Stop
// payload expects: the tasks that are still in progress, so a handler can tell
// "the session ended" from "the session is paused waiting for work".
//
// It is never nil — an empty slice means "looked, nothing running". Only running
// jobs of *this* conversation are reported: Claude Code's array describes what
// would wake the session up, and another conversation's process cannot do that.
func backgroundTasks(j backgroundJobs, sessionID string) []claudehook.BackgroundTask {
	out := make([]claudehook.BackgroundTask, 0, 2)
	if j == nil {
		return out
	}
	scope := workspaces.WebScope(sessionID)
	for _, job := range j.List() {
		if job.Status != jobs.StatusRunning || job.Scope != scope {
			continue
		}
		out = append(out, claudehook.BackgroundTask{
			ID:     job.ID,
			Type:   "shell",
			Status: string(job.Status),
			// §8 caps description and command at 1000 characters with an inline
			// "… [+N chars]" marker when truncated. A job's command can be long,
			// so the same rule is applied here rather than sending the whole thing.
			Description: taskField(job.Name, job.Command),
			Command:     taskField(job.Command, ""),
		})
	}
	return out
}

// taskField renders one capped task field, falling back to alt when the preferred
// value is empty.
func taskField(value, alt string) string {
	if strings.TrimSpace(value) == "" {
		value = alt
	}
	return truncateTaskField(value)
}

// maxTaskFieldChars is §8's 1000-character cap on a task's description, command
// and prompt.
const maxTaskFieldChars = 1000

// truncateTaskField applies §8's cap, marker included.
func truncateTaskField(s string) string {
	runes := []rune(s)
	if len(runes) <= maxTaskFieldChars {
		return s
	}
	kept := string(runes[:maxTaskFieldChars])
	return kept + fmt.Sprintf("… [+%d chars]", len(runes)-maxTaskFieldChars)
}

// PreToolUse implements chat.Hooks.
func (h *hookAdapter) PreToolUse(ctx context.Context, call chat.HookCall) chat.HookDecision {
	verdict := h.svc.Dispatch(ctx, h.input(call, "PreToolUse"))
	if !verdict.Ran {
		return chat.HookDecision{}
	}
	decision := chat.HookDecision{
		Block:       verdict.Block,
		BlockReason: verdict.BlockReason,
		Context:     verdict.AdditionalContext,
	}
	// A deny is a block even when the handler expressed it as a
	// permissionDecision rather than an exit code: the two spellings mean the
	// same thing to the model, and a hook that only denied would otherwise be
	// ignored.
	if verdict.PermissionDecision == "deny" && !decision.Block {
		decision.Block = true
		decision.BlockReason = strings.TrimSpace(firstNonEmptyString(verdict.BlockReason, strings.Join(verdict.SystemMessages, "；")))
	}
	if len(verdict.UpdatedInput) > 0 {
		decision.UpdatedArgs = string(verdict.UpdatedInput)
	}
	return decision
}

// PostToolUse implements chat.Hooks.
func (h *hookAdapter) PostToolUse(ctx context.Context, call chat.HookCall) chat.HookDecision {
	verdict := h.svc.Dispatch(ctx, h.input(call, "PostToolUse"))
	if !verdict.Ran {
		return chat.HookDecision{}
	}
	return chat.HookDecision{
		UpdatedResult:    verdict.UpdatedToolOutput,
		HasUpdatedResult: verdict.HasUpdatedOutput,
		Context:          verdict.AdditionalContext,
	}
}

// Stop implements chat.Hooks.
func (h *hookAdapter) Stop(ctx context.Context, stop chat.HookStop) chat.HookStopDecision {
	verdict := h.svc.Dispatch(ctx, claudehook.Input{
		SessionID:      h.sessionID,
		Cwd:            h.cwd,
		HookEventName:  "Stop",
		PromptID:       firstNonEmptyString(h.promptID, h.svc.PromptID(h.sessionID)),
		Model:          h.model,
		PermissionMode: h.permissionMode,
		Effort:         h.effort,
		StopHookActive: stop.AlreadyBlocked,
		LastAssistant:  stop.LastAssistantMessage,
		// §8's two Stop arrays, and they are never nil: an empty array is how the
		// payload distinguishes "nothing is still running" from "this runtime did
		// not look", and a Stop handler that decides whether the session really
		// ended is reading exactly that.
		BackgroundTasks: backgroundTasks(h.jobs, h.sessionID),
		SessionCrons:    []claudehook.SessionCron{},
	})
	if !verdict.Ran {
		return chat.HookStopDecision{}
	}
	reason := strings.TrimSpace(verdict.BlockReason)
	if reason == "" {
		reason = strings.TrimSpace(verdict.StopReason)
	}
	return chat.HookStopDecision{
		Block:   verdict.Block,
		Reason:  reason,
		Context: verdict.AdditionalContext,
	}
}

// input assembles the payload for a tool event.
//
// The tool input and response travel as raw JSON rather than as strings, because
// that is what a hook reads them as: `jq -r '.tool_input.command'` is how every
// real hook in the wild works, and a stringified object would break all of them.
func (h *hookAdapter) input(call chat.HookCall, event string) claudehook.Input {
	in := claudehook.Input{
		SessionID:     h.sessionID,
		Cwd:           h.cwd,
		HookEventName: event,
		PromptID:      firstNonEmptyString(h.promptID, h.svc.PromptID(h.sessionID)),
		// The name is translated, and that is the whole reason a matcher written
		// for Claude Code works at all: `"matcher": "Bash"` is an exact,
		// case-sensitive match against the tool name, while this agent's tool is
		// called `bash`. Without the translation the configured group would never
		// select anything and only a `*` fallback would run — a hook the operator
		// can see in the table and never in the log.
		ToolName:       claudeCodeToolName(call.ToolName),
		ToolUseID:      call.ToolUseID,
		Model:          h.model,
		PermissionMode: h.permissionMode,
		Effort:         h.effort,
	}
	if raw := strings.TrimSpace(call.Args); raw != "" {
		in.ToolInput = json.RawMessage(raw)
	}
	if event == "PostToolUse" {
		// §8: tool_response is an object whose "确切 schema 取决于工具", and the
		// measured Claude Code payload for Bash is the tool's own output object
		// (stdout, stderr, interrupted, isImage). This agent's tools answer with a
		// JSON object too, so its keys are inlined rather than nested under a
		// "result" key: `jq -r '.tool_response.stdout'` has to work, which is the
		// whole reason a hook reads this field.
		resp := map[string]any{}
		if inlined, ok := inlineToolResult(call.Result); ok {
			for k, v := range inlined {
				resp[k] = v
			}
		} else if call.Result != "" {
			// Not JSON — a text answer has nowhere else to live, and the schema is
			// the tool's to define, so it goes under a name a hook can find.
			resp["result"] = call.Result
		}
		// is_error is ours, not the tool's: §8 has no such field, but a hook that
		// wants the failure branch needs one answer, and inferring it from stderr
		// is exactly the mistake a handler cannot make for itself.
		resp["is_error"] = call.Error != ""
		if call.Error != "" {
			resp["error"] = call.Error
		}
		if encoded, err := json.Marshal(resp); err == nil {
			in.ToolResponse = encoded
		}
		// §8's optional duration_ms: what this call cost, measured by the turn.
		duration := int(call.DurationMs)
		in.DurationMS = &duration
	}
	return in
}

// inlineToolResult decodes a tool's output when it is a JSON object, which is how
// every builtin in this agent answers (the bash tool returns command, exit_code,
// stdout, stderr, …).
//
// A JSON *array* or a scalar is not inlined: the protocol's tool_response is an
// object, and wrapping a non-object in one would invent a schema the tool does not
// have.
func inlineToolResult(result string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(result)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

// claudeCodeToolName maps this agent's tool name onto the name Claude Code gives
// the equivalent tool.
//
// It exists because the two vocabularies differ and the protocol matches names
// literally: the operator's settings.json says `Bash`, `Read`, `Edit`, and this
// agent's registry says `bash`, `read_file`, `edit_file`. A compatibility mode
// that kept our names would leave every tool-specific matcher in that file
// silently dead, which is the one failure a compatibility mode must not have.
//
// The mapping is deliberately one-way and small: only the names whose Claude
// Code counterpart is unambiguous are translated, and a name with no counterpart
// is passed through unchanged rather than guessed at (guessing would make a
// `"matcher": "LS"` selector match something that is not a directory listing).
// tool_input keeps this agent's own shape — a hook reading `.tool_input.command`
// on Bash works, one reaching for a field only a Claude Code tool has does not,
// and that limit is stated in 设置 → ClaudeCode and in docs/claudecode.md.
func claudeCodeToolName(name string) string {
	if mapped, ok := claudeCodeToolNames[name]; ok {
		return mapped
	}
	return name
}

// claudeCodeToolNames is the translation table, keyed by this agent's tool name.
//
// Every entry is a tool both sides have. The ones deliberately absent are the
// ones with no counterpart — this agent's describe_image / generate_image /
// transcribe_audio / save_artifact / apply_patch / plan_* — and MCP tools, whose
// names are the MCP server's own on both sides. Translating one of those onto a
// Claude Code name it does not correspond to would make a matcher select a tool
// it was never written for.
var claudeCodeToolNames = map[string]string{
	"bash":        "Bash",
	"read_file":   "Read",
	"write_file":  "Write",
	"edit_file":   "Edit",
	"glob":        "Glob",
	"grep":        "Grep",
	"list_dir":    "LS",
	"fetch_url":   "WebFetch",
	"skill":       "Skill",
	"spawn_agent": "Task",
	"ask_user":    "AskUserQuestion",
}

// permissionMode translates this deployment's approval gate into the
// permission_mode a hook reads.
//
// The mapping is best-effort and deliberately one-directional: the gate decides
// what actually needs approval here, and permission_mode only tells a hook what
// that decision was. "off" becomes bypassPermissions because it is the only
// Claude Code value that means "nothing is gated"; every gated mode becomes
// "default", which is the value that means "writes and commands may ask".
func (s *Server) permissionMode() string {
	switch strings.ToLower(strings.TrimSpace(s.cfg.ApprovalMode)) {
	case "", "off":
		return "bypassPermissions"
	default:
		return "default"
	}
}

// effort is the effort level from the settings file, or nil.
func (s *Server) effort() *claudehook.Effort {
	if s.claudeCode == nil {
		return nil
	}
	level := strings.TrimSpace(s.claudeCode.Settings().Get(claudecode.EnvEffort))
	if level == "" {
		return nil
	}
	return &claudehook.Effort{Level: level}
}

// sessionWorkspaceRoot is the directory a conversation works in, for the cwd of
// a hook payload and for the directory its command hooks run in.
//
// An unresolvable workspace returns "" rather than a guess: the engine then uses
// the process's own working directory, which is at least a truthful answer about
// where the hook ran.
func (s *Server) sessionWorkspaceRoot(ctx context.Context, sess store.ChatSession) string {
	if s.chat.Workspaces == nil {
		return ""
	}
	spec, ok := s.sessionWorkspace(ctx, sess)
	if !ok {
		return ""
	}
	return spec.Root
}

// HookMCPCaller adapts the MCP runtime onto the hook engine's caller, so an
// mcp_tool hook can invoke a tool on a connected server.
//
// It is exported because the mode has to be handed it after the server exists:
// the MCP runtime is built with the tool registry, which is built from the chat
// wiring the mode is itself part of (see claudecode.Service.SetMCPCaller). A nil
// MCP runtime (chat disabled) returns nil, and the engine then records mcp_tool
// handlers as skipped rather than pretending they ran.
func (s *Server) HookMCPCaller() claudehook.MCPCaller {
	if s.mcp == nil {
		return nil
	}
	return mcpHookCaller{mgr: s.mcp}
}

// mcpHookCaller implements claudehook.MCPCaller.
type mcpHookCaller struct{ mgr *mcp.Manager }

// Call invokes one tool, marshalling the substituted input.
func (c mcpHookCaller) Call(ctx context.Context, server, tool string, input map[string]any) (string, error) {
	args := "{}"
	if len(input) > 0 {
		encoded, err := json.Marshal(input)
		if err != nil {
			return "", fmt.Errorf("claudecode: mcp_tool hook input: %w", err)
		}
		args = string(encoded)
	}
	return c.mgr.CallTool(ctx, server, tool, args)
}

// firstNonEmptyString returns the first non-empty value.
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
