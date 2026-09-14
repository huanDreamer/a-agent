# Capability: local-tracing

## Purpose

Record what an agent turn actually did — which model calls it made, which tools
it invoked, what each cost in tokens and wall time — in the agent's own
database, so the console can explain a turn with no external observability
service, no network access and no account.

## Scope

- `internal/tracing` — the shared vocabulary, the `Recorder`, the `Multi`
  fan-out.
- `internal/store/trace.go` — persistence, the tree query, pruning.
- `internal/chat` and `internal/agent` — the `Tracer` call sites that feed it.
- Config section `tracing`.
- Admin API `/api/traces*`.

## Requirements (MUST)

1. **Built-in by default** — with no observability configuration at all, a fresh
   install MUST record and display traces. The feature MUST NOT require a second
   service, an outbound connection or an account.
2. **Structural record** — a turn MUST be persisted as one trace; each model call
   MUST be persisted as a generation observation carrying its model name and
   token usage; each tool call MUST be persisted as a span observation carrying
   its arguments, its result and its outcome.
3. **Attribution** — a trace MUST carry its session id and user id, and MUST
   carry timestamps sufficient to order traces by start time.
4. **Reconstructible tree** — each observation MUST record its trace and, when a
   parent is known, its parent observation, so the existing waterfall can nest
   children without heuristics.
5. **Failures are visible** — a failed tool or model call MUST be persisted with
   an error level and its status message, not omitted or flattened into a
   success.
6. **Tracing never breaks a turn** — a write failure MUST be logged and counted
   and MUST NOT be propagated to the caller or fail the conversation. Every write
   MUST be bounded by a deadline so a locked or slow database degrades the trace
   rather than stalling the agent.
7. **Offline reads** — the trace list and a single trace with its observations
   MUST be readable with no network access.
8. **Honest state** — the API MUST distinguish three states: tracing disabled,
   tracing enabled with nothing recorded yet, and a store read failure. The empty
   list MUST NOT be used to mean all three.
9. **Backend transparency** — the status endpoint MUST report which backend
   serves the list and whether a mirror is configured, as separate facts.
10. **Bounded retention** — the store MUST prune by age and by total trace count,
    both configurable, running at startup and periodically, and MUST report the
    number pruned. Pruning MUST NOT delete a trace that is still being written.
11. **Bounded pruning cost** — pruning MUST delete in bounded batches, so a large
    cleanup cannot monopolise the single SQLite connection.
12. **Clearable** — an operator MUST be able to delete all traces through the
    admin API, and doing so MUST also delete the observations belonging to them.
13. **Counters** — written, failed and pruned counts MUST be exposed, so an
    operator can tell whether recording actually works.
14. **Optional mirror** — when Langfuse is configured, the same turn MUST also be
    delivered to it, and a mirror failure MUST NOT affect the local record or the
    conversation.
15. **No duplicate work on the hot path** — recording MUST NOT serialise the
    conversation: no write may block for the duration of a model call, and the
    cost of recording MUST NOT grow with the size of the transcript.
16. **Testability** — all local read and write paths MUST be testable against an
    in-process store, with no network access.
17. **Partial traces are readable** — a trace that is still running MUST be
    readable, with the observations recorded so far and no end time on the ones
    still open, so a reader that attaches mid-turn sees the state as it is
    instead of nothing.
18. **Live notification** — the recorder MUST publish a change notification to
    in-process subscribers whenever a trace or observation is written or closed,
    so a reader can follow a turn without polling the database. Publishing MUST
    NOT block the write or the turn: a subscriber that is slow, full or gone MUST
    be dropped or resynchronised, never buffered without bound.
19. **A notification is a hint, not the payload** — a notification MUST identify
    the affected trace and MUST NOT be the only carrier of the record. State MUST
    be reconstructible from the store alone, so a lost notification costs
    freshness, never correctness.
20. **Conversation provenance** — the trace id of a turn MUST be persisted with
    the assistant message it produced and MUST survive a reload, so a
    conversation can be navigated to the trace that explains it. A turn that
    produced no trace MUST store no id rather than a placeholder that resolves to
    nothing.

## Non-goals

- Evaluation, datasets, scores, annotations or LLM-as-judge (Langfuse's
  remaining justification).
- Prompt management.
- OpenTelemetry export or distributed tracing across processes.
- Sampling, or any retention policy based on cost rather than age and count.
- Migrating existing Langfuse history into the local store.
- Full-text or semantic search over recorded payloads.
- A query language, dashboards or alerting on top of the store.
- Replaying history through the live stream (see requirement 18: the stream is
  push-only and carries no backlog).

## Key interfaces

- `tracing.Config`, `tracing.NewRecorder`
- `tracing.TraceSummary`, `tracing.Observation`, `tracing.TraceDetail`,
  `tracing.TraceFilter`, `tracing.Usage`, `tracing.Stats`
- `tracing.Multi` (fan-out), `tracing.ErrDisabled`
- `tracing.Hub`, `tracing.Subscribe`, `tracing.Change`
  (`Change.TraceID`, `Change.Kind`, `Change.At`)
- `store.RecordTrace`, `store.RecordObservation`, `store.EndTrace`,
  `store.EndObservation`, `store.ListTraces`, `store.GetTrace`,
  `store.PruneTraces`, `store.ClearTraces`
- `store.TracePrunePolicy` (age, count)
- `chat.Tracer` (unchanged seam)
- `chat.Result.TraceID`, `chat.Event.TraceID` (set on `done`)
- `store.ChatMessage.TraceID` (migration v7 column)
- `server.TraceReader` (unchanged seam), `server.TraceStream`
- `GET /api/traces/stream`
