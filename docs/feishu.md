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
   - `im:message.p2p_msg:readonly` (optional, for richer event payloads)
2. **事件与回调 / Events** — subscribe to the event:
   - `im.message.receive_v1` (`接收消息`)
   - Request URL: **URL verification is NOT needed** — the WebSocket
     long-connection mode pushes events directly, so leave the callback URL empty.
3. Publish a version of the app (**版本管理与发布 → 创建版本 → 申请发布/刷新**) so the
   permissions take effect.

> On the overseas **Lark** platform the endpoints/domain differ. Set
> `feishu.domain: "https://open.larksuite.com"` (and use the Lark console) in that
> case.

## 3. Configure huan-agent

Edit `configs/config.example.yaml` → copy to `configs/config.yaml` and fill:

```yaml
feishu:
  app_id: "cli_xxxxxxxx"
  app_secret: "xxxxxxxx"
  domain: ""            # empty = Feishu; use https://open.larksuite.com for Lark
  verification_token: ""
  encrypt_key: ""
```

Or via environment (the Viper prefix is `HUAN`):

```bash
export HUAN_FEISHU_APP_ID=cli_xxxxxxxx
export HUAN_FEISHU_APP_SECRET=xxxxxxxx
export HUAN_FEISHU_APP_ID=...
```

An empty `app_id` keeps the bot disabled.

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

## 5. Talk to it

Open a **private chat** with the bot in Feishu and send a text message.
Supported in-chat commands:

- `你好` → replies from the agent.
- `/reset` → clear this user's conversation.
- `/remember <key>: <value>` → persist a fact (memory).
- `/recall <query>` → search stored facts.
- `/provider` → show the active model.

## Troubleshooting

- **No messages arrive** — ensure `im.message.receive_v1` is subscribed and the app
  is **published**; grant the bot the required scopes, and make sure you are
  messaging the bot in a **single (p2p) chat**, not a group.
- **Send fails with scope error** — re-add `im:message:send_as_bot` and re-publish.
- **Domain/endpoint** — on Lark (international), set `feishu.domain =
  https://open.larksuite.com`.
- **App ID/secret wrong** — double-check they belong to the same app and were
  copied after creation.

## Notes

- The SDK used is `github.com/larksuite/oapi-sdk-go/v3` (WebSocket long-connection,
  `ws.NewClient`). Review `internal/platform/feishu` for the wiring.
- Unit tests use a stub `SendMessage` and never touch real credentials.