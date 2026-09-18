// Tiny reactive store — the app has no router and no state library on purpose.
//
// It owns five things:
//   1. navigation: which top-level surface is showing ('chat' | 'settings' |
//      'monitor') and which sub-tab of 统计监控 or 设置 is open;
//   2. the time-range selector, from which every usage query derives its
//      `since` (and the trend chart its `days`);
//   3. the manual-refresh counter: bumping it re-runs the mounted view's
//      queries without the view knowing about the toolbar;
//   4. the cached GET /api/meta payload shown by 设置 → 服务与工具;
//   5. the session: whether this deployment asks for a password at all
//      (`auth.required`) and whether we hold one (`auth.signedIn`).
//
// The login-free default (admin.require_login: false) is the common case and
// none of §5 gates it: the probe answers "no password needed" and the console
// renders immediately, as before.

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

/**
 * The seven sub-tabs of 设置, all inside one page — the same shape as 统计监控.
 *
 * They are grouped by what they configure rather than by which API they call:
 * 外观 is this browser, 模型 is the LLM catalog, MCP and 技能 are the agent's
 * capabilities, OpenViking is the context database the agent remembers through,
 * and 服务与工具 is everything the operator can only read (the server's own facts
 * and the tool set they produce).
 *
 * 后台进程 is deliberately not a sub-tab: those processes belong to the
 * conversation that started them, so the count rides on that conversation's
 * header and the list opens beside it — see components/JobsDrawer.vue. A
 * setting is something the operator turns; what the agent has running is state,
 * and state belongs where it happened.
 */
export const SETTINGS_TABS = [
  { key: 'appearance', label: '外观' },
  { key: 'models', label: '模型' },
  { key: 'budget', label: '对话预算' },
  { key: 'mcp', label: 'MCP' },
  { key: 'openviking', label: 'OpenViking' },
  { key: 'skills', label: '技能' },
  { key: 'service', label: '服务与工具' },
]

export const state = reactive({
  /** 'chat' | 'settings' | 'monitor' */
  tab: 'chat',
  /** Active sub-tab of 统计监控. */
  monitor: 'dashboard',
  /** Active sub-tab of 设置. */
  settings: 'appearance',
  range: '7d',
  refreshToken: 0,
  meta: null,
  metaError: '',
  /**
   * Session state, all of it from GET /api/me and from 401s.
   *
   *   probed     the probe has answered — before that neither screen is right;
   *   required   the server would ask *this browser* for a password: it is
   *              admin.require_login, minus the loopback exemption
   *              admin.trust_loopback gives a browser on the server's own
   *              machine. So a remote console can need one while the desktop
   *              next to the server never sees a form;
   *   signedIn   we hold a session cookie the server accepts;
   *   username   the configured admin name, shown next to 退出登录;
   *   expired    a 401 arrived while the console was open, i.e. the session
   *              died under it (sessions live in server memory, so a restart
   *              does it) — the login form says so instead of looking like a
   *              first visit.
   */
  auth: {
    probed: false,
    required: false,
    signedIn: false,
    username: '',
    expired: false,
  },
  /**
   * A 401 arrived on a deployment that reported no login requirement. Nothing
   * in this console can produce it — the server lets everything through when
   * require_login is off — so it means something else is asking for
   * credentials (a reverse proxy) or the server was restarted with different
   * config. There is no form to offer, so the shell says so where it can be
   * read.
   */
  denied: false,
  /**
   * A trace a view asked to open, as `{ id, session }`; consumed once by
   * 链路追踪. There is no router here — navigation is `tab` plus `monitor` — so a
   * one-shot hand-off is how 对话 passes a target to a panel in 统计监控.
   *
   * It is deliberately cleared on consumption rather than kept: a sticky
   * selection would force that same trace on every later visit to the tab.
   */
  focusTrace: null,
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

/** Switch the 设置 sub-tab. */
export function setSettings(key) {
  state.settings = key
  closeDrawer()
}

/**
 * Open 链路追踪 on one trace, or on everything one conversation recorded.
 *
 * `id` selects a single trace — what an answer's 链路 button passes. `session`
 * prefills the panel's own session filter, which is what the conversation header
 * passes when the question is "what did this whole conversation do?" rather than
 * "what did this one answer do?". Either may be omitted.
 */
export function openTrace({ id = '', session = '' } = {}) {
  state.focusTrace = { id: String(id || ''), session: String(session || '') }
  state.monitor = 'traces'
  state.tab = 'monitor'
  closeDrawer()
}

/**
 * Take the pending trace focus, clearing it. Returns null when nothing asked for
 * one, so the panel behaves normally on an ordinary visit to the tab.
 */
export function claimTraceFocus() {
  const pending = state.focusTrace
  state.focusTrace = null
  return pending
}

export function setRange(range) {
  if (state.range === range) return
  state.range = range
  state.refreshToken += 1
}

/**
 * Called by api.js on every 401 that is not the login attempt itself.
 *
 * Where login is on, this is the session dying under an open console — the
 * server keeps sessions in memory, so a restart or a TTL lapse does it — and
 * the console goes back to its login form, saying the session expired rather
 * than looking like a first visit. Where login is off it cannot normally happen
 * at all, so it is reported as the mismatch it is (see `state.denied`).
 */
function onDenied() {
  if (!state.auth.required) {
    state.denied = true
    return
  }
  state.auth.signedIn = false
  state.auth.expired = true
  // Any drawer open over the (now replaced) shell would swallow the form.
  closeDrawer()
}

setUnauthorizedHandler(onDenied)

/**
 * True when the console must ask for a password before it can show anything.
 * Read by the shell to choose its screen, and by `bootstrap` to avoid firing
 * requests that would each answer 401.
 */
export const needsLogin = computed(
  () => state.auth.probed && state.auth.required && !state.auth.signedIn,
)

/**
 * Boot the shell. `GET /api/me` is the app's bootstrap probe: it says whether
 * this deployment wants a password (login_required) and whether we hold a
 * session, so the console knows which screen to draw before it draws anything.
 *
 * A transport failure is not fatal: nothing is known about the deployment, so
 * the shell renders and each view reports its own loading error. `required` is
 * deliberately left as it was in that case rather than guessed.
 */
export async function bootstrap() {
  try {
    const me = await api.me()
    state.auth.required = me && me.login_required === true
    state.auth.signedIn = Boolean(me && me.authenticated)
    state.auth.username = me && typeof me.username === 'string' ? me.username : ''
    state.auth.expired = false
    state.denied = false
  } catch (err) {
    // Unreachable server: leave the auth state alone and let the panels show
    // the failure, which is what they are for.
  }
  state.auth.probed = true
  // Nothing to read without a session, and asking anyway would answer 401 and
  // mark a first visit as an expired one.
  if (needsLogin.value) return
  await loadMeta()
}

/**
 * Exchange the password for a session cookie.
 *
 * Resolves `{ok}` instead of throwing: the form shows the server's own words,
 * and "密码错误" and the throttle's "too many failed attempts" are different
 * situations that only the server can tell apart.
 */
export async function signIn(password) {
  let reply
  try {
    reply = await api.login(password)
  } catch (err) {
    return { ok: false, error: err && err.message ? err.message : '登录失败' }
  }
  state.auth.signedIn = true
  // A login that just succeeded is proof the deployment enforces one, whatever
  // the probe managed to report earlier.
  state.auth.required = true
  state.auth.expired = false
  state.denied = false
  state.auth.username = reply && typeof reply.username === 'string' ? reply.username : ''
  return { ok: true }
}

/**
 * End the session and show the login form again.
 *
 * The page reloads rather than resetting the stores one at a time: the console
 * is holding a conversation list, open SSE streams and cached usage rows, all
 * read with the session that just ended, and a reload is the only way to be
 * sure none of it outlives the session. It also re-runs the boot probe, which
 * is what chooses between the shell and the form.
 *
 * Returns false when the server could not be told — the cookie may still be
 * live, so the caller must not pretend the operator is logged out.
 */
export async function signOut() {
  try {
    await api.logout()
  } catch (err) {
    return false
  }
  window.location.reload()
  return true
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
