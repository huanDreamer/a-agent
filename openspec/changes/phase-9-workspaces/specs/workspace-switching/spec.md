# Capability: workspace-switching

## Purpose

Let a conversation — in the browser or on Feishu — point the agent at a different
workspace, and let the console organise conversations by workspace, without a
restart and without a mistake silently landing writes in the wrong directory.

## Scope

- `internal/chat` — the per-turn scope and the per-turn tool registry.
- `internal/server` — the workspace API, the session↔workspace binding, and the
  directory-browsing endpoint behind the picker.
- `internal/platform/feishu` + `cmd/huan-agent` — the command surface and the
  natural-language intent.
- `web/src` — the sidebar grouping (folders), which is where a conversation's
  workspace is shown and chosen.

## Requirements (MUST)

1. **Scope is per turn and travels with the context.** A turn MUST be able to
   declare a scope; the scope MUST reach every tool invocation of that turn
   through the context, so a tool never has to guess which directory it is in.
2. **No scope means the same directory the process always used.** A turn without
   a scope MUST run in the seeded workspace (`tools.workspace`, or the process
   working directory) — i.e. exactly the behaviour of a deployment that never
   heard of workspaces.
3. **Per-turn tool set.** The registry a turn uses MUST be derived from that
   turn's workspace root. Tools registered at runtime — MCP servers, the `skill`
   tool — MUST still be present in the derived registry.
4. **Explicit switching.** A surface that offers switching MUST switch its own
   scope's workspace and MUST report the result as old name → new name plus the
   resolved absolute root. A switch MUST NOT be silent.

   Amended after this phase: the **Feishu** surface switches (commands and
   conservative natural language, requirements 8–10), and the HTTP API keeps
   `PUT /api/chat/sessions/{id}/workspace` for any client that wants it. The
   **web console** deliberately has no control for it any more — its header
   picker repeated what the sidebar's folder already says, and removing it left
   no way to move an existing conversation between workspaces from the console.
   A conversation is placed in a workspace when it is created (the + on a folder)
   and stays there. `docs/admin.md` states this for operators.
5. **Switching is per scope.** Switching MUST affect only the requesting scope.
6. **Refusals are answers, not failures.** An unknown workspace name MUST produce
   an actionable message naming what exists, not a generic error and not a silent
   fallback to a different directory.
7. **Feishu has the tools it switches between.** The Feishu channel MUST run turns
   through the same tool-calling runner the web chat uses, so switching a
   workspace there changes something observable. The channel MUST stay usable
   when tools are switched off by configuration (plain conversational answers).
8. **Command surface.** The Feishu channel MUST offer unambiguous commands:
   `/workspace` (report the current one), `/workspaces` (list the available ones),
   `/workspace <name>` (switch). These MUST take priority over any
   natural-language interpretation.
9. **Natural language is conservative.** A message MUST be treated as a switch
   request only when it is a *short, verb-initial imperative* naming a workspace
   (e.g. `切换到 myproj 工作区`), or when it opens with the switch keyword. A
   request that merely *mentions* a workspace — "在工作区里建个 hello.go" — MUST
   NOT be interpreted as a switch, and MUST be answered normally.
10. **Name resolution order.** A requested name MUST be resolved as: exact match →
    unique case-insensitive prefix/substring match → one model call that picks from
    the candidate list → ask the user to choose. An ambiguous or unresolvable name
    MUST NOT be guessed.
11. **The switch is visible in the conversation.** The turn that switches MUST
    answer with the effective workspace and its directory, and a normal answer
    produced inside a workspace MUST be attributable to that workspace.
12. **The browser switches by request, not by prose.** The browser MUST switch
    through the API for the session it is in; it MUST NOT depend on the model
    reading the user's message.
13. **Conversations are grouped by workspace in the sidebar.** The sidebar MUST
    organise conversations under their workspace, MUST let that workspace be
    renamed and deleted in place (two-step, with the consequences stated), and
    MUST let a conversation be created directly in a workspace.
14. **A deleted or dangling binding never strands a conversation.** Listing and
    opening a conversation whose workspace is gone MUST fall back to the seeded
    workspace, and the fallback MUST be recorded in the log rather than hidden.

## Non-goals

- Switching a workspace from inside a tool call by the model's own decision
  (the model may not move its own confinement boundary).
- Migrating, copying or moving files between workspaces.
- Concurrent multi-workspace turns inside one scope.
- A natural-language *admin* surface: creating, renaming or deleting a workspace
  by prose on Feishu is out of scope (the console owns that).
- Dragging a conversation between sidebar folders (the header picker switches it).
- Auto-selecting a workspace from the content of a request (a task must not be
  routed to a directory the user did not name).

## Key interfaces

- `chat.Request.Scope`, `chat.WithScope`, `chat.ScopeFrom`, `chat.Config.ToolsFor`
- `workspaces.Manager.Active` / `Select` / `Resolve` / `Browse`
- `workspaces.ParseSwitchIntent(text, names)`, `workspaces.ResolveName`
- `server.ChatDeps.ToolsFor`, `server.ChatDeps.Workspaces`
- `GET/POST /api/workspaces`, `PATCH/DELETE /api/workspaces/:name`,
  `GET /api/fs/dirs`, `PUT /api/chat/sessions/:id/workspace`
