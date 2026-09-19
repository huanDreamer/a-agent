package claudehook

import (
	"context"
	"encoding/json"
	"time"
)

// Verdict is the aggregated result of one dispatch.
//
// Every field is the caller-facing form of a decision the protocol documents in
// §5.5. Which fields an event can actually produce is not enforced here: the
// aggregation is shared, and an event that has no way to set one simply leaves it
// zero. The field comments name the event each one belongs to, because that is the
// only place the mapping is written down once.
type Verdict struct {
	Ran               bool     `json:"ran"`                // at least one handler ran
	Handlers          int      `json:"handlers"`           // handlers that actually ran
	Skipped           []string `json:"skipped,omitempty"`  // one line per handler that did not run, with the reason
	Failures          []string `json:"failures,omitempty"` // non-blocking errors, verbatim enough to debug
	SystemMessages    []string `json:"system_messages,omitempty"`
	AdditionalContext []string `json:"additional_context,omitempty"`
	// Continue is false when a handler returned continue:false, which §5.4 makes
	// the highest-precedence decision: processing stops entirely, above any
	// event-specific decision field.
	Continue   bool   `json:"continue"`
	StopReason string `json:"stop_reason,omitempty"`
	// Block is set by exit code 2 on a blockable event (§5.2/§5.3), by a top-level
	// decision:"block" (§5.5), or by permissionDecision:"deny" (§8).
	Block              bool   `json:"block"`
	BlockReason        string `json:"block_reason,omitempty"`
	PermissionDecision string `json:"permission_decision,omitempty"` // "allow"|"deny"|"ask"|"defer"|""
	// UpdatedInput is the PreToolUse replacement for the whole tool input object
	// (§8: it replaces, it does not merge).
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	// UpdatedToolOutput is the PostToolUse replacement for the tool result, and
	// HasUpdatedOutput distinguishes "replace it with an empty string" from "do
	// not replace it" — a distinction the protocol needs, because an empty string
	// is a legal replacement value.
	UpdatedToolOutput  string   `json:"updated_tool_output,omitempty"`
	HasUpdatedOutput   bool     `json:"has_updated_output,omitempty"`
	InitialUserMessage string   `json:"initial_user_message,omitempty"`
	SessionTitle       string   `json:"session_title,omitempty"`
	WatchPaths         []string `json:"watch_paths,omitempty"`
	ReloadSkills       bool     `json:"reload_skills,omitempty"`
	Retry              bool     `json:"retry,omitempty"`
}

// Record is one line of the runtime event log, for the console panel.
//
// A record exists for every handler that actually executed, in completion order —
// including async ones, which are recorded at start time so the panel shows a hook
// the moment it fires. Handlers that were skipped appear in Verdict.Skipped
// instead: they produced no execution to log (no exit code, no duration), and a
// record with a fabricated exit code would be worse than no record.
type Record struct {
	At         time.Time `json:"at"`
	Event      string    `json:"event"`
	Matcher    string    `json:"matcher"`
	Value      string    `json:"value"`             // the field the matcher was tested against
	Handler    string    `json:"handler"`           // "command"/"http"/"mcp_tool"/...
	Command    string    `json:"command,omitempty"` // the command/URL/tool, for the "which hook fired" question
	SessionID  string    `json:"session_id,omitempty"`
	ToolName   string    `json:"tool_name,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	ExitCode   int       `json:"exit_code"`
	Blocked    bool      `json:"blocked"`
	Reason     string    `json:"reason,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// MCPCaller invokes a tool on a connected MCP server, for mcp_tool handlers.
type MCPCaller interface {
	// Call returns the tool's text output. server is the configured server
	// name, tool the tool name, input the already-substituted argument object.
	Call(ctx context.Context, server, tool string, input map[string]any) (string, error)
}
