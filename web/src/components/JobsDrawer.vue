<script setup>
// 后台进程 — a conversation's own shell processes, as a drawer beside it.
//
// These belong to the conversation, not to the settings page: a dev server is
// started by a turn, watched by the turns after it, and dies with the agent
// process, so the place to see it is next to the conversation that owns it. The
// chip in the chat header carries the count; this is what it opens.
//
// Three decisions are worth stating:
//
//   1. the conversation's jobs come first, and other conversations' jobs in the
//      same workspace follow under their own heading. The second group is not an
//      afterthought: this drawer is the only place in the console that shows
//      them, and a process nobody can find is a process nobody can stop. (The
//      tools themselves are workspace-scoped, so both groups are manageable
//      here.)
//   2. stopping and forgetting are gated on the job's own status, because the
//      server refuses both with 409 in the wrong state. The UI never offers a
//      button whose request is guaranteed to fail: 停止 appears while running,
//      删除记录 only once the job has ended.
//   3. a bounded log window is reported honestly. The server keeps only the
//      tail, so `dropped_bytes` (and anything this view itself trims) is shown as
//      a notice above the text instead of being silently swallowed — a truncated
//      log read as complete is worse than no log at all. The same reading applies
//      to `skipped_bytes` (the offset asked for was already gone), `log_capped`
//      (the file hit its own limit) and `output_open` (a process outside the
//      job's group still holds the pipe, so output can keep arriving after the
//      job "ended"). That last one is why following is keyed on `output_open`
//      rather than on the status alone.
//
// The list itself lives in jobsStore: the header chip and this drawer read the
// same poller, so the count on the chip and the rows here cannot disagree.
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import { api } from '../api.js'
import { formatAbsolute, formatBytes, formatCount, formatRelative } from '../format.js'
import {
  JOBS_POLL_MS,
  isRunning,
  jobsState,
  otherJobs,
  otherRunning,
  reloadJobsNow,
  sessionJobs,
  sessionRunning,
} from '../jobsStore.js'

const emit = defineEmits(['close'])

/**
 * Log window requested per call. Bounded so one noisy watcher cannot stream a
 * megabyte into the DOM per tick; following in windows is what `from` is for.
 */
const LOG_WINDOW_BYTES = 65536

/**
 * Cap on the text held in the browser for one log view. The server already
 * bounds the file; this bounds the render, and when it bites, the trim is
 * reported in the same notice as a server-side drop.
 */
const LOG_RENDER_CAP = 262144

const jobs = computed(() => jobsState.jobs.value)
const enabled = computed(() => jobsState.enabled.value)
const error = computed(() => jobsState.error.value)
const loading = computed(() => jobsState.loading.value)
const loaded = computed(() => jobsState.loaded.value)
const maxJobs = computed(() => jobsState.maxJobs.value)
const dir = computed(() => jobsState.dir.value)

/** Running first, then what already ended: the live ones are why this is open. */
function ordered(list) {
  return [...list.filter(isRunning), ...list.filter((job) => !isRunning(job))]
}

const mine = computed(() => ordered(sessionJobs.value))
const others = computed(() => ordered(otherJobs.value))
const totalRunning = computed(() => sessionRunning.value + otherRunning.value)

const STATUS_LABEL = { running: '运行中', exited: '已退出', stopped: '已停止' }
const BY_LABEL = { web: '网页', cli: 'CLI', feishu: '飞书' }

/** The tri-state AsyncBlock expects, derived from the shared store. */
const listStatus = computed(() => {
  if (loading.value && !loaded.value) return 'loading'
  if (error.value && !loaded.value) return 'error'
  return 'ready'
})

async function refresh() {
  await reloadJobsNow()
  await refreshOpenLog({ finalRead: true })
}

function statusLabel(job) {
  return STATUS_LABEL[job.status] || job.status || '未知'
}

/** Badge tone per status, never by "did it end" alone — see the exit rule below. */
function statusClass(job) {
  if (isRunning(job)) return 'ok'
  if (job.status === 'stopped') return 'warn'
  // A finished process is only a failure when it did not finish cleanly: a
  // watcher that ended with 0 on purpose must not read like a crash.
  return job.exit_code === 0 ? '' : 'bad'
}

/** The name a row is known by: the given name, else the id the API keyed it on. */
function label(job) {
  return job.name || job.id || '未命名进程'
}

function errorText(err, fallback) {
  if (err && typeof err.message === 'string' && err.message !== '') return err.message
  return fallback
}

/** Human-readable elapsed time: "2 分 13 秒", "1 小时 04 分", "3 天 2 小时". */
function formatUptime(ms) {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '—'
  const total = Math.floor(ms / 1000)
  if (total < 60) return `${total} 秒`
  const minutes = Math.floor(total / 60)
  if (minutes < 60) return `${minutes} 分 ${String(total % 60).padStart(2, '0')} 秒`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} 小时 ${String(minutes % 60).padStart(2, '0')} 分`
  return `${Math.floor(hours / 24)} 天 ${hours % 24} 小时`
}

/**
 * How long one job has been (or was) alive. A running job is measured against
 * the clock rather than the payload's `uptime_ms`, which was already stale when
 * the response landed — the row would otherwise sit frozen between polls.
 */
function uptime(job) {
  const started = Date.parse(job.started_at)
  if (!Number.isFinite(started)) return '—'
  if (isRunning(job)) return formatUptime(now.value - started)
  const ended = Date.parse(job.ended_at)
  return formatUptime(Number.isFinite(ended) ? ended - started : 0)
}

/** The exit code, as a phrase: only a finished process has one, and it can be absent. */
function exitText(job) {
  if (isRunning(job)) return ''
  const code = job.exit_code
  if (code === null || code === undefined) return '无退出码'
  // A negative code is how a signal death arrives; the sign is the mechanism,
  // not a number worth showing.
  if (typeof code === 'number' && code < 0) return `信号 ${Math.abs(code)}`
  return `退出码 ${code}`
}

function exitTitle(job) {
  const code = job.exit_code
  if (isRunning(job) || code === null || code === undefined) return ''
  if (typeof code === 'number' && code < 0) return '被信号终止'
  return code === 0 ? '正常结束' : '非零退出，通常表示失败'
}

/* ------------------------------------------------------------------ clock -- */

/**
 * A one-second tick that exists only while something is running, so the elapsed
 * time advances between polls and stops driving renders once nothing does.
 */
const now = ref(Date.now())
let clockTimer = null

function syncClock() {
  const wanted = totalRunning.value > 0
  if (wanted && !clockTimer) {
    clockTimer = window.setInterval(() => {
      now.value = Date.now()
    }, 1000)
  } else if (!wanted && clockTimer) {
    window.clearInterval(clockTimer)
    clockTimer = null
  }
}

watch(totalRunning, syncClock)
onMounted(syncClock)

/* ---------------------------------------------------------------- actions -- */

/** `'stop' | 'forget'` per job id — at most one request in flight per row. */
const busy = reactive({})
const rowError = reactive({})
/** The job id whose 删除记录 is one click away from happening. */
const confirmForget = ref('')
const notice = ref('')
let noticeTimer = null

function setNotice(text) {
  notice.value = text
  if (noticeTimer) window.clearTimeout(noticeTimer)
  noticeTimer = window.setTimeout(() => {
    noticeTimer = null
    notice.value = ''
  }, 3200)
}

async function stopJob(job) {
  if (busy[job.id]) return
  // Asking is not a request: a cancelled confirmation must not leave the row
  // disabled, so the flag is only set once the answer is yes.
  if (!window.confirm(`停止「${label(job)}」？会先发送终止信号，日志保留。`)) return
  busy[job.id] = 'stop'
  rowError[job.id] = ''
  try {
    await api.stopJob(job.id, { signal: 'term' })
    setNotice('已发送停止信号，进程结束后状态会变为「已停止」')
    await refresh()
  } catch (err) {
    if (err && err.status === 401) return
    rowError[job.id] = errorText(err, '停止失败，请重试')
  } finally {
    busy[job.id] = ''
  }
}

async function forgetJob(job) {
  if (busy[job.id]) return
  if (!window.confirm(`删除「${label(job)}」的记录？它的日志文件会被一起删除。`)) return
  busy[job.id] = 'forget'
  rowError[job.id] = ''
  try {
    await api.forgetJob(job.id)
    confirmForget.value = ''
    if (log.openId === job.id) resetLog()
    setNotice(`${label(job)} 的记录已删除`)
    await refresh()
  } catch (err) {
    if (err && err.status === 401) return
    rowError[job.id] = errorText(err, '删除失败，请重试')
  } finally {
    busy[job.id] = ''
  }
}

/* --------------------------------------------------------------- log view -- */

/**
 * One log open at a time: which row, the text accumulated so far, and where the
 * buffer sits in the file. `from` is the offset of the oldest byte on screen, so
 * `from > 0` means the view is a window over history rather than the tail, and
 * the footer says so.
 */
const log = reactive({
  openId: '',
  job: null,
  text: '',
  from: 0,
  next: 0,
  total: 0,
  dropped: 0,
  skipped: 0,
  trimmed: 0,
  error: '',
  busy: false,
})

/**
 * Whether the log can still grow. Deliberately not just "is the process
 * running": the server reports `output_open` when a process outside the job's
 * own group still holds the output pipe, so a job that ended can keep producing
 * output that a status-only check would stop showing.
 */
const followsLog = computed(() => isRunning(log.job) || Boolean(log.job && log.job.output_open))
/** True when what is on screen reaches the current end of the file. */
const atTail = computed(() => log.from <= 0 || log.next >= log.total)

function resetLog() {
  log.openId = ''
  log.job = null
  log.text = ''
  log.from = 0
  log.next = 0
  log.total = 0
  log.dropped = 0
  log.skipped = 0
  log.trimmed = 0
  log.error = ''
  log.busy = false
}

/** Omitted `from` asks for the tail; `from` asks for the window starting there. */
function loadLogWindow(jobId, from) {
  return api.jobOutput(jobId, from ? { from } : { maxBytes: LOG_WINDOW_BYTES })
}

/** Fold one response into the open log, keeping a truthful account of what was lost. */
function absorbWindow(result) {
  const chunk = typeof result.output === 'string' ? result.output : ''
  log.text += chunk
  if (log.text.length > LOG_RENDER_CAP) {
    const cut = log.text.length - LOG_RENDER_CAP
    log.text = log.text.slice(cut)
    // Counted in characters because characters are what this view dropped.
    log.trimmed += cut
  }
  if (typeof result.next === 'number') log.next = result.next
  if (typeof result.total_bytes === 'number') log.total = result.total_bytes
  // `dropped_bytes` counts what the server discarded before the window. While
  // following it can only grow, so the largest value seen is the current truth.
  if (typeof result.dropped_bytes === 'number' && result.dropped_bytes > log.dropped) {
    log.dropped = result.dropped_bytes
  }
  // `skipped_bytes` is this read's own loss — the offset asked for was already
  // gone — so it accumulates instead of being a running maximum.
  if (typeof result.skipped_bytes === 'number' && result.skipped_bytes > 0) {
    log.skipped += result.skipped_bytes
  }
  if (result.job) log.job = result.job
}

async function openLog(job) {
  if (log.openId === job.id) {
    resetLog()
    return
  }
  resetLog()
  log.openId = job.id
  log.job = job
  log.busy = true
  try {
    absorbWindow(await loadLogWindow(job.id))
    // A job that ended can still be writing (a child outside its group holds the
    // pipe), and the log view should follow that output rather than present a
    // finished transcript. The read itself reports the flag, so it is asked for
    // once here instead of being second-guessed from the row.
    if (!isRunning(job) && job.output_open) await refreshOpenLog({ finalRead: true })
  } catch (err) {
    if (err && err.status === 401) return
    log.error = errorText(err, '读取日志失败')
  } finally {
    log.busy = false
  }
}

/** 查看更早: move the window back by roughly what is already on screen. */
async function loadEarlier() {
  if (!log.openId || log.busy || log.from <= 0) return
  const id = log.openId
  const shownFrom = log.from
  log.busy = true
  log.error = ''
  try {
    const back = Math.max(1, log.text.length)
    const result = await loadLogWindow(id, Math.max(0, shownFrom - back))
    const chunk = typeof result.output === 'string' ? result.output : ''
    log.text = chunk + log.text
    // `from` now points at the start of the whole buffer, so the next 查看更早
    // continues from there instead of re-reading the same window forever.
    log.from = typeof result.from === 'number' ? result.from : 0
    log.next = shownFrom
    if (typeof result.total_bytes === 'number') log.total = result.total_bytes
    if (typeof result.dropped_bytes === 'number' && result.dropped_bytes > log.dropped) {
      log.dropped = result.dropped_bytes
    }
    // Asking for an offset older than the window is exactly when the server
    // skips, so the same accounting as the follow path applies here.
    if (typeof result.skipped_bytes === 'number' && result.skipped_bytes > 0) {
      log.skipped += result.skipped_bytes
    }
    if (result.job) log.job = result.job
  } catch (err) {
    if (err && err.status === 401) return
    log.error = errorText(err, '读取更早的日志失败')
  } finally {
    log.busy = false
  }
}

/**
 * Catch the open log up with the file.
 *
 * Only while the process is still running — except that the read which follows a
 * process ending is exactly the one worth doing: everything it wrote between the
 * last poll and its death is in neither the list nor the screen yet. That case
 * arrives as `finalRead`, and after it the log really cannot grow again, so the
 * periodic follower stays silent. A response superseded by a reset (log closed,
 * another row opened) is dropped rather than written into the wrong view.
 */
let followSeq = 0

async function refreshOpenLog({ finalRead = false } = {}) {
  const id = log.openId
  if (!id) return
  const current = jobs.value.find((job) => job.id === id)
  if (!current) return
  log.job = current
  // The decision is read from the freshly fetched row, not from `log.job`: the
  // cached copy is exactly the thing that is stale when a process has just ended.
  const mayStillWrite = isRunning(current) || Boolean(current.output_open)
  if (!mayStillWrite && !finalRead) return
  const seq = ++followSeq
  log.busy = true
  try {
    const result = await loadLogWindow(id, log.next)
    if (seq !== followSeq || log.openId !== id) return
    const wasBeforeEnd = log.next < log.total
    absorbWindow(result)
    // Reading from the end of what was on screen catches the window back up to
    // the tail, which is what makes the "实时跟随中" label true again.
    if (log.from > 0 && !wasBeforeEnd) log.from = 0
    log.error = ''
  } catch (err) {
    if (err && err.status === 401) return
    if (seq !== followSeq || log.openId !== id) return
    log.error = errorText(err, '日志更新失败')
  } finally {
    if (seq === followSeq) log.busy = false
  }
}

/**
 * The running→ended transition of the job whose log is open is the one moment
 * that needs a last read: the polling loop stops following as soon as the status
 * flips, so without this the final lines before the exit would never be shown.
 *
 * A second read follows shortly after, because `output_open` is about the pipe
 * rather than the process and the server only knows the answer once the child
 * that held it has gone. That read is what decides whether the view keeps
 * following or declares the output finished.
 */
let wasRunning = {} // reassigned, not mutated: see the watch below
let settleTimer = null

watch(
  () => jobs.value.map((job) => `${job.id}:${isRunning(job) ? 1 : 0}`).join(','),
  () => {
    const next = {}
    for (const job of jobs.value) {
      const before = wasRunning[job.id]
      next[job.id] = isRunning(job)
      if (before === true && !next[job.id] && log.openId === job.id) {
        refreshOpenLog({ finalRead: true })
        scheduleSettleCheck(job.id)
      }
    }
    // Replaced rather than merged, so a forgotten job's id does not linger here
    // and look like "it was running" if the server ever reuses the id.
    wasRunning = next
  },
)

/** One delayed re-read of the open log, to learn whether anything still writes. */
function scheduleSettleCheck(id) {
  if (settleTimer) window.clearTimeout(settleTimer)
  settleTimer = window.setTimeout(() => {
    settleTimer = null
    if (log.openId !== id) return
    refreshOpenLog({ finalRead: true })
  }, 1200)
}

onBeforeUnmount(() => {
  // The drawer can be closed at any moment, so every timer it owns dies with it
  // — a live interval after unmount would keep fetching for a view nobody sees.
  if (clockTimer) window.clearInterval(clockTimer)
  if (noticeTimer) window.clearTimeout(noticeTimer)
  if (settleTimer) window.clearTimeout(settleTimer)
  clockTimer = null
  noticeTimer = null
  settleTimer = null
})

// The session this list belongs to is chosen by the view that hosts the drawer
// (the chat header asks for its own conversation), so there is deliberately no
// load on mount here: asking with no session would clear the scope the chip is
// already showing.
onMounted(() => {
  if (log.openId) refreshOpenLog()
})
</script>

<template>
  <aside class="jobs-drawer" aria-label="后台进程">
    <header class="jobs-drawer-head">
      <div class="jobs-drawer-title">
        <Icon name="terminal" :size="16" />
        后台进程
        <span class="muted-note">
          运行中 {{ formatCount(totalRunning) }} · 合计 {{ formatCount(jobs.length) }}
          <template v-if="maxJobs"> · 上限 {{ formatCount(maxJobs) }}</template>
        </span>
        <span v-if="notice" class="chip ok">{{ notice }}</span>
      </div>
      <div class="row">
        <button type="button" class="btn ghost sm" :disabled="loading" @click="refresh">
          <Icon name="refresh" :size="15" />
          刷新
        </button>
        <button type="button" class="btn ghost sm" title="关闭" @click="emit('close')">
          <Icon name="x" :size="15" />
        </button>
      </div>
    </header>

    <div class="jobs-drawer-body">
      <AsyncBlock :state="listStatus" :error="error" :skeleton-rows="3" @retry="refresh">
        <!-- Not wired in this deployment: a state to explain, not a failure. -->
        <div v-if="!enabled" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">当前部署未启用后台进程管理</div>
          <div class="empty-hint">
            服务端没有可用的作业管理器：后台进程工具被关闭（<code class="md-code">tools.enable_background</code>），
            或日志目录不可用（<code class="md-code">tools.background_dir</code>）。
          </div>
        </div>

        <template v-else>
          <div v-if="!jobs.length" class="empty">
            <div class="empty-ico" aria-hidden="true">◍</div>
            <div class="empty-text">当前没有后台进程</div>
            <div class="empty-hint">
              智能体通过 <code class="md-code">bash_background</code> 启动的开发服务器、监听器或常驻服务会出现在这里，
              可以查看日志并停止它们。
            </div>
          </div>

          <section v-if="mine.length" class="jobs-group">
            <div class="jobs-group-head">
              <span class="dot" />
              本会话
              <span class="muted-note">{{ formatCount(mine.length) }} 个</span>
            </div>
            <div
              v-for="job in mine"
              :key="job.id"
              class="job-card"
              :class="{ selected: log.openId === job.id }"
            >
              <div class="job-card-head">
                <span class="job-name" :title="job.command || ''">{{ label(job) }}</span>
                <span class="tag" :class="statusClass(job)">{{ statusLabel(job) }}</span>
              </div>
              <div class="job-command mono" :title="job.command || ''">{{ job.command || '—' }}</div>
              <div class="job-meta">
                <span v-if="job.pid" class="mono">pid {{ job.pid }}</span>
                <span :title="isRunning(job) ? '已运行' : '运行了'">{{ uptime(job) }}</span>
                <span :title="job.log_path || ''">日志 {{ formatBytes(job.log_bytes) }}</span>
                <span v-if="exitText(job)" :title="exitTitle(job)">{{ exitText(job) }}</span>
                <span v-if="job.ended_at" :title="formatAbsolute(job.ended_at)">
                  {{ formatRelative(job.ended_at) }}结束
                </span>
              </div>
              <div class="job-actions">
                <button type="button" class="btn sm" @click="openLog(job)">
                  <Icon :name="log.openId === job.id ? 'chevron-down' : 'eye'" :size="14" />
                  {{ log.openId === job.id ? '收起日志' : '查看日志' }}
                </button>
                <button
                  v-if="isRunning(job)"
                  type="button"
                  class="btn sm ghost danger-text"
                  :disabled="Boolean(busy[job.id])"
                  @click="stopJob(job)"
                >
                  <Icon :name="busy[job.id] === 'stop' ? 'refresh' : 'square'" :size="14" />
                  {{ busy[job.id] === 'stop' ? '停止中…' : '停止' }}
                </button>
                <template v-else>
                  <template v-if="confirmForget === job.id">
                    <span class="dimmer nowrap">确认删除？</span>
                    <button
                      type="button"
                      class="btn sm danger"
                      :disabled="Boolean(busy[job.id])"
                      @click="forgetJob(job)"
                    >
                      {{ busy[job.id] === 'forget' ? '删除中…' : '删除' }}
                    </button>
                    <button type="button" class="btn sm ghost" @click="confirmForget = ''">取消</button>
                  </template>
                  <button
                    v-else
                    type="button"
                    class="btn sm ghost danger-text"
                    title="只删除这条记录和它的日志文件，不影响已经结束的进程"
                    @click="confirmForget = job.id"
                  >
                    <Icon name="trash" :size="14" />
                    删除记录
                  </button>
                </template>
              </div>
              <div v-if="rowError[job.id]" class="cell-err">{{ rowError[job.id] }}</div>
            </div>
          </section>

          <!-- Other conversations of the same workspace. The tools see and manage
               jobs by workspace, so a process started from another session is
               still reachable — and this drawer is the only place that shows it. -->
          <section v-if="others.length" class="jobs-group">
            <div class="jobs-group-head">
              <span class="dot dim" />
              同一工作区的其他进程
              <span class="muted-note">{{ formatCount(others.length) }} 个</span>
            </div>
            <p class="muted-note jobs-group-note">
              由别的会话启动（或未记录会话）。它们跑在同一个工作区里，仍可在这里查看日志和停止。
            </p>
            <div
              v-for="job in others"
              :key="job.id"
              class="job-card"
              :class="{ selected: log.openId === job.id }"
            >
              <div class="job-card-head">
                <span class="job-name" :title="job.command || ''">{{ label(job) }}</span>
                <span class="tag" :class="statusClass(job)">{{ statusLabel(job) }}</span>
              </div>
              <div class="job-command mono" :title="job.command || ''">{{ job.command || '—' }}</div>
              <div class="job-meta">
                <span v-if="job.pid" class="mono">pid {{ job.pid }}</span>
                <span>{{ uptime(job) }}</span>
                <span>日志 {{ formatBytes(job.log_bytes) }}</span>
                <span v-if="job.workspace">{{ job.workspace }}</span>
                <span v-if="job.requested_by">来自{{ BY_LABEL[job.requested_by] || job.requested_by }}</span>
              </div>
              <div class="job-actions">
                <button type="button" class="btn sm" @click="openLog(job)">
                  <Icon :name="log.openId === job.id ? 'chevron-down' : 'eye'" :size="14" />
                  {{ log.openId === job.id ? '收起日志' : '查看日志' }}
                </button>
                <button
                  v-if="isRunning(job)"
                  type="button"
                  class="btn sm ghost danger-text"
                  :disabled="Boolean(busy[job.id])"
                  @click="stopJob(job)"
                >
                  <Icon :name="busy[job.id] === 'stop' ? 'refresh' : 'square'" :size="14" />
                  {{ busy[job.id] === 'stop' ? '停止中…' : '停止' }}
                </button>
              </div>
              <div v-if="rowError[job.id]" class="cell-err">{{ rowError[job.id] }}</div>
            </div>
          </section>

          <!-- The log of the row that asked for it, below the list rather than
               inside a card: the drawer is narrow, and one fixed-height log that
               scrolls in place keeps every row reachable while it is open. -->
          <section v-if="log.openId" class="jobs-group job-log-panel">
            <div class="jobs-group-head">
              <span class="dot" />
              {{ log.job ? label(log.job) : log.openId }}
              <span class="muted-note">
                共 {{ formatBytes(log.total) }} · 已读到 {{ formatCount(log.next) }} 字节
              </span>
              <span v-if="followsLog" class="chip ok">实时跟随中</span>
              <span v-else class="chip warn">已不再产生新输出</span>
            </div>

            <!-- The ways output can go missing are stated, never hidden: what the
                 server discarded before this window, what a 查看更早 request had to
                 skip, what this view trimmed, and a log file that hit its own cap. -->
            <div
              v-if="log.dropped || log.skipped || log.trimmed || (log.job && log.job.log_capped)"
              class="banner warn"
              role="status"
            >
              <Icon name="circle-alert" :size="16" />
              <span class="banner-text">
                <template v-if="log.dropped">
                  更早的 {{ formatBytes(log.dropped) }} 输出已被服务端丢弃（日志只保留一个有上限的窗口）。
                </template>
                <template v-if="log.skipped">
                  本次向前读取跳过了 {{ formatBytes(log.skipped) }}：要读的位置已经不在窗口里。
                </template>
                <template v-if="log.trimmed">本次读取超过显示上限，画面里最早的部分已被移出。</template>
                <template v-if="log.job && log.job.log_capped">
                  日志文件也已达到上限，更早的输出没有写入文件。
                </template>
                完整内容见日志文件<template v-if="log.job && log.job.log_path">：<code class="md-code">{{ log.job.log_path }}</code></template>。
              </span>
            </div>

            <div v-if="log.error" class="banner error" role="alert">
              <Icon name="circle-alert" :size="16" />
              <span class="banner-text">{{ log.error }}</span>
            </div>

            <div v-if="log.busy && !log.text" class="stack" aria-busy="true" aria-live="polite">
              <span class="skeleton" style="height: 14px; width: 42%" />
              <span class="muted-note">正在读取日志…</span>
            </div>

            <pre v-else-if="log.text" class="jobs-log mono">{{ log.text }}</pre>

            <div v-else-if="!log.error" class="muted-note">该进程还没有输出。</div>

            <div class="job-log-actions">
              <button
                type="button"
                class="btn ghost sm"
                :disabled="log.busy || log.from <= 0"
                title="往前读取一段更早的输出"
                @click="loadEarlier"
              >
                <Icon name="chevron-left" :size="14" />
                查看更早
              </button>
              <button type="button" class="btn ghost sm" :disabled="log.busy" @click="refreshOpenLog">
                <Icon name="refresh" :size="15" />
                {{ log.busy ? '读取中…' : '刷新日志' }}
              </button>
              <span v-if="log.text" class="muted-note">
                <template v-if="atTail">已显示到日志末尾</template>
                <template v-else>
                  片段 {{ formatCount(log.next) }} / {{ formatCount(log.total) }} 字节
                </template>
              </span>
            </div>
          </section>

          <p v-if="jobs.length" class="muted-note card-foot">
            每 {{ JOBS_POLL_MS / 1000 }} 秒刷新一次，且只在有「运行中」的进程时进行；
            已经结束的进程仍会列在这里，直到被删除记录<template v-if="dir"> · 日志目录
              <code class="md-code">{{ dir }}</code></template>。
          </p>
        </template>
      </AsyncBlock>
    </div>
  </aside>
</template>

<style scoped>
/* The one thing this drawer needs that the global sheet has no equivalent for: a
   log that scrolls inside its own fixed height instead of growing the panel. */
.jobs-log {
  margin-top: 8px;
  padding: 8px 10px;
  max-height: 260px;
  overflow: auto;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--card);
  font-size: 11.5px;
  line-height: 1.5;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
