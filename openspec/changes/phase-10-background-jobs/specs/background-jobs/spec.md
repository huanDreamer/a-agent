# Capability: background-jobs

## Purpose

Let the agent start, watch and stop **常驻** processes — dev servers, `--watch` compilers,
resident APIs and databases — which by definition outlive the tool call that started them,
without weakening the existing guarantee that a `bash` call leaves nothing behind.

## Scope

- `internal/jobs` — the process manager: start, supervise, read, stop, reap.
- `internal/tool/builtin` — the four model-facing tools (`bash_background`, `bash_jobs`,
  `bash_output`, `bash_stop`).
- `internal/server` — `/api/jobs`: the console's read/stop surface.
- `internal/config`, `cmd/huan-agent` — the knobs, and the per-process lifetime.
- `web/src` — 设置 → 后台进程.

## Requirements (MUST)

1. **Explicit intent.** Starting a resident process MUST be a different tool from `bash`.
   `bash` MUST keep killing its whole process group when the call ends, and MUST NOT grow a
   "sometimes leaves a process behind" mode.
2. **Detached start.** A background job MUST run in its own session (`setsid`) and its own
   process group, so that neither signals directed at the agent nor a group kill aimed at one
   job can reach another job or the agent itself.
3. **Bounded capture.** A job's stdout and stderr MUST be captured into a bounded in-memory
   window **and** a log file with a configured byte cap. Neither MAY grow without bound as the
   process runs. Reaching the file cap MUST be recorded in the file rather than silently
   dropping output.
4. **Offsets, not guesses.** `bash_output` MUST read by absolute byte offset and return the
   offset to continue from, and MUST report how many bytes it could not serve because the
   window had already discarded them. A reader MUST NOT be able to mistake a gap for a
   continuous stream. An optional wait MUST return as soon as new output arrives, the job
   ends, or the wait expires — never later.
5. **Alive means the group is alive.** A job MUST be reported `running` while its process
   group still has a member, even when the shell that was started has already exited (a
   server that forks). When the job ends, the status MUST distinguish "ended on its own"
   (`exited`) from "we stopped it" (`stopped`), and MUST carry the exit code when there is one.
6. **Stop is a group operation.** `bash_stop` MUST signal the whole process group, MUST send
   SIGTERM first and escalate to SIGKILL after the configured grace, and MUST be safe to call
   on a job that is already gone (reported as a conflict, not a panic or a silent success).
7. **Nothing outlives the agent.** When the agent process exits, every job it started MUST be
   terminated (SIGTERM, then SIGKILL after the grace). A job MAY NOT be left running with no
   one tracking it — cross-restart survival is a job for launchd/systemd/a container.
8. **Same guards as `bash`.** The tools MUST apply the same pre-execution checks as `bash`:
   a read-only workspace refuses, `deny_patterns` refuses before anything runs, `cwd` is
   resolved inside the workspace, and with `tools.enable_bash: false` (or
   `tools.enable_background: false`) the tools MUST NOT be registered at all.
9. **Bounded concurrency.** Starting a job beyond the configured maximum MUST be refused with
   a message naming the limit and how many are running, before anything is spawned.
10. **Honest descriptions.** The tool descriptions MUST state what actually happens: no PTY
    and no stdin (an interactive command fails rather than hangs, and the description MUST
    name the non-interactive flags to start it with instead), a single interleaved
    stdout+stderr stream, no resource limits, no confinement beyond the working directory,
    and a lifetime bounded by the agent process.
11. **Failure is reported at start.** A command that dies during the startup grace (a busy
    port, a missing binary) MUST be reported in the start result, with its exit code and the
    captured output, rather than stored as a job the model has to poll to understand.
12. **Audit.** Every one of the four tools MUST be recorded like any other tool invocation,
    with its measured duration; starting a job is an `exec`-capability action.
13. **Console visibility.** The server MUST expose the jobs it owns (list, output window,
    stop, forget) so the operator can see and stop what the agent started, and MUST report
    `enabled: false` rather than failing when the feature is not wired.
14. **Forget is explicit and safe.** Deleting a job's record and log MUST be refused while the
    job is still running, and MUST NOT be implied by any other action.
15. **No panics.** A nil manager, an unknown id, a malformed argument or a closed manager MUST
    return an error, never panic.

## Non-goals

- Surviving an agent restart; a persistent supervisor (launchd/systemd/container) covers that.
- PTY, interactive input, or terminal emulation.
- Scheduling, dependency graphs, restarts-on-crash policy, or log rotation beyond the cap.
- CPU/memory/disk quotas, cgroups, namespaces or any sandboxing stronger than what `bash`
  already has (which is: the working directory, and nothing else).
- Sharing the job list between the Feishu bot process and the admin process.

## Key interfaces

- `jobs.New`, `jobs.Manager.Start/List/Get/Output/Stop/Forget/Close`, `jobs.Options`,
  `jobs.Spec`, `jobs.Job`, `jobs.Status`, `jobs.ErrClosed/ErrUnknownJob/ErrStillRunning/ErrNotRunning`
- `builtin.NewBackgroundTools`, `builtin.BackgroundPolicy`
- `config.ToolsConfig.EnableBackground`, `.JobsDirOrDefault`, `.JobsLimits`
- `internal/server` route group `/api/jobs`
