# Capability: platform-feishu

## Purpose

Let a user converse with `huan-agent` from Feishu (Lark) over the official
lark-oapi-go SDK WebSocket long-connection, reusing the agent + memory + context
stack built in earlier phases.

## Scope

- `internal/platform/feishu` — IM transport (WebSocket + outbound sender abstraction).
- `cmd/huan-agent serve` — long-running bot loop with per-user sessions.
- Config section `feishu`.

## Requirements (MUST)

1. **Transport** MUST use the lark-oapi-go v3 WebSocket long-connection
   (`ws.NewClient` + event dispatcher) so no public callback HTTP server is
   required.
2. **Inbound normalization** MUST map `im.P2MessageReceiveV1` into a Feishu-agnostic
   `Inbound` carrying `OpenID`, `ChatID`, `ChatType`, `MessageID`, `Text`, `MsgType`.
3. **Text routing** MUST only process text messages; other types are ignored.
4. **Outbound Sender** MUST be an interface (`ReplyText`, `ReplyCard`) injectable for
   tests; real implementation sends via `im/v1 message Create`.
5. **Configuration** MUST be a Viper-backed `feishu` section
   (`app_id`, `app_secret`, `domain`, `verification_token`, `encrypt_key`). Empty
   `app_id` disables the bot. No real credentials may be committed.
6. **Serve loop** MUST keep an independent session per sender `open_id`, wired to
   `sessionMemory` (short+long term, context compression).
7. **Build/test** MUST pass `go build ./...`, `go vet`, and `go test ./...` without
   network or credentials; bot pairing is left to manual docs.

## Non-goals

- Card action callbacks, media (image/file) uploads, rich `post` text.
- Multi-tenant or multi-app support.

## Key interfaces

- `Sender { ReplyText(ctx, in, text) error; ReplyCard(ctx, in, title, markdown) error }`
- `Handler.Handle(ctx, in) error`
- `messageCreator.Create(ctx, req, options...) ...` — hides SDK's unexported
  `*im.message` type behind an interface.

## Data flow

```
Feishu --WS event--> dispatcher.OnP2MessageReceiveV1
  -> parseInboundMessage -> Inbound
  -> botHandler.Handle -> (per open_id sessionMemory) -> model/agent.Generate
  -> Sender.ReplyText -> im Create -> Feishu chat
```