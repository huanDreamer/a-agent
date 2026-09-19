package claudehook

import (
	"encoding/json"
	"regexp"
	"strings"
)

// bracedPlaceholder matches the ${NAME} form, which is what §3 documents for path
// placeholders ("${CLAUDE_PROJECT_DIR}"), for mcp_tool input values
// ("${tool_input.file_path}") and for the header interpolation examples.
var bracedPlaceholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.]*)\}`)

// anyPlaceholder additionally matches the unbraced $NAME form, which §3 documents
// for HTTP header values ("$VAR_NAME or ${VAR_NAME}").
//
// The two forms are one alternation in ONE pattern, with the braced form first, so a
// single left-to-right scan suffices and substituted text is never rescanned — a
// value that contained "$SOMETHING" would otherwise be expanded a second time,
// letting the payload influence the result.
var anyPlaceholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// substituteVars replaces every ${NAME} reference in value, resolving the name
// against the payload first and extra second.
//
// Only the braced form is substituted here. That is not an oversight: §3 documents
// "${...}" for command and mcp_tool expansion, and leaving bare "$NAME" alone is
// what keeps a shell-form command working — the expansion happens before sh sees the
// string, so rewriting "$X" in `X=hello; echo $X` would break the command's own
// variable use. Header values are the one place §3 documents the bare form, and they
// use substituteAnyVar instead.
//
// A name that resolves to nothing becomes the empty string. That is the documented
// rule for the case it covers — §3 says an unlisted variable in a header becomes the
// empty string — and it is also the only rule that can be applied uniformly: a
// missing payload field (§10 shows many fields are routinely absent) must not leave
// a literal "${tool_input.file_path}" in a path handed to a tool.
//
// Anything that is not a plausible variable name is left untouched: a "$" that does
// not start a name (a price, a shell snippet, a regexp fragment) is content rather
// than a reference, and blanking it would corrupt the value.
//
// The payload view is the marshalled Input, so ${tool_input.file_path} and every
// other documented path resolve against exactly the JSON a command hook would have
// received on stdin. Resolving through the same serialisation is what keeps a hook's
// stdin and an mcp_tool hook's arguments from disagreeing about a value.
func substituteVars(value string, in Input, extra map[string]string) string {
	return replaceVars(value, bracedPlaceholder, in, extra)
}

// substituteKnownVars replaces only the ${NAME} references whose name is present in
// vars, leaving every other reference exactly as written.
//
// It exists for path expansion (§3's ${CLAUDE_PROJECT_DIR} and friends), where the
// two behaviours differ in a way that matters: an unset CLAUDE_PLUGIN_DATA should
// leave a visible "${CLAUDE_PLUGIN_DATA}" in the path, so the hook fails with a
// message naming the placeholder, rather than becoming "/x.js" and pointing at a
// path nobody configured. Header interpolation is the case where an unknown name is
// documented to become the empty string, and it uses substituteAnyVar.
func substituteKnownVars(value string, vars map[string]string) string {
	if !strings.Contains(value, "$") || len(vars) == 0 {
		return value
	}
	return bracedPlaceholder.ReplaceAllStringFunc(value, func(match string) string {
		if resolved, ok := vars[placeholderName(match)]; ok {
			return resolved
		}
		return match
	})
}

// substituteAnyVar is substituteVars for HTTP header values, where §3 documents both
// "$VAR_NAME" and "${VAR_NAME}". The two forms are resolved in one pass for the
// reason given on anyPlaceholder.
func substituteAnyVar(value string, extra map[string]string) string {
	// No payload: §3 makes header interpolation read environment variables only, so
	// a header value cannot reach into the hook payload.
	return replaceVars(value, anyPlaceholder, Input{}, extra)
}

// replaceVars is the shared implementation.
func replaceVars(value string, pattern *regexp.Regexp, in Input, extra map[string]string) string {
	if !strings.Contains(value, "$") {
		return value
	}
	var view map[string]any
	// The payload is only rendered when a reference actually needs it, so a header
	// full of $PATH-style references does not pay for marshalling the whole payload
	// on every request.
	rendered := false
	return pattern.ReplaceAllStringFunc(value, func(match string) string {
		name := placeholderName(match)
		if name == "" {
			return match
		}
		if !rendered {
			view = payloadView(in)
			rendered = true
		}
		if resolved := lookupPath(view, name); resolved != "" {
			return resolved
		}
		if extra != nil {
			if resolved, ok := extra[name]; ok {
				return resolved
			}
		}
		return ""
	})
}

// placeholderName extracts the variable name from a match of either pattern. The
// braced form is ${NAME}; anything else is $NAME.
func placeholderName(match string) string {
	if strings.HasPrefix(match, "${") {
		return match[2 : len(match)-1]
	}
	return match[1:]
}

// payloadView marshals an Input into a generic map so placeholder paths can be
// walked. A marshal error yields an empty view: the caller is expanding a
// placeholder, and the only alternative to an empty value would be a literal
// placeholder in a path.
func payloadView(in Input) map[string]any {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil
	}
	return view
}

// lookupPath resolves a dotted path ("tool_input.file_path") or a bare field name
// against the payload view. Only string leaves are usable as a substitution; a
// nested object or array is reported as unresolved rather than rendered as JSON,
// because §3 documents substitution into string values only.
func lookupPath(view map[string]any, path string) string {
	if len(view) == 0 || path == "" {
		return ""
	}
	var current any = view
	for _, segment := range strings.Split(path, ".") {
		if segment == "" {
			return ""
		}
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = obj[segment]
		if !ok {
			return ""
		}
	}
	if s, ok := current.(string); ok {
		return s
	}
	return ""
}

// flattenInputVars applies placeholder substitution to every value of a handler
// input map, which is the documented case (§3: a string value may be
// "${tool_input.file_path}"). The result is the map[string]any an MCPCaller takes.
func flattenInputVars(values map[string]string, in Input, extra map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = substituteVars(value, in, extra)
	}
	return out
}
