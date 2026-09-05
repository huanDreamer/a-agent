# Phase 4 — Platform Integration: Feishu (Lark) IM Bot

## Summary

Deliver the Feishu (Lark) IM channel for `huan-agent` (Phase 4 of the roadmap) by
integrating the official **lark-oapi-go v3** SDK over the **WebSocket
long-connection**, so an end user can talk to the agent directly in Feishu without
standing up a public HTTP callback endpoint.

The bot mirrors the `huan-agent chat` runtime: each private user is keyed by their
Feishu `open_id`, gets an independent conversation wired to the same memory +
context machinery built in Phase 3, and receives its replies back into the same
chat as a text message or an interactive card.

## Context

`huan-agent serve` is currently a placeholder that merely waits for a signal. There
is no way for a real user to converse with the agent outside the local REPL. Per the
project plan, Phase 4 ("IM integration") is the step that makes the agent genuinely
usable.

## Proposal

Add a **Feishu IM adapter**:

- `internal/platform/feishu/` — the SDK-facing transport:
  - a small `Sender` / `SendMessage` abstraction so outbound calls are
    dependency-injected and the core is unit-testable without credentials;
  - a WebSocket transport that consumes `im.message.receive_v1` events;
  - an `Inbound` struct normalizing the raw `P2MessageReceiveV1` event (open_id,
    chat_id, message id, text/card content).
- A bot serve loop (in `cmd/huan-agent/serve.go`):
  - routes each private (p2p) message by sender `open_id`,
  - keeps a per-user `sessionMemory` (short + long-term, from Phase 3),
  - delegates to the Eino `agent.Agent` (tools + MCP) when the provider supports
    tool calling, otherwise to a plain model `Generate`,
  - supports `/reset`, `/remember`, `/recall`, `/provider` mirroring the REPL.
- Config: a `feishu` section (`app_id`, `app_secret`, `domain`,
  `verification_token`, `encrypt_key`), default-disabled when `app_id` is empty.

## Approach

- **WebSocket long-connection** (push) rather than an HTTP webhook keeps the bot
  runnable locally without a public callback port — better for a personal agent.
- **Dependency-injected Sender** hides the un-exported lark `message` service
  behind a small `messageCreator` interface, keeping tests mock-based.
- **Per-user session map** is appropriate for a personal single-agent deployment.

## Out of scope (follow-ups)

- Interactive card button callbacks, media uploads, group-chat handling.
- Multi-tenant / multi-app support.

## Docs

- `docs/feishu.md` — creating a self-built app, enabling the bot + scopes, and
  running the bot; pairing is manual (no credentials baked in).
- `configs/config.example.yaml` gets the commented `feishu:` block.

## Test

- `internal/platform/feishu/` unit tests with a fake `SendMessage` (no credentials):
  inbound parsing, text/card payloads, error paths, transport construction.
  Coverage ≥ 70%.
- `go vet ./...` and `go test ./...` pass. Conventional Commits.