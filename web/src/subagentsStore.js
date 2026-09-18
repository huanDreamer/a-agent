// The subagents a conversation delegated.
//
// Modelled on jobsStore, because the header answers the same question for both: what
// is running now, and what ran. Two differences are worth stating:
//
//   - A subagent is short-lived compared with a background process (seconds to a
//     couple of minutes), so the polling interval is faster while something is
//     running and stops entirely when nothing is.
//   - The list keeps finished runs, so the chip stays meaningful after the work is
//     over — "3 个子 agent，2 个已完成" is what a reader wants at that moment, and a
//     list that emptied itself would only ever answer the is-anything-running half.
import { computed, reactive } from 'vue'
import { api } from './api.js'

/** How often to reload while something is running. */
const POLL_MS = 1500

/** A burst of delegations should produce one reload, not one per run. */
const DEBOUNCE_MS = 250

const state = reactive({
  sessionId: '',
  runs: [],
  running: 0,
  total: 0,
  maxConcurrent: 0,
  runningAll: 0,
  tokens: 0,
  enabled: true,
  error: '',
  loaded: false,
})

let seq = 0
let pollTimer = null
let debounceTimer = null

/** The raw state, for tests that need to drive the store without an API. */
export const subagentsStoreState = state

export const subagents = computed(() => state.runs)
export const subagentsRunning = computed(() => state.running)
export const subagentsTotal = computed(() => state.total)
export const subagentsMaxConcurrent = computed(() => state.maxConcurrent)
export const subagentsRunningAll = computed(() => state.runningAll)
export const subagentsEnabled = computed(() => state.enabled)
export const subagentsError = computed(() => state.error)
export const subagentsLoaded = computed(() => state.loaded)

/** True while at least one subagent of this conversation is working. */
export const subagentsLive = computed(() => state.running > 0)

async function load({ quiet = false } = {}) {
  const current = ++seq
  const id = state.sessionId
  if (!id) {
    state.runs = []
    state.running = 0
    state.total = 0
    return
  }
  try {
    const result = await api.listSubagents(id)
    if (current !== seq) return
    state.runs = (result && result.subagents) || []
    state.running = (result && result.running) || 0
    state.total = (result && result.total) || 0
    state.maxConcurrent = (result && result.max_concurrent) || 0
    state.runningAll = (result && result.running_all) || 0
    state.tokens = (result && result.tokens) || 0
    state.error = ''
    state.loaded = true

    // A 404 means this deployment has no subagents: the chip disappears rather
    // than showing an error for a feature that is simply not configured.
    state.enabled = true
  } catch (err) {
    if (current !== seq) return
    if (err && err.status === 401) return
    if (err && err.status === 404) {
      state.enabled = false
      state.runs = []
      state.running = 0
      state.total = 0
      state.loaded = true
      stopPolling()
      return
    }
    state.error = (err && err.message) || '加载失败'
    state.loaded = true
  }
  schedulePoll()
}

/** Poll only while something is running: an idle conversation must not tick. */
function schedulePoll() {
  if (state.running > 0) {
    if (pollTimer) return
    pollTimer = window.setInterval(() => {
      load({ quiet: true })
    }, POLL_MS)
    return
  }
  stopPolling()
}

function stopPolling() {
  if (pollTimer) {
    window.clearInterval(pollTimer)
    pollTimer = null
  }
}

/** Point the store at a conversation, reloading when it changes. */
export async function loadSubagentsFor(sessionId) {
  const next = sessionId || ''
  if (next === state.sessionId && state.loaded) {
    // Same conversation: a fresh look is still wanted, because the reader may have
    // just come back to a conversation that delegated something meanwhile.
    await load({ quiet: true })
    return
  }
  state.sessionId = next
  // The previous conversation's runs must not linger under the new one's header.
  state.runs = []
  state.running = 0
  state.total = 0
  state.tokens = 0
  state.loaded = false
  stopPolling()
  await load()
}

/** Reload soon, collapsing bursts (one reply can delegate several at once). */
export function refreshSubagents() {
  if (debounceTimer) window.clearTimeout(debounceTimer)
  debounceTimer = window.setTimeout(() => {
    debounceTimer = null
    load({ quiet: true })
  }, DEBOUNCE_MS)
}

/** Stop polling; the conversation is no longer on screen. */
export function stopSubagentPolling() {
  stopPolling()
}
