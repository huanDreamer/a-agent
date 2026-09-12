# Phase 6 — Agent tools: file, search and command access

## Summary

Give the agent the ability to actually work on a codebase: read files, search
them, edit them, and run commands — confined to a workspace directory.

## Why

Phases 1–5 built a system that can *talk* about work. The tool registry held
three utilities (`time`, `calc`, `echo`), so asking the agent to fix a bug
produced prose, not a diff. The gap between "an agent platform" and "an agent
that does something" is exactly this: tools with real effects.

The hard part is not the file reading. It is that the same feature lets an LLM
write to a real disk and execute real commands, on a machine that also handles
messages from an IM bot. So the confinement is the substance of this change and
the tools are the easy part.

## What Changes

### ADDED

- `internal/workspace` — the confinement, in one place:
  - `Resolve` turns a caller path into an absolute path proven to be inside the
    root, rejecting `..` escapes, absolute paths outside, NUL bytes, and
    symlinks inside the root that point outside (by evaluating symlinks on the
    deepest existing prefix and re-checking).
  - `ResolveForWrite` adds the read-only and write-size checks.
  - Read/write/listing limits, so one call cannot flood the model's context.
- File tools: `read_file` (line-numbered, binary- and directory-aware,
  truncation reported), `write_file` (atomic via temp file + rename),
  `edit_file` (unique-match requirement), `list_dir`.
- Search tools: `glob` (with `**`, implemented rather than pulled in) and `grep`
  (RE2, `include` filter, context lines, capped matches, clipped lines).
- `bash`, running through `/bin/sh -c` with `cmd.Dir` confined to the workspace,
  a timeout, separately capped stdout/stderr, and a deny list.
- Tool capabilities (`read`/`write`/`exec`) plus `FilterByCapabilities`, so a
  surface can be given less than the whole set.

### CHANGED

- `registerBuiltinTools` takes the config and builds the workspace tools; a
  read-only workspace withholds the write and exec tools rather than refusing
  them at call time.
- Config gains `tools.*`.

## Design decisions

- **One confinement point.** Every tool resolves through `internal/workspace`.
  A per-tool check is how an agent ends up reading `/etc/shadow`: the tool that
  forgets is the one that matters.
- **Symlink escapes are checked by resolving, not by string inspection.** A
  literal-path check passes `link -> /etc` and then follows it.
- **Withhold, do not refuse.** In a read-only workspace the write and exec tools
  are not registered at all, so the model cannot see — or waste a step trying —
  what it may not use.
- **`edit_file` requires a unique match.** A silently-applied edit to the wrong
  occurrence corrupts code in a way that is hard to notice; a refusal is cheap.
- **The deny list is described as a speed bump.** It refuses `rm -rf /` and
  similar, and an LLM can trivially write an equivalent command that does not
  match. The tool description says so rather than implying a boundary that does
  not exist.
- **Deliberately no approval step.** Writes and commands run when the model
  calls them. Adding interactive approval is a separate change; pretending to
  have it would be worse than not having it.

## Impact

- **Security**: this is the change that lets remote input (a Feishu message)
  reach a real filesystem and shell. The confinement bounds the working
  directory and the obvious catastrophes; it is not a sandbox. `docs/tools.md`
  states this plainly, including the recommendation to use a container and a
  dedicated user if a real boundary is wanted.
- **Config**: `tools.workspace` defaults to the process working directory, so
  the tools are useful out of the box; `read_only` and `enable_bash` tighten it.
- **Compatibility**: with an empty workspace the tools are simply absent, and
  nothing else changes.
