// Minimal fetch wrapper for the huan-agent admin API.
//
// - always sends cookies (`credentials: 'same-origin'`), because the session
//   cookie set by POST /api/login is the only authentication mechanism;
// - decodes/encodes JSON and raises ApiError (carrying the HTTP status) on
//   every failure, so views never have to inspect Response objects;
// - a 401 means the session expired: the registered handler flips the app back
//   to the login screen before the error propagates to the caller.

import { createSseParser, parseSsePayload } from './sse.js'

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

/**
 * POST a chat message and consume the Server-Sent Events reply.
 *
 * `EventSource` cannot be used here: it only issues GET requests, while the
 * endpoint is a POST that starts a turn. So the request is a plain `fetch`
 * whose body stream is read and framed by hand (see `sse.js`). Every parsed
 * event object is handed to `onEvent` in arrival order; returning `false` from
 * `onEvent` stops reading (used for the terminal `stream_end`/`error` events).
 *
 * Resolves when the turn is over — including the case where the server closed
 * the stream without a terminating frame — as
 * `{ aborted, stopped, ended }`: `aborted` means the caller's AbortController
 * fired (the 停止 button), which is not an error. Rejects with ApiError when the
 * request itself fails, including a non-200 JSON error before any event.
 */
export async function streamChatTurn(sessionId, content, { signal, onEvent } = {}) {
  const url = buildUrl(`/api/chat/sessions/${encodeURIComponent(sessionId)}/messages`)

  let response
  try {
    response = await fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { Accept: 'text/event-stream', 'Content-Type': 'application/json' },
      body: JSON.stringify({ content }),
      signal,
    })
  } catch (cause) {
    if (isAbort(cause)) return { aborted: true, stopped: false, ended: false }
    throw new ApiError('无法连接到服务器，请确认服务正在运行', 0, null)
  }

  if (response.status === 401) {
    notifyUnauthorized()
    throw new ApiError('登录状态已失效，请重新登录', 401, null)
  }

  if (!response.ok) {
    // A misconfiguration (unknown session, missing API key, chat disabled) is
    // reported as a normal JSON error instead of a broken stream.
    let data = null
    try {
      const text = await response.text()
      data = text ? JSON.parse(text) : null
    } catch (err) {
      data = null
    }
    throw new ApiError(messageOf(data, `发送失败（HTTP ${response.status}）`), response.status, data)
  }

  if (!response.body || typeof response.body.getReader !== 'function') {
    throw new ApiError('当前浏览器不支持流式响应，无法显示实时输出', 0, null)
  }

  const reader = response.body.getReader()
  const decoder = new TextDecoder('utf-8')
  const parser = createSseParser()
  let stopped = false
  let ended = false
  let events = 0
  let invalid = 0

  /** Hand one frame to the caller; returns its verdict (false = stop reading). */
  function consume(frame) {
    const event = parseSsePayload(frame)
    if (event === null) {
      invalid += 1
      return true
    }
    events += 1
    if (event.type === 'stream_end') ended = true
    if (typeof onEvent !== 'function') return true
    return onEvent(event)
  }

  try {
    while (!stopped) {
      const chunk = await reader.read()
      if (chunk.done) {
        // The connection closed: emit whatever the last frame left behind.
        for (const frame of parser.flush()) {
          if (consume(frame) === false) break
        }
        break
      }
      const frames = parser.push(decoder.decode(chunk.value, { stream: true }))
      for (const frame of frames) {
        if (consume(frame) === false) {
          stopped = true
          break
        }
      }
    }
  } catch (cause) {
    if (isAbort(cause)) return { aborted: true, stopped, ended }
    throw new ApiError('读取流式响应失败，请重试', 0, null)
  } finally {
    if (stopped || signal?.aborted) {
      try {
        await reader.cancel()
      } catch (err) {
        // Cancelling an already-closed stream is not an error.
      }
    }
  }

  if (events === 0 && invalid > 0) {
    throw new ApiError('流式响应解析失败，请重试', 0, null)
  }
  return { aborted: false, stopped, ended }
}

function isAbort(cause) {
  return Boolean(cause) && (cause.name === 'AbortError' || cause.code === 20)
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

  // --- chat -------------------------------------------------------------
  chatModels: () => request('/api/chat/models'),
  chatSessions: (limit = 100) => request('/api/chat/sessions', { query: { limit } }),
  createChatSession: (body = {}) => request('/api/chat/sessions', { method: 'POST', body }),
  chatSession: (id) => request(`/api/chat/sessions/${encodeURIComponent(id)}`),
  patchChatSession: (id, patch) =>
    request(`/api/chat/sessions/${encodeURIComponent(id)}`, { method: 'PATCH', body: patch }),
  deleteChatSession: (id) =>
    request(`/api/chat/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  clearChatSession: (id) =>
    request(`/api/chat/sessions/${encodeURIComponent(id)}/clear`, { method: 'POST' }),

  // --- traces (Langfuse) ------------------------------------------------
  traceStatus: () => request('/api/traces/status'),
  traces: ({ limit = 50, page = 1, session, user, name } = {}) =>
    request('/api/traces', { query: { limit, page, session, user, name } }),
  trace: (id) => request(`/api/traces/${encodeURIComponent(id)}`),

  // --- meta -------------------------------------------------------------
  meta: () => request('/api/meta'),
}
