package claudehook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoOutput reports that a handler produced no parseable decision output. It is
// not a failure by itself: with exit code 0 it is the ordinary "the hook chose to
// say nothing" case (§5.2), and with other exit codes the caller decides.
var ErrNoOutput = errors.New("claudehook: handler produced no decision output")

// decisionEnvelope is the JSON output schema of §5.4/§5.5 as it appears on a
// handler's stdout or HTTP response body.
//
// Every field is a RawMessage or a pointer so that "absent", "present but
// wrong type" and "present with a value" stay three distinguishable states: §5.4
// lists fields that individual events discard, and discarding a field is only
// possible if the parser did not already coerce it. A wrong type is reported as a
// non-blocking error rather than absorbed, because silently dropping a decision
// field is how a policy hook appears to work while doing nothing.
type decisionEnvelope struct {
	Continue *bool `json:"continue"`
	// SuppressOutput is accepted and ignored: §5.4 says Claude Code takes the
	// field and does nothing with it.
	SuppressOutput *bool           `json:"suppressOutput"`
	StopReason     json.RawMessage `json:"stopReason"`
	SystemMessage  json.RawMessage `json:"systemMessage"`
	// TerminalSequence is accepted and ignored: §5.4 makes it terminal-only, and
	// this runtime has no terminal to write it to.
	TerminalSequence json.RawMessage `json:"terminalSequence"`
	Decision         json.RawMessage `json:"decision"`
	Reason           json.RawMessage `json:"reason"`
	// sessionTitle is documented inside hookSpecificOutput for UserPromptSubmit
	// and SessionStart, but §5.4's "通用字段" table also lists it as a general
	// field. Both placements are accepted; see parseOutput.
	SessionTitle       json.RawMessage `json:"sessionTitle"`
	HookSpecificOutput json.RawMessage `json:"hookSpecificOutput"`
}

// hookSpecificEnvelope is the nested object of §5.4. hookEventName is mandatory
// and must equal the triggering event: a missing or mismatched value invalidates
// the whole object, which the reference calls out explicitly as a compatibility
// point.
type hookSpecificEnvelope struct {
	HookEventName            json.RawMessage `json:"hookEventName"`
	PermissionDecision       json.RawMessage `json:"permissionDecision"`
	PermissionDecisionReason json.RawMessage `json:"permissionDecisionReason"`
	UpdatedInput             json.RawMessage `json:"updatedInput"`
	UpdatedToolOutput        json.RawMessage `json:"updatedToolOutput"`
	AdditionalContext        json.RawMessage `json:"additionalContext"`
	InitialUserMessage       json.RawMessage `json:"initialUserMessage"`
	SessionTitle             json.RawMessage `json:"sessionTitle"`
	WatchPaths               json.RawMessage `json:"watchPaths"`
	ReloadSkills             json.RawMessage `json:"reloadSkills"`
	Retry                    json.RawMessage `json:"retry"`
	// Decision is the PermissionRequest shape: {"behavior":"allow"|"deny", …}.
	Decision json.RawMessage `json:"decision"`
}

// permissionRequestDecision is that nested decision object (§5.5). This build does
// not dispatch PermissionRequest, but the object is parsed anyway so a config that
// relies on it produces a *reported* outcome instead of an unexplained no-op if
// the event is ever dispatched.
type permissionRequestDecision struct {
	Behavior     string          `json:"behavior"`
	Message      string          `json:"message"`
	UpdatedInput json.RawMessage `json:"updatedInput"`
}

// parsedOutput is the decision contents of one handler's output, before it is
// aggregated into a Verdict. Keeping it separate from Verdict is what lets the
// aggregation rules of §5.5 (first-wins for blocking reasons, deny overrides
// allow) live in one place instead of being spread over the handler runners.
type parsedOutput struct {
	SystemMessages     []string
	AdditionalContext  []string
	Continue           *bool
	StopReason         string
	Block              bool
	BlockReason        string
	PermissionDecision string
	UpdatedInput       json.RawMessage
	UpdatedToolOutput  string
	HasUpdatedOutput   bool
	InitialUserMessage string
	SessionTitle       string
	WatchPaths         []string
	ReloadSkills       bool
	Retry              bool
	// Failures are non-blocking problems found while parsing: an unsupported
	// field, a bad type, an over-long value that was truncated.
	Failures []string
}

// maxContextChars is the 10,000 character cap of §5.8. It applies per string to
// additionalContext, systemMessage and initialUserMessage, and to plain-text
// stdout.
const maxContextChars = 10000

// parseOutput interprets a handler's stdout / HTTP body / MCP text output for one
// event, following §5.1 (how to decide JSON from text), §5.4 and §5.5.
//
// The plain-text cases matter per event: for UserPromptSubmit and SessionStart the
// text reaches the model as context (§5.1, §7 item 8). This build reports it as
// AdditionalContext in both cases — the one channel the caller already has to
// consume — rather than as a separate field, so a caller cannot accidentally act
// on JSON-only context and drop the text one. For PreToolUse, PostToolUse and
// Stop, §5.5 gives those events no text channel at all, so text is recorded as an
// ignored output instead of being invented into a decision.
//
// §5.1's multi-line special case (per-line JSON objects that set no field are
// treated as text) is not implemented beyond "the whole trimmed output must parse
// as one object": if a multi-line body starting with "{" and ending with "}" fails
// to parse, it is treated as plain text — the same outcome the documented rule
// produces for that input.
func parseOutput(event string, data []byte) (parsedOutput, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return parsedOutput{}, ErrNoOutput
	}

	// §5.1: JSON only when it starts with "{" and ends with "}".
	if trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return parsePlainText(event, string(trimmed)), nil
	}

	var env decisionEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		// Starts like JSON but is not: §5.1 makes that plain text, not an error.
		return parsePlainText(event, string(trimmed)), nil
	}

	out := parsedOutput{}
	spec := SpecFor(event)

	if env.Continue != nil {
		// §5.4: continue:false stops everything; stopReason is shown to the user.
		v := *env.Continue
		out.Continue = &v
	}
	if msg, ok, err := optString(env.StopReason, "stopReason"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok {
		// §5.4: stopReason is the message shown to the user when continue is
		// false. It is kept even when continue is absent or true — a handler that
		// set only stopReason has still said something the operator should see.
		out.StopReason = msg
	}

	if msg, ok, err := optString(env.SystemMessage, "systemMessage"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok && msg != "" {
		out.SystemMessages = append(out.SystemMessages, capString(msg, "systemMessage", &out.Failures))
	}
	if _, _, err := optString(env.TerminalSequence, "terminalSequence"); err != nil {
		// Accepted and ignored (§5.4) — but a wrong type is still worth a line,
		// because it means the config author expected a notice to appear.
		out.Failures = append(out.Failures, err.Error())
	}
	// A well-formed terminalSequence is deliberately dropped without comment: §5.4
	// makes it terminal-only, and this runtime has no terminal to write it to.

	applyTopLevelDecision(&out, event, spec, env.Decision, env.Reason)

	if title, ok, err := optString(env.SessionTitle, "sessionTitle"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok {
		out.SessionTitle = title
	}

	if len(bytes.TrimSpace(env.HookSpecificOutput)) > 0 {
		parseHookSpecific(&out, event, spec, env.HookSpecificOutput)
	}
	return out, nil
}

// parsePlainText handles the non-JSON branch of §5.1.
func parsePlainText(event, text string) parsedOutput {
	out := parsedOutput{}
	if text == "" {
		return out
	}
	text = capString(text, "stdout", &out.Failures)

	switch event {
	case "SessionStart", "UserPromptSubmit":
		// §5.1/§8: this event's plain stdout reaches the model as context.
		out.AdditionalContext = append(out.AdditionalContext, text)
	default:
		// The events this build dispatches still have no text channel here, so
		// record why the output had no effect instead of dropping it silently.
		out.Failures = append(out.Failures, fmt.Sprintf(
			"事件 %s 不支持纯文本 stdout，该输出未生效（%d 字符）", event, len([]rune(text))))
	}
	return out
}

// applyTopLevelDecision handles top-level "decision" / "reason" (§5.5).
//
// §5.5 gives the top-level decision field to UserPromptSubmit, PostToolUse and
// Stop, and gives PreToolUse hookSpecificOutput instead (the top-level fields are
// documented as deprecated there, with "approve"/"block" mapping to
// "allow"/"deny"). Both placements are accepted: the mapping is documented, so
// honouring it cannot surprise a caller, and an event that has no blocking
// concept still reports the decision as a failure rather than ignoring it.
func applyTopLevelDecision(out *parsedOutput, event string, spec Spec, decisionRaw, reasonRaw json.RawMessage) {
	if len(bytes.TrimSpace(decisionRaw)) == 0 {
		return
	}
	reason, _, err := optString(reasonRaw, "reason")
	if err != nil {
		out.Failures = append(out.Failures, err.Error())
	}

	// The field is either the string "block" or, for the PermissionRequest
	// shape, an object. Try the string first: that is what §5.5 documents as the
	// only legal value, and treating an object as a string would lose it.
	var verb string
	if err := json.Unmarshal(decisionRaw, &verb); err == nil {
		switch verb {
		case "block":
			out.Block = true
			out.BlockReason = firstNonEmpty(out.BlockReason, reason)
		case "approve", "allow":
			// Deprecated pre-v2.1 value (§8 PreToolUse); maps to allow.
			out.PermissionDecision = "allow"
		default:
			out.Failures = append(out.Failures, fmt.Sprintf(
				"decision 取值 %q 无效（只接受 \"block\"），已忽略", verb))
		}
		return
	}

	var nested permissionRequestDecision
	if err := json.Unmarshal(decisionRaw, &nested); err != nil {
		out.Failures = append(out.Failures, fmt.Sprintf("decision 字段必须是字符串或对象：%v", err))
		return
	}
	switch nested.Behavior {
	case "allow":
		out.PermissionDecision = "allow"
		out.UpdatedInput = firstRaw(out.UpdatedInput, nested.UpdatedInput)
	case "deny":
		out.Block = true
		out.BlockReason = firstNonEmpty(out.BlockReason, nested.Message, reason)
		out.PermissionDecision = "deny"
	case "":
		out.Failures = append(out.Failures, "decision 对象缺少 behavior 字段，已忽略")
	default:
		out.Failures = append(out.Failures, fmt.Sprintf(
			"decision.behavior 取值 %q 无效（只接受 allow/deny），已忽略", nested.Behavior))
	}
	_ = event
	_ = spec
}

// parseHookSpecific handles the nested object of §5.4, whose hookEventName is
// mandatory and must match the event that fired.
func parseHookSpecific(out *parsedOutput, event string, spec Spec, raw json.RawMessage) {
	var hs hookSpecificEnvelope
	if err := json.Unmarshal(raw, &hs); err != nil {
		out.Failures = append(out.Failures, fmt.Sprintf("hookSpecificOutput 不是对象：%v", err))
		return
	}

	name, ok, err := optString(hs.HookEventName, "hookSpecificOutput.hookEventName")
	if err != nil {
		// A wrong-typed name is as unusable as a missing one: the object cannot
		// be attributed to an event, so the whole object is rejected.
		out.Failures = append(out.Failures, fmt.Sprintf(
			"hookSpecificOutput 被拒绝：hookEventName %v（§5.4 要求该字段必填且等于触发事件名）", err))
		return
	}
	if !ok || name == "" {
		out.Failures = append(out.Failures, fmt.Sprintf(
			"hookSpecificOutput 被拒绝：缺少 hookEventName 字段（§5.4 要求它必填且等于 %q）", event))
		return
	}
	if name != event {
		out.Failures = append(out.Failures, fmt.Sprintf(
			"hookSpecificOutput 被拒绝：hookEventName 为 %q，与触发事件 %q 不一致（§5.4）", name, event))
		return
	}

	if pd, ok, err := optString(hs.PermissionDecision, "permissionDecision"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok {
		switch pd {
		case "allow", "deny", "ask", "defer":
			out.PermissionDecision = pd
		case "approve":
			// Deprecated spelling that maps to "allow" (§8 PreToolUse).
			out.PermissionDecision = "allow"
		case "block":
			out.PermissionDecision = "deny"
		default:
			out.Failures = append(out.Failures, fmt.Sprintf(
				"permissionDecision 取值 %q 无效（allow/deny/ask/defer），已忽略", pd))
		}
	}
	pdReason := ""
	if reason, ok, err := optString(hs.PermissionDecisionReason, "permissionDecisionReason"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok {
		pdReason = reason
	}
	if out.PermissionDecision != "" {
		switch out.PermissionDecision {
		case "deny":
			// §8: for deny the reason is shown to Claude, i.e. it is the block
			// reason.
			out.Block = true
			out.BlockReason = firstNonEmpty(out.BlockReason, pdReason)
		case "allow", "ask":
			// Shown to the user, not to Claude — so it must not become a block
			// reason. Keep it as a system message so the operator sees it.
			if pdReason != "" {
				out.SystemMessages = append(out.SystemMessages, pdReason)
			}
		case "defer":
			// §8: ignored for defer.
		}
	}
	if out.PermissionDecision == "" && pdReason != "" {
		// The reason arrived without a decision alongside it. §8 reads this field
		// per decision, so there is nothing to route it to — but dropping it
		// silently would lose the only explanation the handler gave, and §5.2 makes
		// even stderr a usable block reason. It is kept as the block reason, which
		// then only surfaces if something else blocks (an exit code 2 in the same
		// handler, or another blocker in the same dispatch).
		out.BlockReason = firstNonEmpty(out.BlockReason, pdReason)
	}

	if len(bytes.TrimSpace(hs.UpdatedInput)) > 0 {
		var probe any
		if err := json.Unmarshal(hs.UpdatedInput, &probe); err != nil {
			out.Failures = append(out.Failures, fmt.Sprintf("updatedInput 不是合法 JSON：%v", err))
		} else {
			out.UpdatedInput = hs.UpdatedInput
		}
	}
	if v, ok, err := optString(hs.UpdatedToolOutput, "updatedToolOutput"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok {
		// §5.5: PostToolUse uses this to replace the tool result. The value must
		// match the tool's output shape, which this runtime cannot check — it is
		// passed through verbatim.
		out.UpdatedToolOutput = v
		out.HasUpdatedOutput = true
	}
	if v, ok, err := optString(hs.AdditionalContext, "additionalContext"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok && v != "" {
		out.AdditionalContext = append(out.AdditionalContext,
			capString(v, "additionalContext", &out.Failures))
	}
	if v, ok, err := optString(hs.InitialUserMessage, "initialUserMessage"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok && v != "" {
		out.InitialUserMessage = capString(v, "initialUserMessage", &out.Failures)
	}
	if v, ok, err := optString(hs.SessionTitle, "sessionTitle"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok && v != "" {
		out.SessionTitle = v
	}
	if len(bytes.TrimSpace(hs.WatchPaths)) > 0 {
		var paths []string
		if err := json.Unmarshal(hs.WatchPaths, &paths); err != nil {
			out.Failures = append(out.Failures, fmt.Sprintf("watchPaths 必须是字符串数组：%v", err))
		} else {
			out.WatchPaths = append(out.WatchPaths, paths...)
		}
	}
	if v, ok, err := optBool(hs.ReloadSkills, "reloadSkills"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok {
		out.ReloadSkills = v
	}
	if v, ok, err := optBool(hs.Retry, "retry"); err != nil {
		out.Failures = append(out.Failures, err.Error())
	} else if ok {
		out.Retry = v
	}
	if len(bytes.TrimSpace(hs.Decision)) > 0 {
		applyTopLevelDecision(out, event, spec, hs.Decision, nil)
	}
}

// optString extracts an optional string field. It reports a present-but-wrong-type
// value as an error naming the field, which is what makes the "reject, do not
// coerce" policy visible in the console panel.
func optString(raw json.RawMessage, field string) (string, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", false, fmt.Errorf("字段 %s 必须是字符串：%v", field, err)
	}
	return s, true, nil
}

// optBool is optString for boolean fields.
func optBool(raw json.RawMessage, field string) (bool, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false, false, nil
	}
	var b bool
	if err := json.Unmarshal(trimmed, &b); err != nil {
		return false, false, fmt.Errorf("字段 %s 必须是布尔值：%v", field, err)
	}
	return b, true, nil
}

// capString enforces §5.8's 10,000-character limit on one string. The reference
// says Claude Code writes the overflow to a file in the session directory and
// substitutes a path plus a preview; this runtime must not write files, so it
// truncates instead and records the truncation as a non-blocking failure. That is
// the conservative direction: the caller gets less context than Claude Code would
// have given, never more, and the operator is told why.
func capString(s, field string, failures *[]string) string {
	runes := []rune(s)
	if len(runes) <= maxContextChars {
		return s
	}
	*failures = append(*failures, fmt.Sprintf(
		"字段 %s 超过 %d 字符上限（§5.8），已截断 %d 字符", field, maxContextChars, len(runes)-maxContextChars))
	return string(runes[:maxContextChars])
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstRaw returns the first non-empty JSON value.
func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, v := range values {
		if len(bytes.TrimSpace(v)) > 0 {
			return v
		}
	}
	return nil
}
