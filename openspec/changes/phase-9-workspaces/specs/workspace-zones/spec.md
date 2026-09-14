# Capability: workspace-zones

## Purpose

Make "the directory the agent works in" a first-class, named thing that a person
picks from their own filesystem, so the agent can be pointed at several real
projects — one per workspace — and every conversation belongs to exactly one.

## Scope

- `internal/workspace` — the single-root sandbox (unchanged semantics).
- `internal/workspaces` — the set of workspaces, their roots, per-scope selection,
  directory browsing for the picker.
- `internal/store` — persistence of the set, of the scope bindings, and of which
  workspace each conversation is in.
- `cmd/huan-agent` — the per-turn binding of workspace tools.
- `web/src` — the sidebar folders and the directory picker.

## Requirements (MUST)

1. **One confinement point is preserved.** Every workspace MUST be an
   `internal/workspace.Workspace`; no new code path may open, join or stat a
   caller-supplied path outside it. The multi-workspace layer MUST NOT weaken any
   guarantee in `workspace-sandbox`.
2. **A workspace is an existing directory.** Creating one MUST take a path that
   already is a directory (symlinks resolved), and MUST refuse a path that does
   not exist, is a file, or is unreadable. Creating a workspace MUST NOT create a
   directory. The filesystem root MUST be refused.
3. **The name is a label.** A name MUST be non-empty after trimming, MUST NOT
   contain a path separator or a control character, and MUST be unique
   case-insensitively. It MUST default to the directory's base name when the
   caller does not supply one. It MUST NOT be constrained by filesystem rules,
   because it is never a path component.
4. **No per-workspace policy.** A workspace MUST NOT carry read-only, command or
   limit settings: those are process-wide (`tools.*`). A capability that differs
   per workspace is out of scope by construction, not by convention.
5. **Every conversation belongs to exactly one workspace.** Creating a
   conversation MUST record its workspace in the same request; listing
   conversations MUST report one for each. A conversation whose binding is
   missing or points at a deleted workspace MUST resolve to a workspace that
   exists rather than to nothing, and the resolution MUST be logged when it
   happens.
6. **The set is never empty.** On startup, if no workspace is registered, one
   MUST be seeded from `tools.workspace` (or the process working directory),
   named after that directory, so an untouched deployment keeps working in the
   directory it already used.
7. **Rename moves the label only.** Renaming MUST NOT touch the directory or the
   files in it, MUST update the scope bindings in the same transaction, and MUST
   refuse a name that collides.
8. **Delete never deletes files.** Deleting MUST remove the registration and
   MUST leave every byte on disk where it was. Conversations in it MUST be moved
   to another registered workspace, and the answer MUST say where. Deleting the
   last remaining workspace MUST be refused.
9. **Per-scope selection.** The active workspace MUST be tracked per scope
   (`web:<session id>`, `feishu:<open id>`), NOT globally. Selecting one in a
   scope MUST NOT change any other scope.
10. **Browsing is a real directory listing.** The picker MUST be driven by the
    server's own filesystem (a browser `file` input cannot yield a server path),
    MUST list directories only, MUST refuse an unreadable or non-directory path
    with a message, MUST be bounded in the number of entries returned, and MUST
    start at the user's home directory when no path is given.
11. **Isolation must be tested, not asserted.** There MUST be a test that a tool
    bound to workspace A refuses a path inside workspace B, and a test that two
    concurrent turns in two scopes write into their own roots.
12. **Honest limits.** The documentation and the UI MUST state that workspace
    isolation is path confinement, not OS-level isolation, and that a command can
    still reach absolute paths outside its workspace.

## Non-goals

- Containers, namespaces, seccomp, or any OS-level isolation between workspaces.
- Confining what `bash` can reach (unchanged from `workspace-sandbox`).
- Per-workspace read-only / command / limit settings (removed on purpose).
- Creating directories from the console, templates, or dependency bootstrap.
- Git integration, or copying/moving files between workspaces.
- Deleting, moving or archiving a workspace's files.

## Key interfaces

- `workspaces.Manager`: `List` / `Get` / `Create` / `Rename` / `Delete` /
  `Active` / `Select` / `Resolve` / `Open` / `EnsureSeed` / `Browse`
- `workspaces.Spec`, `workspaces.CreateInput`, `workspaces.DirEntry`
- `store.Workspace`, `store.Store.ListWorkspaces` / `UpsertWorkspace` /
  `RenameWorkspace` / `DeleteWorkspace` / `GetWorkspaceBinding` /
  `SetWorkspaceBinding` / `MoveWorkspaceBindings` / `CountSessionsByWorkspace`
- `store.ChatSession.Workspace`, `store.ChatSessionFilter.Workspace`
- `tool.Registry.Clone`, `tool.Registry.Replace`
