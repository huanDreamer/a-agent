package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huan/huan-agent/internal/claudehook"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/store"
)

// writeSettings writes a settings file into a temp directory and returns its
// path. The content is what the test wants Claude Code's file to say; the shape
// is the real one (see ~/.claude/settings.json).
func writeSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

// liveShapedSettings is a settings file shaped like the one Claude Code writes
// on this machine: an env block pointing at an Anthropic-shaped gateway, and
// hooks on the five events this build dispatches plus one it does not.
const liveShapedSettings = `{
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
    "ANTHROPIC_AUTH_TOKEN": "sk-4f94bd7b345b41afb12a8b7e80d52f10",
    "ANTHROPIC_MODEL": "deepseek-flash[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "deepseek-flash[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "deepseek-flash[1m]",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "deepseek-flash",
    "CLAUDE_CODE_SUBAGENT_MODEL": "deepseek-flash",
    "CLAUDE_CODE_EFFORT_LEVEL": "max",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "786432"
  },
  "hooks": {
    "SessionStart": [
      {"matcher": "startup|resume|clear|compact|fork",
       "hooks": [{"type": "command", "command": "/tmp/log-hook.sh", "args": ["matcher=startup"], "timeout": 10}]}
    ],
    "UserPromptSubmit": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "/tmp/log-hook.sh", "timeout": 10}]}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/tmp/log-hook.sh", "timeout": 10}]},
      {"matcher": "*", "hooks": [{"type": "command", "command": "/tmp/log-hook.sh", "timeout": 10}]}
    ],
    "PostToolUse": [
      {"matcher": "Edit|Write", "hooks": [{"type": "command", "command": "/tmp/log-hook.sh", "timeout": 10}]}
    ],
    "Stop": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "/tmp/log-hook.sh", "timeout": 10}]}
    ],
    "Notification": [
      {"matcher": "permission_prompt", "hooks": [{"type": "command", "command": "/tmp/log-hook.sh"}]}
    ]
  }
}`

func TestLoadReadsTheLiveShapedFile(t *testing.T) {
	path := writeSettings(t, liveShapedSettings)
	s := Load(path)

	if !s.Found || s.Err != "" {
		t.Fatalf("Found=%v Err=%q, want a clean read", s.Found, s.Err)
	}
	if got := s.Get(EnvBaseURL); got != "https://api.deepseek.com/anthropic" {
		t.Errorf("%s = %q", EnvBaseURL, got)
	}
	if got := s.Get(EnvAuthToken); !strings.HasPrefix(got, "sk-") {
		t.Errorf("%s = %q, want the token read verbatim", EnvAuthToken, got)
	}
	if got := len(s.Hooks.Groups); got != 6 {
		t.Errorf("parsed %d events, want 6 (all of them, including the one this build cannot fire)", got)
	}
	if got := len(s.Hooks.Groups["PreToolUse"]); got != 2 {
		t.Errorf("PreToolUse has %d matcher groups, want 2", got)
	}
	first := s.Hooks.Groups["PreToolUse"][0].Handlers
	if len(first) != 1 || first[0].Command != "/tmp/log-hook.sh" {
		t.Fatalf("first PreToolUse handler = %+v, want the configured command", first)
	}
	if first[0].Timeout != 10 {
		t.Errorf("timeout = %d, want 10", first[0].Timeout)
	}
	// The matcher travels onto the handler: the console renders the pair, and a
	// handler without its matcher cannot say when it runs.
	if first[0].Matcher != "Bash" {
		t.Errorf("handler matcher = %q, want the owning group's matcher", first[0].Matcher)
	}
}

func TestLoadReportsFailuresWithoutPanicking(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantSub string
		missing bool
	}{
		{name: "invalid JSON", body: "{not json", wantSub: "不是合法的 JSON"},
		{name: "empty file", body: "{}", wantSub: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := Load(writeSettings(t, tc.body))
			if tc.wantSub == "" {
				if s.Err != "" {
					t.Errorf("Err = %q, want none", s.Err)
				}
				return
			}
			if !strings.Contains(s.Err, tc.wantSub) {
				t.Errorf("Err = %q, want it to contain %q", s.Err, tc.wantSub)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		s := Load(filepath.Join(t.TempDir(), "nope.json"))
		if s.Found {
			t.Error("Found = true for a file that does not exist")
		}
		if !strings.Contains(s.Err, "找不到文件") {
			t.Errorf("Err = %q, want it to say the file is missing", s.Err)
		}
		if s.Env == nil {
			t.Error("Env must be non-nil so a caller can index it without a nil check")
		}
	})

	t.Run("no path at all", func(t *testing.T) {
		s := Load("")
		if s.Err == "" {
			t.Error("an empty path must be reported rather than silently reading nothing")
		}
	})
}

func TestLoadHonoursDisableAllHooksAndNonStringEnv(t *testing.T) {
	s := Load(writeSettings(t, `{"env":{"A":1,"B":true,"C":"x"},"disableAllHooks":true}`))
	if !s.DisableAllHooks {
		t.Error("disableAllHooks was not read")
	}
	for key, want := range map[string]string{"A": "1", "B": "true", "C": "x"} {
		if got := s.Get(key); got != want {
			t.Errorf("env %s = %q, want %q", key, got, want)
		}
	}
}

func TestModelPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		wantModel string
		wantAuth  string
		wantReady bool
		problemIn string
	}{
		{
			name:      "ANTHROPIC_MODEL wins",
			env:       map[string]string{EnvModel: "m-main", EnvDefaultSonnet: "m-sonnet", EnvAuthToken: "t"},
			wantModel: "m-main", wantAuth: llm.AuthStyleBearer, wantReady: true,
		},
		{
			// Sonnet before Opus is Claude Code's own fallback order; getting it
			// backwards would silently run conversations on the wrong tier.
			name:      "sonnet before opus",
			env:       map[string]string{EnvDefaultOpusModel: "m-opus", EnvDefaultSonnet: "m-sonnet", EnvAuthToken: "t"},
			wantModel: "m-sonnet", wantAuth: llm.AuthStyleBearer, wantReady: true,
		},
		{
			name:      "opus when no sonnet",
			env:       map[string]string{EnvDefaultOpusModel: "m-opus", EnvAuthToken: "t"},
			wantModel: "m-opus", wantAuth: llm.AuthStyleBearer, wantReady: true,
		},
		{
			name:      "haiku is the last tier",
			env:       map[string]string{EnvDefaultHaikuModel: "m-haiku", EnvAuthToken: "t"},
			wantModel: "m-haiku", wantAuth: llm.AuthStyleBearer, wantReady: true,
		},
		{
			name:      "api key uses x-api-key",
			env:       map[string]string{EnvAPIKey: "k", EnvModel: "m"},
			wantModel: "m", wantAuth: llm.AuthStyleAPIKey, wantReady: true,
		},
		{
			// The auth token is checked first, which is what Claude Code does: a
			// machine with both must send the one the gateway expects.
			name:      "auth token beats api key",
			env:       map[string]string{EnvAuthToken: "tok", EnvAPIKey: "key", EnvModel: "m"},
			wantModel: "m", wantAuth: llm.AuthStyleBearer, wantReady: true,
		},
		{
			name:      "no token",
			env:       map[string]string{EnvModel: "m"},
			wantModel: "m", wantReady: false, problemIn: EnvAuthToken,
		},
		{
			name:      "no model",
			env:       map[string]string{EnvAuthToken: "t"},
			wantModel: "", wantReady: false, problemIn: EnvModel,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			settings := Settings{Env: tc.env}
			m := settings.Model()
			if m.EffectiveModel != tc.wantModel {
				t.Errorf("EffectiveModel = %q, want %q", m.EffectiveModel, tc.wantModel)
			}
			if tc.wantAuth != "" && m.AuthStyle != tc.wantAuth {
				t.Errorf("AuthStyle = %q, want %q", m.AuthStyle, tc.wantAuth)
			}
			if m.Kind != llm.KindAnthropicMessages {
				t.Errorf("Kind = %q, want %q", m.Kind, llm.KindAnthropicMessages)
			}
			if m.Ready != tc.wantReady {
				t.Errorf("Ready = %v (problem %q), want %v", m.Ready, m.Problem, tc.wantReady)
			}
			if tc.problemIn != "" && !strings.Contains(m.Problem, tc.problemIn) {
				t.Errorf("Problem = %q, want it to name %s", m.Problem, tc.problemIn)
			}
		})
	}
}

func TestModelDefaultsTheEndpointAndFallsBackToTheSmallModelName(t *testing.T) {
	// No ANTHROPIC_BASE_URL means api.anthropic.com, the endpoint Claude Code
	// itself defaults to.
	m := Settings{Env: map[string]string{
		EnvAuthToken:      "t",
		EnvModel:          "m",
		EnvSmallFastModel: "m-small",
	}}.Model()
	if m.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", m.BaseURL, DefaultBaseURL)
	}
	// ANTHROPIC_SMALL_FAST_MODEL is the old spelling of the Haiku tier and must
	// only fill a gap, never override the new name.
	if m.HaikuModel != "m-small" {
		t.Errorf("HaikuModel = %q, want the small/fast fallback", m.HaikuModel)
	}
	if m.SubagentModel != "m-small" {
		t.Errorf("SubagentModel = %q, want it to follow the Haiku tier", m.SubagentModel)
	}

	m2 := Settings{Env: map[string]string{
		EnvAuthToken:         "t",
		EnvModel:             "m",
		EnvDefaultHaikuModel: "m-new",
		EnvSmallFastModel:    "m-old",
	}}.Model()
	if m2.HaikuModel != "m-new" {
		t.Errorf("HaikuModel = %q, want the new name to win", m2.HaikuModel)
	}
}

func TestMaskNeverRevealsAUsableSecret(t *testing.T) {
	if got := Mask(""); got != "" {
		t.Errorf("Mask(\"\") = %q, want empty", got)
	}
	short := Mask("sk-abc")
	if strings.Contains(short, "abc") || strings.Contains(short, "sk-") {
		t.Errorf("Mask of a short secret = %q, want it fully hidden", short)
	}
	long := Mask("sk-4f94bd7b345b41afb12a8b7e80d52f10")
	if strings.Contains(long, "345b41afb12a8b7e80d5") {
		t.Errorf("Mask = %q, want the middle of the secret gone", long)
	}
	if !strings.HasPrefix(long, "sk-4f9") || !strings.HasSuffix(long, "2f10") {
		t.Errorf("Mask = %q, want both ends recognisable", long)
	}
}

func TestEnvRowsOrderAndSecrecy(t *testing.T) {
	s := Load(writeSettings(t, `{"env":{
		"ANTHROPIC_AUTH_TOKEN":"sk-secret-value-here",
		"ANTHROPIC_BASE_URL":"https://x.example",
		"ZZZ_CUSTOM":"1"
	}}`))
	rows := s.EnvRows()
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0].Key != EnvBaseURL {
		t.Errorf("first row = %q, want the endpoint first", rows[0].Key)
	}
	if rows[len(rows)-1].Key != "ZZZ_CUSTOM" {
		t.Errorf("last row = %q, want the unknown key last", rows[len(rows)-1].Key)
	}
	if !rows[len(rows)-1].Used == false {
		t.Error("an unknown variable must not be reported as used")
	}
	for _, row := range rows {
		if row.Key == EnvAuthToken {
			if !row.Secret {
				t.Error("the auth token must be marked secret")
			}
			if !row.Used {
				t.Error("the auth token is used by this mode")
			}
		}
	}
	// The subagent model is deliberately displayed but not used: subagents here
	// inherit the main model, and claiming otherwise would be a lie the console
	// repeats.
	for _, known := range knownEnv {
		if known.key == EnvSubagentModel && known.spec.used {
			t.Error("CLAUDE_CODE_SUBAGENT_MODEL is not wired: it must be marked unused")
		}
	}
}

// newStoreForMode opens a real store, for the same reason the tracing tests do:
// the mode's persistence is the store's behaviour, and a fake would only test
// the fake.
func newStoreForMode(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "mode.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestServiceModePrecedenceAndPersistence(t *testing.T) {
	ctx := context.Background()
	st := newStoreForMode(t)
	path := writeSettings(t, liveShapedSettings)

	svc := NewService(ctx, Options{Config: config.ClaudeCodeConfig{SettingsPath: path}, Store: st})
	if svc.Compat() {
		t.Error("the mode must start off: config.yaml says nothing and nothing is stored")
	}
	if svc.Source() != "config" {
		t.Errorf("Source = %q, want config before the switch is touched", svc.Source())
	}

	if err := svc.SetCompat(ctx, true); err != nil {
		t.Fatalf("SetCompat(true): %v", err)
	}
	if !svc.Compat() || svc.Mode() != store.ClaudeCodeModeCompat {
		t.Error("the mode did not turn on")
	}
	if svc.Source() != "console" {
		t.Errorf("Source = %q, want console after the switch", svc.Source())
	}

	// A second service over the same store must see the stored switch: the mode
	// is a deployment fact, not a process fact.
	again := NewService(ctx, Options{Config: config.ClaudeCodeConfig{SettingsPath: path}, Store: st})
	if !again.Compat() {
		t.Error("the stored mode was not read back")
	}
	if err := again.SetCompat(ctx, false); err != nil {
		t.Fatalf("SetCompat(false): %v", err)
	}
	third := NewService(ctx, Options{Config: config.ClaudeCodeConfig{SettingsPath: path}, Store: st})
	if third.Compat() {
		t.Error("turning the mode off was not persisted")
	}
}

func TestServiceConfigEnablesTheMode(t *testing.T) {
	ctx := context.Background()
	path := writeSettings(t, liveShapedSettings)
	svc := NewService(ctx, Options{
		Config: config.ClaudeCodeConfig{Enable: true, SettingsPath: path},
		Store:  newStoreForMode(t),
	})
	if !svc.Compat() {
		t.Error("claudecode.enable = true must turn the mode on")
	}
	if _, _, ok := svc.Target(); !ok {
		t.Error("a usable settings file must yield a target")
	}
}

func TestProviderRowAndModelsFollowTheMode(t *testing.T) {
	ctx := context.Background()
	path := writeSettings(t, liveShapedSettings)

	off := NewService(ctx, Options{Config: config.ClaudeCodeConfig{SettingsPath: path}, Store: newStoreForMode(t)})
	if _, ok := off.Row(ctx); ok {
		t.Error("a mode that is off must not offer a provider")
	}
	if got := off.Models(ctx); len(got) != 0 {
		t.Errorf("a mode that is off must offer no models, got %d", len(got))
	}

	svc := NewService(ctx, Options{
		Config: config.ClaudeCodeConfig{Enable: true, SettingsPath: path},
		Store:  newStoreForMode(t),
	})
	row, ok := svc.Row(ctx)
	if !ok {
		t.Fatal("a mode that is on with a usable file must offer a provider")
	}
	if row.ID != ProviderID || row.Kind != llm.KindAnthropicMessages {
		t.Errorf("row = %+v, want id %q and the Anthropic protocol", row, ProviderID)
	}
	if !row.HasAPIKey || row.APIKeyHint == "" {
		t.Error("the row must report a key without exposing it")
	}
	if strings.Contains(row.APIKeyHint, "345b41af") {
		t.Errorf("APIKeyHint = %q, must not contain the middle of the key", row.APIKeyHint)
	}

	models := svc.Models(ctx)
	// The live-shaped file names one model in three tiers plus one distinct
	// Haiku model, so the duplicates collapse and two rows remain.
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2 after de-duplication: %+v", len(models), models)
	}
	if models[0].ModelID != "deepseek-flash[1m]" {
		t.Errorf("first model = %q, want the effective model first", models[0].ModelID)
	}
	for _, m := range models {
		if !m.Has(store.CapChat) || !m.Has(store.CapTools) {
			t.Errorf("model %q must claim chat and tools", m.ModelID)
		}
		if m.Has(store.CapVision) {
			t.Error("the env block does not claim vision, so neither may we")
		}
	}

	built, err := svc.Build(ctx, "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if built == nil {
		t.Fatal("Build returned nothing")
	}
}

func TestBuildRefusesAnUnusableConfiguration(t *testing.T) {
	ctx := context.Background()
	// No token: the mode is on, the file is there, and nothing can authenticate.
	path := writeSettings(t, `{"env":{"ANTHROPIC_MODEL":"m"}}`)
	svc := NewService(ctx, Options{
		Config: config.ClaudeCodeConfig{Enable: true, SettingsPath: path},
		Store:  newStoreForMode(t),
	})
	if _, ok := svc.Row(ctx); ok {
		t.Error("a configuration that cannot authenticate must not be offered")
	}
	if _, err := svc.Build(ctx, "m"); err == nil {
		t.Error("Build must refuse, not construct a client that cannot work")
	}
	if _, _, ok := svc.Target(); ok {
		t.Error("Target must report nothing rather than a target that fails")
	}
}

func TestHookEngineGating(t *testing.T) {
	ctx := context.Background()
	path := writeSettings(t, liveShapedSettings)
	tests := []struct {
		name       string
		compat     bool
		hooks      *bool
		wantActive bool
		wantReason string
	}{
		{name: "off", compat: false, wantActive: false, wantReason: "兼容模式未开启"},
		{name: "on", compat: true, wantActive: true},
		{name: "hooks switched off", compat: true, hooks: boolPtrTest(false), wantActive: false, wantReason: "claudecode.hooks"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(ctx, Options{
				Config: config.ClaudeCodeConfig{Enable: tc.compat, SettingsPath: path, Hooks: tc.hooks},
				Store:  newStoreForMode(t),
			})
			st := svc.Status(10)
			if st.Hooks.Enabled != tc.wantActive {
				t.Errorf("hooks Enabled = %v, want %v", st.Hooks.Enabled, tc.wantActive)
			}
			if tc.wantReason != "" && !strings.Contains(st.Hooks.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", st.Hooks.Reason, tc.wantReason)
			}
		})
	}
}

func TestStatusDescribesTheConfiguredHooks(t *testing.T) {
	ctx := context.Background()
	path := writeSettings(t, liveShapedSettings)
	svc := NewService(ctx, Options{
		Config: config.ClaudeCodeConfig{Enable: true, SettingsPath: path},
		Store:  newStoreForMode(t),
	})
	st := svc.Status(10)

	if st.Mode != store.ClaudeCodeModeCompat || !st.Compat {
		t.Errorf("Mode = %q compat = %v, want the mode reported on", st.Mode, st.Compat)
	}
	if !st.Available {
		t.Fatalf("Available = false for a usable file: %s", st.Model.Problem)
	}
	if st.Native != nil {
		t.Error("Native is the server's to fill in; this package must not guess it")
	}
	if st.SettingsPath != path || !st.SettingsFound {
		t.Errorf("settings path/found = %q/%v", st.SettingsPath, st.SettingsFound)
	}
	if st.Hooks.TotalHandlers != 7 {
		t.Errorf("TotalHandlers = %d, want 7 (two in PreToolUse, one each elsewhere, Notification included)", st.Hooks.TotalHandlers)
	}
	// Notification is configured and cannot fire here; it must be named so the
	// panel can explain why nothing happens for it.
	if len(st.Hooks.UnsupportedConfigured) != 1 || st.Hooks.UnsupportedConfigured[0] != "Notification" {
		t.Errorf("UnsupportedConfigured = %v, want [Notification]", st.Hooks.UnsupportedConfigured)
	}
	// The supported five must be present and marked, with the meaning the panel
	// renders coming from this package rather than from the UI.
	seen := map[string]bool{}
	for _, e := range st.Hooks.Events {
		seen[e.Event] = true
		if e.Meaning == "" {
			t.Errorf("event %s has no meaning line", e.Event)
		}
	}
	for _, want := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"} {
		if !seen[want] {
			t.Errorf("event %s missing from the status", want)
		}
	}
	if len(st.Notes) == 0 {
		t.Error("the limits must be stated in the payload the panel renders")
	}

	// The credential never leaves in the clear, whatever else is serialised.
	encoded, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(encoded), "sk-4f94bd7b345b41afb12a8b7e80d52f10") {
		t.Fatal("the settings status leaked the token")
	}
	if !strings.Contains(string(encoded), "sk-4f9") {
		t.Error("the masked token should still be shown, so the operator can recognise the key")
	}
}

func TestStatusMarksHandlersThisBuildCannotRun(t *testing.T) {
	body := `{"env":{"ANTHROPIC_AUTH_TOKEN":"t","ANTHROPIC_MODEL":"m"},"hooks":{
		"PreToolUse":[
			{"matcher":"Bash","hooks":[{"type":"prompt","prompt":"judge this"}]},
			{"matcher":"Read","hooks":[{"type":"command","command":"/tmp/x.sh","if":"Bash(git *)"}]},
			{"matcher":"Write","hooks":[{"type":"mcp_tool","server":"s","tool":"t"}]},
			{"matcher":"Edit","hooks":[{"type":"command","command":"/tmp/ok.sh"}]}
		]}}`
	svc := NewService(context.Background(), Options{
		Config: config.ClaudeCodeConfig{Enable: true, SettingsPath: writeSettings(t, body)},
		Store:  newStoreForMode(t),
	})
	st := svc.Status(10)
	if len(st.Hooks.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(st.Hooks.Events))
	}
	got := map[string]string{}
	for _, g := range st.Hooks.Events[0].Groups {
		for _, h := range g.Handlers {
			got[g.Matcher] = h.Unsupported
		}
	}
	if !strings.Contains(got["Bash"], "prompt") {
		t.Errorf("a prompt handler must be marked unrun, got %q", got["Bash"])
	}
	if !strings.Contains(got["Read"], "if") {
		t.Errorf("a handler with if must be marked unrun, got %q", got["Read"])
	}
	if got["Write"] != "" {
		t.Errorf("an mcp_tool handler with server+tool should run, got %q", got["Write"])
	}
	if got["Edit"] != "" {
		t.Errorf("a plain command handler must not be marked, got %q", got["Edit"])
	}
}

func TestDispatchIsANoOpWhenNothingIsOn(t *testing.T) {
	ctx := context.Background()
	path := writeSettings(t, liveShapedSettings)
	svc := NewService(ctx, Options{
		Config: config.ClaudeCodeConfig{SettingsPath: path}, // mode off
		Store:  newStoreForMode(t),
	})
	v := svc.Dispatch(ctx, hookInput("PreToolUse"))
	if v.Ran {
		t.Error("nothing may run while the mode is off")
	}
	if !v.Continue {
		t.Error("a no-op dispatch must answer Continue=true: the zero value means 'stop everything'")
	}
	if got := svc.Recent(10); len(got) != 0 {
		t.Errorf("nothing ran, so nothing may be logged: %+v", got)
	}
}

func TestDispatchRunsTheConfiguredHookAndLogsIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hook.log")
	script := filepath.Join(dir, "hook.sh")
	body := "#!/bin/sh\ncat >> " + logPath + "\nprintf '{\"systemMessage\":\"seen\"}'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	settings := `{"env":{"ANTHROPIC_AUTH_TOKEN":"t","ANTHROPIC_MODEL":"m"},"hooks":{
		"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"` + script + `","timeout":10}]}]}}`
	svc := NewService(ctx, Options{
		Config: config.ClaudeCodeConfig{Enable: true, SettingsPath: writeSettings(t, settings)},
		Store:  newStoreForMode(t),
		Dir:    dir,
	})

	v := svc.Dispatch(ctx, hookInput("PreToolUse"))
	if !v.Ran {
		t.Fatalf("the configured hook did not run: skipped=%v failures=%v", v.Skipped, v.Failures)
	}
	if len(v.SystemMessages) != 1 || v.SystemMessages[0] != "seen" {
		t.Errorf("SystemMessages = %v, want the hook's message", v.SystemMessages)
	}

	// The payload the hook received must be the protocol's: jq-shaped hooks are
	// the reason it is JSON on stdin rather than a string.
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the hook wrote nothing: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("stdin was not one JSON object: %v (%s)", err, raw)
	}
	if payload["hook_event_name"] != "PreToolUse" {
		t.Errorf("hook_event_name = %v, want PreToolUse", payload["hook_event_name"])
	}
	if payload["tool_name"] != "Bash" {
		t.Errorf("tool_name = %v, want Bash", payload["tool_name"])
	}

	records := svc.Recent(10)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Event != "PreToolUse" || records[0].Matcher != "Bash" || records[0].ExitCode != 0 {
		t.Errorf("record = %+v, want a successful Bash PreToolUse", records[0])
	}

	// A matcher that does not select this tool must not run anything.
	if v := svc.Dispatch(ctx, hookInput("PostToolUse")); v.Ran {
		t.Error("PostToolUse has no configured group here, so nothing may run")
	}
}

func TestSessionContextIsPerConversationAndForgettable(t *testing.T) {
	svc := NewService(context.Background(), Options{
		Config: config.ClaudeCodeConfig{Enable: true, SettingsPath: writeSettings(t, liveShapedSettings)},
		Store:  newStoreForMode(t),
	})
	svc.AddSessionContext("a", []string{"line one"})
	svc.AddSessionContext("a", []string{"line two"})
	svc.AddSessionContext("b", []string{"other"})

	if got := svc.SessionContext("a"); len(got) != 2 || got[0] != "line one" {
		t.Errorf("context for a = %v, want both lines in order", got)
	}
	if got := svc.SessionContext("missing"); len(got) != 0 {
		t.Errorf("context for an unknown conversation = %v, want nothing", got)
	}
	svc.ForgetSession("a")
	if got := svc.SessionContext("a"); len(got) != 0 {
		t.Errorf("after ForgetSession, context = %v", got)
	}
	if got := svc.SessionContext("b"); len(got) != 1 {
		t.Error("forgetting one conversation must not touch another")
	}
}

func TestReloadPicksUpAnEditedFile(t *testing.T) {
	ctx := context.Background()
	path := writeSettings(t, liveShapedSettings)
	svc := NewService(ctx, Options{
		Config: config.ClaudeCodeConfig{Enable: true, SettingsPath: path},
		Store:  newStoreForMode(t),
	})
	if _, _, ok := svc.Target(); !ok {
		t.Fatal("the initial file should be usable")
	}

	// The file is rewritten without a token: the mode is still on, and every
	// question it answers must now say it cannot run.
	if err := os.WriteFile(path, []byte(`{"env":{"ANTHROPIC_MODEL":"m"}}`), 0o600); err != nil {
		t.Fatalf("rewrite settings: %v", err)
	}
	svc.Reload()
	if _, _, ok := svc.Target(); ok {
		t.Error("after a reload the edited file must be what is reported")
	}
	if st := svc.Status(10); st.Available {
		t.Error("Status must report the reloaded file as unusable")
	}
}

// hookInput builds a PreToolUse/PostToolUse payload for the dispatch tests.
func hookInput(event string) claudehook.Input {
	return claudehook.Input{
		SessionID:     "sess-1",
		Cwd:           "/tmp",
		HookEventName: event,
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"ls"}`),
		ToolUseID:     "toolu-1",
	}
}

// boolPtrTest is the *bool literal the hook-switch cases need.
func boolPtrTest(v bool) *bool { return &v }
