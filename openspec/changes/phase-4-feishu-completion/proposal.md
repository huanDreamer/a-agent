# Phase 4 补完 — Feishu capability completion

## Summary

Finish the Feishu (Lark) integration started in Phase 4. The original phase
delivered a working end-to-end path (WebSocket long-connection → per-user
session → agent → text/card reply) but explicitly deferred several items, and
live pairing surfaced gaps that only appear against the real platform.

This change closes those gaps: inbound media, a progress placeholder, outbound
resilience, rich-text rendering, an alternative HTTP callback transport, and the
`user-routing` capability spec that was promised by the roadmap but never
written.

## Why

Anything a user actually sends that is not plain text was silently dropped, and
the bot stayed mute for the whole model latency — both are immediately visible in
normal use. A single transient Feishu error also discarded a reply with no retry,
and answers containing markdown (code blocks, lists) rendered as unformatted
text.

## What Changes

### ADDED

- `internal/platform/feishu/inbound.go` (extended): `Resource`, `ResourceKind`,
  rich-text (`post`) flattening, `@`-mention detection, `Handled()`, `PromptText()`.
- `internal/platform/feishu/resource.go`: `ResourceDownloader` that streams
  attachments to a local directory via the message-resource API, with a size cap
  and path-traversal-safe naming.
- `internal/platform/feishu/markdown.go`: markdown → Feishu card element
  conversion (code blocks, dividers, chunking, title extraction).
- `internal/platform/feishu/resilience.go`: exponential-backoff retry, a
  stdlib token-bucket rate limiter, per-attempt timeout, and permanent-error
  classification.
- `internal/platform/feishu/sender_decorators.go`: the same policy layers
  applied to the id-returning create primitive.
- `internal/platform/feishu/callback.go`: HTTP event-callback transport.
- `openspec/changes/phase-4-feishu-completion/specs/user-routing/spec.md`.

### CHANGED

- `Sender` grows `SendText`, `SendCard`, `UpdateCard`, `Recall`; `ReplyCard`
  now takes a title so a call site can send a card without one and have it
  derived from the content.
- The create primitive returns the new message id, which is what makes
  placeholder replacement possible.
- `Transport` is selectable per app (`websocket` | `callback`).
- `cmd/huan-agent serve`: downloads attachments and appends their local paths to
  the prompt, sends a "thinking…" card and updates it in place with the answer,
  and wraps the sender with retry/rate-limit/timeout.
- `config.FeishuConfig` gains `thinking`, `retry_attempts`,
  `retry_base_delay_ms`, `send_timeout_ms`, `rate_limit_per_sec`,
  `rate_limit_burst`, `download_dir`, `max_download_mb`; `FeishuApp` gains
  `transport`, `callback_addr`, `callback_path`.

## Impact

- **Behaviour**: non-text messages now reach the model; replies stream as a
  placeholder→answer card; markdown renders properly; transient failures retry.
- **Security**: the callback transport validates the verification token itself
  (the SDK only enforces it for the URL handshake), so a public endpoint no
  longer accepts forged events when an encrypt key is absent.
- **Compatibility**: WebSocket remains the default and the existing
  `feishu.app_id` / `apps` config keeps working; the new fields are optional
  with defaults.
