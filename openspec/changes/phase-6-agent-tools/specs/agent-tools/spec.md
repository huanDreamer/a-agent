# Capability: agent-tools

## Purpose

Give the agent the tools a coding assistant needs — read, search, edit and
execute — with descriptions good enough that a model uses them correctly, and a
declared capability so a caller can expose a subset.

## Scope

- `internal/tool/builtin` — the file, search and command tools.
- `internal/tool` — capability tagging and filtering.
- `cmd/huan-agent` — registration per surface.

## Requirements (MUST)

1. **Read.** `read_file` MUST return line-numbered text (1-based, fixed-width
   gutter), support an offset and a limit, refuse directories and binary files
   with clear messages, and report truncation in a way the model will notice.
2. **Write.** `write_file` MUST create missing parent directories and write
   atomically (temp file plus rename) so an interrupted write cannot leave a
   half-written file, preserving an existing file's mode.
3. **Edit.** `edit_file` MUST require the target text to occur exactly once
   unless `replace_all` is set; zero matches, multiple matches without
   `replace_all`, and an empty target MUST each be refused with a message that
   tells the model what to do next. A refusal MUST leave the file byte-identical.
4. **List.** `list_dir` MUST report type, size and a workspace-relative path for
   each entry, sorted deterministically (directories first).
5. **Search.** `glob` MUST support `*`, `?` and `**`, match at any depth for a
   separator-free pattern, and skip noise directories (`.git`, `node_modules`,
   …) unless the pattern names one. `grep` MUST use RE2, support an `include`
   filter, case sensitivity and context lines, cap the number of matches, and
   clip a very long matched line.
6. **Execute.** `bash` MUST run through a shell with its working directory
   confined to the workspace, MUST bound the run with a timeout, MUST capture
   stdout and stderr separately with caps that report how much was discarded,
   and MUST kill the whole process group on timeout so a backgrounded child
   cannot outlive the call.
7. **Refusals do not execute.** Every refusal (outside the workspace, read-only,
   disabled, deny pattern) MUST happen before the command is started, and tests
   MUST prove nothing ran.
8. **Honest descriptions.** A tool description MUST NOT claim a safety property
   the implementation cannot provide — in particular the command tool MUST NOT
   claim to confine what a command can reach.
9. **Descriptions survive.** A field description MUST reach the provider intact.
   Because the schema generator splits the struct tag on commas, a description
   containing one is silently truncated; a test MUST fail if that happens.
10. **Capabilities.** Every tool MUST declare `read`, `write` or `exec`; a tool
    that declares nothing MUST be treated as `read`. Filtering a registry by
    capability MUST start from what the source permits, so it can only narrow
    access and never widen it.
11. **Withholding over refusing.** A read-only workspace MUST leave the write
    and command tools unregistered rather than registering and refusing them.
12. **Audit.** Every invocation MUST be recorded with its tool name, arguments,
    outcome and a **measured** duration; a duration that is always zero is a
    defect, not a rounding artefact.
13. **No panics.** A tool called with a nil workspace or malformed arguments
    MUST return an error, never panic.
14. **Testability.** Every tool MUST be exercisable against a temporary
    directory with no network access.

## Non-goals

- Interactive approval before a write or command (a separate change).
- A patch/diff editing tool; `edit_file` covers the need.
- Language-server integration or semantic refactoring.
- Tool plugins loaded from outside the binary (MCP already covers that).

## Key interfaces

- `builtin.NewReadFileTool`, `NewWriteFileTool`, `NewEditFileTool`,
  `NewListDirTool`, `NewGlobTool`, `NewGrepTool`, `NewBashTool`
- `builtin.BashPolicy`, `DefaultBashPolicy`, `DefaultBashTimeout`,
  `DefaultBashMaxOutputBytes`
- `tool.Capability`, `tool.CapRead/CapWrite/CapExec`, `tool.WithCapability`,
  `tool.CapabilityOf`, `tool.FilterByCapabilities`
- `tool.SpecOf` (parameters schema) and `tool.Registry`
