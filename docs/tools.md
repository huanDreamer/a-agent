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
  # match will run. The approval gate (tools.approval.mode) is the gate; this is
  # the bump.
  deny_patterns: []

  # Artifacts: the resources the agent produced — generated pages, reports,
  # images. Their bytes live in a directory of their own on the server (not in a
  # workspace: a deliverable has to survive the working copy), and the console
  # serves them over HTTP. See "Artifacts" below.
  artifacts:
    enable: true          # false = save_artifact refuses and the console says so
    root: ""              # empty = an "artifacts" directory beside the database
    public_urls: false    # true = artifact files need no login (see docs/admin.md)
    max_bytes: 0          # one artifact's cap; 0 = 32 MiB
```

## The tools

| Tool | Capability | What it does |
|---|---|---|
| `read_file` | read | Returns the file with 1-based line numbers (`   12→content`). Optional `offset`/`limit`. Refuses directories and binary files, and reports truncation. |
| `list_dir` | read | Lists a directory: directories first, then files, each alphabetical. |
| `glob` | read | Finds files by path pattern. Supports `*` (within a segment), `?`, and `**` (any depth); a pattern with no `/` matches at any depth. |
| `grep` | read | Searches contents with RE2. Filters by `include` glob, supports `context_lines`, caps matches, clips long lines. |
| `diagnostics` | read | Compiler and analyzer diagnostics for one file, or for the workspace when `path` is omitted. Requires a language server — see "Code intelligence" below. |
| `goto_definition` | read | Where the symbol at a 1-based line/column is defined. |
| `find_references` | read | Every reference to the symbol at a position, from the language server's view rather than a text search. |
| `workspace_symbols` | read | Searches the workspace for symbols by name — the tool for "which code handles X". |
| `spawn_agent` | read | Hands a self-contained question to a subagent that works in its own context and returns one short report. See "Subagents" below. |
| `fetch_url` | read | Fetches a web page and returns its readable text plus its same-site links. See "Reading the web" below. |
| `write_file` | write | Creates or overwrites a file, creating parent directories, writing atomically. |
| `apply_patch` | write | Applies a set of edits to several files, all or nothing. See "Editing across files" below. |
| `edit_file` | write | Replaces exact text. **The text must appear exactly once** unless `replace_all` is set. |
| `bash` | exec | Runs a command through `/bin/sh -c` with the working directory confined to the workspace, waits for it, and kills its whole process group when the call ends. For things that finish on their own. `timeout_ms` may shorten its limit or extend it up to `tools.bash_max_timeout_seconds`. |
| `bash_background` | exec | Starts a long-lived process (dev server, `--watch` build, resident API or database) and returns a job id. The result carries the exit code and output if it died within the startup grace. For things that are meant to keep running. |
| `bash_jobs` | exec | Lists this workspace's background jobs: status, pid, command, run time, log file. Finished jobs stay listed until their record is dropped, which is how a server that crashed while nobody was looking is found. |
| `bash_output` | exec | Reads a job's output by byte offset (`from` → `next`), optionally waiting up to `wait_ms` for new lines. Reports bytes the bound no longer holds instead of pretending the stream is continuous. |
| `bash_stop` | exec | Signals a job's whole process group: SIGTERM, then SIGKILL after the grace. `forget` also deletes the record and the log, and is refused while the job runs. |
| `save_artifact` | write | Stores a resource the agent produced — a page, a report, an image — on the server, in the artifact store rather than in a workspace, and returns a URL the console serves it from. See "Artifacts" below. |
| `time`, `calc`, `echo` | read | The original utility tools. |
| `skill` | read | Loads one enabled skill's instructions. The conversation's system prompt lists the enabled skills by name and description, and the model calls this to pull in a body only when a task matches — so a set of skills costs a few lines of prompt rather than their full text. Managed in 设置 → 技能. |
| `save_document` | read | Stores a document (report, summary, note) in the OpenViking context database, where it stays searchable in later conversations. Registered only when `openviking.documents.enable` is on — the destination is the operator's configuration, so the model passes a title and a body, never a path. See `docs/openviking.md`. |
| *MCP tools* | read | Whatever the configured MCP servers expose, registered under the names the servers report. Managed in 设置 → MCP: saving a server connects it and adds its tools here immediately, deleting or disabling one takes them away. An enabled OpenViking is registered automatically, which is how the model gets `find` / `read` / `write` / `add_resource`. |

Typical use: `glob` to find the file, `read_file` to see it, `grep` to find the
call sites, `edit_file` to change it, `bash` to run the tests.

## Reading the web

`fetch_url` fetches a URL, strips the navigation and scripts, and returns the
readable content. Without it the model's alternative is `curl`, and a modern
documentation page is hundreds of kilobytes of HTML in which the article is a small
fraction — what reaches the context is a truncated shell of it.

```
fetch_url("https://go.dev/blog/context")
  → title + 正文（`<pre>` 原样保留、其他标签剥掉）
  → links: ["https://go.dev/blog/...", …]      ← 同站链接，可以直接读下一篇
```

Long pages come back in windows: `max_chars` (default 20000 runes) bounds one call
and `offset` continues where the previous call stopped. The extracted text is
cached in-process for `cache_ttl_seconds`, so paging does not re-fetch.

Two properties are worth knowing:

- **The content is framed as untrusted.** It is the one input a stranger writes, so
  the tool's result opens with an explicit framing that anything looking like an
  instruction is data, not an instruction, and the prompt says the same.
- **It cannot reach your private network.** Loopback, private ranges, link-local
  addresses and the cloud metadata endpoint (`169.254.169.254`) are refused, and the
  refusal names what it blocked. The check runs on the initial URL, **on every
  redirect**, and again at dial time — a 302 into the metadata service and a DNS
  rebind are the two classic bypasses of a check that runs once. Setting
  `tools.web.allow_private: true` lifts this for a deployment whose wiki is on the
  LAN, and logs a warning at startup because it also lifts it for the agent's own
  local services.

Only text-like content is read (`text/*`, JSON, XML, YAML, Markdown). A PDF, an
image or an archive is refused with its media type in the message rather than
dumped into the context as bytes. A page that extracts to almost nothing — usually
one rendered by JavaScript — says so instead of looking like an empty page.

**Search is not part of this.** `web_search` is deliberately not implemented here:
search belongs to a provider whose ranking, quotas and terms are its own, and this
deployment reaches one through an MCP server (see `docs/mcp.md`). `fetch_url` reads
a page you already have the address of.

## Editing across files

Three tools edit files, and the division of labour is stated in each one's
description because the model is the one who has to pick:

| Tool | Use it when |
|---|---|
| `edit_file` | one change in one file |
| `apply_patch` | a change across several files, where a half-applied result would be worse than none (a rename, a signature change, the same fix in three places) |
| `rename_symbol` | every reference to one symbol — it asks the language server, not a string search |

### `rename_symbol`

It gives the 1-based line and column of the symbol, and the language server returns
the edits that rename it. The difference from a search-and-replace is not
convenience, it is correctness: a string search for `Add` also rewrites
`"call Add to sum"`, a comment that mentions it, and another package's function
with the same name. The server knows which identifier is being pointed at and
which references that binding resolves to — the same information the compiler has.

```
rename_symbol(path: "calc.go", line: 4, column: 6, new_name: "Sum")
  → 已重命名为 Sum：2 个文件、3 处引用（+3 -3）
```

Two things about it are worth knowing:

- **The file's siblings are opened first.** A language server only knows about
  documents it has been told about, so a rename asked for right after the target
  file was opened answers with the references it has seen — which is that file and
  nothing else. Silently missing the other files is the worst thing this tool could
  do: it leaves a repository that does not build and looks like it worked. This was
  found by running it, not by testing it; the fix opens the other files in the same
  directory (bounded to 64) before asking.
- **It is absent, not broken, in two situations**: a read-only workspace, and a
  workspace with no language server configured. Without a server the only
  implementations available are string searches, and a rename that quietly means
  "replace this word everywhere" is exactly what this tool exists to replace.

`dry_run: true` shows the plan — files, references, a bounded diff — and writes
nothing. The change goes through the same all-or-nothing applier as `apply_patch`,
so a rename either lands everywhere or nowhere; and because it is a write, it goes
through the approval gate and gets a checkpoint like any other.

**`apply_patch` is all or nothing**, and that is its entire reason for existing. A
rename that touches seven files, applied one `edit_file` call at a time, leaves a
repository that compiles nowhere the moment the fourth call fails — with three
changes already on disk. So it runs in two phases:

1. **Plan.** Read every file, apply every edit in memory, decide whether the whole
   batch is possible. It touches no disk. Any problem — a target outside the
   workspace, a binary file, an ambiguous or missing `old_string`, a result over
   the write limit, the same path twice — produces one error naming the file and
   the edit index, and the workspace is byte-for-byte what it was.
2. **Apply.** Take a checkpoint of every file about to change, then write each one
   atomically (temp file + rename). If a write fails, the files already written
   are restored from the originals phase 1 kept, and the report says which.

The invariant both phases exist to protect: **either every edit is on disk, or
none is.** If a rollback itself fails, the error says so explicitly — the
workspace is then in a state nobody chose, and that is the one outcome a caller
must not discover by surprise.

`dry_run: true` returns the same structure a real run does, plus a bounded diff
per file, and writes nothing. The approval card shows that same preview, computed
from the plan rather than described, so what a person approves is the change that
will happen.

The matching rules are `edit_file`'s exactly, from the same implementation: a
unique occurrence, or `replace_all`. The failure messages read the same way, and
they name the file and the edit index, because "one of your edits did not match"
is not something a model can act on.

### The two input forms

`apply_patch` takes either a structured `operations` list or a `patch` string:

```
operations: [{ path, edits: [{ old_string, new_string, replace_all? }] }]
patch:      "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -3,4 +3,4 @@\n…"
```

The structured form is the primary one — it is what a model writing a change from
scratch produces, and its matching rules are `edit_file`'s. The diff form exists for
the case where a diff already exists: the model wrote one, or someone pasted one
into the conversation. Both are parsed into the same operations and run through the
same applier, so **the atomicity guarantee does not depend on which form was used**.

The diff form is located by **context lines, not line numbers**. A hunk header's
numbers are used only to warn: a diff whose numbers drifted because someone edited
above the change still applies, and a diff whose *context* does not match is refused
rather than applied three lines off — a misapplied patch is silent, compiles
nowhere, and gives the model nothing to act on. A mismatch between the declared
counts and the hunk body is returned as a `warnings` entry rather than swallowed.

**This stage cannot create or delete files.** A patch that does is refused with the
alternative named: `write_file` for a new file, `bash` (behind the approval gate)
for a deletion. Creating and deleting are the two operations where "all or nothing"
needs a story about the directory entry itself, and that story is not written yet.

## Subagents

`spawn_agent` buys **context isolation, not compute**. Answering one question can
mean twenty greps and thirty file reads; in the parent's window those results stay
for the rest of the turn and crowd out the code being changed. A subagent gets a
window that is allowed to get dirty.

```
spawn_agent("调研 internal/tool 目录下有哪些文件、各自负责什么")
  → 报告 + 尾注：子 agent「调研 internal/tool…」：8 步，用了 read_file×18、grep×3，12000 tokens。
```

A real run of that: the nested run took 8 steps and read 24 files, and what crossed
back was a few hundred words plus the footer. The parent's own context never saw
the 24 reads.

### Several at once

Two ways, and both overlap:

```
spawn_agent(tasks: ["数一下 internal/mcp 有多少 .go 文件",
                    "数一下 internal/web 有多少 .go 文件",
                    "数一下 internal/edit 有多少 .go 文件"])
  → reports[0..2] 各一份，合并文本按任务编号分段
```

or calling the tool several times in one reply. **Use `tasks` when the questions do
not depend on each other** — it is the form that works on every surface, including
the CLI and the bot, whose loop runs a reply's tool calls one at a time: the fan-out
happens inside the tool, so it does not depend on the loop's scheduling.

`subagent.max_concurrent` (default 2) bounds it process-wide. A run that cannot get a
slot waits rather than failing, and the header says how many are queued.

### Watching them

The console's header carries a chip next to the background-process one — same
question, same shape — and it opens a drawer listing what this conversation
delegated: name, task, status, steps, tokens, how long it took, and why a run that
did not finish failed.

One detail in that drawer is deliberate: **`steps: -1` is shown as "步数不可得", never
as "0 步".** Eino's ReAct loop returns only the final message, so a subagent on the
CLI or the bot cannot report a step count, and a failed one never got one at all. A
run that worked for a minute shown as "0 steps" reads as "it did nothing", in the one
place a reader looks to judge whether the work happened.

What it inherits and what it never gets:

| | |
|---|---|
| Inherited by default | the parent's **read-only _and_ parallel-safe** tools |
| On request (`tools:`) | writes and commands, which still pass through the parent's approval gate, checkpoints and sandbox — a subagent is not a way around them |
| Never | `ask_user` (no channel to a person: a card would only hang), `plan_*` (the plan belongs to the top-level turn), `spawn_agent` (depth is capped at one) |

The report is bounded (`subagent.max_report_chars`, default 8000) and truncation is
stated, and the footer says how many steps and tools produced it, so the model can
judge how much to trust it. A subagent that fails is a **report that says so**: the
parent turns keeps running and decides what to do.

Cost is the parent's: the nested run's tokens and wall-clock count against the
parent turn's budget and share its deadline, so `chat.turn_max_tokens` and
`chat.turn_deadline_seconds` stay true. `subagent.max_concurrent` (default 2) is a
**process-wide** gate, because "spawn three explorations" is the intended use and
four parents each spawning four is how a fan-out becomes a bill.

**What a subagent did is visible.** Its own steps — the reads, the greps, the
thinking — arrive on the parent turn's stream tagged with the id of the call that
spawned it, and the console lists them on that call's card, one line per action.
`huan-agent run --output json` reports the same thing under the call's `nested`
field. This is not cosmetic: a card that spawns something and then sits silent for
thirty seconds reads as a hang, and the alternative — appending the subagent's text
to the answer — would show the reader text the model never produced and store a
turn that is not what the model said.

Two differences between the surfaces are worth knowing:

- **The CLI and the bot run subagents on Eino's ReAct loop, which returns only the
  final message.** So a report from those surfaces says the step and tool counts are
  *unavailable* rather than reporting zero — a footer claiming "0 steps, no tools"
  for a subagent that worked for a minute is a lie the parent model will act on.
  The console and `huan-agent run` report the real counts, because the chat loop
  keeps them.
- **Nested steps are summaries, not transcripts.** Each line is one action with its
  outcome, trimmed to a line; the subagent's full conversation is in its own trace,
  which is where a reader goes to audit it. A parent's stored turn carries the same
  summary, so a reloaded conversation shows what a live one showed.

## Parallel tool calls

When one model reply asks for several tools, the ones that declared themselves
**parallel-safe** run at the same time, and everything else is a barrier: a write
or a command waits for everything before it and blocks everything after it.

That ordering is not a performance compromise, it is what the model's program
order means. "Write the config, then run the tests" is two calls with a
dependency the model expressed by ordering them; overlapping them tests the old
config. So the schedule is:

```
[grep, read_file, write_file, read_file, grep]
 └── parallel ──┘   └─ alone ─┘   └─ parallel ─┘
```

Two properties are worth knowing as a caller:

- **The results are always in the order the model wrote the calls**, whatever
  order they finished in. The messages sent back are matched to the calls by
  position, and a plan step lists its calls in the order that explains it.
- **A failure is an observation, not a cancellation.** One call failing does not
  cancel its siblings — the model asked for several things and should see all of
  the answers. A tool that panics is reported as a failed call rather than taking
  the turn down with it.

`tools.max_parallel` (default 4; `0` or `1` means one at a time) bounds how many
may overlap. Which tools may overlap at all is a property of the tool, not of the
setting:

| May overlap | Runs alone |
|---|---|
| `read_file`, `list_dir`, `glob`, `grep`, `time`, `calc`, `echo`, `bash_output`, and the four code-intelligence tools | `write_file`, `edit_file`, `bash`, `bash_background`, `bash_jobs`, `bash_stop`, `ask_user`, `plan_*`, `save_document`, the media tools |

The table is not "reads versus writes", and the difference matters: `ask_user`,
`plan_update` and `save_document` are all read-capability tools that must not
overlap anything — the first parks the whole turn waiting for a person, the second
is a read-modify-write on one shared plan, and the third writes to a store. A new
tool has to declare which it is, and a test fails if it does not
(`tool.UndeclaredTools`).

The CLI and the Feishu bot use Eino's ReAct loop, which runs tools sequentially on
purpose — see the comment in `internal/agent/agent.go` for why, and for the four
properties a loop has to reproduce before it may overlap them.

## Code intelligence

Four read-only tools come from a **language server** (`gopls` by default) rather
than from a text search, because some questions only a compiler can answer:

- **"Did my change break anything?"** — `diagnostics`, and more importantly the
  diagnostics that are attached to what `write_file` / `edit_file` return. That
  attachment is the point of the feature: it turns "change → run tests → notice a
  type error → change again" into "change → know". It is not a tool the model has
  to remember to call, because a check that has to be remembered is not called at
  the moment it matters most — right after it decided the change was correct.
- **"Who calls this?"** — `find_references`. `grep` finds the strings; this finds
  the call sites. A missed call site does not fail loudly, it fails as half a
  repository that no longer compiles.
- **"Where is this defined?"** — `goto_definition`.
- **"Which code handles authentication?"** — `workspace_symbols`. Search a likely
  name and you get real definitions with their file and line.

### Installing a language server

```bash
go install golang.org/x/tools/gopls@latest   # Go
```

**If it is not installed, the four tools are not registered at all**, and the log
says which command to install. That is deliberate: a tool whose every call fails
with "not installed" is worse than a tool that is not on the menu, because the
model will call it and spend a turn on the failure. Nothing else about the agent
changes — no server, no code intelligence, everything else as before.

Other languages work through `tools.lsp.servers` (see
`configs/config.example.yaml`): a command, the languages it handles, and the
files that mark its project root. Nothing about the tool set is Go-specific; only
the built-in default is.

### What it costs

A language server indexes the whole module when it starts, which is seconds, so it
is started **once per workspace root**, kept, and reaped after
`tools.lsp.idle_timeout_seconds` (default 10 minutes) of disuse. Every call after
the first is fast; the first call of a session on a cold cache is not.

Three settings worth knowing:

| Key | Default | What it does |
|---|---|---|
| `tools.lsp.enable` | `true` | Off means no process, no tools, and editing tools that return exactly what they returned before this existed. |
| `tools.lsp.attach_diagnostics` | `true` | Whether the diagnostics for a written file are appended to the tool result. |
| `tools.lsp.diagnostics_wait_ms` | `2000` | How long an edit waits for the server to catch up. On timeout the result says the diagnostics were not ready — an edit never waits longer than this and never fails because of it. |

### Honest limits

- **Positions are 1-based**, line as printed by `read_file`, column counted in
  characters (not bytes, not UTF-16 units — though for ASCII, which is nearly all
  source code, all three agree).
- **"No diagnostics" is only reported when the server said so.** If the server has
  not caught up, the answer says *that* instead. The difference matters: "clean"
  and "not checked" are not the same claim, and reporting the second as the first
  is the most dangerous wrong answer this feature could give.
- **Results outside the workspace are dropped.** A symbol search can return
  results from a dependency, and a path the agent cannot open is not an answer.
- **A stale position is dropped, not clamped.** If the server reports a location
  past the end of the file, it is skipped rather than rounded to a plausible line.
  For a diagnostic the message is kept with a best-effort position, because losing
  a reported problem is worse than an approximate column.

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

## Artifacts

Not everything the agent makes is a file in the project. A page it generated for
you to look at, a report it compiled, a chart it drew — those are **deliverables**,
and their defining feature is that you want them after the workspace they were
made in is gone. `save_artifact` is the tool for them:

```
save_artifact  kind="html"  title="巡检报告"  content="<!doctype html>…"  path="report.html"
               → stored, url=/api/artifacts/files/<session>/1770000000-report.html
save_artifact  kind="image" path="./charts/q3.png"      → stored from a file, not from content
```

What it does:

- Writes the bytes into the **artifact store on the server** — `tools.artifacts.root`,
  defaulting to an `artifacts/` directory beside the database file — and records a
  row that indexes them. Not into the workspace: an artifact that vanished with
  the working copy would not be an artifact.
- Files by **kind and extension from a whitelist** (`html`, `md`, `txt`, `csv`,
  `json`, `xml`, `png`, `jpg`, `gif`, `webp`, `pdf`). Anything else is refused
  rather than stored and worried about later; the tool tells the model which types
  were accepted so it can pick a different one.
- Accepts either `content` (inline text, for HTML/markdown/CSV the model just
  wrote) or `path` (an existing file inside the workspace, read and copied), and
  refuses both at once — the ambiguity has exactly one right answer and it must
  not be guessed.
- Returns the **URL** when the surface serving it has an HTTP server, so the model
  can hand you a link. A CLI (`huan-agent chat`) or one-shot (`huan-agent run`)
  process serves no HTTP: there it reports the path it wrote and says a URL is not
  available, rather than handing out an address nothing answers.
- Caps one artifact at `tools.artifacts.max_bytes` (default 32 MiB). A file over
  the cap is refused before it is written, not truncated after.

Where it shows up, and the security rules behind it, are in
`docs/admin.md` → Artifacts: a conversation's header lists its own, 统计监控 →
产物中心 lists every session's, and the file route refuses to leave the store root.

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
- **Ask for approval — optionally.** With `tools.approval.mode` at its default
  (`off`), a write or a command runs as soon as the model calls it: there is no
  confirmation step. Setting the mode to `writes`, `writes+exec` or `all` puts the
  gated capabilities behind a decision by a person, on the surfaces that can ask
  one. On a surface that cannot (Feishu, `huan-agent run`) the gated tools are not
  registered at all, so nothing is offered that would always be refused. See
  `docs/approvals-and-checkpoints.md` for what a card shows and why a timeout is a
  refusal rather than an allow.
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
- **The code-intelligence tools are missing** — the language server is not
  installed, or `tools.lsp.enable` is false. The log has one line saying which:
  either `code intelligence disabled: the configured language server is not
  installed` with the install command, or `code intelligence disabled: no language
  server is configured`. This is the designed behaviour, not a failure.
- **An edit result says the diagnostics were "not ready"** — the server did not
  catch up within `tools.lsp.diagnostics_wait_ms`. The edit itself succeeded. On
  the first edit of a session this is normal (the server is still indexing);
  raising the wait trades a slower edit for a more complete answer.
- **`find_references` finds nothing for a symbol used elsewhere** — a language
  server indexes the project its root names. If the symbol's other users live
  outside that project, it cannot see them, and the tool says so. Check for a
  root marker (`go.mod`, `package.json`) above the file.
- **A definition points at the wrong line** — positions are 1-based and columns
  count characters, so a column taken from a byte offset is wrong on a line with
  non-ASCII text. Re-read the line with `read_file` and count characters.
- **A skill's tool list is silently too narrow** — a skill's `tools` frontmatter is
  an **allow-list, not a hint**: once it is non-empty, every tool not named in it is
  withheld for that session. This fails quietly in a specific way — the only thing
  the loader checks is that each name is *registered* (`tool.Registry.SetAllowList`
  rejects an unknown name), so a list that is valid but useless, like
  `tools: [calc, echo]` on a code-review skill, loads without complaint and then
  leaves the model unable to read a single file. Run `huan-agent chat --tools
  --skill <name>` and read the `tools available:` line to see the surviving set.
- **An MCP server's tool never appears** — a tool whose name is already taken is
  skipped, not registered, and the reason is logged (`mcp tool not registered` with
  the server and tool name). Skipping is deliberate: a server exposing one
  colliding name is still worth connecting. Note that builtins win — the
  OpenViking server exposes its own `grep` and `glob`, and both are skipped in
  favour of the local tools, which are the ones that respect the workspace.
- **"command is required for a stdio server" for a server you configured as
  http/sse** — the entry's transport was lost on the way to the dialer. This is a
  bug in the conversion, not in your config: every surface must build the runtime
  spec through the same helper (`cmd/huan-agent/mcp.go`), because a hand-written
  field list is a field list that can drop one. Check the reported transport in the
  `connect mcp <name> (<transport>)` error: if it says `stdio` for an entry that
  says `http`, the field was dropped rather than misconfigured.
