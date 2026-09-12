# Capability: workspace-sandbox

## Purpose

Let an LLM read, write and execute against a real directory without giving it
the whole machine, and make that confinement a single, testable boundary rather
than a rule each tool is trusted to remember.

## Scope

- `internal/workspace` — path resolution, containment, limits, read-only mode.
- `internal/tool/builtin` — every tool that touches a path.

## Requirements (MUST)

1. **Single resolution point.** Every path accepted from a tool call MUST be
   resolved through the workspace before it is used. No tool may join paths or
   open a caller path directly.
2. **Containment.** A resolved path MUST be the root or inside it. Escapes via
   `..`, via an absolute path outside the root, and via a symlink inside the
   root whose target is outside MUST all be refused with a distinguishable error.
3. **Symlinks are resolved, not inspected.** Containment MUST be re-checked
   after evaluating symlinks on the deepest part of the path that exists, so a
   symlink cannot be used to step outside — and a path that does not exist yet
   (a write target) MUST be checked through its parent.
4. **Root aliasing.** A root that is itself a symlink MUST be resolved once, so
   children compare against the resolved form rather than being rejected.
5. **Invalid paths.** An empty, whitespace-only or NUL-containing path MUST be
   refused distinctly from an escape.
6. **Read-only mode.** A read-only workspace MUST refuse writes with a
   distinguishable error while still permitting resolution and reads.
7. **Limits.** Reads, writes and listings MUST be bounded by configurable
   limits, and a write over the limit MUST be refused before anything is written.
8. **Binary awareness.** Text tools MUST be able to detect binary content and
   refuse it rather than putting it in the model's context.
9. **Stable display paths.** Tools MUST report workspace-relative paths so
   output does not depend on where the workspace lives on disk.
10. **Platform honesty.** The implementation MUST work on macOS and Linux with
    no OS-specific branching beyond the shell choice, and its assumptions that
    would need review on another platform (case sensitivity, special
    filesystems) MUST be documented rather than assumed away.
11. **Testability of the boundary.** There MUST be tests that assert both the
    refusal AND the absence of the side effect for every escape shape.

## Non-goals

- OS-level isolation (containers, namespaces, seccomp). The confinement bounds
  the working directory; `docs/tools.md` says so explicitly.
- Confining what a command can reach — a shell command can touch absolute paths.
- Multi-root workspaces.
- Per-path ACLs.

## Key interfaces

- `workspace.New(root, Options)`, `Resolve`, `ResolveForWrite`, `Rel`, `Limits`,
  `ReadOnly`, `IsBinary`
- `workspace.ErrOutsideWorkspace`, `ErrInvalidPath`, `ErrReadOnly`
