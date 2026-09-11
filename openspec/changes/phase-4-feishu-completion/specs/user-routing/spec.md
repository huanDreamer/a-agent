# Capability: user-routing

## Purpose

Route each inbound IM message to the correct isolated agent conversation, so a
user's history, memory and in-flight state belong to them alone, and so the
agent knows which chat to answer in.

## Scope

- `internal/platform/feishu` — the normalized `Inbound` routing fields.
- `cmd/huan-agent serve` — the per-user session map.
- `internal/memory` — per-session short/long-term storage.

## Requirements (MUST)

1. **Identity** — every inbound message MUST carry the sender's app-scoped
   `open_id`; a message without one MUST NOT be routed to a session.
2. **Session keying** — the serve loop MUST key sessions by `open_id`, so two
   different users never share a conversation buffer, memory file or context
   window.
3. **Isolation** — a session's memory and conversation history MUST be readable
   only by requests carrying that session's `open_id`.
4. **Reply addressing** — replies MUST be sent to the `chat_id` the message
   arrived in, not to the sender's `open_id` directly, so group and p2p
   conversations both answer in place.
5. **Concurrency** — concurrent messages from the same user MUST be serialized
   against that user's session state (per-session locking); messages from
   different users MUST NOT block each other.
6. **Lifecycle** — a session MUST be created lazily on the first message and
   reused for subsequent messages from the same identity.
7. **Reset** — a user MUST be able to clear their own conversation without
   affecting any other session.
8. **Testability** — routing MUST be exercisable without real credentials.

## Non-goals

- Group-level shared sessions (each sender still gets their own session inside a
  group chat).
- Cross-app identity unification (an `open_id` is scoped to one Feishu app).
- Role-based permissions or admin impersonation.

## Key interfaces

- `Inbound{ OpenID, ChatID, ChatType }` — routing inputs.
- `botHandler.session(openID) *botSession` — per-user lookup.
- `botSession{ sid, mem }` — one user's isolated state.
