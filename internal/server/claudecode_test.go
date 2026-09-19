package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/claudecode"
	"github.com/huan/huan-agent/internal/claudehook"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/jobs"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/workspaces"
)

// These tests cover the two halves of the mode that live in this package: the
// HTTP surface 设置 → ClaudeCode talks to, and the dispatch points that make the
// hooks fire. The mode's own reading of settings.json is tested in
// internal/claudecode; what is tested here is that the console's requests reach
// it and that a configured hook can actually change what a conversation does.

// newClaudeHarness starts a server built with the given options and logs the
// harness in: the mode's routes are behind the admin session like every other
// console route, and a test that forgot to log in would be testing the 401.
func newClaudeHarness(t *testing.T, srv *Server) *harness {
	t.Helper()
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv}
	h.login(t)
	return h
}

// claudeHarnessWithChat builds the harness with the chat routes registered and
// the mode wired from a settings file.
//
// The mode's two request-level hooks (SessionStart and UserPromptSubmit) only
// exist on a deployment with chat enabled — without a runner those routes are
// not registered at all — so a test of them has to wire a chat, and a scripted
// model is enough: neither hook runs a turn.
//
// The mode is built through buildOpts.claudeCodeFor rather than assigned to the
// server afterwards: the harness starts the HTTP engine, and a field written
// after that is read by a handler goroutine — a data race the detector rightly
// reports.
func claudeHarnessWithChat(t *testing.T, path string, enable bool) (*harness, store.Store, *claudecode.Service) {
	t.Helper()
	runner, err := chat.New(chat.Config{Model: &scriptedModel{}, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	var svc *claudecode.Service
	srv, st := buildServerWith(t, buildOpts{
		chat: ChatDeps{Runner: runner},
		claudeCodeFor: func(st store.Store) *claudecode.Service {
			svc = newClaudeService(t, st, path, enable)
			return svc
		},
	})
	return newClaudeHarness(t, srv), st, svc
}

// claudeSettings writes a settings file with the given env and hooks block and
// returns its path.
func claudeSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

// hookScript writes an executable handler and returns its path.
func hookScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}
	return path
}

// usableEnv is the env block a settings file needs before the mode can run.
const usableEnv = `"env":{"ANTHROPIC_BASE_URL":"https://api.deepseek.com/anthropic",
	"ANTHROPIC_AUTH_TOKEN":"sk-test-token-value-1234","ANTHROPIC_MODEL":"deepseek-flash[1m]",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL":"deepseek-flash","CLAUDE_CODE_EFFORT_LEVEL":"max"}`

// newClaudeService builds the mode over a store and a settings file.
func newClaudeService(t *testing.T, st store.Store, path string, enable bool) *claudecode.Service {
	t.Helper()
	return claudecode.NewService(context.Background(), claudecode.Options{
		Config: config.ClaudeCodeConfig{Enable: enable, SettingsPath: path},
		Store:  st,
		Logger: zap.NewNop(),
	})
}

// TestClaudeCodeStatusWhenTheFeatureIsOff pins the "not configured" answer: the
// console renders why the switch is not there, and a 404 would be
// indistinguishable from a broken server.
func TestClaudeCodeStatusWhenTheFeatureIsOff(t *testing.T) {
	srv, _ := buildServerWith(t, buildOpts{})
	h := newClaudeHarness(t, srv)

	var body map[string]any
	h.getJSON(t, "/api/claudecode", http.StatusOK, &body)
	if body["available"] != false || body["compat"] != false {
		t.Errorf("body = %v, want available:false compat:false", body)
	}
	if body["mode"] != store.ClaudeCodeModeNative {
		t.Errorf("mode = %v, want native", body["mode"])
	}

	// Switching it is refused rather than silently ignored.
	resp := h.postJSON(t, "/api/claudecode/mode", map[string]any{"compat": true})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestClaudeCodeStatusReportsTheSettingsFile(t *testing.T) {
	path := claudeSettings(t, `{`+usableEnv+`,`+"\n"+`"hooks":{
		"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/tmp/log-hook.sh","timeout":10}]}],
		"Notification":[{"matcher":"*","hooks":[{"type":"command","command":"/tmp/log-hook.sh"}]}]}}`)
	srv, st := buildServerWith(t, buildOpts{})
	svc := newClaudeService(t, st, path, false)
	srv.claudeCode = svc

	h := newClaudeHarness(t, srv)
	var body struct {
		Mode          string `json:"mode"`
		Compat        bool   `json:"compat"`
		Available     bool   `json:"available"`
		SettingsPath  string `json:"settings_path"`
		SettingsFound bool   `json:"settings_found"`
		Model         struct {
			BaseURL        string `json:"base_url"`
			EffectiveModel string `json:"effective_model"`
			TokenMasked    string `json:"token_masked"`
			HasToken       bool   `json:"has_token"`
			Ready          bool   `json:"ready"`
			Kind           string `json:"kind"`
		} `json:"model"`
		Native *struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"native"`
		Env []struct {
			Key    string `json:"key"`
			Value  string `json:"value"`
			Secret bool   `json:"secret"`
			Used   bool   `json:"used"`
		} `json:"env"`
		Hooks struct {
			Enabled               bool     `json:"enabled"`
			SupportedEvents       []string `json:"supported_events"`
			TotalHandlers         int      `json:"total_handlers"`
			UnsupportedConfigured []string `json:"unsupported_configured"`
			Events                []struct {
				Event     string `json:"event"`
				Supported bool   `json:"supported"`
				Meaning   string `json:"meaning"`
			} `json:"events"`
		} `json:"hooks"`
	}
	h.getJSON(t, "/api/claudecode", http.StatusOK, &body)

	if body.SettingsPath != path || !body.SettingsFound {
		t.Errorf("settings path/found = %q/%v", body.SettingsPath, body.SettingsFound)
	}
	if !body.Available || !body.Model.Ready {
		t.Errorf("a usable settings file must be available: %+v", body.Model)
	}
	if body.Model.Kind != "anthropic-messages" {
		t.Errorf("kind = %q, want the Anthropic Messages protocol", body.Model.Kind)
	}
	if body.Model.EffectiveModel != "deepseek-flash[1m]" {
		t.Errorf("effective_model = %q", body.Model.EffectiveModel)
	}
	// Native is this deployment's own target, which the server fills in because
	// this package has no way to know it.
	if body.Native == nil || body.Native.Provider != "deepseek" || body.Native.Model != "deepseek-chat" {
		t.Errorf("native = %+v, want the server's own target", body.Native)
	}
	// The credential is masked in the payload, never sent whole — including in
	// the env table, which is a copy of the settings file.
	for _, row := range body.Env {
		if strings.Contains(row.Value, "sk-test-token-value-1234") {
			t.Fatalf("env row %s leaked the token", row.Key)
		}
	}
	if body.Model.TokenMasked == "" || !body.Model.HasToken {
		t.Error("the masked token must be reported so the operator can recognise the key")
	}
	if !body.Hooks.Enabled && body.Compat {
		t.Error("hooks should be active once the mode is on")
	}
	if len(body.Hooks.UnsupportedConfigured) != 1 || body.Hooks.UnsupportedConfigured[0] != "Notification" {
		t.Errorf("unsupported_configured = %v, want Notification named", body.Hooks.UnsupportedConfigured)
	}
	for _, e := range body.Hooks.Events {
		if e.Meaning == "" {
			t.Errorf("event %s carries no meaning line", e.Event)
		}
	}
}

func TestClaudeCodeModeSwitchIsPersistedAndDrivesTheCatalog(t *testing.T) {
	ctx := context.Background()
	path := claudeSettings(t, `{`+usableEnv+`}`)
	srv, st := buildServerWith(t, buildOpts{})
	svc := newClaudeService(t, st, path, false)
	srv.claudeCode = svc
	h := newClaudeHarness(t, srv)

	// Off: the catalog has nothing from the mode.
	builder := NewCatalogModelBuilder(st, nil, ModelBuilderOptions{Extra: svc, Logger: zap.NewNop()})
	if hasCompatModel(builder.Catalog(ctx).Models) {
		t.Fatal("a mode that is off must not offer models")
	}

	resp := h.postJSON(t, "/api/claudecode/mode", map[string]any{"compat": true})
	var body struct {
		Compat bool   `json:"compat"`
		Mode   string `json:"mode"`
		Source string `json:"source"`
		Model  struct {
			EffectiveModel string `json:"effective_model"`
		} `json:"model"`
	}
	decodeJSON(t, resp, http.StatusOK, &body)
	if !body.Compat || body.Mode != store.ClaudeCodeModeCompat {
		t.Errorf("body = %+v, want the mode on", body)
	}
	if body.Source != "console" {
		t.Errorf("source = %q, want console", body.Source)
	}

	// On: the provider joins the catalog and is the default a new conversation
	// starts on — which is what makes the switch reach 对话 and not only 设置.
	catalog := builder.Catalog(ctx)
	if !hasCompatModel(catalog.Models) {
		t.Fatal("a mode that is on must offer its models in the catalog")
	}
	defaulted := 0
	for _, m := range catalog.Models {
		if m.Provider == claudecode.ProviderID && m.Default {
			defaulted++
			if m.Model != fmtEffectiveModel(svc) {
				t.Errorf("default compat model = %q, want the effective model", m.Model)
			}
		}
	}
	if defaulted != 1 {
		t.Errorf("%d compat models are marked default, want exactly 1", defaulted)
	}

	// The choice is stored, so a restart keeps it.
	mode, ok, err := st.GetClaudeCodeMode(ctx)
	if err != nil || !ok || mode != store.ClaudeCodeModeCompat {
		t.Fatalf("stored mode = %q ok=%v err=%v, want the switch persisted", mode, ok, err)
	}

	// And off again.
	resp = h.postJSON(t, "/api/claudecode/mode", map[string]any{"mode": "native"})
	requireStatus(t, resp, http.StatusOK)
	_ = resp
	if svc.Compat() {
		t.Error("the mode did not turn off")
	}
	if hasCompatModel(builder.Catalog(ctx).Models) {
		t.Error("the provider must leave the catalog when the mode is off")
	}
}

func TestClaudeCodeModeRejectsABadBody(t *testing.T) {
	path := claudeSettings(t, `{`+usableEnv+`}`)
	srv, st := buildServerWith(t, buildOpts{})
	srv.claudeCode = newClaudeService(t, st, path, false)
	h := newClaudeHarness(t, srv)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{name: "no field", body: map[string]any{}},
		{name: "unknown mode", body: map[string]any{"mode": "yolo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.postJSON(t, "/api/claudecode/mode", tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestClaudeCodeReloadAndEventsRoutes(t *testing.T) {
	path := claudeSettings(t, `{`+usableEnv+`,"hooks":{"Stop":[{"matcher":"*","hooks":[
		{"type":"command","command":"`+hookScript(t, `printf '{"systemMessage":"stopped"}'\nexit 0`)+`"}]}]}}`)
	srv, st := buildServerWith(t, buildOpts{})
	svc := newClaudeService(t, st, path, true)
	srv.claudeCode = svc
	h := newClaudeHarness(t, srv)

	// A hook fires, so the run log has something to report.
	svc.Dispatch(context.Background(), claudehook.Input{SessionID: "s", HookEventName: "Stop"})
	var events struct {
		Events []claudehook.Record `json:"events"`
	}
	h.getJSON(t, "/api/claudecode/events?limit=5", http.StatusOK, &events)
	if len(events.Events) != 1 || events.Events[0].Event != "Stop" {
		t.Fatalf("events = %+v, want the Stop hook that ran", events.Events)
	}

	// 重新读取 re-reads the file and answers the same payload the panel renders.
	resp := h.postJSON(t, "/api/claudecode/reload", nil)
	var status struct {
		Compat        bool `json:"compat"`
		SettingsFound bool `json:"settings_found"`
	}
	decodeJSON(t, resp, http.StatusOK, &status)
	if !status.Compat || !status.SettingsFound {
		t.Errorf("after reload: %+v", status)
	}
}

// TestSessionStartHookNamesANewConversation is the end-to-end half of the wiring:
// a configured SessionStart handler reaches the conversation it was configured
// for, and what it returns lands where Claude Code would put it.
func TestSessionStartHookNamesANewConversation(t *testing.T) {
	script := hookScript(t, `payload=$(cat)
	printf '{"hookSpecificOutput":{"hookEventName":"SessionStart",
		"sessionTitle":"由 hook 命名","additionalContext":"部署目标是生产环境"}}'
	exit 0`)
	path := claudeSettings(t, `{`+usableEnv+`,"hooks":{"SessionStart":[{"matcher":"startup","hooks":[
		{"type":"command","command":"`+script+`","timeout":10}]}]}}`)

	h, _, svc := claudeHarnessWithChat(t, path, true)

	resp := h.postJSON(t, "/api/chat/sessions", map[string]any{})
	var body struct {
		Session store.ChatSession `json:"session"`
	}
	decodeJSON(t, resp, http.StatusOK, &body)
	if body.Session.ID == "" {
		t.Fatal("no session was created")
	}
	if body.Session.Title != "由 hook 命名" {
		t.Errorf("title = %q, want the hook's sessionTitle", body.Session.Title)
	}
	// The injected context is kept for the conversation's next turn, which is the
	// documented injection point.
	if got := svc.SessionContext(body.Session.ID); len(got) != 1 || got[0] != "部署目标是生产环境" {
		t.Errorf("session context = %v, want the hook's additionalContext", got)
	}

	// A conversation created while the hooks are off must not be renamed.
	if err := svc.SetCompat(context.Background(), false); err != nil {
		t.Fatalf("SetCompat: %v", err)
	}
	resp = h.postJSON(t, "/api/chat/sessions", map[string]any{})
	decodeJSON(t, resp, http.StatusOK, &body)
	if body.Session.Title != "" {
		t.Errorf("title = %q, want an untouched title with the mode off", body.Session.Title)
	}
}

// TestUserPromptSubmitHookBlocksAMessage covers the one hook decision that has to
// be honoured before anything is stored.
func TestUserPromptSubmitHookBlocksAMessage(t *testing.T) {
	script := hookScript(t, `cat >/dev/null
echo "这条消息不允许发送" >&2
exit 2`)
	path := claudeSettings(t, `{`+usableEnv+`,"hooks":{"UserPromptSubmit":[{"matcher":"*","hooks":[
		{"type":"command","command":"`+script+`","timeout":10}]}]}}`)

	h, st, _ := claudeHarnessWithChat(t, path, true)
	srv := h.srv

	ctx := context.Background()
	sess := store.ChatSession{ID: "sess-blocked", UserID: "admin"}
	if err := st.CreateChatSession(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	loaded, err := st.GetChatSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	reason, blocked, extra := srv.userPromptSubmitHooks(ctx, loaded, "rm -rf /", "prompt-1")
	if !blocked {
		t.Fatalf("the hook did not block: reason=%q extra=%v", reason, extra)
	}
	if !strings.Contains(reason, "不允许发送") {
		t.Errorf("reason = %q, want the hook's stderr", reason)
	}

	// And the whole request path refuses too, without storing the message.
	resp := h.postJSON(t, "/api/chat/sessions/"+sess.ID+"/messages",
		map[string]any{"content": "rm -rf /"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a blocked message", resp.StatusCode)
	}
	msgs, err := st.ListChatMessages(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("a blocked message must not be stored, found %d", len(msgs))
	}
}

// TestHookContextIsInsertedBeforeTheUserMessage pins the placement of
// hook-injected text: the model's instruction must stay the last thing it reads.
func TestHookContextIsInsertedBeforeTheUserMessage(t *testing.T) {
	history := []*schema.Message{
		{Role: schema.System, Content: "rules"},
		{Role: schema.User, Content: "earlier question"},
		{Role: schema.Assistant, Content: "earlier answer"},
		{Role: schema.User, Content: "now this"},
	}
	out := insertHookContext(history, []string{"hook says hello", "  ", "hook says more"})
	if len(out) != 5 {
		t.Fatalf("got %d messages, want 5 (one inserted, the blank dropped)", len(out))
	}
	if out[len(out)-1].Content != "now this" {
		t.Errorf("last message = %q, want the user's own message last", out[len(out)-1].Content)
	}
	note := out[len(out)-2]
	if note.Role != schema.System {
		t.Errorf("inserted message role = %q, want system", note.Role)
	}
	if !strings.Contains(note.Content, "hook says hello") || !strings.Contains(note.Content, "hook says more") {
		t.Errorf("inserted content = %q, want both hook lines", note.Content)
	}

	// Nothing to insert leaves the history untouched, including its last role.
	if got := insertHookContext(history, nil); len(got) != len(history) {
		t.Errorf("an empty injection changed the history: %d messages", len(got))
	}
}

// TestHookAdapterBuildsTheProtocolPayload checks the tool-event payload, which is
// what a hook written for Claude Code reads with jq.
func TestHookAdapterBuildsTheProtocolPayload(t *testing.T) {
	path := claudeSettings(t, `{`+usableEnv+`}`)
	srv, st := buildServerWith(t, buildOpts{approvalMode: "writes"})
	srv.claudeCode = newClaudeService(t, st, path, true)
	adapter := &hookAdapter{
		svc:            srv.claudeCode,
		sessionID:      "sess-1",
		cwd:            "/tmp/workspace",
		model:          "deepseek-flash[1m]",
		permissionMode: srv.permissionMode(),
		effort:         srv.effort(),
	}

	in := adapter.input(chat.HookCall{
		SessionID: "ignored",
		ToolUseID: "toolu-1",
		ToolName:  "Bash",
		Args:      `{"command":"ls"}`,
		Result:    "file.txt",
	}, "PostToolUse")

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if decoded["hook_event_name"] != "PostToolUse" {
		t.Errorf("hook_event_name = %v", decoded["hook_event_name"])
	}
	if decoded["tool_name"] != "Bash" || decoded["tool_use_id"] != "toolu-1" {
		t.Errorf("tool fields = %v", decoded)
	}
	if decoded["cwd"] != "/tmp/workspace" {
		t.Errorf("cwd = %v, want the conversation's workspace", decoded["cwd"])
	}
	// The arguments and the response are objects, not strings: a hook that reads
	// .tool_input.command must get the command.
	input, ok := decoded["tool_input"].(map[string]any)
	if !ok || input["command"] != "ls" {
		t.Errorf("tool_input = %#v, want the parsed object", decoded["tool_input"])
	}
	resp, ok := decoded["tool_response"].(map[string]any)
	if !ok || resp["result"] != "file.txt" || resp["is_error"] != false {
		t.Errorf("tool_response = %#v", decoded["tool_response"])
	}
	// A gated deployment reports "default"; the approval mode is this agent's own
	// vocabulary and must be translated, not passed through.
	if decoded["permission_mode"] != "default" {
		t.Errorf("permission_mode = %v, want default for a gated deployment", decoded["permission_mode"])
	}
	if decoded["effort"] == nil {
		t.Error("the effort level from settings.json should travel with the payload")
	}
}

func TestPermissionModeTranslation(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{mode: "", want: "bypassPermissions"},
		{mode: "off", want: "bypassPermissions"},
		{mode: "writes", want: "default"},
		{mode: "all", want: "default"},
	} {
		srv := &Server{cfg: Config{ApprovalMode: tc.mode}}
		if got := srv.permissionMode(); got != tc.want {
			t.Errorf("permissionMode(%q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// TestChatHooksAreAbsentWithoutTheMode pins the cheap direction: a deployment
// that never turned the mode on does no hook work at all.
func TestChatHooksAreAbsentWithoutTheMode(t *testing.T) {
	path := claudeSettings(t, `{`+usableEnv+`,"hooks":{"Stop":[{"matcher":"*","hooks":[
		{"type":"command","command":"/tmp/x.sh"}]}]}}`)
	srv, st := buildServerWith(t, buildOpts{})
	ctx := context.Background()
	sess := store.ChatSession{ID: "s"}

	if hooks := srv.chatHooksFor(ctx, sess); hooks != nil {
		t.Error("no mode, no hooks")
	}

	// On but with the hooks switch off is the same answer.
	srv.claudeCode = newClaudeService(t, st, path, true)
	off := false
	srv.claudeCode = claudecode.NewService(ctx, claudecode.Options{
		Config: config.ClaudeCodeConfig{Enable: true, SettingsPath: path, Hooks: &off},
		Store:  st,
		Logger: zap.NewNop(),
	})
	if hooks := srv.chatHooksFor(ctx, sess); hooks != nil {
		t.Error("claudecode.hooks = false must leave the runner with no hooks")
	}

	srv.claudeCode = newClaudeService(t, st, path, true)
	hooks := srv.chatHooksFor(ctx, sess)
	if hooks == nil {
		t.Fatal("a mode with hooks must give the runner a dispatcher")
	}
	// A tool call with no matching handler is a no-op decision, not a block.
	decision := hooks.PreToolUse(ctx, chat.HookCall{ToolName: "Read", Args: `{}`})
	if decision.Block || decision.UpdatedArgs != "" {
		t.Errorf("decision = %+v, want a no-op for a tool with no configured hook", decision)
	}
}

// TestClaudeCodeToolNamesAreTranslated pins the translation that makes a
// Claude Code matcher select anything at all: `"matcher": "Bash"` is an exact,
// case-sensitive comparison, and this agent's tool is called `bash`.
func TestClaudeCodeToolNamesAreTranslated(t *testing.T) {
	for ours, want := range map[string]string{
		"bash": "Bash", "read_file": "Read", "write_file": "Write", "edit_file": "Edit",
		"glob": "Glob", "grep": "Grep", "list_dir": "LS", "fetch_url": "WebFetch",
		"skill": "Skill", "spawn_agent": "Task",
	} {
		if got := claudeCodeToolName(ours); got != want {
			t.Errorf("claudeCodeToolName(%q) = %q, want %q", ours, got, want)
		}
	}
	// A tool with no Claude Code counterpart is passed through rather than
	// guessed at, so a matcher naming one of Claude Code's tools cannot select a
	// tool that is not it.
	for _, ours := range []string{"describe_image", "save_artifact", "apply_patch", "mcp__memory__search", "plan_create"} {
		if got := claudeCodeToolName(ours); got != ours {
			t.Errorf("claudeCodeToolName(%q) = %q, want it unchanged", ours, got)
		}
	}
}

// TestPreToolUseMatcherSelectsOurTool is the end-to-end half of the translation:
// a hook configured with Claude Code's matcher must fire for this agent's tool,
// and the payload must name the tool the way the hook's author wrote it.
func TestPreToolUseMatcherSelectsOurTool(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "seen.log")
	script := hookScript(t, `payload=$(cat)
printf '%s\n' "$payload" >> `+logPath+`
printf '{"systemMessage":"bash 被拦了一次"}'
exit 0`)
	path := claudeSettings(t, `{`+usableEnv+`,"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[
		{"type":"command","command":"`+script+`","timeout":10}]}]}}`)

	srv, st := buildServerWith(t, buildOpts{})
	svc := newClaudeService(t, st, path, true)
	srv.claudeCode = svc
	srv.cfg.ApprovalMode = "off"

	hooks := srv.chatHooksFor(context.Background(), store.ChatSession{ID: "s"})
	if hooks == nil {
		t.Fatal("the mode is on with hooks, so the runner must have a dispatcher")
	}
	// The name this agent produces. A matcher of "Bash" must select it.
	decision := hooks.PreToolUse(context.Background(), chat.HookCall{
		ToolName: "bash", Args: `{"command":"ls"}`, ToolUseID: "t1",
	})
	if len(decision.Context) == 0 && !decision.Block {
		// The SystemMessages are not part of the decision; what proves the hook
		// ran is the file it wrote.
		if _, err := os.Stat(logPath); err != nil {
			t.Fatalf("the Bash matcher did not select our bash tool: %v", err)
		}
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the hook never ran: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &payload); err != nil {
		t.Fatalf("payload: %v (%s)", err, raw)
	}
	if payload["tool_name"] != "Bash" {
		t.Errorf("tool_name = %v, want the Claude Code name a hook author writes", payload["tool_name"])
	}

	// A tool the matcher does not name must not run it, and the file must not
	// grow.
	before := len(raw)
	hooks.PreToolUse(context.Background(), chat.HookCall{ToolName: "read_file", Args: `{}`})
	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(after) != before {
		t.Error("the Bash matcher selected a Read tool call")
	}
}

// hasCompatModel reports whether a catalog offers the compatibility provider.
func hasCompatModel(models []ModelChoice) bool {
	for _, m := range models {
		if m.Provider == claudecode.ProviderID {
			return true
		}
	}
	return false
}

// fmtEffectiveModel reads the effective model the mode resolves to.
func fmtEffectiveModel(svc *claudecode.Service) string {
	m, _ := svc.Model()
	return m.EffectiveModel
}

// TestTheCompatProviderCannotBeEditedInModelManagement pins the guard that keeps
// the mode's provider from becoming a stored row: a save with its id would create
// a real-looking, keyless endpoint in 模型管理 while the mode is off, and be
// shadowed by the mode while it is on.
func TestTheCompatProviderCannotBeEditedInModelManagement(t *testing.T) {
	path := claudeSettings(t, `{`+usableEnv+`}`)
	srv, st := buildServerWith(t, buildOpts{})
	srv.claudeCode = newClaudeService(t, st, path, true)
	h := newClaudeHarness(t, srv)

	body := map[string]any{
		"id":       claudecode.ProviderID,
		"base_url": "https://example.invalid/v1",
		"enabled":  false,
	}
	resp := h.postJSON(t, "/api/llm/providers", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for the reserved provider id", resp.StatusCode)
	}
	// And nothing was written.
	if _, err := st.GetProvider(context.Background(), claudecode.ProviderID); err == nil {
		t.Error("a provider row was created for the compatibility mode")
	}
}

// TestToolPayloadShapeMatchesTheReference pins the PostToolUse payload against
// what Claude Code actually sends (§8/§10 of the hook reference): tool_response is
// the *tool's own output object*, so a handler reading .tool_response.stdout — the
// documented shape for Bash — finds it instead of a string it would have to parse
// twice.
func TestToolPayloadShapeMatchesTheReference(t *testing.T) {
	path := claudeSettings(t, `{`+usableEnv+`}`)
	srv, st := buildServerWith(t, buildOpts{})
	srv.claudeCode = newClaudeService(t, st, path, true)
	adapter := &hookAdapter{
		svc:            srv.claudeCode,
		sessionID:      "sess-1",
		cwd:            "/tmp/ws",
		model:          "deepseek-flash[1m]",
		permissionMode: srv.permissionMode(),
	}

	// What this agent's bash tool really returns: a JSON object.
	toolOutput := `{"command":"ls","exit_code":0,"stdout":"a.txt\n","stderr":"","duration_ms":9}`
	in := adapter.input(chat.HookCall{
		ToolName:   "bash",
		ToolUseID:  "call_1",
		Args:       `{"command":"ls"}`,
		Result:     toolOutput,
		DurationMs: 9,
	}, "PostToolUse")

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	resp, ok := decoded["tool_response"].(map[string]any)
	if !ok {
		t.Fatalf("tool_response = %#v，必须是对象", decoded["tool_response"])
	}
	if resp["stdout"] != "a.txt\n" {
		t.Errorf("tool_response.stdout = %#v，期望工具自己的 stdout", resp["stdout"])
	}
	if resp["exit_code"] != float64(0) {
		t.Errorf("tool_response.exit_code = %#v", resp["exit_code"])
	}
	if resp["is_error"] != false {
		t.Errorf("tool_response.is_error = %#v，期望 false", resp["is_error"])
	}
	if _, nested := resp["result"]; nested {
		t.Error("结果对象不该再被包一层 result：那会让 jq -r '.tool_response.stdout' 取不到值")
	}
	// §8's optional duration_ms, measured by the turn.
	if decoded["duration_ms"] != float64(9) {
		t.Errorf("duration_ms = %#v，期望 9", decoded["duration_ms"])
	}
	// The tool name is the Claude Code one, so a "Bash" matcher and a handler that
	// logs tool_name both see what they expect.
	if decoded["tool_name"] != "Bash" {
		t.Errorf("tool_name = %#v", decoded["tool_name"])
	}

	// A tool that answers with plain text has nowhere else to put it, and the
	// schema belongs to the tool: it goes under result rather than being dropped.
	textIn := adapter.input(chat.HookCall{ToolName: "read_file", Result: "hello"}, "PostToolUse")
	textRaw, _ := json.Marshal(textIn)
	var textDecoded map[string]any
	_ = json.Unmarshal(textRaw, &textDecoded)
	if resp, _ := textDecoded["tool_response"].(map[string]any); resp["result"] != "hello" {
		t.Errorf("文本结果 = %#v，期望放在 result 下", textDecoded["tool_response"])
	}

	// A failure carries the reason plus is_error, so a handler can branch on one
	// field instead of guessing from stderr.
	failIn := adapter.input(chat.HookCall{ToolName: "bash", Error: "exit status 1"}, "PostToolUse")
	failRaw, _ := json.Marshal(failIn)
	var failDecoded map[string]any
	_ = json.Unmarshal(failRaw, &failDecoded)
	failResp, _ := failDecoded["tool_response"].(map[string]any)
	if failResp["is_error"] != true || failResp["error"] != "exit status 1" {
		t.Errorf("失败结果 = %#v", failDecoded["tool_response"])
	}
}

// TestStopPayloadCarriesTheTaskArrays pins §8's Stop fields against a real job
// registry: an empty array (not a missing field) when nothing is running, and the
// session's own running process when there is one.
func TestStopPayloadCarriesTheTaskArrays(t *testing.T) {
	// No registry: still an empty array, because "looked, nothing running" and
	// "did not look" are different answers for a Stop handler.
	empty := backgroundTasks(nil, "sess-1")
	if empty == nil || len(empty) != 0 {
		t.Fatalf("没有作业管理器时必须给出空数组，得到 %#v", empty)
	}

	path := claudeSettings(t, `{`+usableEnv+`}`)
	srv, st := buildServerWith(t, buildOpts{})
	srv.claudeCode = newClaudeService(t, st, path, true)
	adapter := &hookAdapter{
		svc: srv.claudeCode, sessionID: "sess-1", cwd: "/tmp/ws", model: "m",
		jobs: fakeJobRegistry{jobs: []jobs.Job{
			{ID: "j1", Name: "跑测试", Command: "npm test", Status: jobs.StatusRunning,
				Scope: workspaces.WebScope("sess-1")},
			// Another conversation's job: it cannot wake this session up, so §8's
			// "进行中的任务" does not include it.
			{ID: "j2", Command: "ls", Status: jobs.StatusRunning, Scope: workspaces.WebScope("other")},
			// A finished job is not in progress.
			{ID: "j3", Command: "done", Status: jobs.StatusExited, Scope: workspaces.WebScope("sess-1")},
		}},
	}

	in := claudehook.Input{
		SessionID:       "sess-1",
		HookEventName:   "Stop",
		BackgroundTasks: backgroundTasks(adapter.jobs, "sess-1"),
		SessionCrons:    []claudehook.SessionCron{},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tasks, ok := decoded["background_tasks"].([]any)
	if !ok || len(tasks) != 1 {
		t.Fatalf("background_tasks = %#v，期望只有本会话在跑的那一个", decoded["background_tasks"])
	}
	task := tasks[0].(map[string]any)
	if task["id"] != "j1" || task["type"] != "shell" || task["status"] != "running" {
		t.Errorf("task = %#v", task)
	}
	if task["command"] != "npm test" {
		t.Errorf("task.command = %#v", task["command"])
	}
	if _, ok := decoded["session_crons"].([]any); !ok {
		t.Errorf("session_crons = %#v，期望空数组", decoded["session_crons"])
	}
	// stop_hook_active is present as an explicit false on Stop (§10).
	if v, ok := decoded["stop_hook_active"]; !ok || v != false {
		t.Errorf("stop_hook_active = %#v，Stop 载荷必须显式带上它", decoded["stop_hook_active"])
	}
}

// TestPromptIDIsSharedByEveryEventOfOneTurn pins the correlation a handler uses to
// join its own log lines: Claude Code stamps one prompt_id across a turn.
func TestPromptIDIsSharedByEveryEventOfOneTurn(t *testing.T) {
	path := claudeSettings(t, `{`+usableEnv+`}`)
	srv, st := buildServerWith(t, buildOpts{})
	svc := newClaudeService(t, st, path, true)
	srv.claudeCode = svc
	srv.cfg.ApprovalMode = "off"

	svc.SetPromptID("sess-1", "prompt-abc")
	adapter := &hookAdapter{
		svc: svc, sessionID: "sess-1", cwd: "/tmp/ws", model: "m",
		permissionMode: srv.permissionMode(),
	}
	for _, event := range []string{"PreToolUse", "PostToolUse"} {
		in := adapter.input(chat.HookCall{ToolName: "bash", Args: `{}`, Result: `{}`}, event)
		if in.PromptID != "prompt-abc" {
			t.Errorf("事件 %s 的 prompt_id = %q，期望本轮注册的 id", event, in.PromptID)
		}
	}

	// A fresh turn replaces it, and /clear forgets it — otherwise every later
	// turn would keep reporting the first prompt's id.
	svc.SetPromptID("sess-1", "prompt-def")
	if got := svc.PromptID("sess-1"); got != "prompt-def" {
		t.Errorf("PromptID = %q，期望最新一轮的 id", got)
	}
	svc.ForgetSession("sess-1")
	if got := svc.PromptID("sess-1"); got != "" {
		t.Errorf("ForgetSession 之后 PromptID = %q，期望已清空", got)
	}
}

// TestSessionStartSendsTheTitleWhenThereIsOne pins §8's guidance that a hook
// returning sessionTitle should check the existing one first, so it cannot
// overwrite a rename the user made.
func TestSessionStartSendsTheTitleWhenThereIsOne(t *testing.T) {
	if extra := sessionTitleExtra("  "); extra != nil {
		t.Errorf("没有标题时不该发 session_title：%#v", extra)
	}
	extra := sessionTitleExtra("  我的对话  ")
	if extra == nil || extra["session_title"] != "我的对话" {
		t.Fatalf("session_title = %#v，期望去掉空白后的标题", extra)
	}

	// And it reaches the payload as a top-level field.
	in := claudehook.Input{SessionID: "s", HookEventName: "SessionStart",
		Source: "resume", Extra: extra}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["session_title"] != "我的对话" {
		t.Errorf("session_title = %#v", decoded["session_title"])
	}
}

// TestTaskFieldTruncationFollowsTheDocumentedMark pins §8's marker: a truncated
// task field ends with "… [+N chars]", and N counts the characters that were cut.
func TestTaskFieldTruncationFollowsTheDocumentedMark(t *testing.T) {
	short := strings.Repeat("a", 1000)
	if got := truncateTaskField(short); got != short {
		t.Errorf("正好 1000 字符不该被截断（%d）", len(got))
	}
	long := strings.Repeat("b", 1010)
	got := truncateTaskField(long)
	if !strings.HasSuffix(got, "… [+10 chars]") {
		t.Errorf("截断标记 = %q", got[len(got)-20:])
	}
	if job := taskField("", "npm test"); job != "npm test" {
		t.Errorf("空名字应回退到命令行，得到 %q", job)
	}
}

// fakeJobRegistry stands in for the process's job supervisor.
type fakeJobRegistry struct{ jobs []jobs.Job }

func (f fakeJobRegistry) List() []jobs.Job { return f.jobs }
