# Capability: trace-visualization (amended)

## Purpose

Make an agent turn explainable: report each turn's structure (model calls with
token usage, tool invocations with their outcome) to the agent's own store, and
let an operator inspect that structure in the admin UI — with no external
observability service required, and with Langfuse available as an optional
mirror.

This spec amends the Phase 5b `trace-visualization` capability. The reporting
seam, the observation model, the UI and the fail-soft posture all stand; what
changes is where traces are **stored and read**. Requirements 1, 5, 7, 8, 9, 10,
11 and 14 are replaced by the versions below, requirements 2, 3, 4, 6, 12 and 13
stand unchanged, the original testability requirement is carried forward as a new
requirement 15 extended to the local store, requirements 16–22 are new (live
updates and navigation between a conversation and its traces), and the non-goals
change as noted.

## Scope

- `internal/tracing` — the shared vocabulary, the local `Recorder` and the
  `Multi` fan-out.
- `internal/langfuse` — the ingestion client and its `Tracer` adapter, now an
  exporter only.
- `internal/chat` — the `Tracer` seam the runner reports through.
- Admin API under `/api/traces/*`.
- The 链路追踪 tab of the web UI.

## Requirements (MUST)

1. **Availability** — tracing MUST work on a fresh install with no external
   configuration, MUST be off only when explicitly disabled, and MUST remain
   inert (a no-op needing no nil checks at the call site) when disabled.
   *(was: off unless explicitly enabled and fully configured)*
2. **Structure** — a turn MUST be reported as a trace, each model call as a
   generation observation (with its model and token usage), and each tool call
   as a span observation nested under the trace. *(unchanged)*
3. **Attribution** — a trace MUST carry the session id and user id so a
   conversation can be grouped and usage attributed. *(unchanged)*
4. **Failures are visible** — a failed tool call or model call MUST be reported
   with an error level and a status message, not silently omitted.
   *(unchanged)*
5. **Storage must not block or break the agent** — a storage write MUST NOT be
   allowed to fail a turn or delay it beyond a short bound; a write failure MUST
   be counted and logged. An optional mirror MUST be buffered and sent off the
   request path, MUST drop the oldest events when its buffer is full, and MUST
   NOT affect the local record when it fails.
   *(was: all delivery buffered and sent off the request path)*
6. **Flush on shutdown** — closing the tracer MUST flush buffered events, so the
   last turn of a session is not lost. *(unchanged; local writes need no flush)*
7. **Mirror tolerance** — the export path MUST tolerate the field-set and
   response-code differences between Langfuse versions, and MUST NOT fail a turn
   or the local record because the mirror rejected or ignored an event.
   *(was: the read client tolerating version differences)*
8. **Secret containment** — the browser MUST never receive the Langfuse secret
   key. Trace reads MUST NOT require it at all, and any deep link the UI renders
   MUST NOT embed credentials. *(was: reads proxied through the server)*
9. **Honest state** — the trace endpoints MUST distinguish tracing disabled,
   tracing enabled with nothing recorded, and a store read failure, and MUST
   name the configuration to change in the disabled case. An empty list MUST NOT
   be used to represent all three. *(was: off vs. empty vs. outage)*
10. **Mirror outage is distinguishable** — an unreachable export target MUST be
    reported as a distinct upstream failure affecting the mirror only, never as
    "tracing off" and never as a failure of the admin server itself.
    *(was: an unreachable read backend)*
11. **Counters** — written, failed, dropped and pruned counts MUST be exposed, so
    an operator can tell whether traces are actually arriving and whether
    retention is discarding them. *(was: sent/failed/dropped/queued)*
12. **Waterfall rendering** — the UI MUST render a trace's observations as a
    timeline positioned and sized by each observation's own start/end relative to
    the trace, with children nested by parent, and MUST NOT produce invalid
    geometry for a single-observation or zero-duration trace. *(unchanged)*
13. **Arbitrary payloads** — observation input/output MUST be rendered
    defensively as arbitrary JSON of unknown shape, with the ability to expand.
    *(unchanged)*
14. **Retention is visible** — an operator MUST be able to see the retention
    policy in effect and to clear the store from the UI. *(was: testability,
    moved to `local-tracing`)*
15. **Testability** — both the recording and reading paths MUST be testable
    against an in-process store, and the mirror against a fake server, with no
    real network access in tests.
16. **Live list** — while the change stream is connected, a new trace MUST appear
    in the list without a manual reload, and a trace that just finished MUST stop
    looking like it is still running.
17. **Live detail** — while the change stream is connected, an open trace MUST
    gain its observations as they are recorded, and a running observation MUST
    acquire its end time when it closes, without the operator reloading anything.
18. **In-progress rendering** — an observation with no end time MUST be rendered
    as still running rather than as a zero-duration bar, and the trace's own
    running state MUST be visible in the list and the detail header, so a
    partial waterfall is never mistaken for a complete one.
19. **Degradation is honest** — if the stream cannot be established, is
    unavailable on the server, or drops, the panel MUST say that updates are not
    live and fall back to reading on demand, rather than presenting stale data as
    current. A manual reload MUST remain available in every state.
20. **Conversation → trace** — an assistant message that produced a trace MUST
    offer a way to open it, which MUST land on 链路追踪 with that trace selected
    and loaded. A message with no trace MUST NOT render a control that leads
    nowhere.
    *(Delivered in two places, because one is precise and only the other is
    discoverable: a 链路 button in the message's own turn footer, and a session
    header button that opens the panel filtered to that conversation. The footer
    entry MUST NOT be gated on the turn having token usage — a failed turn is
    both the likeliest to have none and the one whose trace matters most.)*
21. **Trace → conversation** — a trace carrying a session id MUST offer a way
    back to that conversation, so the two views are navigable in both directions.
22. **Missing target is explained** — opening a trace id that no longer exists
    (pruned, cleared, or a stale link) MUST show an explanatory state naming the
    reason, and MUST NOT show a blank panel or an endless spinner.

## Non-goals

- A full Langfuse replacement (no prompt management, datasets, scores, evals).
- Distributed tracing across services / OpenTelemetry.
- Sampling or cost-based trace retention policy.
- ~~Writing traces anywhere other than Langfuse~~ — inverted: traces are written
  to the local store first, and Langfuse is an optional mirror.
- Serving the console's trace list from a merged local + remote view.
- A second, stream-specific event model: the stream is a change notification and
  the panel re-reads the trace (see `local-tracing`).
- Replacing manual refresh with the stream: reading on demand stays available in
  every state, including while the stream is healthy.

## Key interfaces

- `tracing.Config`, `tracing.NewRecorder`, `tracing.Multi`
- `tracing.TraceSummary`, `tracing.Observation`, `tracing.TraceDetail`,
  `tracing.TraceFilter`, `tracing.Usage`, `tracing.Stats`, `tracing.ErrDisabled`
- `tracing.Hub`, `tracing.Change`
- `langfuse.Config`, `langfuse.New`, `langfuse.Client`, `langfuse.NewTracer`
  (export only: `langfuse.TraceEvent`, `langfuse.SpanEvent`,
  `langfuse.GenerationEvent`)
- `store.PruneTraces`, `store.ClearTraces`
- `chat.Tracer` (the seam the runner depends on)
- `chat.Result.TraceID`, `store.ChatMessage.TraceID`
- `server.TraceReader`, `server.TraceStream`
- `GET /api/traces`, `GET /api/traces/{id}`, `GET /api/traces/status`,
  `GET /api/traces/stream`, `DELETE /api/traces`
- web: `state.focusTrace` (the pending trace a navigation asked for)
- web: `useTraceStream` (the shared subscription the panel and the chat view use)
