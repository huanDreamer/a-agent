package claudehook

import (
	"encoding/json"
	"testing"
)

// placeholderPayload is a representative payload for the substitution tests: the
// fields §3's examples reference.
func placeholderPayload() Input {
	return Input{
		SessionID:     "sess-1",
		Cwd:           "/private/tmp/capture",
		HookEventName: "PostToolUse",
		ToolName:      "Write",
		ToolInput:     json.RawMessage(`{"file_path":"/tmp/a.txt","content":"x"}`),
		ToolUseID:     "call_1",
		Prompt:        "写一个文件",
		Source:        "startup",
	}
}

// TestSubstituteVarsBracedOnly pins the split that matters most for a command
// handler: only ${NAME} is substituted, because a shell-form command's own "$NAME"
// must survive the expansion untouched (§3 documents "${...}" for command and
// mcp_tool expansion).
func TestSubstituteVarsBracedOnly(t *testing.T) {
	in := placeholderPayload()
	vars := map[string]string{"CLAUDE_PROJECT_DIR": "/proj", "CLAUDE_PLUGIN_ROOT": "/plug"}

	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"payload 路径", "${tool_input.file_path}", "/tmp/a.txt"},
		{"payload 顶层字段", "${session_id}", "sess-1"},
		{"额外变量", "${CLAUDE_PROJECT_DIR}/x.sh", "/proj/x.sh"},
		{"插件根目录", "${CLAUDE_PLUGIN_ROOT}/s.js", "/plug/s.js"},
		{"中文 payload 字段", "${cwd}", "/private/tmp/capture"},

		// A bare reference is NOT expanded: this is what keeps a shell command's
		// own variables working.
		{"裸变量不替换", "$CLAUDE_PROJECT_DIR/x.sh", "$CLAUDE_PROJECT_DIR/x.sh"},
		{"shell 变量不被吃掉", "X=hello; echo $X", "X=hello; echo $X"},

		// A reference with no source becomes the empty string.
		{"未知名替换为空串", "${NOPE}", ""},
		{"缺失 payload 字段替换为空串", "${tool_input.nope}", ""},
		{"未知变量后仍有字面量", "a${NOPE}b", "ab"},
		// A path that exists but is not a string leaf is not rendered as JSON.
		{"对象叶子替换为空串", "${tool_input}", ""},

		// Non-references are content.
		{"没有美元符号", "/bin/echo hi", "/bin/echo hi"},
		{"美元符号后不是名字", "cost is $5", "cost is $5"},
		{"只有美元符号", "$", "$"},
		{"未闭合花括号", "${unclosed", "${unclosed"},

		// A substitution result is never rescanned, so a payload value that itself
		// looks like a reference cannot be expanded a second time.
		{"结果不再二次替换", "${session_id}", "sess-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := substituteVars(tc.value, in, vars); got != tc.want {
				t.Errorf("substituteVars(%q) = %q，期望 %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestSubstituteVarsNoRescan proves the no-rescan property directly, by making a
// payload field whose value is itself a reference.
func TestSubstituteVarsNoRescan(t *testing.T) {
	in := placeholderPayload()
	in.Prompt = "${session_id}"
	got := substituteVars("${prompt}", in, map[string]string{"session_id": "SHOULD-NOT-APPEAR"})
	// The prompt's value is the literal text "${session_id}", because the payload
	// supplies it and the substitution pass already moved past that position.
	if got != "${session_id}" {
		t.Errorf("substituteVars 二次展开 = %q，期望字面量 ${session_id}", got)
	}
}

// TestSubstituteAnyVarBareForm covers the header-only bare form (§3): "$VAR_NAME"
// and "${VAR_NAME}" both resolve from the allowed table, and an unlisted name
// becomes the empty string.
func TestSubstituteAnyVarBareForm(t *testing.T) {
	vars := map[string]string{"TOKEN": "s3cr3t", "EMPTY": ""}
	cases := map[string]string{
		"Bearer $TOKEN":   "Bearer s3cr3t",
		"Bearer ${TOKEN}": "Bearer s3cr3t",
		"$TOKEN:$TOKEN":   "s3cr3t:s3cr3t",
		"v=$UNLISTED":     "v=",
		"v=${UNLISTED}":   "v=",
		"v=$EMPTY":        "v=",
		"no vars here":    "no vars here",
		"$5 and $ and $1": "$5 and $ and $1",
		// A bare name ends where the name charset ends, so ".suffix" is literal
		// text appended to the expanded value.
		"v=$TOKEN.suffix": "v=s3cr3t.suffix",
	}
	for value, want := range cases {
		if got := substituteAnyVar(value, vars); got != want {
			t.Errorf("substituteAnyVar(%q) = %q，期望 %q", value, got, want)
		}
	}
}

// TestSubstituteAnyVarIgnoresPayload covers the deliberate restriction of header
// interpolation: it reads the allowed environment table only, so a header cannot
// pull a value out of the hook payload.
func TestSubstituteAnyVarIgnoresPayload(t *testing.T) {
	// substituteAnyVar takes no Input at all, which is the compile-time half of the
	// guarantee; this asserts the runtime behaviour a caller would see.
	// A payload-shaped name is simply not in the table, so it resolves to empty.
	if got := substituteAnyVar("$session_id", map[string]string{"TOKEN": "x"}); got != "" {
		t.Errorf("substituteAnyVar($session_id) = %q，期望空串（不应读取 payload）", got)
	}
}

// TestFlattenInputVars covers the mcp_tool input map being substituted key by key.
func TestFlattenInputVars(t *testing.T) {
	got := flattenInputVars(map[string]string{
		"file_path": "${tool_input.file_path}",
		"literal":   "static",
		"missing":   "${nope}",
	}, placeholderPayload(), nil)

	if got["file_path"] != "/tmp/a.txt" {
		t.Errorf("file_path = %v", got["file_path"])
	}
	if got["literal"] != "static" {
		t.Errorf("literal = %v", got["literal"])
	}
	if got["missing"] != "" {
		t.Errorf("missing = %v，期望空串", got["missing"])
	}
	if len(got) != 3 {
		t.Errorf("结果有 %d 个键，期望 3", len(got))
	}
}

// TestLookupPath covers the path walker, including the cases that must NOT resolve.
func TestLookupPath(t *testing.T) {
	view := payloadView(placeholderPayload())
	cases := map[string]string{
		"session_id":           "sess-1",
		"tool_input.file_path": "/tmp/a.txt",
		"tool_input.content":   "x",
		"hook_event_name":      "PostToolUse",
		// Non-string leaves and bad paths resolve to nothing.
		"tool_input":             "",
		"tool_input.missing":     "",
		"tool_input.file_path.x": "",
		"":                       "",
		"nope":                   "",
	}
	for path, want := range cases {
		if got := lookupPath(view, path); got != want {
			t.Errorf("lookupPath(%q) = %q，期望 %q", path, got, want)
		}
	}
}

// TestExpandTilde covers the "~" prefix rule: only a leading segment is expanded, as
// a shell would do.
func TestExpandTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := map[string]string{
		"~/hooks/x.sh": home + "/hooks/x.sh",
		"~":            home,
		"/a/~/b":       "/a/~/b",
		"a~b":          "a~b",
		"":             "",
		"/abs/path":    "/abs/path",
	}
	for value, want := range cases {
		if got := expandTilde(value); got != want {
			t.Errorf("expandTilde(%q) = %q，期望 %q", value, got, want)
		}
	}
}

// TestExpandStringPlaceholders covers the command-side expansion helper: the three
// documented path placeholders, the working-directory fallback for
// CLAUDE_PROJECT_DIR, and the ~ rule applied after substitution.
func TestExpandStringPlaceholders(t *testing.T) {
	dir := t.TempDir()
	engine := NewEngine(Config{}, Options{Dir: dir, Env: []string{"CLAUDE_PLUGIN_ROOT=/plug"}})

	cases := map[string]string{
		"${CLAUDE_PROJECT_DIR}/hook.sh": dir + "/hook.sh",
		"${CLAUDE_PLUGIN_ROOT}/x.js":    "/plug/x.js",
		// Not set anywhere and with no fallback: the reference is left as written
		// rather than blanked, because blanking would silently point the hook at a
		// different path (see expandString's comment).
		"${CLAUDE_PLUGIN_DATA}/d": "${CLAUDE_PLUGIN_DATA}/d",
		"${SOME_UNKNOWN_VAR}/x":   "${SOME_UNKNOWN_VAR}/x",
		"/bin/echo plain":         "/bin/echo plain",
	}
	for value, want := range cases {
		if got := engine.expandString(value); got != want {
			t.Errorf("expandString(%q) = %q，期望 %q", value, got, want)
		}
	}

	// A bare $VAR must survive, because a shell-form command needs it.
	if got := engine.expandString(`X=1; echo $X`); got != `X=1; echo $X` {
		t.Errorf("expandString 破坏了 shell 变量：%q", got)
	}
}
