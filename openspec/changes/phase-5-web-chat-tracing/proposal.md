# Phase 5b — Web chat and Langfuse tracing

## Summary

Turn the admin UI from a read-only dashboard into a working agent client: a real
chat interface with session management, per-conversation model selection,
streamed answers that expose the model's reasoning and every tool call, plus
Langfuse tracing with an in-app trace visualisation.

## Why

Phase 5 delivered usage statistics and a dashboard, but the only way to actually
talk to the agent was the `chat` CLI or Feishu. Three concrete gaps:

1. **No conversation surface.** The admin UI could show what had been spent but
   not let anyone use the agent, which made it a report rather than a tool.
2. **Invisible work.** The agent's most interesting behaviour — reasoning and
   tool calls — was discarded. The IM bot shows a placeholder and a final answer;
   nothing exposed *how* the answer was reached.
3. **No tracing.** When an answer was slow or wrong there was no way to see the
   sequence of model calls and tool invocations behind it, and no way to inspect
   token usage per step.

## What Changes

### ADDED

- `internal/chat/` — a streaming conversational runner that emits structured
  events (step start, reasoning delta, text delta, tool call, tool result,
  usage, done, error) and a narrow `Tracer` interface it reports through.
- `internal/langfuse/` — a client for the Langfuse ingestion and read APIs
  (batched background delivery, tolerant decoders) plus a `Tracer` adapter.
- `internal/store/chatsession.go` — sessions and messages persisted in SQLite
  (migration v4), with cascade delete and a full-text-free but ordered history.
- Admin API: `/api/chat/models`, `/api/chat/sessions` (CRUD + clear), and
  `POST /api/chat/sessions/{id}/messages` as a **Server-Sent Events** stream.
- Admin API: `/api/traces`, `/api/traces/{id}`, `/api/traces/status`, proxied so
  the Langfuse secret key never reaches the browser.
- Web UI: a 对话 tab (session sidebar, model selector, streaming transcript with
  reasoning panels and tool cards, stop/clear/rename/delete) and a 链路追踪 tab
  (trace list and an observation waterfall).

### CHANGED

- `llm.Registry` gains `GetWithModel` (a model override per conversation) and
  `Catalog` (the selectable providers).
- `huan-agent admin serve` builds the chat wiring and the tracing client, and
  both fail soft: a missing model or an unreachable Langfuse leaves the rest of
  the admin server running.

## Design decisions

- **Server-Sent Events, not WebSocket or polling.** Reasoning and tool calls are
  only interesting as they happen, so partial output is the point. SSE works over
  the existing authenticated cookie, needs no second protocol, and — verified
  against Hertz — genuinely flushes per frame rather than buffering the response.
  The chat model is streamed through eino's stream reader and reassembled with
  `schema.ConcatMessages`, which is what allows tool-call deltas to be
  reconstructed correctly.
- **The runner is separate from `internal/agent`.** `internal/agent` wraps eino's
  ReAct flow for the IM bot, which returns one final message; the web UI needs
  per-step visibility, so the loop is written explicitly over the streaming API.
- **A failed tool is not a failed turn.** The error is streamed to the UI, recorded
  in the audit log, and fed back to the model as the tool observation so it can
  adapt — which is what a user expects from an agent.
- **Reasoning and tool metadata are stored but never replayed to the model.**
  They are display-only; replaying an assistant `tool_call` without its paired
  tool result is precisely what makes providers reject a request, a failure this
  project has already hit once.
- **Tracing is fail-soft.** With no Langfuse configuration every tracer call is a
  no-op, and a Langfuse outage answers the trace endpoints with an explanatory
  200/502 rather than breaking the UI.

## Impact

- **Schema**: migration v4 is additive (two new tables).
- **Config**: new `chat.*` (enable, max_steps, history_limit, system_prompt) and
  `langfuse.*` (enable, host, keys, environment, release) sections. Chat is on by
  default; tracing is off until configured.
- **Behaviour**: the admin UI can now spend LLM tokens, which is why the chat
  feature has its own enable switch.
- **Compatibility**: no existing CLI command or endpoint changes meaning.
