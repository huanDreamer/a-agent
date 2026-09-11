# Capability: web-chat

## Purpose

Let a person converse with the agent from the admin UI: manage conversations,
choose the model per conversation, and watch the agent work — its reasoning and
every tool call — as it happens rather than only seeing a final answer.

## Scope

- `internal/chat` — the streaming conversational runner and its event model.
- `internal/store` — chat session and message persistence.
- Admin API under `/api/chat/*`.
- The 对话 tab of the web UI.

## Requirements (MUST)

1. **Sessions** — a conversation MUST be a persisted session with an id, title,
   the user it belongs to, and the provider/model it runs on. Creating, listing,
   fetching, renaming, changing the model, clearing messages and deleting MUST be
   supported.
2. **Ownership and isolation** — session endpoints MUST require authentication,
   and clearing a session MUST empty its history without destroying the session.
3. **Deletion** — deleting a session MUST remove its messages too, leaving no
   orphaned rows.
4. **Model selection** — the model MUST be selectable per session from the
   configured providers, the choice MUST persist, and a turn MUST run on the
   session's current model. Changing only the provider MUST reselect that
   provider's default model rather than keeping a model from the previous one.
5. **Streaming** — sending a message MUST stream partial output to the client as
   it is produced, because reasoning and tool calls are only useful while they
   happen. A buffered response is not acceptable.
6. **Event granularity** — the stream MUST distinguish at least: step start,
   assistant text delta, reasoning delta, tool call, tool result, token usage,
   successful completion, and failure.
7. **Reasoning separation** — the model's reasoning MUST be delivered and stored
   separately from the answer, and MUST NOT be presented as or mixed into the
   answer text.
8. **Tool visibility** — each tool call MUST be reported with its name, arguments
   and a stable id, and each result MUST pair to its call by that id, carrying the
   output, the error (if any) and the duration.
9. **Tool failure is not turn failure** — a failing tool MUST be reported to the
   client, recorded in the audit log, and fed back to the model as the tool
   observation so the model can adapt; the turn MUST continue.
10. **History replay** — a subsequent turn MUST replay the system prompt and the
    stored conversation so the assistant has context.
11. **No display state in model input** — stored reasoning and tool-call metadata
    MUST NOT be replayed as model input, because an assistant tool call without
    its paired tool result is rejected by providers.
12. **Persistence across disconnects** — the answer MUST be stored even if the
    client disconnects mid-turn, and the user's message MUST be stored before the
    model is called so it is never lost.
13. **Usage** — token usage MUST be reported per turn and persisted with the
    answer.
14. **Cancellation** — the client MUST be able to stop a turn, and a stopped turn
    MUST still leave what was produced in the history.
15. **Untrusted output** — model output MUST be rendered as text (never injected
    as HTML).
16. **Misconfiguration is reported** — an unusable model choice MUST produce a
    clear error response rather than a stream that fails mid-flight.
17. **Testability** — the runner MUST be exercisable without a real LLM, and the
    streaming endpoint MUST be testable end to end.

## Non-goals

- Multi-user accounts or per-user isolation beyond an authenticated admin.
- Attachments or image input in the web chat.
- Editing or deleting individual messages.
- Branching/regenerating a conversation.

## Key interfaces

- `chat.Runner.Run(ctx, Request, Emitter) (*Result, error)`, `chat.Event`,
  `chat.EventType`, `chat.Result`, `chat.Tracer`
- `store.ChatSession`, `store.ChatMessage`, `store.ChatSessionPatch`
- `server.ChatDeps`, `server.ModelBuilder`, `server.ModelChoice`
- `GET/POST /api/chat/sessions`, `GET/PATCH/DELETE /api/chat/sessions/{id}`,
  `POST /api/chat/sessions/{id}/clear`, `GET /api/chat/models`,
  `POST /api/chat/sessions/{id}/messages` (SSE)
