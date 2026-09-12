// Tiny reactive store — the app has no router and no state library on purpose.
//
// It owns four things:
//   1. navigation: which top-level surface is showing ('chat' | 'settings' |
//      'monitor') and which sub-tab of 统计监控 is open;
//   2. the time-range selector, from which every usage query derives its
//      `since` (and the trend chart its `days`);
//   3. the manual-refresh counter: bumping it re-runs the mounted view's
//      queries without the view knowing about the toolbar;
//   4. the cached GET /api/meta payload shown by 设置.
//
// There is no authentication state: the console is login-free
// (admin.require_login defaults to false) and renders immediately on load.

import { computed, reactive } from 'vue'
import { api, setUnauthorizedHandler } from './api.js'
import { closeDrawer } from './ui.js'

/** Time ranges offered by the usage views. `hours: null` means "全部". */
export const RANGES = [
  { key: '24h', label: '24小时', hours: 24, note: '最近 24 小时' },
  { key: '7d', label: '7天', hours: 24 * 7, note: '最近 7 天' },
  { key: '30d', label: '30天', hours: 24 * 30, note: '最近 30 天' },
  { key: 'all', label: '全部', hours: null, note: '全部时间' },
]

/**
 * The two surfaces pinned at the bottom of the sidebar. `icon` names come from
 * components/Icon.vue; 对话 is the default surface and has no nav entry of its
 * own (the session list is the navigation).
 */
export const NAV = [
  { key: 'settings', label: '设置', icon: 'settings' },
  { key: 'monitor', label: '统计监控', icon: 'bar-chart' },
]

/** The six sub-tabs of 统计监控, all inside one page. */
export const MONITOR_TABS = [
  { key: 'dashboard', label: '总览' },
  { key: 'model', label: '按模型' },
  { key: 'user', label: '按用户' },
  { key: 'recent', label: '调用记录' },
  { key: 'audit', label: '审计日志' },
  { key: 'traces', label: '链路追踪' },
]

export const state = reactive({
  /** 'chat' | 'settings' | 'monitor' */
  tab: 'chat',
  /** Active sub-tab of 统计监控. */
  monitor: 'dashboard',
  range: '7d',
  refreshToken: 0,
  meta: null,
  metaError: '',
  /**
   * Set when the server answers 401 — only possible on a deployment that turned
   * 登录校验 back on (admin.require_login: true). This console has no login
   * flow, so it explains the situation instead of redirecting anywhere.
   */
  denied: false,
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

/** Human wording of the active range ("最近 7 天"), for view subtitles. */
export const rangeNote = computed(() => {
  const found = RANGES.find((r) => r.key === state.range)
  return found ? found.note : ''
})

/** Filters object handed to every usage endpoint. */
export function currentFilters() {
  return { since: sinceFor(state.range) }
}

export function requestRefresh() {
  state.refreshToken += 1
  if (state.metaError) loadMeta()
}

/** Switch top-level surface. Navigating means the drawer is no longer needed. */
export function setTab(tab) {
  state.tab = tab
  closeDrawer()
}

/** Switch the 统计监控 sub-tab (also reached from 总览's 查看全部). */
export function setMonitor(key) {
  state.monitor = key
  closeDrawer()
}

export function setRange(range) {
  if (state.range === range) return
  state.range = range
  state.refreshToken += 1
}

/** Called by api.js on any 401: the server wants a session this UI cannot give. */
function onDenied() {
  state.denied = true
}

setUnauthorizedHandler(onDenied)

/**
 * Boot the shell. `GET /api/me` is the app's bootstrap probe: with login
 * disabled it answers 200 ({"authenticated":false} without a cookie) and the
 * console is usable right away — nothing is gated on it. A transport failure
 * is not fatal either: each view reports its own loading error.
 */
export async function bootstrap() {
  try {
    await api.me()
    state.denied = false
  } catch (err) {
    if (err && err.status === 401) state.denied = true
  }
  await loadMeta()
}

/** Provider / model / version / feature flags, rendered read-only by 设置. */
export async function loadMeta() {
  try {
    state.meta = await api.meta()
    state.metaError = ''
  } catch (err) {
    if (err && err.status === 401) return
    state.metaError = err && err.message ? err.message : '无法读取服务信息'
  }
}
