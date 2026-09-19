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

### One instance at a time

A start is a restart. On startup the long-running commands (`admin serve` and
`serve`) record their pid in `.huan-agent.pid` in the working directory, and the
next start stops whatever that file names before opening the database or binding
the port:

```
INFO  pidfile/pidfile.go  stopping the previous instance        {"pid": 43886, "grace": 10}
INFO  instance.go         previous instance stopped; this process is taking over  {"previous_pid": 43886, "killed": false}
INFO  instance.go         instance registered                   {"pid": 43890, "pid_file": "/path/.huan-agent.pid"}
```

The old instance gets SIGTERM and ten seconds to exit cleanly (in-flight turns
are cancelled and the store is closed); if it is still there after that, SIGKILL.
Two instances therefore never fight over the SQLite file or the port. The two
commands share one pid file, so starting the bot stops the console and vice
versa — the Feishu long connection is a second copy of the same service, and two
of them answer each message twice. On a clean exit the file is removed; after a
crash or a SIGKILL it is left behind and the next start reports it as stale.

If the file names a live process that is **not** huan-agent, that process is left
alone and the reason is logged as a warning — the file is only read, never
trusted:

```
WARN  pidfile/pidfile.go  pid file names a process that is not huan-agent; leaving it running
      {"pid": 44426, "reason": "it is \"sleep\", not this program", "hint": "if this file is stale ... delete it"}
```

Two limits worth knowing:

- **Only the long-running commands are guarded.** `run` is a one-shot command
  with an exit-code contract (`docs/run.md`); guarding it would make two scripts
  kill each other.
- **Two simultaneous cold starts are not prevented.** The guard is a pid file,
  not a lock file, and deliberately so: a pid file survives a crash or a SIGKILL
  and is recognised as stale by the next start, whereas an exclusive lock is
  released automatically in exactly the case where it should not be. The cost is
  the race where both processes look at a file that does not exist yet. Restarts —
  a file that already holds a pid — are always caught.

Use `--pid-file <path>` (or leave it defaulted) to point the guard somewhere
else, e.g. to keep several deployments apart in one directory.

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
| `GET /api/artifacts?session=` | artifacts, newest first; `session` narrows it to one conversation, omitting it lists every session's |
| `GET /api/artifacts/{id}` | one artifact's row |
| `DELETE /api/artifacts/{id}` | delete the row and then the file |
| `GET /api/artifacts/files/{path}` | an artifact's bytes. On the unauthenticated group, and requires the session itself unless `tools.artifacts.public_urls` is on. Served with `nosniff` and, for HTML/SVG, a sandbox CSP. |
| `GET /api/skills` | skills, with their file, size and declared tools |
| `POST /api/skills/{name}` | `{"enabled": true|false}` |
| `GET /api/skills/{name}` | one skill's markdown body |
| `PUT /api/skills/{name}` | write a skill file (`{"description","tools","body"}`) |
| `DELETE /api/skills/{name}` | delete a skill file |
| `POST /api/skills-draft` | 技能写作助手: a description in, a draft skill out |
| `GET /api/mcp/servers` | MCP servers + their live connection state |
| `POST /api/mcp/servers` | create / edit / toggle a server |
| `DELETE /api/mcp/servers/{id}` | delete a server (refused for `source=config`) |
| `POST /api/mcp/servers/{id}/test` | connect, list tools, disconnect |
| `POST /api/mcp/probe` | test an unsaved definition (no write, no register) |
| `POST /api/mcp/reload` | reconnect every enabled server |
| `POST /api/mcp/draft` | 配置助手: a description in, a draft definition out |
| `GET /api/openviking/status` | OpenViking connection, memory counters, document counters, last sync report |
| `POST /api/openviking/sync` | `{"full": false|true}` — publish the workspace (409 when a sync is already running) |
| `POST /api/openviking/save` | `{"title","content","tags","source"}` — store one document |
| `GET /api/openviking/documents` | documents and workspace files already stored |
| `POST /api/openviking/flush` | submit buffered memory now |
| `GET /api/workspaces` | workspaces, the built-in one first, plus the tools each produces |
| `POST /api/workspaces` | `{"name","description","read_only","enable_bash"}` — create one |
| `POST /api/workspaces/preview` | `{"name"}` — where it *would* be created, creating nothing |
| `GET /api/workspaces/{name}` | one workspace |
| `PATCH /api/workspaces/{name}` | edit description and policy (unsent fields are unchanged) |
| `DELETE /api/workspaces/{name}` | unregister (refused for `default`; never deletes files) |
| `GET /api/chat/sessions/{id}/workspace` | which workspace that conversation is in |
| `PUT /api/chat/sessions/{id}/workspace` | `{"name"}` — switch that conversation |
| `GET /api/jobs` | background processes of this server: status, pid, workspace, log file |
| `GET /api/jobs/{id}?from=&max_bytes=` | a window of one job's output, with the offset to continue from |
| `POST /api/jobs/{id}/stop` | `{"signal":"term"|"kill"}` — stop it (409 when it already ended) |
| `DELETE /api/jobs/{id}` | drop the record and delete its log (409 while it runs) |

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

Note that options 1 and 3 both make remote callers look local (see the next
section): with the default `trust_loopback: true`, a password set under option 2
is not asked of anything arriving through that tunnel or proxy. Set
`trust_loopback: false` if the password has to apply there as well.

### Local callers are not asked for it (`trust_loopback`)

Turning `require_login` on does not put a password in front of everyone. A
request that arrives over the loopback interface — `127.0.0.1` or `::1` — is
treated as already authenticated:

```yaml
admin:
  require_login: true
  trust_loopback: true       # default
```

The reasoning is the same one `require_login`'s default rests on: this is a
single-user console, and a caller who is already on this machine is not what the
password is for. In practice it means the browser on the desktop where the server
runs walks straight in, while the same URL opened from another machine gets the
login form. What it gives up is protection against **other users and processes on
the same host** — the console can execute commands as the server's account.

**The caveat is what "127.0.0.1" also looks like.** A reverse proxy, an SSH
tunnel, a container port-forward or a `socat` bridge all make remote clients
arrive *from* the loopback interface, so they skip the password too. If that is
how the console is reached, set `trust_loopback: false` — otherwise
`require_login` protects only the direct network path, and the proxy in front
(the one `allow_insecure_bind` trusts for authentication) is doing all the work.
The server logs a warning at startup while both are on, naming this.

Two further details:

- **The decision is the socket's peer address, never a header.** Hertz's
  `ClientIP()` reads `X-Forwarded-For` first; this deliberately does not, because
  a header is written by whoever is talking to us and every remote caller would
  then be able to claim to be local. A consequence worth expecting: the tunnel
  case above is not detected, only documented — hence the flag.
- **"Local" is about the packet, not about the browser.** Reaching the server at
  the machine's own LAN address (`http://192.168.1.5:8080`) or at a `.local`
  hostname is a *remote* path as far as the check is concerned, and the password
  is asked. Use `http://127.0.0.1:8080` (or an SSH tunnel) to be treated as
  local.

### What the console does with it

The web console has a login screen, and it appears **only** for a caller that
would be asked for the password. On boot it asks `GET /api/me`, which answers
`login_required` — for *this caller*: `admin.require_login` and
`admin.trust_loopback` together — and `authenticated` (whether the caller's
cookie is live), and draws either the console or the password form from that. So
a login-free deployment never sees a form, a browser on the server's own desktop
does not either, and the laptop across the network does. The form has one field:
the account is single-user (`admin.username`), so there is no username to type.

A few consequences worth knowing:

- **The session lives in the server's memory.** Restarting `admin serve` ends
  every session; the console then answers 401 on the next call and returns to
  the login form saying the session expired. `server.session_ttl_minutes`
  (default 720) is how long an idle session lasts — activity slides it forward.
- **Failed attempts are throttled per process**: after 8 failures within 5
  minutes, further logins are rejected until the window expires. The form shows
  that message as-is.
- **退出登录** in the sidebar revokes the session server-side
  (`POST /api/logout`) and then reloads the page, so no data read with the old
  session stays on screen.
- The console is still reachable *only* as an authenticated user: the metrics
  endpoint (`/metrics`, when enabled) and `/api/health`, `/api/me` and
  `/api/login` are the unauthenticated routes; everything else needs the cookie.
  `/api/me` is deliberately one of them — the console has to ask its question
  before it holds a session.

## Web chat (对话)

The admin UI can run conversations against the agent, with the model's reasoning
and tool calls visible as they happen.

```yaml
chat:
  enable: true             # off hides the 对话 tab
  max_steps: 12            # tool-calling iterations per turn
  history_limit: 40        # stored messages replayed to the model
  system_prompt: ""        # empty = the built-in Chinese general-purpose agent
                           # prompt in internal/prompt (see docs/prompt.md)
  ask_user_timeout_seconds: 600   # how long an ask_user card waits (see below)
  step_retry:               # retry ONE step whose model call died mid-stream
    enable: true
    max_attempts: 3
  plan:                     # the plan behind 任务看板 (and 继续执行)
    enable: true
    max_tasks: 50
```

Model-call retrying lives under `llm.retry` (it applies to every surface, not
just this one), and the whole recovery story — backoff, step retry, 继续执行,
the plan tools — is in `docs/long-tasks.md`.

`system_prompt` replaces the built-in prompt — here **and in the Feishu bot**,
which reads the same field — and only when it is not blank: a value that is all
whitespace falls back to the default. Whatever it says, the per-turn **可用技能**
section is appended to it, so a custom prompt that forgets about skills still
gets the list of names. It is read at startup from `configs/config.yaml`: unlike
the three budget fields, 设置 → 对话预算 cannot change it at runtime. See
`docs/prompt.md` for what the default says and how the surfaces differ.

Endpoints (session cookie required):

| Endpoint | Purpose |
|---|---|
| `GET /api/chat/models` | selectable providers/models + the tool list |
| `GET/POST /api/chat/sessions` | list / create |
| `GET/PATCH/DELETE /api/chat/sessions/{id}` | fetch / rename or change model / delete |
| `POST /api/chat/sessions/{id}/clear` | empty the history, keep the session |
| `POST /api/chat/sessions/{id}/messages` | **start** one turn (202; the turn then belongs to the conversation) |
| `GET /api/chat/sessions/{id}/turn` | **attach**: replay the running turn, then follow it live (SSE) |
| `POST /api/chat/sessions/{id}/turn/stop` | stop the running turn |
| `POST /api/chat/sessions/{id}/resume` | **continue** the last turn from where it stopped (202 / 409 busy / 400 nothing to continue) |
| `POST /api/chat/sessions/{id}/attachments` | upload a file for a message |
| `GET /api/chat/attachments/{id}` | download it again |
| `POST /api/chat/sessions/{id}/questions/{qid}/answer` | answer an `ask_user` card |

`GET .../turn` returns `text/event-stream`. Each frame is `data: {json}\n\n`, with
a `type` of `step_start`, `reasoning_delta`, `text_delta`, `step_end`,
`tool_call`, `tool_result`, `ask_user`, `plan`, `step_retry`, `usage`,
`context_compressed`, `budget_stop`, `done`, `error`, or `stream_end` (always
last). A `: ping` comment
is sent periodically so an idle stream is not mistaken for a dead one. It answers
`{"streaming": false}` — not a stream — when nothing is running.

Because `EventSource` cannot POST, the UI reads the body with `fetch` +
`ReadableStream` and parses the framing itself.

Every event carries the `step` it belongs to, which is what lets a reader put the
model's thinking next to the tool call it prompted.

Behaviour worth knowing:

- **A finished turn shows one line of process and then its answer.** Once a turn
  is over, everything it did before answering — every step's thinking and the
  tools it ran — is folded behind a single line reading
  `执行过程 · N 次工具调用 · M 条消息` (M is the number of ReAct iterations, i.e.
  the assistant messages the turn exchanged with the model), with the answer
  directly under it. Clicking that line brings the steps back as one row each;
  a step's tool cards still fold on their own, because the cards are the bulky
  half. While a turn is running the process stays open — it is what there is to
  watch. Nothing about the fold is stored: it is derived from "the turn is over",
  so a reload shows the folded state again.
  `step_end` is what marks a step's text as process rather than answer.
- **Reasoning is separate from the answer.** Reasoning models stream their
  thinking in a distinct field; it is kept out of the answer text, and stored both
  per step (`steps`) and as the turn's whole thinking (`reasoning`).
- **A failing tool does not fail the turn.** The error is shown, recorded in the
  audit log, and handed back to the model so it can adapt.
- **A failing model call does not either, up to a point.** A transient call
  failure (a dropped connection, a 429, a 5xx) is retried with exponential
  backoff — see `llm.retry`. If the stream dies *after* the model started
  answering, the retry happens one level up, at the step (`chat.step_retry`),
  and the console is told to drop the half-answer it received via `step_retry`.
- **A plan belongs to one request.** When every task is done (or skipped) and the
  user sends the next message, the finished plan is deleted before the new turn
  runs and an empty plan is published, so the board disappears from every open
  page. A plan with unfinished work is kept — it is what 继续执行 continues from.
- **A turn that dies can be continued.** The plan the model maintains
  (`plan_create` / `plan_add` / `plan_update` / `plan_read`) is stored per
  conversation and rendered above the composer as 任务看板 (执行中 / 待执行 /
  已完成); `POST .../resume` starts a new turn carrying that plan, the previous
  turn's steps and its tool observations, so the model continues instead of
  starting over. See `docs/long-tasks.md`.
- **Stopping a turn keeps what was produced.** Cancelling the request persists
  the partial answer and the steps already taken, so it is still there after a
  reload.
- **Disconnecting does not lose the answer.** Persistence runs on its own
  context, not the request's.
- **Reasoning and tool metadata are never replayed to the model** on later
  turns — they are display-only. Replaying an assistant tool call without its
  paired result is what makes providers reject a request.
- The first message auto-titles an untitled session.

Stored assistant rows carry `content` (the answer), `reasoning` (the turn's whole
thinking), `tool_calls` (a flat list of every call, for the audit and the
statistics) and `steps` — a JSON array of `{index, reasoning, text, tools[]}`
where `text` of the last step is the answer and every earlier one is process. A
row written before `steps` existed reads back with it empty, and the console
rebuilds a single process block from `reasoning` + `tool_calls`.

### Asking the user (the `ask_user` card)

When the model cannot decide something on its own — which of two workable designs
to take, which file to change, which environment to target — it can call the
`ask_user` tool. A card appears in the conversation with the question, the
choices it offers and a free-text box, and **the turn waits**: the model is
parked inside that tool call until an answer arrives, then carries on with the
same context and the answer as the tool's result.

- The tool is registered on the web surface only (`feishu` and the CLI have
  nowhere to render the card, so it is not offered there at all).
- `ask_user_timeout_seconds` (default 600) bounds the wait. On a timeout the
  model is told nobody answered and decides for itself; it is not an error, and
  the turn still finishes. The waiting time counts against
  `chat.turn_deadline_seconds`, so keep the wait well below that.
- The answer travels on its own request to
  `POST /api/chat/sessions/{id}/questions/{qid}/answer`, because the streaming
  response is already committed to its event stream. A question can be answered
  once: a late submission gets 404 (already answered, timed out, or the turn is
  over), and the card says so instead of asking again.
- The question and the answer are persisted with the turn — they are the tool
  call's arguments and its result — so a reloaded conversation still shows the
  card and what was chosen. **Reloading or closing the tab cancels the turn**,
  and with it the question: this is a live-only exchange, not a queue the model
  waits in across requests.

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

Every chat turn is traced into the agent's own database — one trace per turn, a
generation observation per model call (with token usage), and a span per tool
call. The admin UI has a 链路追踪 tab that lists traces and renders a trace's
observations as a waterfall. Tracing works out of the box: there is no external
service to run, no network call and no account.

Traces live in the same SQLite file as the conversations (`database.path`), in the
`traces` and `observations` tables. An assistant message stores the id of the
trace that produced it, which is what the 链路 button on an answer opens; the
conversation header's 链路 button opens the same panel filtered to that
conversation.

Langfuse is **optional**, as a mirror: if it is configured, every turn is sent
there too, which is what its evaluations and prompt management need. It is not
read back — the console always reads the local store.

```yaml
langfuse:
  enable: true
  host: "https://cloud.langfuse.com"     # or self-hosted
  public_key: ""                          # use HUAN_LANGFUSE_PUBLIC_KEY
  secret_key: ""                          # use HUAN_LANGFUSE_SECRET_KEY
  environment: "production"
  release: ""                             # e.g. a git sha
```

Endpoints (session cookie required; no secret key is involved in reading):

| Endpoint | Purpose |
|---|---|
| `GET /api/traces/status` | enabled?, host, delivery counters |
| `GET /api/traces?limit&page&session&user&name` | recent traces |
| `GET /api/traces/{id}` | one trace with its observations |

Notes:

- **Reading is local.** With no observability configuration at all the panel
  lists and renders what the agent recorded, and a trace id that no longer exists
  (pruned or cleared) answers 404 — not an outage.
- **The conversation is unaffected by tracing.** A write that fails is logged and
  counted and never propagated; every write is bounded by a short deadline so a
  locked database degrades the trace rather than the turn.
- **Storage is not pruned yet.** Nothing deletes old traces on its own, so the
  two tables grow with the conversation volume.
- **An optional mirror is fail-soft.** With Langfuse unconfigured its client is
  inert; when it is unreachable the local record is unaffected.
- The counters on the status strip are the quickest way to tell whether traces
  are actually being written.

## MCP servers (设置 → MCP)

MCP servers live in the **database**, not only in the config file: the console
creates, edits, tests and deletes them, and a save takes effect immediately —
the runtime connects the server, registers its tools into the same registry a
conversation reads, and the model can call them on the next message. No restart.

```yaml
mcp:
  servers:
    - name: "files"                 # display name; the console id is a slug of it
      transport: "stdio"            # stdio (default) | sse | http
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
      env: ["LOG_LEVEL=warn"]       # "KEY=value", merged onto the process env
      enabled: true                 # default true; a disabled entry is never connected
    - name: "remote"
      transport: "sse"
      url: "https://example.com/sse"
      headers: ["Authorization: Bearer ..."]
```

How the pieces behave:

- **Config-sourced rows are read-only.** Every entry above is mirrored into the
  database at startup (`source=config`) and re-applied from the file each time,
  so the console only lets you toggle `enabled`; editing the rest is refused with
  a 409 pointing at the config file. An entry removed from the file has its row
  dropped; rows the console created are never touched.
- **不变就不重连.** The runtime compares the connection-relevant fields, so
  renaming a server keeps its live connection while changing its command or URL
  reconnects it.
- **测试连接 is a dial, not a save.** `probe` connects, lists the tools and
  disconnects, and registers nothing — a definition being edited cannot put a
  broken server in front of the model. Testing a *saved* server connects
  separately and leaves the running connection alone.
- **A failed connect is a result.** The definition is valid even when the
  connection is not, so the save succeeds, the row shows 未连接 with the reason,
  and the message is stored in `last_error` so it survives a restart.
- **Tool name collisions are reported, not fatal.** A tool whose name is already
  registered is skipped and the collision is recorded on that server, so one
  clash cannot take a whole server offline.
- **进程退出会关闭连接** (a stdio server is a child process).

The 配置助手 (`POST /api/mcp/draft`) turns a sentence — or a pasted README —
into a draft definition using the default chat model. It **never writes**: the
console pre-fills its form with the draft, flags anything left for you (an empty
token, a missing command), and shows the model's raw reply. Saving remains a
separate, deliberate click.

## Skills (设置 → 技能)

A skill is one markdown file (YAML frontmatter + instructions) in
`skills.dir`. The console creates, edits, deletes and toggles them, and the
server renders the frontmatter so the file always parses back.

Enabled skills are offered to the model through two things, both recomputed per
turn (so a toggle takes effect on the next message):

1. a **可用技能** section in the conversation's system prompt, listing only the
   names and descriptions;
2. the `skill` tool, which the model calls to load one skill's instructions —
   the bodies stay out of the prompt until a task actually matches.

A skill's `tools` frontmatter field is an allow-list; a name this build does not
have is removed on save (and reported), because an allow-list naming a missing
tool cannot be applied. The 技能写作助手 (`POST /api/skills-draft`) drafts a
skill from a description and fills the editor — writing the file stays a
deliberate save.

## OpenViking (设置 → OpenViking)

OpenViking is the context database the agent keeps long-term memory and
documents in. The panel shows the connection
(`GET /api/openviking/status`: address, account, subtree, health, version,
auth mode), the memory counters (submitted turns, extracted facts, pending,
failures, last error), the document counters and the last sync report, and it
offers three actions: 立即同步工作区 (`POST /api/openviking/sync` with
`{"full":false}`), 全量重传 (`{"full":true}`) and 刷新记忆
(`POST /api/openviking/flush`). A form saves one document by hand
(`POST /api/openviking/save`), and `GET /api/openviking/documents` lists what
has been stored.

Two states are deliberately rendered as states rather than errors. With
`openviking.enable: false` every route answers 200 with `{"enable":false,
"message":"…"}`, because the fix is a config file and a restart, not a retry.
And a sync that cannot run (no workspace configured) answers 200 with
`{"ok":false,"error":"…"}` next to the button that asked, which is where the
reason is useful. Only a sync already in flight is a 409.

The settings themselves (`openviking.*`) live in the config file and are
reported read-only here, like the MCP servers declared under `mcp.servers`.
Full configuration reference: `docs/openviking.md`.

## Workspaces (the sidebar's folders)

A workspace is a directory the agent may work in, and every conversation belongs
to one. The console manages them in the **left sidebar** — that is where they are
visible, as folders over the session list — not in 设置: a workspace is a
directory, so it has nothing to configure beyond which directory it is.

| Endpoint | Purpose |
|---|---|
| `GET /api/workspaces` | the workspaces with their conversation counts, the default for a new conversation, the home directory, and warnings for directories that no longer exist |
| `POST /api/workspaces` | `{"root","name"}` — register an **existing** directory (`name` defaults to its base name) |
| `PATCH /api/workspaces/:name` | `{"name"}` — rename the label |
| `DELETE /api/workspaces/:name` | unregister; conversations move, files stay |
| `GET /api/chat/sessions/:id/workspace` | which workspace that conversation is in |
| `PUT /api/chat/sessions/:id/workspace` | `{"name"}` — move that conversation |
| `GET /api/fs/dirs?path=` | the subdirectories of a path, for the picker |

Contracts worth knowing when driving the API:

- **`root` must already be a directory.** It is resolved (symlinks included)
  before it is stored, and a missing path, a file, or the filesystem root is
  refused with 400 and a reason. Creating a workspace never writes to disk.
- **Names are labels, not paths.** They default to the directory's base name and
  must be non-empty, unique case-insensitively, and free of `/`, `\` and control
  characters. Chinese and spaces are fine.
- **Every conversation belongs to a workspace.** `POST /api/chat/sessions` takes
  an optional `workspace`; omitted, the conversation starts in the workspace the
  last one used. `GET /api/chat/sessions` carries a `workspace` on every row (the
  sidebar groups by it) and accepts `?workspace=` to list one folder.
- **Renaming moves the bindings with the label**, in one transaction: a
  conversation that referred to the old name would otherwise fall back to another
  workspace while the sidebar showed it elsewhere.
- **Deleting never deletes files** and never strands a conversation: the
  conversations move to another workspace, the answer says where, and deleting the
  last remaining workspace is refused (409). The directory is left exactly as it
  was.
- **`GET /api/fs/dirs`** is what makes "pick a local directory" possible: a
  browser `file` input cannot hand the server a path, and this console is served
  by the process whose filesystem is being chosen. It lists directories only,
  answers `{"ok":false,"error":…}` (200) for a path it cannot read, and never
  modifies anything. The picker drives it as a Finder-style column view: each
  column is one directory, clicking a folder fetches that folder's listing for
  the column to its right, and the selected directory (the deepest one) is what
  the create call sends.

## Background processes (conversation header → 后台进程 drawer)

The agent can start long-lived processes — a dev server, a `--watch` build, a
resident API or database — with the `bash_background` tool (see `docs/tools.md`).
What it started, what it printed and how to stop it appear **on the conversation
that started them**: a chip in the chat header carries the count, and clicking it
opens a drawer beside the conversation.

It used to live under 设置 → 后台进程. It does not any more, because a background
process is state a conversation produced rather than a knob the operator turns,
and because stopping one is something you do while looking at the conversation
that started it. The drawer shows two groups — this conversation's processes
first, then the ones another conversation of the same workspace started — so
removing the settings panel hides nothing.

| | |
|---|---|
| Chip | `N 个后台进程` while something runs, `N 条进程记录` when only finished ones remain, `同工作区 N 个后台进程` when this conversation has none but a neighbour has one running. Hidden when there is nothing to show. |
| Status | `running` while the job's **process group** has a member (a server that forked is still running even after its shell exited), `exited` when it ended on its own, `stopped` when it was asked to. |
| Log | Follows the job's output by byte offset, so watching a running server costs only its new lines. A fixed-size window is kept in memory and the full output up to `tools.background_log_max_mb` is on disk at the job's `log_path`; when the file hits its cap it says so in the file. |
| 停止 | SIGTERM to the whole group, SIGKILL after `tools.background_stop_grace_seconds`. A job that already ended answers 409 — the drawer never claims to have stopped something that stopped itself. |
| 删除记录 | Drops the record and deletes the log file. Refused while the job runs: the record is how a live process can be found and stopped. |
| Lifetime | Jobs belong to the process serving this console and are terminated when it exits. Each surface supervises its own: a Feishu bot started with `huan-agent serve` has its own jobs, which this drawer does not show. |
| Refreshing | The list is polled every 3s **only while something is running**; a job started mid-turn also refreshes it immediately (the stream reports the tool result), so the count appears without waiting for a poll. |
| Directory | `tools.background_dir`, defaulting to a `jobs/` directory beside the database file. Log files survive the records being pruned, so the directory is the operator's to clean up. |

With `tools.enable_background: false` the drawer says the feature is off rather
than showing an empty list, and the four tools are not offered to the model at
all.

## Artifacts (产物)

An artifact is a resource the agent *produced*: a generated page, a report, a
chart. The model saves one with the `save_artifact` tool, the bytes land in a
directory of the server's own — `tools.artifacts.root`, defaulting to an
`artifacts/` directory beside the database file — and the console reaches them
over HTTP.

It is deliberately not an attachment. An attachment is something a *person*
uploaded, its bytes live in the workspace, and the workspace is what makes it
reachable. An artifact is something the *agent* produced, its bytes live on the
server, and the recording in the database is only an index into them: the file is
the truth, the row is how it gets listed.

### Where they show up

| Surface | What it shows |
|---|---|
| Conversation header → 产物 | This conversation's artifacts, newest first. The chip carries the count (`N 个产物`) and is hidden while the conversation has none. |
| 统计监控 → 产物中心 | Every session's artifacts, with the owning session on each row, a kind filter and a session filter. Artifacts from a surface with no session (a one-shot `huan-agent run`) say so rather than showing a blank cell. |

Each row offers 打开 (a new tab), 下载, and 删除. Deleting asks first: it removes
the database row and then the file, and a file that refuses to go is reported —
the row is already gone at that point, and a listing entry that 404s is worse
than an honest warning in the log.

The tool is registered on every surface, but only the console can hand back a
URL: a CLI (`huan-agent chat`) or one-shot (`huan-agent run`) process serves no
HTTP, so `save_artifact` there reports the path it wrote and says the URL is not
available. The console still lists those artifacts, because the store is shared.

### Configuration

```yaml
tools:
  artifacts:
    enable: true          # false = the tool refuses and every endpoint says so
    root: ""              # empty = an artifacts/ directory beside the database
    public_urls: false    # true = artifact files are served without a login
    max_bytes: 0          # one artifact's cap; 0 = 32 MiB
```

The accepted types are a whitelist — `html`, `md`, `txt`, `csv`, `json`, `xml`,
`png`, `jpg`, `gif`, `webp`, `pdf` — and an extension outside it is refused
rather than stored and worried about later.

### `public_urls` and why the default is off

Artifact bytes are authored by the model, and the model is steered by whatever
text reached it. So:

- Files are served **behind the admin session** by default. `public_urls: true`
  serves them to anyone holding the link — including a page the agent read from
  the workspace and pasted into an artifact. A non-loopback bind with
  `public_urls: true` logs a warning at startup.
- An HTML artifact is served with `Content-Security-Policy: sandbox` (without
  `allow-same-origin`), so it runs in an opaque origin: it cannot read the
  console's cookie, its `localStorage` or its API. That is the difference between
  "the agent can show me a page" and "the agent can hand itself my session".
  Scripts, forms and downloads still work, which is what keeps a generated
  dashboard useful.
- `X-Content-Type-Options: nosniff` prevents a browser from re-interpreting the
  bytes as something other than the recorded type.
- The serving route resolves `path` under the configured root **and nowhere
  else** — absolute paths, `..` and symlink escapes are refused. (In practice an
  unencoded `..` never reaches the handler: Hertz normalises it before route
  matching. The check that provably fires is the symlink one, which was verified
  against a real server; see `internal/server/artifacts_test.go`.)

### Design notes for maintainers

Full rationale is in `openspec/changes/phase-25-artifacts/` (proposal, spec,
tasks). The four things worth knowing before changing this feature:

- **The files are the truth, the row is an index.** `artifacts.path` is both the
  address and the identity: the serving route resolves exactly that string under
  the root and nowhere else, so a corrupted row cannot become a read of the
  server's filesystem. A file whose row is gone is simply not listed.
- **Bytes are written first and the row second.** A file with no row is bytes
  nothing lists (harmless, `ls`-visible); a row with no file is a listing entry
  that 404s. So a failed insert deletes the file it just wrote. Deletion runs the
  other way round, for the same reason: the row first, because a row is not
  reversible.
- **The model chooses a name, never a path.** The extension whitelist decides
  what may be stored, and it is refused at write time rather than served as an
  opaque download later.
- **A served document is in an opaque origin.** The sandbox CSP is what makes
  "the agent can show me a page" different from "the agent can hand itself my
  session". If you relax it, say so here and in the change record.

## Web UI

The UI is a Vue 3 SPA in `web/`, built into `internal/server/webui/dist/` and
embedded in the binary — there is nothing to deploy separately.

```bash
make web          # install deps and build into the embedded directory
cd web && npm run dev   # dev server with /api proxied to :8080
```

Surfaces: **对话** (the primary one) plus two menu entries — **设置** (外观 /
模型 / MCP / OpenViking / 技能 / 服务与工具, as sub-tabs of one page) and
**统计监控** (总览 / 按模型 / 按用户 / 调用记录 / 审计日志 / 链路追踪). Both use
the same shape: a page head with a sub-tab strip, then one scrolling panel.

The **sidebar** is where conversations and workspaces live together: sessions are
grouped under their workspace (a folder per directory), each folder can create a
conversation, be renamed, or be deleted, and 新建工作区 opens a picker that
browses the server's filesystem. 服务与工具 lists the tools the model can
actually call — builtin, plus whatever the configured MCP servers expose, plus
`skill`; which directory they run in is the conversation's workspace.

The chat header used to repeat the conversation's workspace as a picker. It does
not any more: the folder in the sidebar already says which workspace a
conversation is in, and the only thing the picker added was the ability to move
an existing conversation to another one. A conversation is now placed in a
workspace when it is created (the + on a folder) and stays there. The routes
that read and move it (`GET` / `PUT /api/chat/sessions/{id}/workspace`) still
exist for a script, but the console has no control for them.

## Security notes

- Data endpoints require a session; the cookie is `HttpOnly`, `SameSite=Lax`,
  and `Secure` only over TLS (so plain-HTTP localhost works).
- Failed logins are throttled; while throttled, even the correct password is
  refused.
- Unconfigured (no metrics) or unwired features degrade gracefully instead of
  failing startup.
- **MCP `env` and `headers` are returned to the console verbatim**, unlike an
  LLM provider's API key (which the store never serialises). An MCP definition
  is a list the operator wrote and must be able to edit, and a masked list could
  not be edited without retyping every entry. This rests on the deployment being
  loopback-only, which `server.New` enforces for a login-free console — keep
  `admin.require_login: true` (or a proxy in front) if you expose it.
- **An MCP server is a program the agent starts.** A `stdio` entry spawns
  `command` with `args` and `env`; saving one is equivalent to running it, under
  the account the server runs as. Treat the MCP tab with the same trust as the
  tool configuration, and remember the tools it exposes are callable by anyone
  who can talk to the agent.
- The server does **not** terminate TLS itself. For anything other than
  localhost, put it behind a reverse proxy that does, and keep it off the public
  internet.

## Troubleshooting

- **`admin password is not configured`** — run `huan-agent admin set-password`
  and set `admin.password_hash`, or export `HUAN_ADMIN_PASSWORD_HASH`.
- **`password_hash is not a valid bcrypt hash`** — the value is not a bcrypt
  hash (it must start with `$2`). Regenerate it; do not put a plaintext password
  there.
- **401 on every request** — the session expired (`session_ttl_minutes`, or the
  server restarted), the browser is not sending the cookie (check for a proxy
  stripping `Set-Cookie`), or the console is open on a host other than the one
  the cookie was set for. The console returns to its login form in the first
  case and says the session expired.
- **The password is never asked from a host that should need it** — that caller
  arrives through a proxy, tunnel or port-forward running on the server's own
  machine, so it looks like `127.0.0.1` and `admin.trust_loopback` (default
  `true`) lets it in. Set `admin.trust_loopback: false`; the server warns about
  this combination at startup.
- **The console asks for a password on the machine the server runs on** —
  `trust_loopback` is off, or the browser is reaching the server by an address
  that is not loopback (the machine's LAN IP, a `.local` hostname, or a proxy).
  Open `http://127.0.0.1:<port>` to be treated as local.
- **The console shows 401 but no login form** — `GET /api/me` reports
  `login_required: false` (this deployment does not ask for a password) while
  the data calls are answered 401 by something else: a reverse proxy demanding
  its own credentials, or a server that was restarted with `require_login: true`
  after the page loaded. Reload the page; if it persists, look at the proxy.
- **Forgot the admin password** — there is no recovery flow (the password is a
  hash in the config, not an account). Run `huan-agent admin set-password` and
  restart the server; the hash is the only copy.
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
- **任务看板 never appears** — no plan exists yet (the model only creates one for
  a task it judges multi-step), or `chat.plan.enable` is false. A model that
  narrates a plan in prose instead of calling `plan_create` is a prompt problem,
  not a console one.
- **继续执行 answers 400** — the last turn finished normally *and* the plan has no
  unfinished task (`skipped` counts as finished). The response says so; it is not
  a failure.
- **链路追踪 says disabled** — tracing is on by default, so this means the store
  was not wired; check the server log for `trace store enabled`.
- **A message has no 链路 button** — that turn recorded no trace (the answer is
  also missing when the browser disconnected mid-turn: the message is persisted
  before the run reports its trace id). The trace itself is still in the list;
  filter by session to find it.
- **链路追踪 answers 404 for a trace** — the record is gone: pruned, cleared, or
  the link is stale. This is not a backend failure.
- **链路追踪 shows a 502** — the trace backend itself failed to answer. With the
  built-in store that means a database problem, not a configuration one.
- **Traces never appear in Langfuse** — check the status strip counters: a rising
  `failed` means the keys or host are wrong; `dropped` means the buffer overflowed.
  An empty Langfuse is not a sign that tracing is off — the console reads locally.
