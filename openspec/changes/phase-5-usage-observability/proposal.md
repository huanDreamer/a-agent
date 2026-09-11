# Phase 5 — Usage statistics and observability

## Summary

Make consumption and behaviour visible. Phase 1 recorded raw LLM calls and
Phase 2 recorded tool invocations, but nothing could answer "who spent what, on
which model, and when" — there was no attribution, no aggregation, no cost, and
no way to look at any of it.

This change adds per-user attribution, aggregation queries, a configurable price
table, a single-user authenticated admin HTTP API, Prometheus metrics, an audit
log view, and a Vue web UI embedded in the binary.

## Why

Without this, an operator running the agent cannot tell:

- how many tokens (and how much money) a user or model consumed;
- whether a spike is a single session or a broad increase;
- which tools ran, and which failed;
- whether the service is healthy at all.

Phase 6 (production hardening) also needs the health and metrics surface that
this phase introduces.

## What Changes

### ADDED

- `internal/store/analytics.go` — `UsageWindow`, `UsageTotals`, `UsageGroupRow`,
  `UsageDayRow`, `ProviderModelGroup` and six aggregation queries (totals,
  by-user, by-model, by-provider, by-provider-and-model, by-day) plus a recent
  listing. One shared WHERE/projection builder keeps filtering consistent.
- Migration v3 — a `user_id` column on `usage_logs` and `tool_invocations` with
  indexes, so usage can be attributed to an end user.
- `internal/pricing/` — a `(provider, model)` price table resolved from config
  with `provider/model` → `model` → `provider` → fallback precedence, and
  `Cost`/`CostOf` for accurate per-call pricing.
- `internal/metrics/` — eight Prometheus collectors (LLM calls/latency/tokens,
  tool calls/latency, HTTP requests/latency, rate-limit hits) plus a
  `build_info` gauge, on a private registry.
- `internal/server/` — the admin service: bcrypt single-user login with
  HttpOnly session cookies, login throttling, the usage/audit/skills REST API,
  the `/metrics` exposition, and the embedded web UI with an SPA fallback.
- `web/` — a Vue 3 + Vite admin UI (dashboard, by-model, by-user, recent calls,
  audit log, skills) built into `internal/server/webui/dist/` and embedded.
- `huan-agent admin serve` and `huan-agent admin set-password` commands.

### CHANGED

- `store.UsageFilter` / `InvocationFilter` gain `UserID`, `Since`, `Until`, and
  the record types gain JSON tags defining the API wire shape.
- The Feishu bot records LLM latency/outcome/token metrics per call.
- Config gains `server.enable`, `server.session_ttl_minutes`,
  `server.metrics_enable`, `server.metrics_path`, `pricing.*` and `admin.*`.

## Design decisions

- **Cost is summed per (provider, model) pair**, not looked up once for the
  whole window: a window mixes models, and the same model name can cost
  differently per provider. A single lookup would understate or zero the total.
- **`/metrics` always has content.** Prometheus omits label vectors with no
  series, so a freshly started process exposed an empty body and an operator
  could not distinguish "no traffic" from "broken". A `build_info` gauge is
  published at startup.
- **The admin server is opt-in** (`server.enable`) and bound to localhost by
  default, because it exposes usage data.
- **Skill toggles never edit skill files** — they are recorded in a small JSON
  override file beside the database.
- **Unpriced is reported as unpriced**, not as zero cost (`Cost.Priced`).

## Impact

- **Schema**: migration v3 is additive (`ADD COLUMN` with a default); existing
  rows read back with an empty `user_id`.
- **Security**: the admin API is authenticated; the session cookie is HttpOnly
  and `SameSite=Lax`, and `Secure` is set only over TLS so localhost works.
  Login attempts are throttled after repeated failures.
- **Compatibility**: no existing command or config key changes meaning; the
  admin server is off unless enabled.
