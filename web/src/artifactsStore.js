// 产物 — the resources this conversation produced, shared by everything that
// shows them.
//
// Modelled on subagentsStore, for the same reason: the header button and the
// drawer it opens must not disagree, and a turn can add an artifact while both
// are on screen. One loader, one answer.
//
// Three things differ from a running process, and each one shapes the code:
//
//   1. an artifact is finished the moment it exists. There is nothing to poll
//      for — no interval, no liveness. The list is reloaded when the session
//      changes, when a save_artifact call appears in the stream, and when the
//      reader asks (the drawer's 刷新). A timer would only re-read a list that
//      cannot have changed.
//   2. the count on the button is a fact about the past. It is kept per session
//      and shown as a plain count; there is no "运行中" half to lead with.
//   3. a deployment with the feature off says so instead of 404ing, so `enabled`
//      is read from the response body and the button disappears rather than
//      reporting an error about something the operator never turned on.
import { computed, reactive, watch } from 'vue'
import { api } from './api.js'
import { state } from './state.js'

/** A burst of saves should produce one reload, not one per artifact. */
const DEBOUNCE_MS = 250

const store = reactive({
  sessionId: '',
  /** This conversation's artifacts, newest first (the server's order). */
  items: [],
  enabled: true,
  /** The reason the feature is off, straight from the server. */
  message: '',
  error: '',
  loading: false,
  loaded: false,
})

let seq = 0
let debounceTimer = null

/** The raw state, for tests that drive the store without an API. */
export const artifactsStoreState = store

export const artifacts = computed(() => store.items)
export const artifactsCount = computed(() => store.items.length)
export const artifactsEnabled = computed(() => store.enabled)
export const artifactsMessage = computed(() => store.message)
export const artifactsError = computed(() => store.error)
export const artifactsLoading = computed(() => store.loading)
export const artifactsLoaded = computed(() => store.loaded)

async function load({ quiet = false } = {}) {
  const current = ++seq
  const id = store.sessionId
  if (!id) {
    store.items = []
    return
  }
  if (!quiet) store.loading = true
  try {
    const result = await api.listArtifacts({ session: id })
    // A slow response for a conversation that is no longer selected must not
    // land under the new one's title.
    if (current !== seq) return
    store.enabled = !result || result.enabled !== false
    store.message = (result && result.message) || ''
    store.items = (result && result.artifacts) || []
    store.error = ''
    store.loaded = true
  } catch (err) {
    if (current !== seq) return
    // 401 already flipped the app back to the login screen; nothing to show.
    if (err && err.status === 401) return
    store.error = (err && err.message) || '加载失败'
    store.loaded = true
  } finally {
    if (current === seq) store.loading = false
  }
}

/** Point the store at a conversation, reloading when it changes. */
export async function loadArtifactsFor(sessionId) {
  const next = sessionId || ''
  if (next === store.sessionId && store.loaded) {
    // Same conversation: a fresh look is still wanted, because a turn may have
    // saved something while the reader was elsewhere.
    await load({ quiet: true })
    return
  }
  store.sessionId = next
  // The previous conversation's artifacts must not linger under the new one's
  // button.
  store.items = []
  store.error = ''
  store.loaded = false
  await load()
}

/** Reload now: for 刷新 and for the drawer's delete, whose result must be shown. */
export function reloadArtifacts() {
  if (debounceTimer) {
    window.clearTimeout(debounceTimer)
    debounceTimer = null
  }
  return load()
}

/** The tools whose having run means the list is now stale. */
const ARTIFACT_TOOLS = new Set(['save_artifact'])

/**
 * A tool that saves artifacts just ran, so what is on screen is now stale.
 *
 * Called from the chat stream. Unlike the jobs chip there is no poller to fall
 * back on: without this signal an artifact the model saved mid-turn would only
 * appear after switching conversations.
 */
export function noteArtifactToolRan(toolName) {
  if (!ARTIFACT_TOOLS.has(toolName)) return
  if (debounceTimer) window.clearTimeout(debounceTimer)
  debounceTimer = window.setTimeout(() => {
    debounceTimer = null
    load({ quiet: true })
  }, DEBOUNCE_MS)
}

/** Delete one artifact, row and bytes, then reload so the list matches. */
export async function removeArtifact(id) {
  await api.deleteArtifact(id)
  await reloadArtifacts()
}

// The shell's 刷新 button reloads whatever is on screen; this list is part of it.
watch(
  () => state.refreshToken,
  () => {
    if (store.loaded) reloadArtifacts()
  },
)
