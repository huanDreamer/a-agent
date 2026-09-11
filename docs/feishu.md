# Feishu (Lark) IM bot — setup & pairing

This documents how to pair `huan-agent serve` with a real Feishu self-built app.
The code itself never requires live credentials to build or test; only this manual
step is needed to actually talk to the agent.

## 1. Create a self-built app

1. Open the [Feishu Open Platform](https://open.feishu.cn) → 开发者后台 →
   **创建企业自建应用**.
2. Give it a name (e.g. `huan-agent`) and icon.
3. Go to **凭证与基础信息** and copy:
   - **App ID** — `cli_xxxxxxxx`
   - **App Secret** — keep this private; never commit it.

## 2. Enable the bot + scopes

1. **权限管理 / Permissions** — enable the scopes the bot needs to read and send
   messages:
   - `im:message` (read)
   - `im:message:send_as_bot` (send as the bot)
   - `im:message.p2p_msg:readonly` — **required** to receive private-chat
     messages. Without it the WebSocket connects but `im.message.receive_v1`
     events for p2p chats are never delivered (the most common "connected but
     nothing arrives" cause).
2. **事件与回调 / Events** — subscribe to the event:
   - `im.message.receive_v1` (`接收消息`)
   - Request URL: **URL verification is NOT needed** — the WebSocket
     long-connection mode pushes events directly, so leave the callback URL empty.
   - If you set an **加解密密钥 (Encrypt Key)** here, it must match
     `encrypt_key` in config, or events fail decryption and are silently dropped.
3. **发布应用**: every permission/scope/event change requires creating a new
   version and publishing it (**版本管理与发布 → 创建版本 → 申请发布/刷新**). An app
   that is built but not republished after adding scopes will keep running with
   the old permissions.

> On the overseas **Lark** platform the endpoints/domain differ. Set
> `feishu.domain: "https://open.larksuite.com"` (and use the Lark console) in that
> case.

## 3. Configure huan-agent

Edit `configs/config.example.yaml` → copy to `configs/config.yaml`. Multiple
Feishu apps are supported (like `llm.providers`); pick the active one with
`feishu.active`. The legacy single `app_id`/`app_secret` block still works.

```yaml
feishu:
  active: "primary"     # whichever app in `apps` to run
  apps:
    primary:
      app_id: "cli_xxxxxxxx"
      app_secret: "xxxxxxxx"
      domain: ""         # empty = Feishu; use https://open.larksuite.com for Lark
      verification_token: ""
      encrypt_key: ""
    secondary:
      app_id: "cli_yyyyyyyy"
      app_secret: "yyyyyyyy"
      domain: "https://open.larksuite.com"   # Lark (international)
      verification_token: ""
      encrypt_key: ""

  # Legacy single-app alternative to `apps` + `active`:
  # app_id: "cli_xxxxxxxx"
  # app_secret: "xxxxxxxx"
```

Secrets can also come from environment (Viper prefix `HUAN`, path separator
`_`):

```bash
export HUAN_FEISHU_ACTIVE=primary
export HUAN_FEISHU_APPS_PRIMARY_APP_ID=cli_xxxxxxxx
export HUAN_FEISHU_APPS_PRIMARY_APP_SECRET=xxxxxxxx
```

If the active app has an empty `app_id`, the bot stays disabled.

> **Env vs file (LLM keys)** — for nested `llm.providers.*`, a non-empty env
> value always wins over an empty YAML value, so you can keep secrets purely in
> the environment (e.g. `HUAN_LLM_PROVIDERS_DEEPSEEK_API_KEY`) and leave
> `api_key: ""` in the file. `configs/config.yaml` is gitignored, so real
> secrets should never be committed.

## 4. Run the bot

```bash
go build ./cmd/huan-agent
./huan-agent serve --config configs/config.yaml
# or with env vars only:
HUAN_FEISHU_APP_ID=cli_xxx HUAN_FEISHU_APP_SECRET=xxx ./huan-agent serve
```

The bot opens a WebSocket long-connection and stays up until you send SIGINT/SIGTERM.
Each private user (`open_id`) gets an independent session with the same memory +
context machinery as `huan-agent chat`.

### Behavior options

```yaml
feishu:
  thinking: true            # send 🤔 正在思考… then replace it with the answer
  retry_attempts: 3         # outbound send attempts (1 = no retry)
  retry_base_delay_ms: 300  # exponential backoff start
  send_timeout_ms: 15000    # per-attempt timeout
  rate_limit_per_sec: 5.0   # outbound token bucket (0 = unlimited)
  rate_limit_burst: 10
  download_dir: ""          # empty = $TMPDIR/huan-agent-downloads
  max_download_mb: 32       # per-attachment cap
```

### Event transport: WebSocket vs callback

By default events arrive over the **WebSocket long-connection**, which needs no
public URL. To use an HTTP callback instead (e.g. behind a load balancer), set it
per app:

```yaml
feishu:
  apps:
    primary:
      transport: "callback"
      callback_addr: "0.0.0.0:8081"
      callback_path: "/feishu/event"
      verification_token: "your-verification-token"
```

Accepted `transport` values: `websocket` (default, aliases `ws`), `callback`
(aliases `http`, `webhook`). In callback mode:

- Register the public URL (`https://host/feishu/event`) in the Feishu console
  under **事件与回调 → 请求地址**.
- Set `verification_token` — the endpoint rejects requests whose token does not
  match. **Without it (and without an encrypt key) the endpoint accepts
  unauthenticated events**, and the bot logs a warning at startup.
- Set `encrypt_key` to additionally have Feishu's request signature verified.
- `GET /healthz` returns `ok` for liveness probes.

## 5. Talk to it

Open a **private chat** with the bot in Feishu and send a message. Plain text,
rich text (`post`), images, files, voice and video are all accepted; attachments
are downloaded locally and their paths are handed to the model.

Supported in-chat commands (plain text only):

- `你好` → replies from the agent.
- `/reset` → clear this user's conversation.
- `/remember <key>: <value>` → persist a fact (memory).
- `/recall <query>` → search stored facts.
- `/provider` → show the active model.

Replies with markdown (code blocks, lists) are sent as interactive cards so they
render properly. While the model is generating you will see a
`🤔 正在思考…` card, which is replaced in place by the answer.

### Tables

A markdown table in an answer is rendered as a **native Feishu table** (aligned
columns, header row, horizontal alignment taken from the `:--`/`--:` markers).

When a table does not fit one card, it is split across several messages instead
of being truncated:

- **Too many rows** → chunked, with the header repeated in every chunk.
- **Too many columns** → chunked into column groups; the first column is
  repeated in each group so rows stay identifiable.
- **A single cell that is too long** to render at all → the table degrades to a
  markdown table inside a text block rather than being lost.

Defaults are 6 columns, 20 rows and 120 characters per cell (see
`feishu.DefaultTableLimits`). Continuation messages start with `（表格续）`.

If the platform rejects the card (for example when the native table component is
unavailable for your tenant), delivery automatically retries with a
maximum-compatibility rendering that turns tables into markdown text, and only
falls back to a plain-text message if that also fails — so an answer is never
lost to an unsupported card component.

## Troubleshooting

- **No messages arrive even though connected** — the WebSocket reports
  `connected to wss://...` but events never reach the handler. Check, in order:
  1. `im:message.p2p_msg:readonly` is granted and the app **republished**.
  2. `im.message.receive_v1` is subscribed (WebSocket mode, no callback URL).
  3. You are messaging the bot in a **single (p2p) chat**, not a group
     (groups need `im:message.group_msg:readonly` + `@` the bot).
  4. No Encrypt Key mismatch (see §2).
  5. There is only **one** serve process — running two instances on the same
     app splits the single connection, so events appear to go nowhere.
- **`missing field tool_call_id` (HTTP 400)** — a stale assistant `tool_call`
  without a matching tool result was sent to the model. The bot now strips these
  via `sanitizeBotHistory` and uses a plain chat model, so this should not recur.
- **401 `Authentication Fails (governor)`** — the LLM API key is wrong, empty, or
  not reaching the code. Set it in config *or* as
  `HUAN_LLM_PROVIDERS_<NAME>_API_KEY` (e.g. `HUAN_LLM_PROVIDERS_DEEPSEEK_API_KEY`);
  env now overrides an empty YAML value (see §3).
- **Event arrives but no reply** — enable `HUAN_LOGGING_LEVEL=debug` to see
  `feishu message received` + `bot handler received`; a line in the log for every
  inbound message tells you whether the event reached the handler.
- **Send fails with scope error** — re-add `im:message:send_as_bot` and re-publish.
- **Attachment not read by the model** — check the log for
  `feishu: download attachment failed`. Inbound files need the
  `im:resource` permission (alongside `im:message`) and the app must be
  republished. Downloads are capped by `feishu.max_download_mb`.
- **Replies never update the placeholder** — the update path needs the message id
  returned by the create call; a `patch message: code=...` warning means Feishu
  refused the update (card too large, or the message was already replaced). The
  bot then sends the answer as a fresh message.
- **A table arrived as plain markdown text instead of a table** — the native
  table component was rejected or the table was outside the limits, so the
  compatibility rendering was used. Look for
  `delivering the answer card failed; retrying without native tables` in the log
  and check the table's shape against the limits above.
- **A long answer arrived as several messages** — expected: that is a table
  split across messages. The extra messages start with `（表格续）`.
- **Callback mode returns 403** — the request's `verification_token` does not
  match `feishu.apps.<name>.verification_token`. Copy the token from
  **事件与回调 → 加密策略** in the console.
- **Domain/endpoint** — on Lark (international), set `feishu.domain =
  https://open.larksuite.com`.
- **App ID/secret wrong** — double-check they belong to the same app and were
  copied after creation.

## Notes

- The SDK used is `github.com/larksuite/oapi-sdk-go/v3` (`ws.NewClient` for the
  long-connection, `core/httpserverext` for the callback transport). Review
  `internal/platform/feishu` for the wiring.
- Outbound sends are wrapped as rate limit → retry → per-attempt timeout
  (`WrapCreate`); permanent errors (permission/invalid-request) are not retried.
- Unit tests use stub send/primitives and never touch real credentials.