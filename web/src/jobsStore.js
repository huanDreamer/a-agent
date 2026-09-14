// Background jobs, shared by everything that shows them.
//
// Two places read this list at the same time — the chip in a conversation's
// header and the drawer it opens — and a job is also started by the agent
// mid-turn, without either of them asking. So the list lives here rather than in
// a component: one poller, one answer to "what is running", and no chance of the
// chip and the drawer disagreeing while a stop is in flight.
//
// Two decisions carry over from the panel this replaced:
//
//   1. polling only while something is running. A list of finished jobs cannot
//      change on its own, so an always-on interval would be pure noise. A job
//      that appears while nothing is running is noticed because the turn that
//      started it says so — see noteJobsToolRan, which the chat stream calls.
//   2. the list is the whole process, and the scope is what makes it a
//      conversation's. The server answers with the scope key the current session
//      is recorded under (it is the one place that knows how a session id maps
//      to one), and the two derived lists below are what a reader renders.

import { computed, ref, watch } from 'vue'
import { api } from './api.js'
import { state } from './state.js'

/**
 * How often the list is re-read while at least one job is running. Three seconds
 * matches the log follower's pace: a dev server that died is noticed, and an open
 * tab does not hammer the server.
 */
const POLL_MS = 3000

/** Wait this long after the last request before reloading, so bursts collapse. */
const DEBOUNCE_MS = 80

const jobs = ref([])
const enabled = ref(true)
const dir = ref('')
const maxJobs = ref(0)
const running = ref(0)
const total = ref(0)
/** The scope key of the session the last load was made for, from the server. */
const scope = ref('')
const error = ref('')
const loading = ref(false)
const loaded = ref(false)

/** The session the drawer and the chip are showing, so a refresh keeps asking. */
let sessionId = ''
let pollTimer = null
let debounceTimer = null
let inFlight = false
/** Guards against a slow response for an old session landing after a switch. */
let seq = 0

export const jobsState = {
  jobs,
  enabled,
  dir,
  maxJobs,
  running,
  total,
  scope,
  error,
  loading,
  loaded,
}

/** The conversation's own jobs: the ones its header counts. */
export const sessionJobs = computed(() =>
  jobs.value.filter((job) => Boolean(job.scope) && job.scope === scope.value),
)

/**
 * Jobs started by another conversation of the same workspace. They are listed
 * below the session's own rather than hidden: the console is the only place that
 * can see them, and a dev server nobody can stop is exactly the leak these tools
 * exist to prevent.
 */
export const otherJobs = computed(() =>
  jobs.value.filter((job) => !job.scope || job.scope !== scope.value),
)

export const sessionRunning = computed(() => sessionJobs.value.filter(isRunning).length)
export const otherRunning = computed(() => otherJobs.value.filter(isRunning).length)

export function isRunning(job) {
  return Boolean(job) && job.status === 'running'
}

/** Load the list for one conversation. */
async function load({ quiet = false } = {}) {
  const current = ++seq
  if (!quiet) loading.value = true
  try {
    const result = await api.listJobs(sessionId ? { session: sessionId } : {})
    if (current !== seq) return
    jobs.value = (result && result.jobs) || []
    enabled.value = result ? result.enabled !== false : false
    dir.value = (result && result.dir) || ''
    maxJobs.value = (result && result.max_jobs) || 0
    running.value = (result && result.running) || 0
    total.value = (result && result.total) || 0
    scope.value = (result && result.scope) || ''
    error.value = ''
    loaded.value = true
  } catch (err) {
    if (current !== seq) return
    // 401 already flipped the app back to the login screen; nothing to show.
    if (err && err.status === 401) return
    error.value = (err && err.message) || '加载失败'
    loaded.value = true
  } finally {
    if (current === seq) loading.value = false
  }
}

/** Switch the conversation this list is read for, and reload immediately. */
export async function loadForSession(id) {
  const next = id || ''
  if (next === sessionId && loaded.value) return
  sessionId = next
  // The previous session's jobs must not linger under the new session's header.
  jobs.value = []
  scope.value = ''
  loaded.value = false
  await load()
}

/** Reload soon, collapsing bursts (a turn can start several jobs at once). */
export function refreshJobs() {
  if (debounceTimer) window.clearTimeout(debounceTimer)
  debounceTimer = window.setTimeout(() => {
    debounceTimer = null
    load({ quiet: true })
  }, DEBOUNCE_MS)
}

/**
 * Reload now: for the 刷新 button and for an action whose own result the caller
 * is about to display. Debouncing those would show a row that no longer matches
 * the request that just succeeded.
 */
export function reloadJobsNow() {
  if (debounceTimer) {
    window.clearTimeout(debounceTimer)
    debounceTimer = null
  }
  return load()
}

/** Reload the list for a session that is already loaded (the drawer's 刷新). */
export function reloadJobsForSession() {
  return load()
}

/**
 * A tool that manages background processes just ran, so a count on screen is now
 * stale. Called from the chat stream: it is the only signal that a new job
 * exists while nothing was running to poll for.
 */
export function noteJobsToolRan(toolName) {
  if (!JOBS_TOOLS.has(toolName)) return
  refreshJobs()
}

const JOBS_TOOLS = new Set([
  'bash_background',
  'bash_jobs',
  'bash_output',
  'bash_stop',
])

/** The interval exists only under the condition that makes new data possible. */
function syncPolling() {
  const wanted = running.value > 0 || otherRunning.value > 0
  if (wanted && !pollTimer) {
    pollTimer = window.setInterval(() => {
      if (inFlight) return
      inFlight = true
      load({ quiet: true }).finally(() => {
        inFlight = false
      })
    }, POLL_MS)
  } else if (!wanted && pollTimer) {
    window.clearInterval(pollTimer)
    pollTimer = null
  }
}

// Watched rather than driven from each call site: whichever way the list changes
// (a poll, a stop, the header's global 刷新), the timer follows the data.
watch([running, () => otherRunning.value], syncPolling)

// The shell's 刷新 button reloads whatever is on screen; this list is part of it.
watch(
  () => state.refreshToken,
  () => {
    if (loaded.value) refreshJobs()
  },
)

/** Drop the timers when nothing is showing jobs any more. */
export function stopJobPolling() {
  if (pollTimer) window.clearInterval(pollTimer)
  if (debounceTimer) window.clearTimeout(debounceTimer)
  pollTimer = null
  debounceTimer = null
}

export { POLL_MS as JOBS_POLL_MS }
