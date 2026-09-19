package claudehook

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMatchesSection2Table walks the four rows of §2's matcher table.
//
// event is "PreToolUse" throughout because that is an event this build dispatches
// and whose matcher field is the tool name, so the values read like real tool
// names instead of abstract strings.
func TestMatchesSection2Table(t *testing.T) {
	cases := []struct {
		name    string
		matcher string
		value   string
		want    bool
		why     string
	}{
		// Row 1: "*", "" and omitted match everything.
		{"星号匹配全部", "*", "Bash", true, "§2：\"*\" 匹配全部"},
		{"星号匹配空值", "*", "", true, "§2：\"*\" 匹配全部，包括空值"},
		{"空 matcher 匹配全部", "", "Bash", true, "§2：\"\" 匹配全部"},
		{"空 matcher 匹配空值", "", "", true, "§2：\"\" 匹配全部"},

		// Row 2: only letters/digits/_/-/space/,/| -> exact match or exact list.
		{"精确匹配", "Bash", "Bash", true, "§2：只含字母数字的 matcher 是精确匹配"},
		{"精确匹配大小写敏感", "bash", "Bash", false, "§2：精确匹配区分大小写"},
		{"精确匹配不是前缀", "Bas", "Bash", false, "§2：精确匹配是整串相等"},
		{"精确匹配不是子串", "ash", "Bash", false, "§2：精确匹配不是「包含」"},
		{"竖线列表命中第一项", "Edit|Write", "Edit", true, "§2：\"|\" 分隔的精确列表"},
		{"竖线列表命中第二项", "Edit|Write", "Write", true, "§2：\"|\" 分隔的精确列表"},
		{"竖线列表不命中", "Edit|Write", "Read", false, "§2：列表里没有的值不匹配"},
		{"逗号列表命中", "Edit, Write", "Write", true, "§2：\",\" 分隔，分隔符周围空格容错"},
		{"逗号列表无空格", "Edit,Write", "Edit", true, "§2：\",\" 分隔"},
		{"连字符是精确匹配字符", "mcp-tool", "mcp-tool", true, "§2：\"-\" 属于精确匹配字符集"},
		{"空格是精确匹配字符", "my tool", "my tool", true, "§2：空格属于精确匹配字符集"},
		{"下划线是精确匹配字符", "my_tool", "my_tool", true, "§2：\"_\" 属于精确匹配字符集"},
		{"列表命中首项", "Bash,Edit,Bash", "Bash", true, "§2：列表可含重复项"},

		// Row 3: anything else is an unanchored JavaScript-style regexp.
		{"正则锚定开头", "^Notebook", "NotebookEdit", true, "§2：\"^Notebook\" 例"},
		{"正则锚定开头不命中", "^Notebook", "ReadNotebook", false, "§2：\"^\" 锚定开头"},
		{"正则任意位置匹配", "Edit.*", "NotebookEdit", true, "§2 坑：不锚定，Edit.* 会命中 NotebookEdit"},
		{"正则任意位置匹配前置", "Edit.*", "Edit", true, "§2 坑：Edit.* 也命中 Edit"},
		{"正则锚定整串", "^Edit$", "Edit", true, "§2：整串匹配必须写 ^Edit$"},
		{"正则锚定整串不命中", "^Edit$", "NotebookEdit", false, "§2 坑：要整串匹配必须写 ^Edit$"},
		{"MCP 工具名正则", "mcp__memory__.*", "mcp__memory__read", true, "§2：mcp__memory__.* 例"},
		{"MCP 工具名正则不命中", "mcp__memory__.*", "mcp__files__read", false, "§2：前缀不符"},
		{"点号匹配任意字符", "a.c", "abc", true, "§2：正则元字符生效"},
		{"点号匹配任何单字符", "a.c", "abc", true, "§2：\".\" 在正则里匹配任意单字符"},
		{"点号匹配连字符", "a.c", "a-c", true, "§2：\".\" 也匹配连字符"},
		{"点号不匹配两字符", "a.c", "a--c", false, "§2：\".\" 只匹配一个字符"},
		{"正则字符类", "[BH]ash", "Hash", true, "§2：字符类走正则路径"},
		{"无效正则不匹配", "([", "anything", false, "保守读取：无法编译的正则不能选中任何值（见 Matches 注释）"},

		// Deliberately not a match: the tool name is compared as a whole value.
		{"正则不匹配空值", "^Bash$", "", false, "§2：正则对空值求值"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Matches("PreToolUse", tc.matcher, tc.value)
			if got != tc.want {
				t.Errorf("Matches(PreToolUse, %q, %q) = %v，期望 %v（%s）",
					tc.matcher, tc.value, got, tc.want, tc.why)
			}
		})
	}
}

// TestMatchesFileChangedNarrowCharset covers §2's narrower exact-match charset:
// FileChanged and StopFailure accept only letters, digits, "_" and "|", so a
// hyphen, space or comma pushes the matcher onto the regexp path. The observable
// consequence is that "a,b" is no longer a two-value list in those events.
func TestMatchesFileChangedNarrowCharset(t *testing.T) {
	cases := []struct {
		name    string
		matcher string
		value   string
		want    bool
	}{
		// Only "|" separates values here.
		{"竖线仍分隔", ".envrc|.env", ".env", true},
		{"竖线另一项", ".envrc|.env", ".envrc", true},
		{"竖线不含的值", ".envrc|.env", "other", false},

		// Letters, digits and "_" stay on the exact path.
		{"字母数字下划线精确匹配", "my_file2", "my_file2", true},
		{"精确匹配不命中子串", "my_file2", "my_file2x", false},

		// A comma is NOT a separator in the narrow charset: the matcher becomes a
		// regexp, and a comma is a literal in a regexp, so it matches only a value
		// that literally contains the comma.
		{"逗号不是分隔符", ".env,.envrc", ".env", false},
		{"逗号作为字面量", ".env,.envrc", ".env,.envrc", true},

		// Same for the hyphen: it is not in the narrow charset, so "a-b" is a
		// regexp. As a regexp it happens to match the literal "a-b", but the code
		// path taken is the regexp one — which is exactly what §2 warns about, and
		// what this case documents.
		{"连字符走正则路径", "a-b", "a-b", true},
		{"连字符正则可命中更长串", "a-b", "xa-by", true},

		// The compare against PreToolUse proves the charset really differs: there
		// ",", "-" and " " split a list, here they do not.
		{"窄字符集下空格逗号不分隔", "a, b", "b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Matches("FileChanged", tc.matcher, tc.value)
			if got != tc.want {
				t.Errorf("Matches(FileChanged, %q, %q) = %v，期望 %v",
					tc.matcher, tc.value, got, tc.want)
			}
		})
	}

	// The control: the identical matcher and value against an event with the wide
	// charset must behave as a list, which is what proves the narrow charset is
	// what changed the answer.
	if !Matches("PreToolUse", "a, b", "b") {
		t.Error("对照：PreToolUse 下 \"a, b\" 应作为逗号列表命中 \"b\"")
	}
	if !Matches("PreToolUse", "mcp-tool", "mcp-tool") {
		t.Error("对照：PreToolUse 下连字符属于精确匹配字符集")
	}
}

// TestMatchesStopFailureNarrowCharset pins that StopFailure shares the narrow
// charset with FileChanged (§2).
func TestMatchesStopFailureNarrowCharset(t *testing.T) {
	// "rate_limit" is all narrow-charset characters, so it is an exact match.
	if !Matches("StopFailure", "rate_limit", "rate_limit") {
		t.Error("StopFailure 的 rate_limit 应精确匹配")
	}
	if Matches("StopFailure", "rate_limit", "rate_limit_exceeded") {
		t.Error("StopFailure 的精确匹配不应命中更长的值")
	}
	// The documented values include "oauth_org_not_allowed"; a value containing
	// only narrow characters must still be an exact match rather than a regexp.
	if !Matches("StopFailure", "unknown|billing_error", "billing_error") {
		t.Error("StopFailure 的竖线列表应命中")
	}
}

// TestMatchesEventsWithoutMatcher covers §2.1's "不支持 matcher" list: those events
// fire on every trigger, so a configured matcher must be ignored rather than used
// to filter them out. Getting this backwards would silently disable the two most
// commonly configured events of the live settings file.
func TestMatchesEventsWithoutMatcher(t *testing.T) {
	for _, event := range []string{"UserPromptSubmit", "Stop", "PostToolBatch", "MessageDisplay"} {
		for _, matcher := range []string{"", "*", "Bash", "^Bash$", "nothing-matches-this"} {
			if !Matches(event, matcher, "") {
				t.Errorf("Matches(%q, %q, \"\") = false，该事件不支持 matcher，应始终匹配（§2.1）",
					event, matcher)
			}
		}
	}
}

// TestMatchesSessionStartSource covers §2.1: SessionStart's matcher filters on the
// session start source, so the pipe list from the live settings file selects the
// five documented sources and nothing else.
func TestMatchesSessionStartSource(t *testing.T) {
	const matcher = "startup|resume|clear|compact|fork"
	for _, source := range []string{"startup", "resume", "clear", "compact", "fork"} {
		if !Matches("SessionStart", matcher, source) {
			t.Errorf("Matches(SessionStart, %q, %q) = false", matcher, source)
		}
	}
	for _, source := range []string{"", "forked", "start"} {
		if Matches("SessionStart", matcher, source) {
			t.Errorf("Matches(SessionStart, %q, %q) = true，应为 false", matcher, source)
		}
	}
}

// TestMatchesUnknownEvent pins the conservative reading for an event name this
// build does not know: it matches, and the matcher is not used to filter it out.
//
// The alternative readings are worse in different ways — refusing to match hides a
// handler the operator configured, and evaluating a matcher against an empty value
// would reject exact matchers for reasons the operator cannot see. The comment on
// SpecFor records this choice.
func TestMatchesUnknownEvent(t *testing.T) {
	if !Matches("SomeFutureEvent", "Bash", "") {
		t.Error("未知事件应保守地视为匹配（SpecFor 注释）")
	}
	if SpecFor("SomeFutureEvent").Blockable {
		t.Error("未知事件不应被视为可阻塞（§5.3 只列了明确的事件）")
	}
}

// TestMatchValue covers §2.1's field selection for the five dispatched events: the
// value reported in the log is the one the matcher was tested against.
func TestMatchValue(t *testing.T) {
	in := Input{
		ToolName:  "Bash",
		Source:    "startup",
		AgentType: "Explore",
		Model:     "some-model",
		Extra: map[string]any{
			"reason":       "clear",
			"command_name": "/deploy",
			// to_model is what Pre/PostModelSwitch match on; §2.1 says the value is
			// the canonical name derived from it.
			"to_model": "some-model",
		},
	}
	cases := []struct {
		event string
		want  string
		has   bool
	}{
		{"PreToolUse", "Bash", true},
		{"PostToolUse", "Bash", true},
		{"SessionStart", "startup", true},
		{"SessionEnd", "clear", true},
		{"UserPromptExpansion", "/deploy", true},
		{"SubagentStart", "Explore", true},
		{"PreModelSwitch", "some-model", true},
		// Events without matcher support report no value at all, which is what
		// lets the log show an empty matcher column honestly.
		{"UserPromptSubmit", "", false},
		{"Stop", "", false},
		// A field this build's Input does not model and the payload does not carry
		// resolves to empty rather than a guess.
		{"Notification", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			got, has := matchValue(SpecFor(tc.event), in)
			if got != tc.want || has != tc.has {
				t.Errorf("matchValue(%q) = (%q, %v)，期望 (%q, %v)",
					tc.event, got, has, tc.want, tc.has)
			}
		})
	}
}

// TestMatchesNonStringExtraValue covers the fallback that reads a matcher field out
// of Extra: a non-string value cannot be matched against, and must resolve to the
// empty string rather than to Go's rendering of the value ("%!s(int=5)" and
// friends, which would then be compared as if it were a tool name).
func TestMatchesNonStringExtraValue(t *testing.T) {
	in := Input{Extra: map[string]any{"reason": 5}}
	if got := payloadField(in, "reason"); got != "" {
		t.Errorf("payloadField(非字符串) = %q，期望空串", got)
	}
}

// TestSplitExactMatcher covers the list splitting rules, including the empty
// entries an operator leaves when they write a trailing separator.
func TestSplitExactMatcher(t *testing.T) {
	cases := map[string][]string{
		"Bash":        {"Bash"},
		"Edit|Write":  {"Edit", "Write"},
		"Edit, Write": {"Edit", "Write"},
		"Edit ,Write": {"Edit", "Write"},
		"Edit||Write": {"Edit", "Write"},
		"Edit|":       {"Edit"},
		"|Edit":       {"Edit"},
		"  Edit  ":    {"Edit"},
	}
	for matcher, want := range cases {
		got := splitExactMatcher(matcher)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("splitExactMatcher(%q) = %#v，期望 %#v", matcher, got, want)
		}
	}
}

// TestMatcherValueComesFromPayload is the end-to-end check that the value in a log
// record is the payload field §2.1 names — not, say, the session id.
func TestMatcherValueComesFromPayload(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	engine := NewEngine(cfg, helper.options(t))
	verdict := engine.Dispatch(t.Context(), Input{
		SessionID:     "sess-1",
		HookEventName: "PreToolUse",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"echo hello"}`),
	})
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d，期望 1（skipped=%v failures=%v）",
			verdict.Handlers, verdict.Skipped, verdict.Failures)
	}
	records := engine.Recent(10)
	if len(records) != 1 {
		t.Fatalf("Recent 返回 %d 条，期望 1", len(records))
	}
	if records[0].Value != "Bash" {
		t.Errorf("Record.Value = %q，期望 %q（§2.1：PreToolUse 比对 tool_name）", records[0].Value, "Bash")
	}
	if records[0].Matcher != "Bash" {
		t.Errorf("Record.Matcher = %q，期望 %q", records[0].Matcher, "Bash")
	}
	if records[0].ToolName != "Bash" || records[0].SessionID != "sess-1" {
		t.Errorf("Record 缺少工具名/会话号：%+v", records[0])
	}
	if records[0].Handler != HandlerCommand {
		t.Errorf("Record.Handler = %q，期望 %q", records[0].Handler, HandlerCommand)
	}
}
