package claudehook

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInputMarshalKeys pins §4's field names on the wire. A handler that parses
// its stdin by name (the §11 logger hook does, through jq) breaks silently if any
// of these is renamed, so the key set is asserted rather than assumed.
//
// The payload is built on Stop because that is the event §10 measures with the
// largest field set: it carries stop_hook_active (explicitly false there),
// background_tasks and session_crons, which no other event carries at all.
func TestInputMarshalKeys(t *testing.T) {
	durationMS := 5
	in := Input{
		SessionID:      "61361905-1e0b-4538-b736-601108686a40",
		TranscriptPath: "/Users/huan/.claude/projects/x/session.jsonl",
		Cwd:            "/private/tmp/capture",
		PermissionMode: "default",
		HookEventName:  "Stop",
		ToolName:       "Read",
		ToolInput:      json.RawMessage(`{"file_path":"/tmp/a.txt"}`),
		ToolUseID:      "call_00_abc",
		ToolResponse:   json.RawMessage(`{"type":"text"}`),
		Prompt:         "读一下 a.txt",
		PromptID:       "fd31d741-d4a1-4125-b31c-bbbcb45c262e",
		StopHookActive: true,
		LastAssistant:  "done",
		BackgroundTasks: []BackgroundTask{{
			ID: "j1", Type: "shell", Status: "running", Command: "npm test",
		}},
		SessionCrons: []SessionCron{},
		Source:       "startup",
		Model:        "some-model",
		Effort:       &Effort{Level: "max"},
		AgentID:      "aca079092d0ddd9de",
		AgentType:    "Explore",
	}
	// duration_ms belongs to PostToolUse; a Stop payload must not carry it, which
	// the count assertion below is what proves.
	in.DurationMS = &durationMS

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化 Input 失败：%v", err)
	}

	// Decoding into a map is what proves the keys, not the struct tags the
	// compiler already checked.
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}

	wantKeys := []string{
		"session_id", "transcript_path", "cwd", "permission_mode", "hook_event_name",
		"tool_name", "tool_input", "tool_use_id", "tool_response", "prompt",
		"prompt_id", "stop_hook_active", "last_assistant_message", "background_tasks",
		"session_crons", "source", "model", "effort", "agent_id", "agent_type",
	}
	for _, key := range wantKeys {
		if _, ok := got[key]; !ok {
			t.Errorf("payload 缺少字段 %q（§4）", key)
		}
	}
	if len(got) != len(wantKeys) {
		t.Errorf("payload 有 %d 个字段，期望 %d：%v", len(got), len(wantKeys), sortedAnyKeys(got))
	}

	// The nested effort object is §4's {"level": "..."} shape.
	effort, ok := got["effort"].(map[string]any)
	if !ok {
		t.Fatalf("effort 不是对象：%T", got["effort"])
	}
	if effort["level"] != "max" {
		t.Errorf("effort.level = %v，期望 max", effort["level"])
	}

	// tool_input must stay raw JSON, not a string containing JSON: §8 documents
	// handlers indexing into it (tool_input.file_path), which a double-encoded
	// value would break.
	toolInput, ok := got["tool_input"].(map[string]any)
	if !ok {
		t.Fatalf("tool_input 不是对象：%T", got["tool_input"])
	}
	if toolInput["file_path"] != "/tmp/a.txt" {
		t.Errorf("tool_input.file_path = %v", toolInput["file_path"])
	}
}

// TestInputMarshalFieldOrder pins §4's order. The order is observable to a
// handler that logs its stdin, and keeping it identical to Claude Code's own
// capture is what makes the two files diffable.
func TestInputMarshalFieldOrder(t *testing.T) {
	in := Input{
		SessionID:      "s",
		TranscriptPath: "t",
		Cwd:            "c",
		PermissionMode: "default",
		HookEventName:  "PreToolUse",
		ToolName:       "Bash",
		ToolInput:      json.RawMessage(`{}`),
		ToolUseID:      "u",
		ToolResponse:   json.RawMessage(`{}`),
		Prompt:         "p",
		PromptID:       "pid",
		StopHookActive: true,
		LastAssistant:  "la",
		Source:         "startup",
		Model:          "m",
		Effort:         &Effort{Level: "low"},
		AgentID:        "a",
		AgentType:      "at",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	order := []string{"session_id", "hook_event_name", "tool_name", "prompt_id", "source"}
	last := -1
	for _, key := range order {
		at := strings.Index(string(data), `"`+key+`"`)
		if at < 0 {
			t.Fatalf("payload 缺少 %q：%s", key, data)
		}
		if at < last {
			t.Errorf("字段 %q 出现在更早的位置，字段顺序不符合 §4：%s", key, data)
		}
		last = at
	}
}

// TestInputMarshalOmitsAbsentFields covers the measured payloads of §10: a
// SessionStart payload has no model, no permission_mode and no effort, so those
// keys must be absent rather than present-and-empty. A handler that checks
// presence (the documented advice) would otherwise see fields Claude Code never
// sends.
func TestInputMarshalOmitsAbsentFields(t *testing.T) {
	data, err := json.Marshal(Input{
		SessionID:     "61361905-1e0b-4538-b736-601108686a40",
		HookEventName: "SessionStart",
		Source:        "startup",
	})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	const want = `{"session_id":"61361905-1e0b-4538-b736-601108686a40","hook_event_name":"SessionStart","source":"startup"}`
	if string(data) != want {
		t.Errorf("SessionStart payload = %s\n期望 = %s", data, want)
	}
}

// TestInputMarshalStopPayload matches §10's measured Stop payload, including the
// two fields §10 notes are present as empty arrays rather than absent. Those
// fields are not modelled by Input, which is exactly what Extra is for.
func TestInputMarshalStopPayload(t *testing.T) {
	in := Input{
		SessionID:      "61361905-1e0b-4538-b736-601108686a40",
		TranscriptPath: "/x.jsonl",
		Cwd:            "/private/tmp/capture",
		PromptID:       "fd31d741",
		PermissionMode: "default",
		HookEventName:  "Stop",
		LastAssistant:  "done",
		Effort:         &Effort{Level: "max"},
		Extra: map[string]any{
			"stop_hook_active": false,
			"background_tasks": []any{},
			"session_crons":    []any{},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	for _, want := range []string{
		`"last_assistant_message":"done"`,
		`"effort":{"level":"max"}`,
		`"background_tasks":[]`,
		`"session_crons":[]`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("payload 缺少 %s：%s", want, data)
		}
	}
	// stop_hook_active is in Extra here, so the typed field's omission does not
	// hide it — and it must appear as a real false, not be dropped.
	if !strings.Contains(string(data), `"stop_hook_active":false`) {
		t.Errorf("payload 缺少 stop_hook_active:false：%s", data)
	}
}

// TestInputExtraKeysCannotOverwriteTypedFields covers the documented constraint on
// Extra: a caller adding an event-specific field must not be able to replace a
// typed one, because the typed value is the one the matcher and the decision logic
// already reasoned about.
func TestInputExtraKeysCannotOverwriteTypedFields(t *testing.T) {
	in := Input{
		SessionID:     "real-session",
		HookEventName: "PreToolUse",
		ToolName:      "Bash",
		Extra: map[string]any{
			"session_id": "impostor",
			"tool_name":  "Edit",
			// A genuinely new key must still be merged in.
			"mcp_server": map[string]any{"name": "demo", "source": "user"},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if got["session_id"] != "real-session" {
		t.Errorf("Extra 覆盖了 session_id：%v", got["session_id"])
	}
	if got["tool_name"] != "Bash" {
		t.Errorf("Extra 覆盖了 tool_name：%v", got["tool_name"])
	}
	mcp, ok := got["mcp_server"].(map[string]any)
	if !ok || mcp["source"] != "user" {
		t.Errorf("Extra 的新字段未合并：%v", got["mcp_server"])
	}
}

// TestInputExtraIsDeterministic covers the reason extra keys are sorted: two
// dispatches of the same input must produce byte-identical stdin.
func TestInputExtraIsDeterministic(t *testing.T) {
	build := func() string {
		in := Input{
			SessionID:     "s",
			HookEventName: "PostToolUse",
			Extra:         map[string]any{"z": 1, "a": 2, "m": 3},
		}
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("序列化失败：%v", err)
		}
		return string(data)
	}
	first := build()
	for i := 0; i < 50; i++ {
		if got := build(); got != first {
			t.Fatalf("Extra 输出不确定：\n%s\n%s", first, got)
		}
	}
	if !strings.HasSuffix(first, `"a":2,"m":3,"z":1}`) {
		t.Errorf("Extra 未按字典序输出：%s", first)
	}
}

// TestInputMarshalInvalidRawMessage covers the one input that cannot be rendered:
// a RawMessage that is not valid JSON would produce a payload every handler would
// then fail to parse, so it is reported as an error instead.
func TestInputMarshalInvalidRawMessage(t *testing.T) {
	in := Input{
		SessionID:     "s",
		HookEventName: "PreToolUse",
		ToolInput:     json.RawMessage(`{"broken":`),
	}
	if _, err := json.Marshal(in); err == nil {
		t.Error("非法 tool_input 应返回错误")
	}
}

// TestInputMarshalIsValidJSON covers the payload being parseable at all, including
// when Extra carries a value json.Marshal cannot render.
func TestInputMarshalIsValidJSON(t *testing.T) {
	in := Input{
		SessionID:     "s",
		HookEventName: "UserPromptSubmit",
		Prompt:        "带引号 \" 和反斜杠 \\ 以及中文",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("payload 不是合法 JSON：%s", data)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if got["prompt"] != in.Prompt {
		t.Errorf("prompt 往返不一致：%v", got["prompt"])
	}
}

// TestInputMarshalUnsupportedExtraValue covers a value Extra cannot render: the
// error must name the field, because "json: unsupported type" alone does not say
// which caller added it.
func TestInputMarshalUnsupportedExtraValue(t *testing.T) {
	in := Input{
		SessionID:     "s",
		HookEventName: "PreToolUse",
		Extra:         map[string]any{"channel": make(chan int)},
	}
	_, err := json.Marshal(in)
	if err == nil {
		t.Fatal("无法序列化的 Extra 值应返回错误")
	}
	if !strings.Contains(err.Error(), "channel") {
		t.Errorf("错误信息应指明字段名：%v", err)
	}
}

// TestInputPayloadReachesHandler proves the marshalled Input is what a handler
// actually receives on stdin — the shape assertions above are only meaningful if
// that is true.
func TestInputPayloadReachesHandler(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	cfg := groupsFor("UserPromptSubmit", "*", helper.handler())
	opts := helper.options(t)

	verdict := dispatchInDir(t, cfg, opts, Input{
		SessionID:      "sess-42",
		TranscriptPath: "/x.jsonl",
		Cwd:            "/private/tmp/capture",
		PermissionMode: "default",
		HookEventName:  "UserPromptSubmit",
		Prompt:         "把 a.txt 读出来",
		PromptID:       "pid-1",
		Effort:         &Effort{Level: "max"},
	})
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（skipped=%v failures=%v）", verdict.Handlers, verdict.Skipped, verdict.Failures)
	}

	record := helper.read()
	if record == "" {
		t.Fatal("helper 没有写下记录文件")
	}
	stdin := stdinFromRecord(t, record)

	var got map[string]any
	if err := json.Unmarshal([]byte(stdin), &got); err != nil {
		t.Fatalf("handler 收到的 stdin 不是合法 JSON：%v\n%s", err, stdin)
	}
	for key, want := range map[string]any{
		"session_id":      "sess-42",
		"transcript_path": "/x.jsonl",
		"cwd":             "/private/tmp/capture",
		"permission_mode": "default",
		"hook_event_name": "UserPromptSubmit",
		"prompt":          "把 a.txt 读出来",
		"prompt_id":       "pid-1",
	} {
		if got[key] != want {
			t.Errorf("stdin[%q] = %v，期望 %v", key, got[key], want)
		}
	}
	effort, ok := got["effort"].(map[string]any)
	if !ok || effort["level"] != "max" {
		t.Errorf("stdin 的 effort = %v", got["effort"])
	}
	// §2.1: UserPromptSubmit has no matcher field, so no tool_name is invented.
	if _, ok := got["tool_name"]; ok {
		t.Errorf("UserPromptSubmit payload 不应带 tool_name：%v", got["tool_name"])
	}
}

// stdinFromRecord extracts the stdin line the helper wrote.
func stdinFromRecord(t *testing.T, record string) string {
	t.Helper()
	const marker = "stdin="
	at := strings.Index(record, marker)
	if at < 0 {
		t.Fatalf("记录文件没有 stdin 段：%s", record)
	}
	return strings.TrimPrefix(record[at+len(marker):], "\n")
}

// sortedAnyKeys lists a map's keys for a failure message.
func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	// Insertion sort: the list is small and this keeps the helper free of another
	// import.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// TestStopHookActiveIsAlwaysPresentOnStop pins §10's measured shape: the Stop
// payload carries stop_hook_active as an explicit false, because a handler uses
// it as the loop guard and must not have to tell "false" from "the runtime
// forgot to send it". The earlier implementation dropped the false value, which
// is the bug this test exists to keep out.
func TestStopHookActiveIsAlwaysPresentOnStop(t *testing.T) {
	for _, active := range []bool{true, false} {
		in := Input{SessionID: "s", HookEventName: "Stop", StopHookActive: active}
		got := marshalToMap(t, in)
		value, ok := got["stop_hook_active"]
		if !ok {
			t.Fatalf("stop_hook_active（%v）没有出现在 Stop 载荷里", active)
		}
		if value != active {
			t.Errorf("stop_hook_active = %v，期望 %v", value, active)
		}
	}

	// On any other event it is absent, which is what §10 measures.
	for _, event := range []string{"PreToolUse", "PostToolUse", "UserPromptSubmit", "SessionStart"} {
		in := Input{SessionID: "s", HookEventName: event, StopHookActive: true}
		if _, ok := marshalToMap(t, in)["stop_hook_active"]; ok {
			t.Errorf("事件 %s 不该带 stop_hook_active（§4/§10）", event)
		}
	}
}

// TestStopCarriesTheTaskArrays pins §8's "空数组与字段缺失不同" rule: a Stop
// payload says whether anything is still running, and an empty array is how it
// says no. A nil slice must still omit the key, so a caller that never looked
// does not claim there is nothing running.
func TestStopCarriesTheTaskArrays(t *testing.T) {
	running := Input{
		SessionID:     "s",
		HookEventName: "Stop",
		BackgroundTasks: []BackgroundTask{{
			ID: "j1", Type: "shell", Status: "running", Description: "npm test", Command: "npm test",
		}},
		SessionCrons: []SessionCron{{ID: "c1", Schedule: "0 9 * * *", Recurring: true}},
	}
	got := marshalToMap(t, running)
	tasks, ok := got["background_tasks"].([]any)
	if !ok || len(tasks) != 1 {
		t.Fatalf("background_tasks = %#v，期望一个元素", got["background_tasks"])
	}
	task := tasks[0].(map[string]any)
	for key, want := range map[string]any{
		"id": "j1", "type": "shell", "status": "running", "command": "npm test",
	} {
		if task[key] != want {
			t.Errorf("background_tasks[0].%s = %v，期望 %v", key, task[key], want)
		}
	}
	crons, ok := got["session_crons"].([]any)
	if !ok || len(crons) != 1 {
		t.Fatalf("session_crons = %#v", got["session_crons"])
	}

	empty := Input{SessionID: "s", HookEventName: "Stop",
		BackgroundTasks: []BackgroundTask{}, SessionCrons: []SessionCron{}}
	got = marshalToMap(t, empty)
	if tasks, ok := got["background_tasks"].([]any); !ok || len(tasks) != 0 {
		t.Errorf("空的 background_tasks 必须是 []，得到 %#v", got["background_tasks"])
	}
	if _, ok := got["session_crons"]; !ok {
		t.Error("空的 session_crons 必须出现（空数组 ≠ 字段缺失）")
	}

	// A caller that did not look must not claim there is nothing running.
	absent := Input{SessionID: "s", HookEventName: "Stop"}
	got = marshalToMap(t, absent)
	if _, ok := got["background_tasks"]; ok {
		t.Error("nil 的 background_tasks 必须省略，而不是报告「没有在跑的任务」")
	}
	// And no other event carries them.
	for _, event := range []string{"PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit"} {
		in := Input{SessionID: "s", HookEventName: event, BackgroundTasks: []BackgroundTask{}}
		if _, ok := marshalToMap(t, in)["background_tasks"]; ok {
			t.Errorf("事件 %s 不该带 background_tasks", event)
		}
	}
}

// TestDurationMSBelongsToPostToolUse pins §8's optional field: it is a number on
// PostToolUse, including a measured 0, and absent everywhere else.
func TestDurationMSBelongsToPostToolUse(t *testing.T) {
	zero := 0
	post := Input{SessionID: "s", HookEventName: "PostToolUse", DurationMS: &zero}
	got := marshalToMap(t, post)
	if value, ok := got["duration_ms"]; !ok || value.(float64) != 0 {
		t.Errorf("duration_ms = %#v，期望一个真实的 0（0ms 是事实，不是缺值）", got["duration_ms"])
	}

	pre := Input{SessionID: "s", HookEventName: "PreToolUse", DurationMS: &zero}
	if _, ok := marshalToMap(t, pre)["duration_ms"]; ok {
		t.Error("PreToolUse 不该带 duration_ms（§8 只把它列在 PostToolUse）")
	}
	stop := Input{SessionID: "s", HookEventName: "Stop", DurationMS: &zero}
	if _, ok := marshalToMap(t, stop)["duration_ms"]; ok {
		t.Error("Stop 不该带 duration_ms")
	}
	if _, ok := marshalToMap(t, Input{SessionID: "s", HookEventName: "PostToolUse"})["duration_ms"]; ok {
		t.Error("没有测量过的时长必须省略，而不是报 0")
	}
}

// marshalToMap serialises an Input and decodes it back into a map, which is how
// a test can assert the wire keys rather than the struct tags.
func marshalToMap(t *testing.T, in Input) map[string]any {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化 Input 失败：%v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	return got
}
