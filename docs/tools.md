# Tools: file, search and command access

These tools are what turn huan-agent from a chat bot into something that can
actually work on a codebase: read files, search them, edit them, and run
commands.

> **Read this first.** Enabling these tools lets an LLM read, write and execute
> against a real directory. Anyone who can send the bot a message can therefore
> drive them, including over Feishu. The working directory is confined; **what a
> command can reach is not**. If that is not what you want, set
> `tools.read_only: true`, or `tools.enable_bash: false`, or point
> `tools.workspace` at a scratch directory.

## Configuration

```yaml
tools:
  # The confinement root. All file and command operations resolve inside it.
  # Empty = the process working directory (where you launched huan-agent).
  workspace: "/Users/me/code/my-project"

  # Reads and searches only: write_file, edit_file and bash are withheld.
  read_only: false

  # Shell execution. Enabled by default because it is what makes the agent
  # useful; turn it off to keep file editing without command execution.
  enable_bash: true
  bash_timeout_seconds: 120

  # Per-call limits, to stop one tool call from flooding the model's context.
  max_read_kb: 512
  max_write_mb: 4
  max_list_entries: 500

  # RE2 patterns that refuse a matching command. Empty = the built-in set.
  # A SPEED BUMP, not a security boundary: an equivalent command that does not
  # match will run.
  deny_patterns: []
```

## The tools

| Tool | Capability | What it does |
|---|---|---|
| `read_file` | read | Returns the file with 1-based line numbers (`   12→content`). Optional `offset`/`limit`. Refuses directories and binary files, and reports truncation. |
| `list_dir` | read | Lists a directory: directories first, then files, each alphabetical. |
| `glob` | read | Finds files by path pattern. Supports `*` (within a segment), `?`, and `**` (any depth); a pattern with no `/` matches at any depth. |
| `grep` | read | Searches contents with RE2. Filters by `include` glob, supports `context_lines`, caps matches, clips long lines. |
| `write_file` | write | Creates or overwrites a file, creating parent directories, writing atomically. |
| `edit_file` | write | Replaces exact text. **The text must appear exactly once** unless `replace_all` is set. |
| `bash` | exec | Runs a command through `/bin/sh -c` with the working directory confined to the workspace. |
| `time`, `calc`, `echo` | read | The original utility tools. |

Typical use: `glob` to find the file, `read_file` to see it, `grep` to find the
call sites, `edit_file` to change it, `bash` to run the tests.

## What the confinement does and does not do

**Does:**

- Every path a tool accepts goes through one resolver (`internal/workspace`).
  Escapes via `..`, absolute paths outside the root, NUL bytes, and symlinks
  inside the root that point outside are all refused. The symlink case is the
  one a naive literal-path check misses, so containment is re-checked after
  evaluating symlinks on the deepest existing prefix.
- `read_file`/`grep` refuse binary files rather than dumping bytes into the
  model's context; reads, writes and listings are bounded by the limits above.
- `bash` runs with `cmd.Dir` set to a resolved directory inside the workspace.
- A command that outlives its timeout is killed **as a process group**, so a
  backgrounded child cannot survive the call.
- Every tool invocation is written to the audit log with its duration and
  outcome, and shown in the web UI as it happens.

**Does not:**

- **Confine what a command can touch.** `bash` sets the working directory; a
  command can still read and write absolute paths elsewhere, and there is no
  mount namespace, container or seccomp filter. The deny list refuses a handful
  of obviously catastrophic commands and nothing more.
- **Ask for approval.** A write or a command runs when the model calls it. There
  is no confirmation step to accept or reject.
- **Protect secrets that are readable by the process user.** `~/.ssh`, `.env`
  or any credential file inside the workspace is readable by `read_file`, and
  anything the process can reach is reachable by a command.

If you want a real boundary, run the agent as a dedicated user in a container
with only the project mounted, and keep `tools.workspace` inside that mount.

## Per-surface access

Each tool declares a capability (`read`, `write`, `exec`), and a registry can be
filtered to a subset with `tool.FilterByCapabilities`. Filtering starts from what
the source registry permits, so it can only narrow access, never widen it.

Both the web admin and the Feishu bot currently receive the full set, which is
the configured intent. To give the bot less, filter its registry down to
`tool.CapRead` where the handler is built.

## macOS and Linux

Nothing here is macOS-specific: paths use `filepath`, and `bash` picks its shell
by `runtime.GOOS` (`/bin/sh` on darwin and linux), returning a clear error on a
platform it does not know. Linux works as-is.

Two things to review before treating Linux as first-class:

- **Case sensitivity.** Containment compares paths as written; on a
  case-insensitive filesystem (macOS default) two spellings can name one file.
  The resolver does not normalise case, which is correct on Linux and merely
  permissive on macOS.
- **`/proc` and `/sys`.** A command can read them regardless of the workspace.

## Troubleshooting

- **The agent says it has no tools / calls nothing** — check that
  `tools.workspace` resolves and that `enable_bash` is what you expect; a
  read-only workspace deliberately omits the write and exec tools. The tool list
  is in the web chat header and in `huan-agent chat`'s `/tools` output.
- **"path is outside the workspace"** — the path escapes the root. Use a path
  relative to the workspace, or change `tools.workspace`.
- **"tool ... is not permitted"** — the registry's allow-list excludes it; see
  `agent.allowed_tools`.
- **A command was refused by a deny pattern** — the error names the pattern.
  Override `tools.deny_patterns` if the pattern is wrong for you.
- **A command hit the timeout** — raise `tools.bash_timeout_seconds`; the output
  says how long it ran and what the limit was.
- **Output ends with a truncation marker** — raise `max_read_kb`, or have the
  agent read a smaller range with `read_file`'s `offset`/`limit`.
