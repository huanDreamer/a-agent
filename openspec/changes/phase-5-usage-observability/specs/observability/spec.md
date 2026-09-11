# Capability: observability

## Purpose

Expose machine-readable health and telemetry so the agent's behaviour can be
watched and alerted on, and so tool activity leaves an audit trail.

## Scope

- `internal/metrics` — the Prometheus collectors.
- `internal/server` — the `/metrics` endpoint.
- `internal/store` — the tool invocation audit log.

## Requirements (MUST)

1. **Metrics endpoint** — the admin server MUST expose a Prometheus text
   exposition at a configurable path (default `/metrics`), and that endpoint
   MUST be servable without authentication so a scraper needs no credentials.
2. **LLM instrumentation** — every model call MUST record: a call counter split
   by provider, model and outcome (`ok`/`error`), a latency histogram, and token
   counters split by prompt/completion.
3. **Tool instrumentation** — every tool invocation MUST record a call counter
   split by tool and outcome, plus a latency histogram.
4. **HTTP instrumentation** — every admin request MUST record a counter split by
   method, route and numeric status, plus a latency histogram.
5. **Rate limiting visibility** — throttled or delayed outbound calls MUST be
   counted, split by component.
6. **Namespacing** — all metric names MUST share the `huan_agent_` prefix and
   carry help text.
7. **Private registry** — instrumentation MUST register on its own registry and
   MUST NOT touch the process-global default registerer, so multiple instances
   (and tests) do not collide.
8. **No panics on registration** — a duplicate registration MUST be reported as
   an error while leaving the usable collectors working.
9. **Non-empty exposition** — starting the service MUST publish a build-info
   gauge, so a scrape before any traffic returns content instead of an empty
   body.
10. **Optional instrumentation** — every observation entry point MUST be a safe
    no-op when observability is disabled (nil receiver), so callers never need a
    nil check and a metrics failure cannot take down the bot or the server.
11. **Audit log** — tool calls MUST be recorded with user, session, tool name,
    arguments, result, error and duration, and the log MUST be queryable
    filtered by tool, user and time window.
12. **Health** — an unauthenticated health endpoint MUST report liveness,
    version and uptime.
13. **Quiet logging** — the HTTP framework's own logger MUST be routed through
    the application logger at warning level, so a normal start does not dump the
    route table.

## Non-goals

- Prometheus Alertmanager rules or Grafana dashboards (the `/metrics` surface is
  the deliverable).
- Distributed tracing / OpenTelemetry spans.
- Long-term metric retention (the store holds usage, not metrics).

## Key interfaces

- `metrics.New`, `metrics.Metrics.ObserveLLMCall`, `ObserveToolCall`,
  `ObserveHTTP`, `IncRateLimited`, `SetBuildInfo`, `Gatherer`, `WriteText`
- `GET /metrics`, `GET /api/health`, `GET /api/audit`
- `store.InvocationEvent`, `store.InvocationFilter`, `store.QueryInvocations`
