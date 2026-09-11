// Tiny reactive store — the app has no router and no state library on purpose.
//
// It owns three things:
//   1. authentication state (whether the login screen is shown);
//   2. the global time-range selector, which every usage query derives `since`
//      from;
//   3. the manual-refresh counter: bumping it re-runs the active view's queries
//      without each view having to know about the toolbar.

import { computed, reactive } from 'vue'
import { api, setUnauthorizedHandler } from './api.js'
import { resetChat } from './chatStore.js'

/** Time ranges offered in the header. `hours: null` means "全部". */
export const RANGES = [
  { key: '24h', label: '24小时', hours: 24 },
  { key: '7d', label: '7天', hours: 24 * 7 },
  { key: '30d', label: '30天', hours: 24 * 30 },
  { key: 'all', label: '全部', hours: null },
]

export const TABS = [
  { key: 'chat', label: '对话' },
  { key: 'dashboard', label: '总览' },
  { key: 'model', label: '按模型' },
  { key: 'user', label: '按用户' },
  { key: 'recent', label: '调用记录' },
  { key: 'audit', label: '审计日志' },
  { key: 'skills', label: '技能' },
  { key: 'traces', label: '链路追踪' },
]

export const state = reactive({
  /** 'checking' | 'login' | 'ready' */
  phase: 'checking',
  username: '',
  tab: 'dashboard',
  range: '7d',
  refreshToken: 0,
  meta: null,
  metaError: '',
  /** Set by the login form or by an expired session, shown on the login card. */
  authNotice: '',
  /** True once a session was established — a first visit is not an "expiry". */
  hadSession: false,
})

/**
 * The RFC3339 lower bound for the current range, or undefined for "全部".
 * Computed fresh on every read so a long-lived tab keeps a sliding window.
 */
export function sinceFor(rangeKey) {
  const range = RANGES.find((r) => r.key === rangeKey)
  if (!range || range.hours === null) return undefined
  return new Date(Date.now() - range.hours * 3600 * 1000).toISOString()
}

/** The `days` window for the daily trend chart, derived from the range. */
export const trendDays = computed(() => {
  switch (state.range) {
    case '24h':
      return 1
    case '30d':
      return 30
    case 'all':
      return 90
    default:
      return 7
  }
})

/** Filters object handed to every usage endpoint. */
export function currentFilters() {
  return { since: sinceFor(state.range) }
}

export function requestRefresh() {
  state.refreshToken += 1
  if (state.metaError) loadMeta()
}

export function setTab(tab) {
  state.tab = tab
}

export function setRange(range) {
  if (state.range === range) return
  state.range = range
  state.refreshToken += 1
}

/** Called by api.js on any 401 — drops us back to the login screen. */
export function onUnauthorized() {
  if (state.phase === 'login') return
  // Only an *expired* session deserves the notice; a first visit is not one.
  state.authNotice = state.hadSession ? '登录状态已失效，请重新登录' : ''
  state.phase = 'login'
  state.username = ''
  state.hadSession = false
}

setUnauthorizedHandler(onUnauthorized)

/** Runs on boot: is an admin session already active? */
export async function checkSession() {
  try {
    const res = await api.me()
    if (res && res.authenticated) {
      state.username = res.username || 'admin'
      state.hadSession = true
      state.phase = 'ready'
      loadMeta()
      return
    }
    state.phase = 'login'
  } catch (err) {
    // A 401 already flipped us to 'login' (and set the notice) in onUnauthorized.
    state.phase = 'login'
  }
}

export async function loadMeta() {
  try {
    state.meta = await api.meta()
    state.metaError = ''
  } catch (err) {
    if (err && err.status === 401) return
    state.metaError = err && err.message ? err.message : '无法读取服务信息'
  }
}

export async function login(password) {
  const res = await api.login(password)
  state.username = (res && res.username) || 'admin'
  state.authNotice = ''
  state.hadSession = true
  state.phase = 'ready'
  state.refreshToken += 1
  loadMeta()
}

export async function logout() {
  try {
    await api.logout()
  } catch (err) {
    // Logging out locally is best-effort: even if the request failed (offline,
    // expired cookie) we must not keep the user inside the admin UI.
  }
  // Conversations belong to the previous session: drop them (and any stream
  // still running) so the next login starts clean.
  resetChat()
  state.phase = 'login'
  state.username = ''
  state.authNotice = ''
  state.hadSession = false
  state.meta = null
  state.tab = 'dashboard'
}
