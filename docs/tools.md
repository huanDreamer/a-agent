# Tools: file, search and command access

These tools are what turn huan-agent from a chat bot into something that can
actually work on a codebase: read files, search them, edit them, and run
commands.

> **Read this first.** Enabling these tools lets an LLM read, write and execute
> against a real directory. Anyone who can send the bot a message can therefore
> drive them, including over Feishu, where the bot has the tools by default
> (`feishu.enable_tools`). The working directory is confined; **what a command
> can reach is not**. If that is not what you want, set `tools.read_only: true`,
> or `tools.enable_bash: false`, or point `tools.workspace` at a scratch
> directory, or give each project its own read-only workspace.

## Configuration

```yaml
tools:
  # The directory the agent works in when no workspace has been set up yet: it
  # becomes the first workspace on a fresh install (see "Workspaces" below).
  # All file and command operations resolve inside one workspace. Empty = the
  # process working directory (where you launched huan-agent).
  workspace: "/Users/me/code/my-project"

  # Reads and searches only: write_file, edit_file and bash are withheld.
  read_only: false

  # Shell execution. Enabled by default because it is what makes the agent
  # useful; turn it off to keep file editing without command execution.
  enable_bash: true
  bash_timeout_seconds: 120
  # The ceiling one call may raise its own timeout to. A build or a test suite
  # that legitimately needs longer asks for it with timeout_ms, so the default
  # every other command waits under does not have to grow. A value below
  # bash_timeout_seconds is raised to it.
  bash_max_timeout_seconds: 900

  # Background processes: dev servers, --watch builds, resident APIs and
  # databases. These are the tools that may leave something running after the
  # call; `bash` never does. See "Background processes" below.
  enable_background: true
  background_dir: ""            # empty = a "jobs" directory beside the database
  background_max_jobs: 8        # starting one more is refused, with a message
  background_log_max_mb: 8      # one job's log file stops here (and says so)
  background_window_kb: 256     # how much output a reader can still be served
  background_stop_grace_seconds: 5   # SIGTERM to SIGKILL when stopping a job

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
| `bash` | exec | Runs a command through `/bin/sh -c` with the working directory confined to the workspace, waits for it, and kills its whole process group when the call ends. For things that finish on their own. `timeout_ms` may shorten its limit or extend it up to `tools.bash_max_timeout_seconds`. |
| `bash_background` | exec | Starts a long-lived process (dev server, `--watch` build, resident API or database) and returns a job id. The result carries the exit code and output if it died within the startup grace. For things that are meant to keep running. |
| `bash_jobs` | exec | Lists this workspace's background jobs: status, pid, command, run time, log file. Finished jobs stay listed until their record is dropped, which is how a server that crashed while nobody was looking is found. |
| `bash_output` | exec | Reads a job's output by byte offset (`from` → `next`), optionally waiting up to `wait_ms` for new lines. Reports bytes the bound no longer holds instead of pretending the stream is continuous. |
| `bash_stop` | exec | Signals a job's whole process group: SIGTERM, then SIGKILL after the grace. `forget` also deletes the record and the log, and is refused while the job runs. |
| `time`, `calc`, `echo` | read | The original utility tools. |
| `skill` | read | Loads one enabled skill's instructions. The conversation's system prompt lists the enabled skills by name and description, and the model calls this to pull in a body only when a task matches — so a set of skills costs a few lines of prompt rather than their full text. Managed in 设置 → 技能. |
| `save_document` | read | Stores a document (report, summary, note) in the OpenViking context database, where it stays searchable in later conversations. Registered only when `openviking.documents.enable` is on — the destination is the operator's configuration, so the model passes a title and a body, never a path. See `docs/openviking.md`. |
| *MCP tools* | read | Whatever the configured MCP servers expose, registered under the names the servers report. Managed in 设置 → MCP: saving a server connects it and adds its tools here immediately, deleting or disabling one takes them away. An enabled OpenViking is registered automatically, which is how the model gets `find` / `read` / `write` / `add_resource`. |

Typical use: `glob` to find the file, `read_file` to see it, `grep` to find the
call sites, `edit_file` to change it, `bash` to run the tests.

### No terminal

`bash` and the background tools run with standard input at `/dev/null` and no
PTY, so a command that waits for an answer gets EOF and fails instead of hanging
the turn. That is deliberate — nothing is there to type into a prompt — and both
descriptions say so *and* say what to do instead: pass the non-interactive flag
the tool offers (`-y` / `--yes`, `--no-input`, `CI=1`, `git commit -m`), or pipe
the answers in. `npm init` without `-y`, a scaffolder's questionnaire, an editor
(`git rebase -i`, `git commit` with no `-m`) and an `ssh` password prompt are the
usual ways to land here. The default system prompt carries the same instruction,
since a model that reads it in the prompt does not have to learn it by failing.

## Background processes

`bash` ends with nothing left behind — that is a guarantee, not a limitation, and
it is why it cannot start a dev server. A server, a watcher or a resident database
is defined by outliving the call that started it, so it gets **its own tool**:

```
bash_background  command="npm run dev"  name="vite"     → job-4f2a91, running
bash_output      id=job-4f2a91 wait_ms=2000             → "  ➜  Local: http://localhost:5173/"
bash_jobs                                               → one running job, its pid, its log file
bash_stop        id=job-4f2a91                          → stopped, exit code, tail of the log
```

What it does:

- Runs the command in **its own session and process group**, so it is not killed
  by a signal aimed at the agent and cannot be caught by another job's stop.
- Captures stdout **and** stderr into one interleaved stream: a bounded in-memory
  tail (`background_window_kb`) plus a log file capped at `background_log_max_mb`.
  Neither grows with how long the process runs, and hitting the file cap is
  recorded in the file.
- Reports a job as **running** while its process *group* has a member. That
  matters because a wrapper script, a pipeline or anything that forks leaves the
  server behind after the shell exits; "the sh is gone" is not the question
  anyone is asking.
- Stops the **whole group**, SIGTERM first and SIGKILL after
  `background_stop_grace_seconds`, so a server with worker processes actually
  stops.

What it does not do — and the tool descriptions say so, so the model is not
surprised:

- **Nothing survives the agent.** Every job is terminated when the process that
  started it exits (`admin serve`, `chat`, `serve` — each owns its own jobs). This
  is a feature: a process with no supervisor is a leak. A service that must
  survive a reboot belongs to launchd, systemd or a container.
- **No terminal and no input.** stdin is `/dev/null`, so a command that prompts
  fails rather than hanging.
- **No quotas.** No CPU, memory or disk limit, and no confinement beyond the
  working directory — a job has the agent user's full rights, exactly like `bash`.
- **One stream.** stdout and stderr are merged and cannot be told apart.
- **Per process, not per conversation.** Jobs are visible to the conversation that
  started them — the count sits in that conversation's header and opens a list
  beside it — and to every conversation of the same workspace, listed there as
  "同一工作区的其他进程". A Feishu bot running as its own process has its own
  jobs; the console does not show them.

The four tools follow the same policy as `bash`: a read-only workspace refuses,
`deny_patterns` refuses before anything runs, `cwd` is resolved inside the
workspace, and with `enable_bash: false` or `enable_background: false` they are
not registered at all.

## Workspaces

One root is rarely enough: the same agent is asked to work on a website one
minute and a service the next. A **workspace** is a directory the agent may work
in, and **every conversation belongs to one** — the sidebar lists conversations
under their workspace, like folders.

| | |
|---|---|
| Creating | In the console's left sidebar: 新建工作区 → browse the machine and pick an **existing** directory. Nothing is created on disk, and a typo ("that directory is not there") is reported at the form. `<root>` is resolved (symlinks included), and the filesystem root `/` is refused. |
| Naming | The label defaults to the directory's base name and can be renamed at any time. Renaming changes the label only — never the directory. |
| Per conversation | A new conversation is filed in a workspace immediately (the one the last conversation used, unless the sidebar's folder button was used). 对话's header switch moves a conversation between folders. |
| The first one | On a database with no workspaces, `tools.workspace` (or the process working directory) is registered as the first workspace, and conversations that predate the feature are filed into it. |
| Deleting | Removes the registration **and nothing else**: the directory and every file in it stay. The conversations inside move to another workspace, and the answer says which. The last remaining workspace cannot be deleted — a conversation must belong to one. |
| Missing directory | If a workspace's directory is deleted or moved behind the agent's back, the sidebar says so. Point it at a new directory by creating a workspace for it, or delete the stale one. |

A workspace carries **no policy**. Whether the agent may write files or run
commands is process-wide, as it always was:

- `tools.read_only: true` → `write_file`, `edit_file`, `generate_image` and
  `bash` are not registered at all (a shell can write, so read-only has to
  withhold it). Every workspace is affected equally.
- `tools.enable_bash: false` → no `bash`, everywhere.
- The size limits (`max_read_kb`, `max_write_mb`, `max_list_entries`) apply to
  every workspace too.

Capabilities are withheld rather than refused: the model is never offered a tool
it may not use, which is why a read-only deployment shows the model an "unknown
tool" error if it asks for `write_file` anyway.

### Switching a workspace

- **Web console**: 对话's header shows the current workspace (name, with the
  directory in its tooltip); the picker switches this conversation only.
- **Feishu**: `/workspace` reports the current one, `/workspaces` lists them, and
  `/workspace <name>` switches — or say it: `切换到 blog-site 工作区`. The choice
  is per sender, so in a group one person switching does not move anybody else.
  Workspaces are *created* in the console, not over chat.

### Isolation, honestly

Two workspaces are two roots with the same confinement rules — better than one
root, and **not** a sandbox between them:

- Every path a tool accepts is resolved inside the workspace it was given, so a
  tool bound to `alpha` cannot read `beta`'s files, even by absolute path:
  `workspace.ErrOutsideWorkspace`.
- `bash` is unchanged by this: a command's confinement is its working directory,
  and it can still read and write absolute paths anywhere the process user can
  reach.
- If you need a real boundary between projects, give each one a container (or a
  dedicated user) with only its own directory mounted.

Uploaded chat attachments are stored in the workspace their conversation was in,
and are recorded with it, so moving a conversation to another workspace does not
make an old attachment resolve to whatever sits at the same relative path there.
An attachment that belongs to another workspace is reported by name rather than
by a path that would resolve elsewhere.

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
  backgrounded child cannot survive the call. Starting something that *should*
  outlive the call is a separate, explicit tool (`bash_background`), and what it
  starts is still terminated when the agent exits.
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

The list above is what the **web chat** registers. `huan-agent chat --tools` builds
its own registry (builtins + MCP from the config file) and the Feishu bot builds
one too when `feishu.enable_tools` is on (the default), so the exact set differs
per surface. Each surface also supervises its own background jobs (see above). 设置 → 服务与工具 shows the set the running server exposes; the
left sidebar shows which workspace a conversation runs in.

## Per-surface access

Each tool declares a capability (`read`, `write`, `exec`), and a registry can be
filtered to a subset with `tool.FilterByCapabilities`. Filtering starts from what
the source registry permits, so it can only narrow access, never widen it.

Both the web admin and the Feishu bot receive the full configured set. To give
the bot less, set `feishu.enable_tools: false` (it then answers with no tools at
all), set `tools.read_only` / `tools.enable_bash`, or filter its registry down to
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
- **"path is outside the workspace"** — the path escapes the workspace root. Use
  a path relative to it, or point the conversation at a workspace that contains
  the file.
- **"工作区不存在: x"** — no workspace is registered under that name. `GET
  /api/workspaces` (or the console's sidebar) lists what exists; names are
  matched case-insensitively.
- **"unknown tool \"write_file\"" in the transcript** — the deployment is
  read-only (`tools.read_only`), so the tool was never offered. That error is the
  model asking for something that does not exist in its tool list, not a
  permission failure.
- **A workspace shows "目录已不存在"** — the directory was deleted or moved. Create
  a workspace for wherever it is now, or delete the stale one; until then,
  conversations in it fall back to another workspace rather than failing.
- **"tool ... is not permitted"** — the registry's allow-list excludes it; see
  `agent.allowed_tools`.
- **A command was refused by a deny pattern** — the error names the pattern.
  Override `tools.deny_patterns` if the pattern is wrong for you.
- **A command hit the timeout** — a single slow command should ask for more with
  `timeout_ms` (up to `tools.bash_max_timeout_seconds`); raise
  `tools.bash_timeout_seconds` only when *every* command legitimately needs longer.
  The output says how long it ran and what the limit was, and `timeout_capped`
  reports a request that the ceiling cut down.
- **A turn stopped partway through a long task** — that is the turn budget, not a
  tool failure. See `docs/long-tasks.md`.
- **Output ends with a truncation marker** — raise `max_read_kb`, or have the
  agent read a smaller range with `read_file`'s `offset`/`limit`.
