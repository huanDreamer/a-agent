# Capability: claudecode

## Purpose

Run this agent on Claude Code's own configuration: the model in
`~/.claude/settings.json`'s `env` block, and the hooks registered there — read at
runtime, switched from 设置 → ClaudeCode, and never written back.

## Scope

- `internal/llm/anthropic.go` — the Anthropic Messages protocol adapter, and the
  `Kind`/`AuthStyle` fields that select it.
- `internal/claudehook` — the hook configuration model, matcher semantics, the
  five handler types, the stdout/exit-code contract and the run log.
- `internal/claudecode` — the settings file, the model projection, the mode, the
  catalog's extra-provider source, the panel payload.
- `internal/chat/hook.go`, `internal/chat/runner.go` — the hook seam inside a turn.
- `internal/server/claudecode.go` — the routes, the four dispatch points, the
  tool-name translation, the permission/effort payload fields.
- `internal/config`, `internal/store` — `claudecode.*` and `app_settings.claudecode.mode`.
- `cmd/huan-agent/claudecode.go`, `cmd/huan-agent/admin.go` — construction and wiring.
- `web/` — the panel, the sidebar badge, the shared state.
- `docs/claudecode.md`, `docs/admin.md`.

## Requirements (MUST)

### Reading the settings file

1. **One source of truth** — the mode MUST read `~/.claude/settings.json`
   (`claudecode.settings_path` overrides the location) and MUST NOT write to it.
2. **Tolerant read** — a missing file, a malformed file or a malformed hooks
   entry MUST be reported in the payload rather than raised as a startup failure:
   the conversations keep working, and 设置 → ClaudeCode says what could not be
   read. A malformed group MUST NOT discard the groups that parsed.
3. **No secret leaves the process** — the credential MUST be masked at the Go
   boundary, in every field of every response. No client, log or proxy may be able
   to receive the plaintext by forgetting to mask it.

### The model

4. **Claude Code's precedence** — the model MUST be projected from the `env`
   block with Claude Code's own order: `ANTHROPIC_MODEL`, then
   `ANTHROPIC_DEFAULT_SONNET_MODEL`, then `..._OPUS_...`, then `..._HAIKU_...`;
   `ANTHROPIC_SMALL_FAST_MODEL` fills the Haiku tier only when it is empty.
5. **Credential style** — `ANTHROPIC_AUTH_TOKEN` MUST be sent as
   `Authorization: Bearer`, `ANTHROPIC_API_KEY` as `x-api-key`, and the token MUST
   win when both are set — the distinction Claude Code makes, and the one a
   gateway rejects as an invalid key when it is wrong.
6. **Anthropic Messages** — the request MUST be `POST {base}/v1/messages` (with a
   `/v1` suffix deduplicated), carry `anthropic-version`, hoist system messages
   into the top-level `system` field, merge consecutive tool results into one
   user turn, and translate `tool_use` / `tool_result` / `thinking` blocks and the
   usage counters both ways. Streaming MUST parse the SSE event stream.
7. **Unusable means unusable** — a settings file without an endpoint, a credential
   or any model name MUST NOT be offered as a provider, and turning the mode on
   with one MUST leave every conversation running on the deployment's own model,
   with the reason shown.

### The switch

8. **One switch, one effect** — while the mode is on, **every** conversation MUST
   run on the mode's model, overriding the per-conversation choice; a new
   conversation MUST resolve to it too. Turning it off MUST restore the
   deployment's model.
9. **Persisted, effective next message** — the choice MUST be stored
   (`app_settings.claudecode.mode`, absent = `claudecode.enable` from the config),
   and the switch MUST take effect on the next message without a restart, which
   means the cached per-model runners are dropped.
10. **Visible from the conversation list** — the active mode and the model it runs
    on MUST be shown next to 新建对话, not only inside 设置.

### The hooks

11. **Five events, stated** — exactly `SessionStart`, `UserPromptSubmit`,
    `PreToolUse`, `PostToolUse` and `Stop` MUST be dispatched. Every other
    configured event MUST be parsed, listed, and marked as having no trigger point.
12. **Where they fire** — `SessionStart` on create (`startup`), resume (`resume`)
    and clear (`clear`); `UserPromptSubmit` before the message is persisted;
    `PreToolUse`/`PostToolUse` around every tool call; `Stop` when the turn is
    about to answer.
13. **Matcher semantics** — `""`/`*`/absent MUST match everything; a matcher of
    letters, digits, `_`, `-`, spaces, `,` and `|` only MUST be an exact,
    case-sensitive match or a `,`/`|`-separated list; anything else MUST be an
    unanchored regular expression. `FileChanged` and `StopFailure` MUST use the
    documented narrower character set.
14. **`exit 2` blocks, nothing else does** — on the events that can block, `exit
    2` MUST refuse even when the hook also printed a decision JSON; any other
    non-zero code MUST be a non-blocking failure carrying its stderr as its
    message. A handler that cannot be executed (missing binary) MUST be a
    non-blocking failure, never a silent no-op.
15. **stdout contract** — JSON only when the trimmed output starts with `{` and
    ends with `}`; otherwise plain text, which on `SessionStart` and
    `UserPromptSubmit` MUST reach the model as context.
16. **Decision contract** — `continue`/`stopReason`, `systemMessage`, top-level
    `decision: "block"`, and `hookSpecificOutput` (which MUST carry a matching
    `hookEventName` or be rejected as a non-blocking error) including
    `permissionDecision`, `updatedInput`, `updatedToolOutput`,
    `additionalContext`, `sessionTitle`, `initialUserMessage`, `watchPaths`,
    `reloadSkills` and `retry`.
17. **The decision has consequences** — a blocked `PreToolUse` MUST skip the call
    and hand the reason to the model as the call's failure; `updatedInput` MUST
    replace the arguments before the tool sees them; `updatedToolOutput` MUST
    replace what the model is told the tool returned; `additionalContext` MUST be
    added to the model's context; a blocked `Stop` MUST continue the turn with the
    reason as a system message, bounded by the step budget and reporting whether
    it already blocked once.
18. **A blocked message is not stored** — a refused `UserPromptSubmit` MUST leave
    no message behind and MUST answer the client with the reason.
19. **`SessionStart` effects** — `sessionTitle` MUST rename a conversation that
    has no title (and MUST NOT overwrite one the user set); `additionalContext`
    and `initialUserMessage` MUST be kept for that conversation's next turn and
    dropped on `/clear`.
20. **Handler types** — `command` (exec form when `args` is present, `sh -c`
    otherwise, payload on stdin), `http` (payload as the request body, header
    interpolation restricted to `allowedEnvVars`) and `mcp_tool` (with `${...}`
    substitution) MUST run. `prompt` and `agent` MUST NOT be executed by this
    build, and a handler carrying `if` MUST NOT be run as if it matched.
21. **Parallel, bounded, observable** — every matching handler of an event MUST
    run in parallel; each execution MUST be bounded by its own `timeout` (capped
    by `claudecode.hook_timeout_seconds`) and MUST be recorded in a bounded,
    concurrency-safe run log that the panel and `GET /api/claudecode/events` read.
22. **Honoured kill switches** — `claudecode.hooks: false` and the file's own
    `disableAllHooks: true` MUST each stop every handler while leaving the
    configuration visible, and the panel MUST say which one did it.
23. **The payload fields match the reference** — every event MUST send the fields
    the reference documents for it, with the measured shapes (§4 and §10):
    `tool_response` on `PostToolUse` MUST be the tool's own output object (so
    `.tool_response.stdout` works), plus the measured `duration_ms`;
    `stop_hook_active` MUST appear on `Stop` as an explicit `false`, never
    omitted; `Stop` MUST carry `background_tasks` (the session's own running
    processes, shaped per §8 with the 1000-character cap and marker) and
    `session_crons`, and MUST send `[]` rather than omitting them, because §8
    distinguishes "nothing in progress" from "the field is absent"; one
    `prompt_id` MUST be shared by every event of a turn; and `SessionStart` MUST
    send `session_title` when the conversation has one, so a hook that sets a
    title can check for a user's rename first.
24. **Tool names are translated** — the payload's `tool_name` MUST be the Claude
    Code name for the same tool (`bash` → `Bash`, `read_file` → `Read`,
    `edit_file` → `Edit`, `write_file` → `Write`, …), so that a matcher written
    for Claude Code selects this agent's tool. A tool with no counterpart MUST be
    passed through unchanged rather than mapped onto a name it does not
    correspond to.

### Surfaces

25. **Routes** — `GET /api/claudecode`, `POST /api/claudecode/mode` (accepting
    `{"compat":bool}` or `{"mode":"claudecode"|"native"}`), `POST
    /api/claudecode/reload` and `GET /api/claudecode/events?limit=` MUST exist
    behind the admin session. A deployment without the mode MUST answer
    `available:false` with the reason rather than 404.
26. **No provider row** — the mode's provider MUST be contributed to the model
    catalog at read time and MUST NOT be stored in the database; it MUST NOT
    appear while the mode is off, and a request to save a provider with its id
    MUST be refused rather than creating a keyless duplicate.
27. **Reload** — editing the settings file MUST take effect on the next message
    (mtime check bounded by `claudecode.reload_seconds`), and `reload` MUST apply
    it immediately, including rebuilding the hook engine and dropping built
    models.

## Non-goals

The 28 other events, `prompt`/`agent`/`if`, writing the settings file,
project-level settings files, `CLAUDE_CODE_SUBAGENT_MODEL` and
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` (displayed only), and every entry point other
than `admin serve`. Each of these is stated in the panel's own notes.
