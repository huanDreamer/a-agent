# Admin server (Phase 5)

The admin server exposes usage statistics, the audit log, Prometheus metrics and
a web UI. It is **opt-in** and bound to localhost by default, because it serves
usage data.

## 1. Set an admin password

The admin account is a single user with a bcrypt hash in config. Generate one:

```bash
huan-agent admin set-password
```

It prompts twice without echoing (a `--password` flag also exists for scripts,
but a flag value is visible in the process list). The command prints the config
block and the equivalent environment variable:

```yaml
admin:
  username: "admin"
  password_hash: "$2a$10$..."
```

Prefer the environment variable so the hash never lands in a file:

```bash
export HUAN_ADMIN_PASSWORD_HASH='$2a$10$...'
```

> A missing or malformed hash makes the server **fail at startup** rather than
> at the first login, so a misconfiguration is caught immediately.

## 2. Enable the server

```yaml
server:
  host: "127.0.0.1"     # keep on localhost unless you put auth in front
  port: 8080
  enable: true          # off by default
  session_ttl_minutes: 720
  metrics_enable: true
  metrics_path: "/metrics"
```

## 3. Run it

```bash
huan-agent admin serve --config configs/config.yaml
# or
make run-admin
```

Then open <http://127.0.0.1:8080> and sign in.

## Endpoints

Unauthenticated:

| Endpoint | Purpose |
|---|---|
| `GET /api/health` | liveness, version, uptime |
| `GET /metrics` | Prometheus exposition (no auth, so a scraper needs no credentials) |
| `POST /api/login` | exchange the password for a session cookie |

Authenticated (session cookie):

| Endpoint | Purpose |
|---|---|
| `GET /api/me` | current session |
| `POST /api/logout` | revoke the session |
| `GET /api/meta` | active provider/model, version, feature flags |
| `GET /api/usage/summary` | totals + cost for a window |
| `GET /api/usage/by-model` | breakdown by model |
| `GET /api/usage/by-provider` | breakdown by provider |
| `GET /api/usage/by-user` | breakdown by end user |
| `GET /api/usage/by-day?days=30` | daily trend |
| `GET /api/usage/recent?limit=20` | most recent calls |
| `GET /api/audit?tool=&user=` | tool invocation audit log |
| `GET /api/skills` | skills and their enabled state |
| `POST /api/skills/{name}` | `{"enabled": true|false}` |

Usage endpoints accept `since` and `until` (RFC3339) plus `user`, `provider` and
`model` filters. A malformed timestamp returns 400, not 500.

## Cost configuration

Prices are USD per 1000 tokens. Keys are matched most-specific first:
`provider/model` → `model` → `provider` → `fallback`.

```yaml
pricing:
  fallback:
    prompt_per_1k: 0.0
    completion_per_1k: 0.0
  models:
    deepseek/deepseek-chat:
      prompt_per_1k: 0.00014
      completion_per_1k: 0.00028
```

Notes:

- A total is summed over each **(provider, model)** pair in the window, so a
  window mixing models is priced correctly.
- When nothing in a window matches the table, the cost is reported as
  **unpriced** (`"priced": false`, shown as `—` in the UI) rather than as a
  confident `0`. Models missing a price appear as `priced: false` in the
  by-model view, which is how you spot a gap in the table.
- Editing the table re-prices history; nothing is frozen at write time.

## Metrics

All metrics use the `huan_agent_` prefix:

| Metric | Type | Labels |
|---|---|---|
| `huan_agent_llm_calls_total` | counter | provider, model, status |
| `huan_agent_llm_call_duration_seconds` | histogram | provider, model |
| `huan_agent_llm_tokens_total` | counter | provider, model, kind |
| `huan_agent_tool_calls_total` | counter | tool, status |
| `huan_agent_tool_call_duration_seconds` | histogram | tool |
| `huan_agent_http_requests_total` | counter | method, route, status |
| `huan_agent_http_request_duration_seconds` | histogram | method, route |
| `huan_agent_rate_limited_total` | counter | component |
| `huan_agent_build_info` | gauge | version |

The registry is private (the process-global default is never touched), and every
metric is emitted through a nil-safe API, so observability can be disabled
without touching call sites.

## Authentication

The admin UI is a **single-user local console**, so there is no login by
default:

```yaml
admin:
  require_login: false       # default
  allow_insecure_bind: false
```

The only way in is loopback, and a password you retype on every restart protects
nothing. Leave it alone if that describes your setup.

**Turn it on when the console is not only yours.** The agent's tools can read,
write and execute, so a login-free admin reachable from the network is a remote
shell. `huan-agent admin serve` therefore **refuses to start** with
`require_login: false` on a non-loopback address:

```
refusing to serve a login-free admin on "0.0.0.0": it would expose command
execution to the network. Bind 127.0.0.1, set admin.require_login: true, or set
admin.allow_insecure_bind: true if it sits behind another authenticating layer
```

Three ways forward, in order of preference:

1. Bind `127.0.0.1` (the default) and reach it over an SSH tunnel.
2. Set `require_login: true` and a password:
   ```bash
   huan-agent admin set-password          # prompts, no echo
   ```
3. Set `allow_insecure_bind: true` **only** when a reverse proxy, VPN or tunnel
   already authenticates callers — the app is then trusting that layer.

## Web chat (对话)

The admin UI can run conversations against the agent, with the model's reasoning
and tool calls visible as they happen.

```yaml
chat:
  enable: true             # off hides the 对话 tab
  max_steps: 12            # tool-calling iterations per turn
  history_limit: 40        # stored messages replayed to the model
  system_prompt: ""        # empty = built-in default
```

Endpoints (session cookie required):

| Endpoint | Purpose |
|---|---|
| `GET /api/chat/models` | selectable providers/models + the tool list |
| `GET/POST /api/chat/sessions` | list / create |
| `GET/PATCH/DELETE /api/chat/sessions/{id}` | fetch / rename or change model / delete |
| `POST /api/chat/sessions/{id}/clear` | empty the history, keep the session |
| `POST /api/chat/sessions/{id}/messages` | **SSE** stream for one turn |

`POST .../messages` returns `text/event-stream`. Each frame is
`data: {json}\n\n`, with a `type` of `step_start`, `reasoning_delta`,
`text_delta`, `tool_call`, `tool_result`, `usage`, `done`, `error`, or
`stream_end` (always last). A `: ping` comment is sent periodically so an
idle stream is not mistaken for a dead one.

Because `EventSource` cannot POST, the UI reads the body with `fetch` +
`ReadableStream` and parses the framing itself.

Behaviour worth knowing:

- **Reasoning is separate from the answer.** Reasoning models stream their
  thinking in a distinct field; it is shown in a collapsible 思考过程 panel and
  stored separately, never mixed into the answer.
- **A failing tool does not fail the turn.** The error is shown, recorded in the
  audit log, and handed back to the model so it can adapt.
- **Stopping a turn keeps what was produced.** Cancelling the request persists
  the partial answer, so it is still there after a reload.
- **Disconnecting does not lose the answer.** Persistence runs on its own
  context, not the request's.
- **Reasoning and tool metadata are never replayed to the model** on later
  turns — they are display-only. Replaying an assistant tool call without its
  paired result is what makes providers reject a request.
- The first message auto-titles an untitled session.

## Model management (模型管理)

The model catalog lives in the **database** and is the single source of truth for
both surfaces: `设置 → 模型管理` writes it (providers, their fetched model lists,
per-model capabilities, capability bindings) and `对话`'s composer reads it. A
provider added in the console is therefore immediately usable in a conversation,
and both panels group, name and flag models identically.

```yaml
llm:
  default_provider: "deepseek"    # still honoured, and still seeds the catalog
  auto_refresh_models: true       # refetch stale lists in the background at start
  models_cache_ttl_hours: 24      # how long a fetched list counts as fresh
  providers:                      # seeded into the catalog as source=config
    deepseek:
      api_key: ""                 # or HUAN_LLM_PROVIDERS_DEEPSEEK_API_KEY
```

The config file still declares providers; it is seeded into the catalog at
startup. Whether a provider is *enabled* is a runtime choice the console makes,
and it is never overwritten by a restart.

Endpoints (session cookie required):

| Endpoint | Purpose |
|---|---|
| `GET /api/chat/models` | the catalog both surfaces read: models + providers + tools |
| `GET/POST /api/llm/providers` | list / add or edit a provider |
| `PUT /api/llm/providers/{id}/key` | set or clear a stored API key |
| `POST /api/llm/providers/{id}/test` | call `{base_url}/models` and record the outcome |
| `POST /api/llm/providers/{id}/models/refresh` | refetch one provider's list |
| `POST /api/llm/models/refresh-all` | refetch every enabled provider that has a key |
| `GET/PUT/DELETE /api/llm/models` | list / edit capabilities / delete a model |
| `GET/PUT /api/llm/bindings` | the capability → provider/model bindings |

`GET /api/chat/models` answers:

```json
{
  "models": [
    {
      "provider": "deepseek", "provider_name": "DeepSeek",
      "model": "deepseek-chat", "display_name": "DeepSeek Chat",
      "capabilities": ["chat"], "chat_capable": true,
      "default": true, "has_api_key": true
    }
  ],
  "providers": [
    {
      "id": "deepseek", "name": "DeepSeek", "source": "config", "enabled": true,
      "has_api_key": true, "model_count": 3, "enabled_model_count": 3,
      "chat_model_count": 2, "last_fetched_at": "2026-09-12T14:03:06Z",
      "stale": false, "last_error": ""
    }
  ],
  "tools": ["read_file"], "max_steps": 12, "system_prompt": "…"
}
```

Which models are offered:

- only **enabled** providers contribute, and only their **enabled** models;
- a provider's **chat-capable** models are what the chat can run, so they are
  what is offered; when a provider has no chat-capable model at all its models
  are still offered, marked `chat_capable: false`, because capabilities are
  inferred from the model name and a wrong inference must not make a provider
  vanish from the selector;
- a provider with **no API key** is still listed, marked `has_api_key: false`,
  so the composer can warn instead of hiding it;
- ordering is deterministic: providers by name, then models by name.

`POST /api/llm/models/refresh-all` answers `200` with a per-provider report —
`{"results": [{"provider_id", "ok", "models_count", "error"}]}` — even when
providers failed, because a broken key is a result rather than a request error.

Which model a new conversation starts on, in order:

1. `llm.default_provider`, when that provider is still offered and has a
   chat-capable model (preferring the model the config names for it);
2. the `chat` capability binding;
3. the first chat-capable model in catalog order.

`default: true` is set on at most one entry, and on none when nothing is
chat-capable.

Automatic refresh: at startup, every enabled provider that has a key and whose
cached list is stale (never fetched, or older than `models_cache_ttl_hours`) is
refetched in the background — bounded to three providers at a time and to 90s
for the whole pass. A failure is recorded as the provider's `last_error` and
logged at warn; it never stops the pass or the server. The console also triggers
one automatic pass per provider when 设置 is opened and that provider is stale;
`刷新全部` is the manual equivalent.

## Trace visualization (链路追踪)

Every chat turn can be traced to Langfuse: one trace per turn, a generation
observation per model call (with token usage), and a span per tool call. The
admin UI has a 链路追踪 tab that lists traces and renders a trace's
observations as a waterfall.

```yaml
langfuse:
  enable: true
  host: "https://cloud.langfuse.com"     # or self-hosted
  public_key: ""                          # use HUAN_LANGFUSE_PUBLIC_KEY
  secret_key: ""                          # use HUAN_LANGFUSE_SECRET_KEY
  environment: "production"
  release: ""                             # e.g. a git sha
```

Endpoints (session cookie required; the secret key never reaches the browser):

| Endpoint | Purpose |
|---|---|
| `GET /api/traces/status` | enabled?, host, delivery counters |
| `GET /api/traces?limit&page&session&user&name` | recent traces |
| `GET /api/traces/{id}` | one trace with its observations |

Notes:

- **Tracing is fail-soft.** With no configuration every trace call is a no-op,
  the chat path never checks, and the UI is told tracing is off along with which
  config keys to set. A Langfuse outage returns 502 so it is distinguishable from
  "not enabled".
- **Reporting never blocks a turn.** Events are buffered and flushed in the
  background (2s or 20 events); a full buffer drops the oldest and counts it;
  failures are counted and logged, never propagated.
- **`Close` flushes**, so the last turn of a session is not lost.
- The counters on the status strip are the quickest way to tell whether traces
  are actually arriving.

## Web UI

The UI is a Vue 3 SPA in `web/`, built into `internal/server/webui/dist/` and
embedded in the binary — there is nothing to deploy separately.

```bash
make web          # install deps and build into the embedded directory
cd web && npm run dev   # dev server with /api proxied to :8080
```

Tabs: 对话 (chat), 仪表盘 (summary + trend), 按模型, 按用户, 调用记录,
审计日志, 链路追踪, 技能.

## Security notes

- Data endpoints require a session; the cookie is `HttpOnly`, `SameSite=Lax`,
  and `Secure` only over TLS (so plain-HTTP localhost works).
- Failed logins are throttled; while throttled, even the correct password is
  refused.
- Unconfigured (no metrics) or unwired features degrade gracefully instead of
  failing startup.
- The server does **not** terminate TLS itself. For anything other than
  localhost, put it behind a reverse proxy that does, and keep it off the public
  internet.

## Troubleshooting

- **`admin password is not configured`** — run `huan-agent admin set-password`
  and set `admin.password_hash`, or export `HUAN_ADMIN_PASSWORD_HASH`.
- **`password_hash is not a valid bcrypt hash`** — the value is not a bcrypt
  hash (it must start with `$2`). Regenerate it; do not put a plaintext password
  there.
- **401 on every request** — the session expired (`session_ttl_minutes`) or the
  browser is not sending the cookie (check for a proxy stripping `Set-Cookie`).
- **`/metrics` is empty** — only possible if metrics are disabled; a running
  server always publishes `huan_agent_build_info`.
- **Cost shows `—`** — no price matched; add an entry to `pricing.models`.
- **"admin UI is not built" warning** — `internal/server/webui/dist` was not
  built into the binary; run `make web` and rebuild.
- **对话 tab is missing** — `chat.enable` is false, or no `llm.default_provider`
  is configured (the server logs which).
- **Chat replies but shows no thinking** — the model is not a reasoning model, so
  it sends no reasoning content. `deepseek-reasoner` does.
- **Token counts show 0 in chat** — usage must be requested on the stream
  (`stream_options.include_usage`); the shipped provider sets it, so a provider
  that ignores it will report none.
- **链路追踪 says disabled** — set `langfuse.enable` plus host/keys and restart;
  the tab names the keys it wants.
- **链路追踪 shows a 502** — Langfuse was unreachable (check the host and that
  the container is up), which is different from "not configured".
- **Traces never appear in Langfuse** — check the status strip counters: a rising
  `failed` means the keys or host are wrong; `dropped` means the buffer overflowed.
