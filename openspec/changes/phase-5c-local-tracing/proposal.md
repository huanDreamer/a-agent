# Phase 5c — Built-in trace store

## Summary

Make the trace backend part of the product instead of an external service:
persist each turn's structure (model calls, tool calls, token usage) in the
agent's own SQLite database, serve the 链路追踪 tab from it live as a turn runs,
connect it to the conversation the trace belongs to in both directions, and keep
Langfuse as an optional write-only mirror for its eval and prompt-management
features.

## Why

Phase 5b gave the console a real trace UI, but the data lived in a Langfuse
server. Four problems with that:

1. **Not actually built-in.** The console renders the traces, but a second
   service must exist — cloud or self-hosted — before the tab shows anything.
   The single-binary, single-database shape that the rest of the product has
   does not extend to observability.
2. **Dead by default.** `langfuse.enable` defaults to false, so out of the box
   the tab is an instruction manual ("配置这四项") rather than a feature. Every
   other capability added in Phase 5 and 6 works on a fresh install.
3. **The same turn is already half-recorded, twice, in neither place
   completely.** `usage_logs` and `tool_invocations` hold per-call tokens and
   durations; `chat_messages` holds the transcript. None of them holds the
   *structure* — which generation belonged to which step, which tool call
   belonged to which turn — so nothing can be reassembled into a timeline.
4. **A trace you have to go looking for is a trace you never read.** The panel
   only updates when someone presses 重新加载, and nothing connects an answer in a
   conversation to the trace that produced it: after a bad answer the operator
   has to guess which trace to open, then hunt for it. Observability that is not
   reachable from the thing being observed does not get used.

## What Changes

### ADDED

- `internal/tracing/` — the neutral observability vocabulary (`TraceSummary`,
  `Observation`, `TraceDetail`, `TraceFilter`, `Usage`, `Stats`) moved out of
  `internal/langfuse`, a local `Recorder` that implements both `chat.Tracer`
  and `server.TraceReader`, and a `Multi` fan-out tracer.
- `internal/store/trace.go` — trace and observation persistence (migration v7),
  the tree query, and bounded pruning.
- Config section `tracing.*` (enable, retention_days, max_traces,
  prune_interval).
- `internal/tracing.Hub` — an in-process, bounded change-notification hub the
  recorder publishes to after each write, plus `GET /api/traces/stream` as a
  Server-Sent Events endpoint that forwards those notifications.
- Trace provenance: `chat.Result.TraceID`, `chat.Event.TraceID` on the `done`
  frame, and a `chat_messages.trace_id` column, so a conversation carries the
  trace that explains each of its answers.
- Admin API: `DELETE /api/traces` to clear the store.
- Web UI: a live trace list and live trace detail, a 链路 affordance on each
  assistant message that opens its trace, and a back-link from a trace to its
  conversation.

### CHANGED

- `internal/langfuse` becomes an **exporter**: it keeps the ingestion client and
  its `chat.Tracer` adapter, and loses the read client and the shared types.
- `server.TraceReader` is served by the local store, so `/api/traces` and
  `/api/traces/{id}` no longer touch the network.
- `/api/traces/status` reports the serving backend and the mirror state
  separately, instead of a single `enabled`.
- The 链路追踪 tab distinguishes "not enabled" from "enabled, nothing recorded
  yet", shows the serving backend, keeps a Langfuse deep link only when a host is
  configured, and follows a running turn as it happens.
- `chat.Runner` reports its trace id outwards; `internal/server/chat.go` persists
  it with the assistant message and streams it on the `done` frame.

## Design decisions

- **Local owns reads; Langfuse is write-only.** Merging two trace sources into
  one list means reconciling two id spaces, two sort orders and two paging
  models for no user benefit. The console reads the database; a mirrored
  deployment keeps a "在 Langfuse 中打开" link for the features the local store
  deliberately does not have.
- **Synchronous local writes, not the buffered queue.** The Langfuse client
  buffers because it writes over the network; a local insert has no such
  latency, and buffering would buy nothing while adding a second class of
  "trace never arrived" bugs. This is cheap only because the events are rare —
  a turn produces one trace, a handful of generations and a few spans across
  seconds of model time, not a stream.
- **Writes are bounded, because the store is single-writer.** `store.Open`
  pins SQLite to `SetMaxOpenConns(1)`, so a trace write can queue behind
  another statement on the same connection. Each write therefore carries a
  short deadline and counts its own failure; a locked or slow database must
  degrade the trace, never stall the turn. Terminal writes (`EndTrace`,
  `EndSpan`, `EndGeneration`) use a detached context, matching the existing
  precedent that a disconnected client still persists its partial answer.
- **Pruning is a requirement, not a nicety.** Langfuse used to absorb unbounded
  growth; once the database is the store, an unpruned trace table is a bug that
  surfaces as a full disk months later. Pruning runs in bounded batches for the
  same single-connection reason.
- **The types move to `internal/tracing`.** Only three files outside the package
  reference `langfuse.*` (`cmd/huan-agent/admin.go`, `internal/server/traces.go`
  and its test), so the move is cheap — and without it the local backend would
  have to import the external client package to describe its own data.
- **Live updates are a notification, not a second event model.** The stream says
  which trace changed; the panel re-reads the store. The alternative — streaming
  the observation payloads themselves — would duplicate the snake_case contract
  the UI already consumes, add a second thing to keep in sync, and turn a dropped
  frame into a wrong waterfall instead of a stale one.
- **Server-Sent Events again, over `fetch` rather than `EventSource`.** The chat
  path already streams SSE and `web/src/sse.js` already frames it by hand; a
  `GET` endpoint plus the same parser keeps one SSE implementation with one
  auth-failure behaviour. `EventSource` would bring free reconnection but a
  second, differently-behaving client that cannot share the existing
  `setUnauthorizedHandler` path.
- **Real-time matters most where there is no browser turn to watch.** The web
  chat already shows its own progress; the Feishu and CLI paths do not, and once
  task group 7 brings them onto the same seam, the live trace view is the only
  way to watch them at all. That is the case the stream is really for.
- **The trace id travels with the message, not with the trace.** The reverse
  direction is already free (a trace carries `session_id`); the forward
  direction is not, because the runner currently keeps its `traceID` local and
  discards it. Persisting it on the assistant message is what makes a
  conversation navigable months later, and it is one column rather than a join
  table because a message has at most one trace.

## Impact

- **Schema**: migration v7 is additive — two new tables with indexes, plus one
  `chat_messages.trace_id` column. No existing table is rewritten, and pre-change
  messages carry an empty id, i.e. "no trace recorded" rather than a broken link.
- **Config**: new `tracing.*` section. `tracing.enable` defaults to **true**,
  unlike `langfuse.enable` — it has no external dependency and no per-trace cost,
  and a trace table is cheaper than the confusion of a tab that never works.
- **Behaviour change**: the trace list shows what the local store recorded, so
  traces previously sent to Langfuse are no longer listed in the console. They
  remain in Langfuse and stay reachable through the deep link. Existing
  Langfuse history is **not** migrated into the local store.
- **Coverage gap made explicit**: today only web-chat turns report to the
  tracer. `internal/agent` (the CLI and Feishu path — the primary interface per
  `PLAN.md`) has no tracer call sites at all. An observability backend that
  cannot see Feishu turns is half-built, so this change includes bringing that
  path onto the same seam; the task group is separable if it grows.
- **Compatibility**: the `/api/traces` response shape is unchanged apart from one
  added `backend` field, so `TraceView.vue` keeps working against an older
  server. The stream endpoint is new, so an older server simply has no live
  updates — the panel must degrade to reading on demand rather than assume it
  (requirement 19 of the amended spec).
- **Tests**: the local read and write paths are testable against the in-process
  store, so the fake Langfuse server is no longer required for the default
  configuration. The stream is testable in-process for the same reason, but its
  subscriber-drop and late-attach behaviour needs a real time-bounded test.
