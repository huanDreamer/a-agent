// Minimal fetch wrapper for the huan-agent admin API.
//
// - always sends cookies (`credentials: 'same-origin'`), because the session
//   cookie set by POST /api/login is the only authentication mechanism;
// - decodes/encodes JSON and raises ApiError (carrying the HTTP status) on
//   every failure, so views never have to inspect Response objects;
// - a 401 means the session expired: the registered handler flips the app back
//   to the login screen before the error propagates to the caller.

export class ApiError extends Error {
  constructor(message, status = 0, body = null) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.body = body
  }

  /** Network failure / server unreachable / response not JSON. */
  get isTransport() {
    return this.status === 0
  }
}

let unauthorizedHandler = null

/** Register the callback invoked whenever the API answers 401. */
export function setUnauthorizedHandler(handler) {
  unauthorizedHandler = handler
}

function apiBase() {
  // The Go server serves the API at the origin root by default; a deployment
  // behind a path prefix can override it by setting window.__ADMIN_API_BASE__
  // before the bundle loads.
  const injected = typeof window !== 'undefined' ? window.__ADMIN_API_BASE__ : null
  if (typeof injected === 'string' && injected !== '') return injected.replace(/\/+$/, '')
  return ''
}

function buildUrl(path, query) {
  const url = `${apiBase()}${path}`
  if (!query) return url
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === null || value === undefined || value === '') continue
    params.set(key, String(value))
  }
  const qs = params.toString()
  return qs ? `${url}?${qs}` : url
}

async function request(path, options = {}) {
  const { method = 'GET', body, query } = options
  const init = {
    method,
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
  }
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }

  let response
  try {
    response = await fetch(buildUrl(path, query), init)
  } catch (cause) {
    throw new ApiError('无法连接到服务器，请确认服务正在运行', 0, null)
  }

  const text = await response.text()
  let data = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch (cause) {
      if (response.status === 401) {
        notifyUnauthorized()
        throw new ApiError('登录状态已失效，请重新登录', 401, null)
      }
      throw new ApiError(`服务器返回了非 JSON 响应（HTTP ${response.status}）`, response.status, text)
    }
  }

  if (response.status === 401) {
    notifyUnauthorized()
    throw new ApiError(messageOf(data, '密码错误或登录状态已失效'), 401, data)
  }

  if (!response.ok) {
    throw new ApiError(messageOf(data, `请求失败（HTTP ${response.status}）`), response.status, data)
  }

  return data
}

function notifyUnauthorized() {
  if (typeof unauthorizedHandler === 'function') unauthorizedHandler()
}

function messageOf(data, fallback) {
  if (data && typeof data.error === 'string' && data.error !== '') return data.error
  if (data && typeof data.message === 'string' && data.message !== '') return data.message
  return fallback
}

/** Optional usage filters: since/until are RFC3339, the rest are exact matches. */
function usageQuery(filters = {}) {
  return {
    since: filters.since,
    until: filters.until,
    user: filters.user,
    provider: filters.provider,
    model: filters.model,
  }
}

export const api = {
  // --- auth -------------------------------------------------------------
  login: (password) => request('/api/login', { method: 'POST', body: { password } }),
  logout: () => request('/api/logout', { method: 'POST' }),
  me: () => request('/api/me'),
  health: () => request('/api/health'),

  // --- usage ------------------------------------------------------------
  usageSummary: (filters) => request('/api/usage/summary', { query: usageQuery(filters) }),
  usageByModel: (filters, limit = 50) =>
    request('/api/usage/by-model', { query: { ...usageQuery(filters), limit } }),
  usageByProvider: (filters, limit = 50) =>
    request('/api/usage/by-provider', { query: { ...usageQuery(filters), limit } }),
  usageByUser: (filters, limit = 50) =>
    request('/api/usage/by-user', { query: { ...usageQuery(filters), limit } }),
  usageByDay: (filters, days = 30) =>
    request('/api/usage/by-day', { query: { ...usageQuery(filters), days } }),
  usageRecent: (filters, limit = 20) =>
    request('/api/usage/recent', { query: { ...usageQuery(filters), limit } }),

  // --- audit ------------------------------------------------------------
  audit: ({ limit = 50, tool, user } = {}) => request('/api/audit', { query: { limit, tool, user } }),

  // --- skills -----------------------------------------------------------
  skills: () => request('/api/skills'),
  setSkillEnabled: (name, enabled) =>
    request(`/api/skills/${encodeURIComponent(name)}`, { method: 'POST', body: { enabled } }),

  // --- meta -------------------------------------------------------------
  meta: () => request('/api/meta'),
}
