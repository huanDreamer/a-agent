# Capability: trace-visualization

## Purpose

Make an agent turn explainable: report each turn's structure (model calls with
token usage, tool invocations with their outcome) to Langfuse, and let an
operator inspect that structure in the admin UI without needing the Langfuse
account's own UI.

## Scope

- `internal/langfuse` — the ingestion client, the read client and the `Tracer`
  adapter.
- `internal/chat` — the `Tracer` seam the runner reports through.
- Admin API under `/api/traces/*`.
- The 链路追踪 tab of the web UI.

## Requirements (MUST)

1. **Optionality** — tracing MUST be off unless explicitly enabled and fully
   configured, and with tracing unavailable every tracer call MUST be an inert
   no-op so the chat path needs no nil checks.
2. **Structure** — a turn MUST be reported as a trace, each model call as a
   generation observation (with its model and token usage), and each tool call
   as a span observation nested under the trace.
3. **Attribution** — a trace MUST carry the session id and user id so a
   conversation can be grouped and usage attributed.
4. **Failures are visible** — a failed tool call or model call MUST be reported
   with an error level and a status message, not silently omitted.
5. **Delivery must not block or break the agent** — reporting MUST be buffered
   and sent off the request path; a full buffer MUST drop the oldest events and
   count them; a delivery failure MUST be counted and logged, never propagated
   into the conversation.
6. **Flush on shutdown** — closing the tracer MUST flush buffered events, so the
   last turn of a session is not lost.
7. **Version tolerance** — the read client MUST tolerate the field-set
   differences between Langfuse versions and MUST NOT fail a response because an
   optional field is absent.
8. **Read proxying** — the trace list and a single trace with its observations
   MUST be readable through the admin API, so the browser never receives the
   Langfuse secret key.
9. **Honest state** — when tracing is off, the trace endpoints MUST say so and
   name the configuration to set, rather than returning 404 or an empty list that
   is indistinguishable from "no traces yet".
10. **Backend outage is distinguishable** — an unreachable Langfuse MUST be
    reported as a distinct upstream failure, not as "disabled" and not as a
    failure of the admin server itself.
11. **Counters** — delivery counters (sent, failed, dropped, queued) MUST be
    exposed so an operator can tell whether traces are actually arriving.
12. **Waterfall rendering** — the UI MUST render a trace's observations as a
    timeline positioned and sized by each observation's own start/end relative to
    the trace, with children nested by parent, and MUST NOT produce invalid
    geometry for a single-observation or zero-duration trace.
13. **Arbitrary payloads** — observation input/output MUST be rendered
    defensively as arbitrary JSON of unknown shape, with the ability to expand.
14. **Testability** — both the ingestion and read paths MUST be testable against
    a fake server, with no real network access in tests.

## Non-goals

- A full Langfuse replacement (no prompt management, datasets, scores).
- Distributed tracing across services / OpenTelemetry.
- Sampling or cost-based trace retention policy.
- Writing traces anywhere other than Langfuse.

## Key interfaces

- `langfuse.Config`, `langfuse.New`, `langfuse.Client`, `langfuse.NewTracer`
- `langfuse.TraceEvent`, `langfuse.SpanEvent`, `langfuse.GenerationEvent`,
  `langfuse.Usage`, `langfuse.Stats`
- `langfuse.TraceSummary`, `langfuse.Observation`, `langfuse.TraceDetail`,
  `langfuse.TraceFilter`, `langfuse.ErrDisabled`
- `chat.Tracer` (the seam the runner depends on)
- `server.TraceReader`, `GET /api/traces`, `GET /api/traces/{id}`,
  `GET /api/traces/status`
