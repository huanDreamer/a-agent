package claudehook

import (
	"regexp"
	"strings"
)

// maxRegexLen bounds a matcher that takes the regex path. A config is operator
// input, and an unbounded Go regexp (with nested alternation) can be made to
// compile for a very long time; refusing is the honest answer, because a matcher
// this build cannot compile is a matcher it must not silently treat as "no
// match" or "match everything".
const maxRegexLen = 4096

// exactMatcherChars are the characters that keep a matcher on the exact-match
// path (§2): letters, digits, "_", "-", space, ",", "|". Anything else — a
// regexp metacharacter, a dot, a bracket — means the matcher is a JavaScript
// regexp evaluated with RegExp.prototype.test, i.e. an unanchored match.
const exactMatcherChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_- ,|"

// exactMatcherNarrowChars are the narrower charset of FileChanged and StopFailure
// (§2): letters, digits, "_", "|" only. A hyphen, space or comma in those two
// events therefore takes the regexp path, and only "|" separates values.
const exactMatcherNarrowChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_|"

// Spec describes how one event matches (§2.1): which payload field the matcher is
// tested against, and whether the event supports a matcher at all.
type Spec struct {
	// Event is the hook event name.
	Event string
	// MatchField is the payload field name the matcher filters on, for the
	// operator-facing display. Empty means the event does not support a matcher.
	MatchField string
	// matcherValueKey is this package's internal accessor name for that field.
	// It is separate from MatchField because the table has to name fields this
	// build's Input type does not model (background_task type, command_name, …)
	// and a reader should be able to tell a display name from an accessor.
	matcherValueKey string
	// narrowCharset marks the two events with the reduced exact-match charset.
	narrowCharset bool
	// Blockable reports whether exit code 2 blocks on this event (§5.3).
	Blockable bool
}

// specTable is the complete §2.1 table, with §5.3's blockability folded in so the
// runtime never has to consult two tables that could disagree.
//
// Events that the reference lists as "不支持 matcher" (UserPromptSubmit, Stop,
// PostToolBatch, TeammateIdle, TaskCreated, TaskCompleted, WorktreeCreate,
// WorktreeRemove, MessageDisplay, CwdChanged) carry no MatchField and always
// match: §2.1 is explicit that they fire on every trigger.
var specTable = map[string]Spec{
	"SessionStart": {Event: "SessionStart", MatchField: "source", matcherValueKey: "source"},
	"Setup":        {Event: "Setup", MatchField: "trigger", matcherValueKey: "trigger"},
	"SessionEnd":   {Event: "SessionEnd", MatchField: "reason", matcherValueKey: "reason"},
	"Notification": {Event: "Notification", MatchField: "notification_type", matcherValueKey: "notification_type"},
	"SubagentStart": {Event: "SubagentStart", MatchField: "agent_type",
		matcherValueKey: "agent_type"},
	"SubagentStop": {Event: "SubagentStop", MatchField: "agent_type", matcherValueKey: "agent_type"},
	"PreCompact":   {Event: "PreCompact", MatchField: "trigger", matcherValueKey: "trigger"},
	"PostCompact":  {Event: "PostCompact", MatchField: "trigger", matcherValueKey: "trigger"},
	"PreModelSwitch": {Event: "PreModelSwitch", MatchField: "to_model",
		matcherValueKey: "to_model"},
	"PostModelSwitch": {Event: "PostModelSwitch", MatchField: "to_model",
		matcherValueKey: "to_model"},
	"ConfigChange": {Event: "ConfigChange", MatchField: "source", matcherValueKey: "source"},
	"DirectoryAdded": {Event: "DirectoryAdded", MatchField: "source",
		matcherValueKey: "source"},
	"FileChanged": {Event: "FileChanged", MatchField: "file_name",
		matcherValueKey: "file_name", narrowCharset: true},
	"StopFailure": {Event: "StopFailure", MatchField: "error",
		matcherValueKey: "error", narrowCharset: true},
	"InstructionsLoaded": {Event: "InstructionsLoaded", MatchField: "reason",
		matcherValueKey: "reason"},
	"UserPromptExpansion": {Event: "UserPromptExpansion", MatchField: "command_name",
		matcherValueKey: "command_name"},
	"Elicitation": {Event: "Elicitation", MatchField: "server_name",
		matcherValueKey: "server_name"},
	"ElicitationResult": {Event: "ElicitationResult", MatchField: "server_name",
		matcherValueKey: "server_name"},

	"PreToolUse": {Event: "PreToolUse", MatchField: "tool_name", matcherValueKey: "tool_name",
		Blockable: true},
	"PostToolUse": {Event: "PostToolUse", MatchField: "tool_name",
		matcherValueKey: "tool_name"},
	"PostToolUseFailure": {Event: "PostToolUseFailure", MatchField: "tool_name",
		matcherValueKey: "tool_name"},
	"PermissionRequest": {Event: "PermissionRequest", MatchField: "tool_name",
		matcherValueKey: "tool_name"},
	"PermissionDenied": {Event: "PermissionDenied", MatchField: "tool_name",
		matcherValueKey: "tool_name"},

	"UserPromptSubmit": {Event: "UserPromptSubmit", Blockable: true},
	"Stop":             {Event: "Stop", Blockable: true},

	// Documented as "no matcher": these fire on every trigger, so whatever the
	// config wrote in matcher is ignored rather than used to filter them out.
	"PostToolBatch":  {Event: "PostToolBatch", Blockable: true},
	"TeammateIdle":   {Event: "TeammateIdle", Blockable: true},
	"TaskCreated":    {Event: "TaskCreated", Blockable: true},
	"TaskCompleted":  {Event: "TaskCompleted", Blockable: true},
	"WorktreeCreate": {Event: "WorktreeCreate", Blockable: true},
	"WorktreeRemove": {Event: "WorktreeRemove", Blockable: true},
	"MessageDisplay": {Event: "MessageDisplay"},
	"CwdChanged":     {Event: "CwdChanged"},
}

// SpecFor returns the §2.1 spec for an event. An event this build does not know
// gets a zero Spec, which means "matches but cannot block" — the conservative
// reading for an event name that arrived from a newer Claude Code: matching
// everything would run hooks the operator attached to a different event, and
// refusing to match would hide them silently.
func SpecFor(event string) Spec {
	if spec, ok := specTable[event]; ok {
		return spec
	}
	return Spec{Event: event}
}

// Matches reports whether a matcher selects a value for an event, following §2
// and §2.1 of the reference.
//
// The rules, in order:
//
//   - An event that does not support a matcher always matches (§2.1), no matter
//     what the config wrote.
//   - "", "*" and an omitted matcher match everything (§2).
//   - A matcher made only of letters/digits/_/-/space/,/| is an exact match or a
//     comma/pipe separated list of exact matches, with whitespace tolerated
//     around the separators.
//   - FileChanged and StopFailure use the narrower charset (letters, digits, _,
//     |), so a hyphen, space or comma in those events falls through to the regexp
//     path.
//   - Anything else is an unanchored JavaScript-style regexp: the match succeeds
//     if the pattern is found anywhere in value.
//
// A regexp that does not compile cannot select anything. That is the conservative
// direction: a broken pattern must not be read as "match everything", because
// that would run a policy hook on inputs its author never considered.
func Matches(event, matcher, value string) bool {
	spec := SpecFor(event)
	if spec.matcherValueKey == "" {
		return true
	}
	return matchesMatcher(matcher, value, spec.narrowCharset)
}

// matchesMatcher is the charset rule itself, split out so the narrow-charset path
// is testable without an event that this build dispatches.
func matchesMatcher(matcher, value string, narrow bool) bool {
	if matcher == "*" {
		return true
	}
	// An empty matcher matches everything. This is also the case where the config
	// omitted "matcher" entirely: the group decodes to the zero value, which is
	// indistinguishable from an explicit "" and has the same documented meaning.
	if matcher == "" {
		return true
	}

	charset := exactMatcherChars
	if narrow {
		charset = exactMatcherNarrowChars
	}
	if isExactMatcher(matcher, charset) {
		for _, want := range splitExactMatcher(matcher) {
			if want == value {
				return true
			}
		}
		return false
	}

	if len(matcher) > maxRegexLen {
		return false
	}
	re, err := regexp.Compile(matcher)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

// isExactMatcher reports whether every rune of the matcher is in the exact-match
// charset, i.e. whether it stays on the exact path.
func isExactMatcher(matcher, charset string) bool {
	for _, r := range matcher {
		if !strings.ContainsRune(charset, r) {
			return false
		}
	}
	return true
}

// splitExactMatcher splits an exact matcher into its candidate values. Both "," and
// "|" separate values and whitespace around a separator is tolerated (§2), so
// "Edit, Write" and "Edit|Write" are the same list.
func splitExactMatcher(matcher string) []string {
	fields := strings.FieldsFunc(matcher, func(r rune) bool { return r == ',' || r == '|' })
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// matchValue returns the payload value the matcher is tested against for one
// dispatch, per §2.1. The second result is false when the event does not support
// a matcher, which is reported to the caller so the log can say why the matcher
// column is empty rather than look like a matcher that matched nothing.
func matchValue(spec Spec, in Input) (string, bool) {
	if spec.matcherValueKey == "" {
		return "", false
	}
	return payloadField(in, spec.matcherValueKey), true
}

// payloadField resolves a §2.1 matcher field from the payload. Unknown fields fall
// back to the Extra map, which is how a caller supplies an event's own matcher
// field without this package modelling every one of the 33 events' payloads.
func payloadField(in Input, key string) string {
	switch key {
	case "tool_name":
		return in.ToolName
	case "source":
		return in.Source
	case "agent_type":
		return in.AgentType
	case "model", "to_model":
		// §2.1 matches Pre/PostModelSwitch on the canonical name derived from
		// to_model. This build does not dispatch those events, so the field is
		// looked up in Extra rather than modelled: a caller that ever dispatches
		// them supplies it there.
		return in.Model
	}
	if in.Extra == nil {
		return ""
	}
	if v, ok := in.Extra[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
