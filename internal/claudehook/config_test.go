package claudehook

import (
	"encoding/json"
	"strings"
	"testing"
)

// realSettingsHooks is the "hooks" value of ~/.claude/settings.json on the
// machine this package was written for (Claude Code v2.1.278, the live config the
// reference document's Appendix B describes). It is pasted here rather than read
// from the user's home directory: a test that read $HOME would pass or fail
// depending on whose machine ran it, and would silently stop covering the shape it
// exists to cover the moment the operator edited their config.
//
// It exercises the parts that matter for parsing: all five dispatched events, a
// pipe-separated matcher, a "*" matcher, exec form with args (no shell), a numeric
// timeout, and an args element containing Chinese text and parentheses.
const realSettingsHooks = `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear|compact|fork",
        "hooks": [
          {
            "type": "command",
            "command": "/Users/huan/.claude/hooks/log-hook.sh",
            "args": ["matcher=startup|resume|clear|compact|fork"],
            "timeout": 10
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "/Users/huan/.claude/hooks/log-hook.sh",
            "args": ["matcher=*（该事件不支持 matcher）"],
            "timeout": 10
          }
        ]
      }
    ],
    "PreToolUse": [
      { "matcher": "Bash", "hooks": [
        { "type": "command", "command": "/Users/huan/.claude/hooks/log-hook.sh",
          "args": ["matcher=Bash"], "timeout": 10 } ] },
      { "matcher": "Edit|Write", "hooks": [
        { "type": "command", "command": "/Users/huan/.claude/hooks/log-hook.sh",
          "args": ["matcher=Edit|Write"], "timeout": 10 } ] },
      { "matcher": "*", "hooks": [
        { "type": "command", "command": "/Users/huan/.claude/hooks/log-hook.sh",
          "args": ["matcher=*（兜底）"], "timeout": 10 } ] }
    ],
    "PostToolUse": [
      { "matcher": "Bash", "hooks": [
        { "type": "command", "command": "/Users/huan/.claude/hooks/log-hook.sh",
          "args": ["matcher=Bash"], "timeout": 10 } ] },
      { "matcher": "Edit|Write", "hooks": [
        { "type": "command", "command": "/Users/huan/.claude/hooks/log-hook.sh",
          "args": ["matcher=Edit|Write"], "timeout": 10 } ] },
      { "matcher": "*", "hooks": [
        { "type": "command", "command": "/Users/huan/.claude/hooks/log-hook.sh",
          "args": ["matcher=*（兜底）"], "timeout": 10 } ] }
    ],
    "Stop": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "/Users/huan/.claude/hooks/log-hook.sh",
            "args": ["matcher=*（该事件不支持 matcher）"],
            "timeout": 10
          }
        ]
      }
    ]
  }
}`

func TestParseConfigRealSettings(t *testing.T) {
	cfg, err := ParseConfig([]byte(realSettingsHooks))
	if err != nil {
		t.Fatalf("真实配置解析失败：%v", err)
	}

	wantOrder := []string{"PostToolUse", "PreToolUse", "SessionStart", "Stop", "UserPromptSubmit"}
	if len(cfg.EventOrder) != len(wantOrder) {
		t.Fatalf("EventOrder = %v，期望 %v", cfg.EventOrder, wantOrder)
	}
	for i, want := range wantOrder {
		if cfg.EventOrder[i] != want {
			t.Errorf("EventOrder[%d] = %q，期望 %q", i, cfg.EventOrder[i], want)
		}
	}
	if cfg.DisableAll {
		t.Error("DisableAll 应为 false")
	}

	// The three PreToolUse groups must survive in configured order: order is what
	// decides the order of skip/failure lines in a Verdict.
	pre := cfg.Groups["PreToolUse"]
	if len(pre) != 3 {
		t.Fatalf("PreToolUse 有 %d 个 matcher 组，期望 3", len(pre))
	}
	for i, want := range []string{"Bash", "Edit|Write", "*"} {
		if pre[i].Matcher != want {
			t.Errorf("PreToolUse 第 %d 组 matcher = %q，期望 %q", i, pre[i].Matcher, want)
		}
	}

	// The matcher is denormalised onto each handler, which is what dispatch reads.
	h := pre[1].Handlers[0]
	if h.Matcher != "Edit|Write" {
		t.Errorf("handler.Matcher = %q，期望 %q", h.Matcher, "Edit|Write")
	}
	if h.Type != HandlerCommand {
		t.Errorf("handler.Type = %q，期望 %q", h.Type, HandlerCommand)
	}
	if h.Timeout != 10 {
		t.Errorf("handler.Timeout = %d，期望 10", h.Timeout)
	}
	// Exec form: Args is non-nil, which is what selects spawning over sh -c.
	if h.Args == nil {
		t.Fatal("handler.Args 应为非 nil（exec form）")
	}
	if len(h.Args) != 1 || h.Args[0] != "matcher=Edit|Write" {
		t.Errorf("handler.Args = %#v", h.Args)
	}

	// The Chinese text in the UserPromptSubmit args must round-trip; a parser that
	// mangled it would corrupt the logger hook's matcher label.
	prompt := cfg.Groups["UserPromptSubmit"][0].Handlers[0]
	if prompt.Args[0] != "matcher=*（该事件不支持 matcher）" {
		t.Errorf("UserPromptSubmit args = %q", prompt.Args[0])
	}

	// All five events of the live config are dispatched by this build.
	for _, event := range cfg.EventOrder {
		if !IsSupported(event) {
			t.Errorf("IsSupported(%q) = false，期望 true", event)
		}
	}
}

func TestParseConfigEmpty(t *testing.T) {
	for name, raw := range map[string]string{
		"nil":    "",
		"null":   "null",
		"spaces": "   ",
		"empty":  "{}",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfig(json.RawMessage(raw))
			if err != nil {
				t.Fatalf("ParseConfig(%q) 返回错误：%v", raw, err)
			}
			if len(cfg.Groups) != 0 || len(cfg.EventOrder) != 0 {
				t.Errorf("空配置应产生空 Config，得到 %+v", cfg)
			}
			if cfg.DisableAll {
				t.Error("DisableAll 应为 false")
			}
			if cfg.Groups == nil {
				t.Error("Groups 应为非 nil map，方便调用方直接写入")
			}
		})
	}
}

func TestParseConfigDisableAllHooks(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"disableAllHooks": true, "hooks": {"Stop": [
		{"hooks": [{"type": "command", "command": "/bin/echo"}]}]}}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !cfg.DisableAll {
		t.Error("DisableAll 应为 true")
	}
	// §1.3: disableAllHooks keeps the configuration; it only stops it running.
	if len(cfg.Groups["Stop"]) != 1 {
		t.Error("disableAllHooks 不应丢弃配置")
	}
}

// TestParseConfigBrokenEntryKeepsTheRest covers the contract that a single broken
// handler does not hide the other events — the /hooks menu shows what it could
// read rather than refusing to display anything (§1.3).
func TestParseConfigBrokenEntryKeepsTheRest(t *testing.T) {
	raw := `{"hooks": {
		"Stop": [
			{"hooks": [{"type": "command", "command": "/bin/true"}]},
			{"hooks": [{"type": "command", "command": "/bin/true", "timeout": "十秒"}]}
		],
		"PreToolUse": [
			{"matcher": "Bash", "hooks": [{"type": "command", "command": "/bin/true"}]}
		],
		"SessionStart": [
			{"hooks": [{"type": "command", "command": "/bin/true", "timeout": -5}]}
		],
		"UserPromptSubmit": [
			{"hooks": [{"type": "command", "command": "/bin/true", "async": "yes"}]}
		]
	}}`

	cfg, err := ParseConfig([]byte(raw))
	if err == nil {
		t.Fatal("期望返回错误，得到 nil")
	}
	msg := err.Error()
	for _, want := range []string{"timeout", "async"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应提到 %q：%s", want, msg)
		}
	}

	// The good parts must still be there.
	if len(cfg.Groups["Stop"]) != 1 {
		t.Errorf("Stop 应保留 1 个可用组，得到 %d", len(cfg.Groups["Stop"]))
	}
	if len(cfg.Groups["PreToolUse"]) != 1 {
		t.Errorf("PreToolUse 应保留 1 个组，得到 %d", len(cfg.Groups["PreToolUse"]))
	}
	// The broken events keep whatever groups did parse: none here, since each
	// event's only group was the broken one.
	if len(cfg.Groups["SessionStart"]) != 0 {
		t.Errorf("SessionStart 的组全部损坏，应无组，得到 %d", len(cfg.Groups["SessionStart"]))
	}
}

func TestParseConfigMalformedShapes(t *testing.T) {
	cases := map[string]string{
		"hooks 不是对象":          `{"hooks": []}`,
		"事件值不是数组":             `{"hooks": {"Stop": {"hooks": []}}}`,
		"事件值是字符串":             `{"hooks": {"Stop": "nope"}}`,
		"matcher 不是字符串":       `{"hooks": {"Stop": [{"matcher": 1, "hooks": []}]}}`,
		"handler 缺少 type":     `{"hooks": {"Stop": [{"hooks": [{"command": "/bin/true"}]}]}}`,
		"handler 不是对象":        `{"hooks": {"Stop": [{"hooks": ["nope"]}]}}`,
		"if 不是字符串":            `{"hooks": {"Stop": [{"hooks": [{"type":"command","command":"/bin/true","if":5}]}]}}`,
		"不是合法 JSON":           `{"hooks": `,
		"handler timeout 为负数": `{"hooks": {"Stop": [{"hooks": [{"type":"command","command":"/bin/true","timeout":-1}]}]}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(raw)); err == nil {
				t.Errorf("ParseConfig(%s) 期望错误，得到 nil", raw)
			}
		})
	}
}

// TestParseConfigHandlerTypeIsNotValidatedAtParseTime pins the division of labour:
// an unknown handler type is carried through parsing (a newer Claude Code may
// understand it, and the operator must still see it) and is refused at dispatch
// time with a reason.
func TestParseConfigHandlerTypeIsNotValidatedAtParseTime(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"hooks": {"PostToolUse": [
		{"matcher": "Bash", "hooks": [{"type": "hologram", "command": "/bin/true"}]}]}}`))
	if err != nil {
		t.Fatalf("未知 handler 类型不应在解析阶段报错：%v", err)
	}
	got := cfg.Groups["PostToolUse"][0].Handlers[0].Type
	if got != "hologram" {
		t.Errorf("Type = %q，期望原样保留 %q", got, "hologram")
	}
}

// TestParseConfigAsyncRewakeAlias covers both spellings the wire format may use:
// the documented "asyncRewake" and the "async_rewake" spelling of this struct's
// JSON tag.
func TestParseConfigAsyncRewakeAlias(t *testing.T) {
	for name, raw := range map[string]string{
		"asyncRewake": `{"hooks": {"PostToolUse": [{"matcher": "Bash", "hooks": [
			{"type": "command", "command": "/bin/true", "asyncRewake": true}]}]}}`,
		"async_rewake": `{"hooks": {"PostToolUse": [{"matcher": "Bash", "hooks": [
			{"type": "command", "command": "/bin/true", "async_rewake": true}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := configFromJSON(t, raw)
			if !cfg.Groups["PostToolUse"][0].Handlers[0].AsyncRewake {
				t.Errorf("%s 未被解析为 AsyncRewake", name)
			}
		})
	}
}

// TestParseConfigAllHandlerFields is the "every field of §3 survives the parser"
// test: a field the parser drops is a field that silently does nothing.
func TestParseConfigAllHandlerFields(t *testing.T) {
	raw := `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [
		{"type": "http", "url": "https://example.test/hook",
		 "headers": {"Authorization": "Bearer $TOKEN", "X-Static": "v"},
		 "allowedEnvVars": ["TOKEN"],
		 "timeout": 7, "async": false, "shell": "bash",
		 "if": "Bash(git *)", "statusMessage": "正在检查", "once": true}]}]}}`
	cfg := configFromJSON(t, raw)
	h := cfg.Groups["PreToolUse"][0].Handlers[0]

	if h.Type != HandlerHTTP || h.URL != "https://example.test/hook" {
		t.Errorf("type/url = %q/%q", h.Type, h.URL)
	}
	if h.Headers["Authorization"] != "Bearer $TOKEN" || h.Headers["X-Static"] != "v" {
		t.Errorf("headers = %#v", h.Headers)
	}
	if len(h.AllowedEnvVars) != 1 || h.AllowedEnvVars[0] != "TOKEN" {
		t.Errorf("allowedEnvVars = %#v", h.AllowedEnvVars)
	}
	if h.Timeout != 7 {
		t.Errorf("timeout = %d", h.Timeout)
	}
	if h.If != "Bash(git *)" {
		t.Errorf("if = %q", h.If)
	}
	if h.StatusMessage != "正在检查" {
		t.Errorf("statusMessage = %q", h.StatusMessage)
	}
	if !h.Once {
		t.Error("once 应为 true")
	}
}

func TestParseConfigMCPToolAndPromptFields(t *testing.T) {
	cfg := configFromJSON(t, `{"hooks": {"PostToolUse": [{"matcher": "Bash", "hooks": [
		{"type": "mcp_tool", "server": "plugin:demo:fs", "tool": "read_file",
		 "input": {"path": "${tool_input.file_path}", "mode": "ro"}},
		{"type": "prompt", "prompt": "Evaluate $ARGUMENTS", "model": "fast"}]}]}}`)
	handlers := cfg.Groups["PostToolUse"][0].Handlers

	mcp := handlers[0]
	if mcp.Server != "plugin:demo:fs" || mcp.Tool != "read_file" {
		t.Errorf("server/tool = %q/%q", mcp.Server, mcp.Tool)
	}
	if mcp.Input["path"] != "${tool_input.file_path}" || mcp.Input["mode"] != "ro" {
		t.Errorf("input = %#v", mcp.Input)
	}
	if handlers[1].Prompt != "Evaluate $ARGUMENTS" || handlers[1].Model != "fast" {
		t.Errorf("prompt/model = %q/%q", handlers[1].Prompt, handlers[1].Model)
	}
}

// TestParseConfigGroupWithoutHandlers covers the empty matcher group §2's example
// shows ("hooks": []): it selects nothing and must not be an error.
func TestParseConfigGroupWithoutHandlers(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"hooks": {"Stop": [{"matcher": "*", "hooks": []}]}}`))
	if err != nil {
		t.Fatalf("空 handler 数组不应报错：%v", err)
	}
	if len(cfg.Groups["Stop"]) != 0 {
		t.Errorf("无 handler 的组不应被保留，得到 %d 个", len(cfg.Groups["Stop"]))
	}
}

// TestParseConfigSnakeCaseKeys covers the frozen API's own JSON tags. They are not
// the wire format (Claude Code uses camelCase), but a caller that round-trips a
// Handler must get its fields back, so both spellings parse.
func TestParseConfigSnakeCaseKeys(t *testing.T) {
	cfg := configFromJSON(t, `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [
		{"type": "http", "url": "https://example.test", "allowed_env_vars": ["A","B"],
		 "status_message": "运行中", "async_rewake": true}]}]}}`)
	h := cfg.Groups["PreToolUse"][0].Handlers[0]
	if len(h.AllowedEnvVars) != 2 || h.AllowedEnvVars[0] != "A" {
		t.Errorf("allowed_env_vars = %#v", h.AllowedEnvVars)
	}
	if h.StatusMessage != "运行中" {
		t.Errorf("status_message = %q", h.StatusMessage)
	}
	if !h.AsyncRewake {
		t.Error("async_rewake 未被解析")
	}
}

// TestHandlerRoundTripsThroughJSON pins the frozen API's external form: what a
// caller marshals is what the parser reads back.
func TestHandlerRoundTripsThroughJSON(t *testing.T) {
	original := Handler{
		Type:           HandlerHTTP,
		URL:            "https://example.test/hook",
		Headers:        map[string]string{"Authorization": "Bearer $TOKEN"},
		AllowedEnvVars: []string{"TOKEN"},
		Timeout:        9,
		Async:          true,
		AsyncRewake:    true,
		StatusMessage:  "检查中",
		Once:           true,
		If:             "Bash(git *)",
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var back Handler
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if back.AllowedEnvVars == nil || back.AllowedEnvVars[0] != "TOKEN" {
		t.Errorf("AllowedEnvVars 未往返：%#v（JSON：%s）", back.AllowedEnvVars, data)
	}
	if back.StatusMessage != "检查中" {
		t.Errorf("StatusMessage 未往返：%q（JSON：%s）", back.StatusMessage, data)
	}
	if !back.AsyncRewake || !back.Async || !back.Once {
		t.Errorf("布尔字段未往返：%+v（JSON：%s）", back, data)
	}
	if back.Headers["Authorization"] != "Bearer $TOKEN" || back.Timeout != 9 || back.If != "Bash(git *)" {
		t.Errorf("字段未往返：%+v", back)
	}
}

func TestIsSupported(t *testing.T) {
	for _, event := range SupportedEvents {
		if !IsSupported(event) {
			t.Errorf("IsSupported(%q) = false", event)
		}
	}
	if len(SupportedEvents) != 5 {
		t.Errorf("SupportedEvents 应有 5 个事件，得到 %d：%v", len(SupportedEvents), SupportedEvents)
	}
	// Recognised by the protocol, deliberately not dispatched by this build.
	for _, event := range []string{"SessionEnd", "Notification", "PreCompact", "SubagentStop"} {
		if IsSupported(event) {
			t.Errorf("IsSupported(%q) = true，本版本不应派发该事件", event)
		}
	}
	if IsSupported("") {
		t.Error("IsSupported(\"\") = true")
	}
}

func TestSpecForBlockable(t *testing.T) {
	// §5.3: exactly these five events of this build's five can block on exit 2.
	for event, want := range map[string]bool{
		"PreToolUse":       true,
		"UserPromptSubmit": true,
		"Stop":             true,
		"PostToolUse":      false,
		"SessionStart":     false,
	} {
		if got := SpecFor(event).Blockable; got != want {
			t.Errorf("SpecFor(%q).Blockable = %v，期望 %v", event, got, want)
		}
	}
}

func TestSpecForMatchFields(t *testing.T) {
	// §2.1: the four events of this build, plus the two narrow-charset ones.
	for event, want := range map[string]string{
		"PreToolUse":       "tool_name",
		"PostToolUse":      "tool_name",
		"SessionStart":     "source",
		"UserPromptSubmit": "",
		"Stop":             "",
		"FileChanged":      "file_name",
		"StopFailure":      "error",
	} {
		if got := SpecFor(event).MatchField; got != want {
			t.Errorf("SpecFor(%q).MatchField = %q，期望 %q", event, got, want)
		}
	}
}
