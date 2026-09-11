# Capability: platform-feishu (completion)

## Purpose

Extend the Feishu transport from text-only to the message shapes and delivery
behaviours a real user expects: attachments, rich text, visible progress,
resilient outbound sends, and a choice of event transport.

This spec amends the Phase 4 `platform-feishu` capability; the requirements
below are added to, not replaced by, the original ones.

## Scope

- `internal/platform/feishu` — inbound normalization, resource download,
  markdown rendering, resilience, callback transport.
- `cmd/huan-agent serve` — prompt assembly, placeholder handling, sender wiring.
- Config section `feishu`.

## Requirements (MUST)

1. **Attachments** — inbound `image`, `file`, `audio`, `media` and `sticker`
   messages MUST be normalized into `Inbound.Resources`, and the serve loop MUST
   download them to a local directory and make the paths available to the model.
2. **Rich text** — `post` messages MUST be flattened into text (paragraphs,
   links, `@` mentions, inline images) with inline images collected as resources.
3. **Supported-type gate** — the transport MUST only invoke the handler for
   message types that carry actionable content (`text`, `post`, or anything with
   resources); everything else is ignored without an error.
4. **Progress feedback** — when `feishu.thinking` is enabled, the bot MUST send
   a placeholder immediately and replace it in place once the answer is ready,
   falling back to a new message if the update fails, so the user always
   receives the answer exactly once.
5. **Retry** — outbound sends MUST be retried with exponential backoff; retries
   MUST stop early on context cancellation and MUST NOT re-send errors
   classified as permanent (auth/permission/invalid-request).
6. **Rate limiting** — outbound sends MUST pass through a configurable token
   bucket so a burst cannot exceed Feishu's limits.
7. **Timeout** — each send attempt MUST be bounded by a configurable timeout.
8. **Rich rendering** — markdown answers MUST be rendered as interactive cards
   with proper code blocks, dividers and chunking; single elements MUST stay
   within the platform's content budget.
9. **Native tables** — a markdown (GFM) table MUST be rendered as a native
   Feishu `table` element carrying per-column alignment and a header style.
10. **Table overflow** — a table exceeding the per-message limits MUST be split
    across several messages rather than truncated: rows are chunked with the
    header repeated, and columns are grouped with the first column repeated as
    a key. Continuation messages MUST be marked as such.
11. **Table fallback** — a table that cannot be rendered natively at all (a cell
    exceeding the cell limit) MUST degrade to a markdown table in a text block
    so no data is lost. When the platform rejects a card containing a native
    table, delivery MUST retry with a maximum-compatibility rendering (tables as
    markdown, no native table element) before falling back to plain text.
9. **Transport selection** — the transport MUST be selectable per app
   (`websocket` default, `callback` optional), with aliases accepted, and an
   invalid value MUST fail at startup.
10. **Callback authentication** — in callback mode the endpoint MUST reject a
    request whose verification token does not match the configured one, and MUST
    keep Feishu's signature verification enabled when an encrypt key is set; a
    callback endpoint configured with neither MUST log a warning.
11. **Safe file names** — downloaded attachments MUST be written to bare file
    names inside the download directory (no path separators, no leading dots)
    and MUST be capped by a configurable size limit.
13. **Build/test** — `go build ./...`, `go vet ./...` and `go test ./...` MUST
    pass without network access or real credentials.

## Non-goals

- Uploading files outbound (the agent sends text/cards only).
- Streaming partial model output into the chat.
- Card button/action callbacks.

## Key interfaces

- `ResourceDownloader{ Download(ctx, messageID, Resource) (string, error) }`
- `Sender{ ReplyText, ReplyCard, SendText, SendCard, UpdateCard, Recall }`
- `CreateMessageFn(ctx, receiveID, receiveIDType, msgType, content) (string, error)`
- `RetryPolicy`, `Limiter`, `WithRetry`, `WithRateLimit`, `WithSendTimeout`,
  `WrapCreate`
- `MarkdownToCardElements`, `CardTitleFromMarkdown`, `SplitMarkdownBlocks`
- `ParseMode`, `ModeWebSocket`, `ModeCallback`
