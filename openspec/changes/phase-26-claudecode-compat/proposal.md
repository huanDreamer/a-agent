# Phase 26 — ClaudeCode compatibility mode (ClaudeCode 兼容模式)

## Summary

Let this agent run on a configuration its operator already has. Claude Code keeps
its endpoint, credential, models and hooks in `~/.claude/settings.json`; this
change reads that file, adds a switch in 设置 → ClaudeCode that makes every
conversation run on that model configuration, and dispatches the hooks registered
there on the five events this agent actually has — with the hook protocol
implemented as documented, not approximated.

The mode is visible from the conversation list: a badge under 新建对话 says which
mode is active and which model it is running on.

## Why

The gap is a second configuration that drifts:

- **Two answers to "which model".** A machine set up for Claude Code already names
  an endpoint, a bearer token and four model tiers. Making the operator transcribe
  those into `llm.providers` produces a copy that is correct on the day it is
  written and wrong the next time the settings file changes — and the failure is
  silent, because both configurations look plausible.
- **Hooks are the interesting half.** `~/.claude/settings.json` is where a real
  safeguard lives: a formatter after every edit, a policy check before a command,
  a logger on every tool call. Those hooks have no equivalent here at all, so a
  user moving between the two tools gets the safeguards in one and not the other.
  The mode closes that asymmetry in the only way that keeps one source of truth.
- **The protocol is documented, and the document is in the repository's own
  reach.** `~/.claude/hooks/HOOKS-REFERENCE.md` exists precisely to be implemented
  by a compatible tool: 33 events, five handler types, exact `exit 2` semantics,
  a JSON decision contract, matcher rules that differ per event. Approximating it
  would produce hooks that appear to work and fire on the wrong things.
- **An Anthropic-shaped endpoint is not callable today.** `ANTHROPIC_BASE_URL`
  points at an Anthropic **Messages** API (e.g. `https://api.deepseek.com/anthropic`),
  and every provider in this build speaks OpenAI chat-completions. Reading the
  settings file without the protocol would be a switch that changes a label.

## What this changes

| Area | Change |
| :--- | :--- |
| `internal/llm` | A second wire protocol: `anthropic-messages` (`anthropic.go`), dispatched by `Provider.Kind`. Streaming, tool calls, usage, multimodal, both credential styles |
| `internal/claudehook` (new) | The hook runtime: config model, matcher semantics, `command`/`http`/`mcp_tool` handlers, stdout/exit-code contract, per-event decisions, run log |
| `internal/claudecode` (new) | `settings.json` reading, the `env` → model projection, the switch, the catalog provider source, the panel payload |
| `internal/chat` | A `Hooks` seam: `PreToolUse` / `PostToolUse` around every tool call, `Stop` when a turn is about to answer |
| `internal/server` | `/api/claudecode` routes, the four dispatch points, the tool-name translation, the catalog's extra-provider source |
| `internal/config`, `internal/store` | The `claudecode.*` section and the switch's persistence in `app_settings` |
| `web/` | 设置 → ClaudeCode panel, the sidebar mode badge, the shared state the two read |
| `docs/` | `docs/claudecode.md` plus the console reference section |

## Explicit non-goals

1. **The other 28 events.** They are parsed and displayed, marked as having no
   trigger point here. Inventing triggers (a `Notification` this agent never
   raises, a `PreCompact` that fires at the wrong moment) would be worse than
   saying so.
2. **`prompt` and `agent` handlers, and `if`.** All three need a subsystem this
   build does not have (an LLM judge; permission-rule evaluation). Each is marked
   in the table *and* recorded as skipped at runtime, never silently run or
   silently dropped.
3. **Writing to `settings.json`.** It is Claude Code's file. A compatibility mode
   that became its second author would be the drift this change exists to remove.
4. **Project-level settings.** Claude Code also merges `.claude/settings.json` and
   `.claude/settings.local.json` per directory, which this process would have to
   resolve per conversation.
5. **The other entry points.** `chat`, `run` and the Feishu bot keep their own
   configuration; only `admin serve` (where the console lives) has the mode.
