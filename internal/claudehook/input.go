package claudehook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Input is one hook payload — the JSON written to a command hook's stdin and
// POSTed to an http hook.
//
// The field order matches §4's common-field table and the shapes measured in §10,
// because a handler that logs its stdin (the logger hook in §11 does exactly
// that) is easier to diff against Claude Code's own capture when the order is the
// same. Field presence is what carries meaning here: §10's measured payloads show
// that permission_mode, effort, model, agent_id and agent_type are absent on
// many events, so every non-universal field is omitempty. stop_hook_active is the
// deliberate exception — §10's Stop payload carries it as an explicit false, and a
// handler that tests the field must not be able to confuse "false" with "the
// runtime forgot to send it".
type Input struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	Cwd            string          `json:"cwd,omitempty"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"`
	// DurationMS is §8's optional PostToolUse field: how long the call took,
	// excluding any time spent waiting for a permission prompt or a PreToolUse
	// hook. It is a pointer so "0 ms" and "not measured" stay distinguishable —
	// a tool that returned instantly is a fact, not a missing value.
	DurationMS *int   `json:"duration_ms,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	PromptID   string `json:"prompt_id,omitempty"`
	// StopHookActive is §8's Stop field, and it is emitted as an explicit false
	// on Stop rather than omitted: §10's measured payload carries it, and a
	// handler that tests it must not be able to confuse "false" with "this
	// runtime forgot to send it". On every other event it is absent, as measured.
	StopHookActive bool   `json:"stop_hook_active,omitempty"`
	LastAssistant  string `json:"last_assistant_message,omitempty"`
	// BackgroundTasks and SessionCrons are §8's Stop fields: what is still
	// running, and what is scheduled to wake the session up. An empty array is
	// meaningful and different from an absent field (§8 says so explicitly), so
	// a nil slice omits the key while an empty non-nil slice emits [].
	BackgroundTasks []BackgroundTask `json:"background_tasks,omitempty"`
	SessionCrons    []SessionCron    `json:"session_crons,omitempty"`
	Source          string           `json:"source,omitempty"`
	Model           string           `json:"model,omitempty"`
	Effort          *Effort          `json:"effort,omitempty"`
	AgentID         string           `json:"agent_id,omitempty"`
	AgentType       string           `json:"agent_type,omitempty"`
	// Extra carries event-specific payload fields this type does not model (for
	// example a matcher field of one of the 28 events this build does not
	// dispatch). MarshalJSON merges its keys at the top level.
	Extra map[string]any `json:"-"`
}

// Effort is the nested {"level": "..."} object §4 documents. Only the four levels
// are listed there; a level this build does not recognise is carried through
// verbatim rather than rejected, because the value is informational for a handler
// and a newer Claude Code may add one.
type Effort struct {
	Level string `json:"level"`
}

// BackgroundTask is one in-progress task, as §8 documents it for Stop.
//
// The field set is the documented one, so a handler that reads
// .background_tasks[].type or .command works unchanged. Fields that belong to a
// kind of task this runtime does not produce (agent_type for a subagent, server
// and tool for an MCP monitor) are omitted rather than sent empty, which is what
// §8 means by "仅对 … 任务存在".
type BackgroundTask struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	Command     string `json:"command,omitempty"`
	AgentType   string `json:"agent_type,omitempty"`
	Server      string `json:"server,omitempty"`
	Tool        string `json:"tool,omitempty"`
}

// SessionCron is one session-scoped scheduled wakeup, as §8 documents it for Stop.
type SessionCron struct {
	ID        string `json:"id"`
	Schedule  string `json:"schedule"`
	Recurring bool   `json:"recurring"`
	Prompt    string `json:"prompt,omitempty"`
}

// inputFieldOrder is the emission order of MarshalJSON, matching §4.
var inputFieldOrder = []string{
	"session_id",
	"transcript_path",
	"cwd",
	"permission_mode",
	"hook_event_name",
	"tool_name",
	"tool_input",
	"tool_use_id",
	"tool_response",
	"duration_ms",
	"prompt",
	"prompt_id",
	"stop_hook_active",
	"last_assistant_message",
	"background_tasks",
	"session_crons",
	"source",
	"model",
	"effort",
	"agent_id",
	"agent_type",
}

// MarshalJSON emits the payload with Extra's keys merged in at the top level, so a
// caller can add an event-specific field without changing Input.
//
// The default encoding/json output cannot be reused here: Extra has no tag of its
// own (`json:"-"`), and map keys always sort after struct fields, so a merge-on-top
// would push a caller's extra fields behind every typed one. Building the object
// explicitly also lets the field order follow §4 instead of struct declaration
// order.
//
// Extra keys must not overwrite the typed fields. A collision means the caller
// tried to replace a field that has a typed home; the typed value wins, because it
// is the one the rest of this package (and the matcher) has already reasoned
// about, and honouring the overwrite would let a caller desynchronise the payload
// from the decision logic without any error.
func (in Input) MarshalJSON() ([]byte, error) {
	extra := make(map[string]json.RawMessage, len(in.Extra))
	for key, value := range in.Extra {
		if key == "" {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("claudehook: 序列化附加字段 %s：%w", key, err)
		}
		extra[key] = encoded
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	written := make(map[string]bool, len(inputFieldOrder))
	writeField := func(key string, encoded []byte) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		name, _ := json.Marshal(key)
		buf.Write(name)
		buf.WriteByte(':')
		buf.Write(encoded)
		written[key] = true
	}

	// Each typed field is emitted only when it carries a value. This mirrors the
	// omitempty tags and, more importantly, keeps the payload honest: §10 shows
	// Claude Code omitting fields that do not apply to an event, and a handler
	// that checks presence (the documented advice for model, effort,
	// permission_mode) must see the same thing from this runtime.
	for _, key := range inputFieldOrder {
		encoded, ok, err := in.encodeField(key)
		if err != nil {
			return nil, err
		}
		if ok {
			writeField(key, encoded)
		}
	}
	for _, key := range sortedKeys(extra) {
		if written[key] {
			continue
		}
		writeField(key, extra[key])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// encodeField renders one typed field, reporting false when it is absent.
func (in Input) encodeField(key string) ([]byte, bool, error) {
	marshal := func(v any) ([]byte, bool, error) {
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, false, fmt.Errorf("claudehook: 序列化字段 %s：%w", key, err)
		}
		return encoded, true, nil
	}
	str := func(s string) ([]byte, bool, error) {
		if s == "" {
			return nil, false, nil
		}
		return marshal(s)
	}
	raw := func(r json.RawMessage) ([]byte, bool, error) {
		trimmed := bytes.TrimSpace(r)
		if len(trimmed) == 0 {
			return nil, false, nil
		}
		// A RawMessage that is not valid JSON would produce an invalid payload
		// for every handler at once, so surface it here rather than let the
		// handler's own parser report a mystery.
		if !json.Valid(trimmed) {
			return nil, false, fmt.Errorf("claudehook: 字段 %s 不是合法 JSON", key)
		}
		return trimmed, true, nil
	}

	switch key {
	case "session_id":
		return str(in.SessionID)
	case "transcript_path":
		return str(in.TranscriptPath)
	case "cwd":
		return str(in.Cwd)
	case "permission_mode":
		return str(in.PermissionMode)
	case "hook_event_name":
		return str(in.HookEventName)
	case "tool_name":
		return str(in.ToolName)
	case "tool_input":
		return raw(in.ToolInput)
	case "tool_use_id":
		return str(in.ToolUseID)
	case "tool_response":
		return raw(in.ToolResponse)
	case "duration_ms":
		// §8 documents it for PostToolUse only; on any other event it is absent,
		// and a hook must not read a number that means nothing there.
		if in.HookEventName != "PostToolUse" || in.DurationMS == nil {
			return nil, false, nil
		}
		return marshal(*in.DurationMS)
	case "prompt":
		return str(in.Prompt)
	case "prompt_id":
		return str(in.PromptID)
	case "stop_hook_active":
		// Emitted on the events it belongs to, including as an explicit false.
		// The earlier version dropped the false value, which contradicted this
		// file's own note above and cost a handler the one field the protocol
		// gives it to avoid blocking a Stop forever.
		if in.HookEventName != "Stop" && in.HookEventName != "SubagentStop" {
			return nil, false, nil
		}
		return marshal(in.StopHookActive)
	case "last_assistant_message":
		return str(in.LastAssistant)
	case "background_tasks":
		// §8's "空数组" rule: an empty array says "nothing is in progress", which
		// is different from the field being absent. Only Stop receives them.
		if in.HookEventName != "Stop" || in.BackgroundTasks == nil {
			return nil, false, nil
		}
		return marshal(in.BackgroundTasks)
	case "session_crons":
		if in.HookEventName != "Stop" || in.SessionCrons == nil {
			return nil, false, nil
		}
		return marshal(in.SessionCrons)
	case "source":
		return str(in.Source)
	case "model":
		return str(in.Model)
	case "effort":
		if in.Effort == nil {
			return nil, false, nil
		}
		return marshal(in.Effort)
	case "agent_id":
		return str(in.AgentID)
	case "agent_type":
		return str(in.AgentType)
	}
	return nil, false, nil
}

// sortedKeys returns the map's keys in sorted order so a payload's extra fields
// are emitted deterministically: two dispatches of the same input must produce
// byte-identical stdin, or a handler that hashes its input would see a change
// where the caller made none.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
