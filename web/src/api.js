// Minimal fetch wrapper for the huan-agent admin API.
//
// - sends cookies (`credentials: 'same-origin'`) so a deployment with
//   `admin.require_login: true` carries its session cookie; the login-free
//   default (require_login: false) has no cookie to carry;
// - decodes/encodes JSON and raises ApiError (carrying the HTTP status) on
//   every failure, so views never have to inspect Response objects;
// - a 401 on a data call means the session the console held is no longer
//   accepted: the registered handler sends the console back to its login form
//   (or, where login is off, reports the mismatch) before the error propagates.
//   `POST /api/login` opts out of that handler — see `own401`.

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
  // `own401` marks a request that interprets 401 itself: on POST /api/login a
  // 401 means "wrong password" (or the login throttle), which is the form's
  // business, not a dead session — telling the console its session expired
  // while it is trying to obtain one would be nonsense.
  const { method = 'GET', body, query, own401 = false } = options
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
        if (!own401) notifyUnauthorized()
        throw new ApiError('服务端要求登录（HTTP 401）', 401, null)
      }
      throw new ApiError(`服务器返回了非 JSON 响应（HTTP ${response.status}）`, response.status, text)
    }
  }

  if (response.status === 401) {
    if (!own401) notifyUnauthorized()
    throw new ApiError(messageOf(data, '服务端要求登录（HTTP 401）'), 401, data)
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
export async function streamChatTurn(sessionId, content, { signal, onEvent, attachments } = {}) {
  const url = buildUrl(`/api/chat/sessions/${encodeURIComponent(sessionId)}/messages`)
  const body = { content }
  // Omitted entirely when there is nothing attached, so the request body keeps
  // the shape the pre-attachment server expects.
  if (Array.isArray(attachments) && attachments.length) body.attachments = attachments

  let response
  try {
    response = await fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { Accept: 'text/event-stream', 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal,
    })
  } catch (cause) {
    if (isAbort(cause)) return { aborted: true, stopped: false, ended: false }
    throw new ApiError('无法连接到服务器，请确认服务正在运行', 0, null)
  }

  if (response.status === 401) {
    notifyUnauthorized()
    throw new ApiError('服务端要求登录（HTTP 401）', 401, null)
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

/**
 * Upload one file as an attachment of a conversation.
 *
 * `XMLHttpRequest` rather than `fetch` on purpose: it is the only way to get
 * real upload progress in a browser without a streaming request body, and the
 * composer shows a percentage while a large file is in flight.
 *
 * Resolves with the `attachment` object of the response (id, kind, mime, bytes,
 * name, path, url). Rejects with ApiError carrying the HTTP status, so a 415
 * (unsupported type) and a 413 (too large) both reach the caller as messages.
 */
export function uploadAttachment(sessionId, file, { onProgress } = {}) {
  const url = buildUrl(`/api/chat/sessions/${encodeURIComponent(sessionId)}/attachments`)
  return new Promise((resolve, reject) => {
    const form = new FormData()
    form.append('file', file, file.name || 'upload')

    const xhr = new XMLHttpRequest()
    xhr.open('POST', url)
    xhr.responseType = 'text'
    xhr.withCredentials = true
    xhr.setRequestHeader('Accept', 'application/json')

    if (xhr.upload && typeof onProgress === 'function') {
      xhr.upload.onprogress = (event) => {
        if (event.lengthComputable) onProgress(event.loaded / event.total)
      }
    }

    xhr.onload = () => {
      let data = null
      try {
        data = xhr.responseText ? JSON.parse(xhr.responseText) : null
      } catch (err) {
        data = null
      }
      if (xhr.status === 401) {
        notifyUnauthorized()
        reject(new ApiError(messageOf(data, '服务端要求登录（HTTP 401）'), 401, data))
        return
      }
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(new ApiError(messageOf(data, `上传失败（HTTP ${xhr.status}）`), xhr.status, data))
        return
      }
      const attachment = data && data.attachment ? data.attachment : null
      if (!attachment || !attachment.id) {
        reject(new ApiError('服务器没有返回附件信息', 0, data))
        return
      }
      resolve(attachment)
    }

    xhr.onerror = () => reject(new ApiError('无法连接到服务器，上传失败', 0, null))
    xhr.onabort = () => reject(new ApiError('上传已取消', 0, null))
    xhr.send(form)
  })
}

/**
 * Absolute URL of a stored attachment.
 *
 * The upload response carries `url` as a root-relative path; a message reloaded
 * from the database only carries the asset id, so the path is rebuilt from the
 * documented route. Both go through `apiBase()` for a deployment behind a path
 * prefix.
 */
export function assetUrl({ id, url } = {}) {
  const path =
    typeof url === 'string' && url !== ''
      ? url
      : `/api/chat/attachments/${encodeURIComponent(id === null || id === undefined ? '' : id)}`
  if (/^https?:\/\//i.test(path)) return path
  return `${apiBase()}${path}`
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
  // --- session ----------------------------------------------------------
  // The boot probe. It always answers 200 and says two things: whether *this
  // caller* would be asked for a password (`login_required` — admin.require_login,
  // minus the loopback exemption admin.trust_loopback gives a browser on the
  // server's own machine) and whether we hold a session. The console picks
  // between the shell and the login form from that, instead of mounting every
  // panel and reading the answer out of a dozen 401s.
  me: () => request('/api/me'),
  // Exchanges the single admin password for a session cookie. The token is
  // httpOnly and never reaches this code: the browser stores it and sends it
  // back on its own.
  login: (password) => request('/api/login', { method: 'POST', body: { password }, own401: true }),
  // Revokes the session server-side; the response also expires the cookie.
  logout: () => request('/api/logout', { method: 'POST' }),

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
  // A skill is one markdown file (YAML frontmatter + instructions) in the
  // configured skills directory. The list is the toggle surface; the detail
  // route carries the body the editor loads, and PUT writes it back.
  skills: () => request('/api/skills'),
  setSkillEnabled: (name, enabled) =>
    request(`/api/skills/${encodeURIComponent(name)}`, { method: 'POST', body: { enabled } }),
  skillDetail: (name) => request(`/api/skills/${encodeURIComponent(name)}`),
  saveSkill: (name, body) =>
    request(`/api/skills/${encodeURIComponent(name)}`, { method: 'PUT', body }),
  deleteSkill: (name) =>
    request(`/api/skills/${encodeURIComponent(name)}`, { method: 'DELETE' }),
  // 技能写作助手: a description in, a draft skill out. Nothing is written —
  // saving is a separate, deliberate PUT.
  draftSkill: (body) => request('/api/skills-draft', { method: 'POST', body }),

  // --- MCP servers -------------------------------------------------------
  // The row shape is the stored definition plus `runtime` (connected / tools /
  // error), so one object is enough to render a row.
  mcpServers: () => request('/api/mcp/servers'),
  saveMcpServer: (body) => request('/api/mcp/servers', { method: 'POST', body }),
  deleteMcpServer: (id) =>
    request(`/api/mcp/servers/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  // 测试连接: on a saved server, or on a definition that has not been saved yet
  // (probe), which is what the form uses so a broken server never reaches the
  // runtime.
  testMcpServer: (id) =>
    request(`/api/mcp/servers/${encodeURIComponent(id)}/test`, { method: 'POST' }),
  probeMcpServer: (body) => request('/api/mcp/probe', { method: 'POST', body }),
  // 重连: reconnect everything, for a transport that died under a running server.
  reloadMcp: () => request('/api/mcp/reload', { method: 'POST' }),
  // 配置助手: a description in, a draft definition out.
  draftMcpServer: (body) => request('/api/mcp/draft', { method: 'POST', body }),

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
  // The answer to an ask_user card. It is a second request on purpose: the
  // streaming response for this turn is already committed to its event stream,
  // so the question id is the only thing that can join the two.
  answerQuestion: (sessionId, questionId, body) =>
    request(
      `/api/chat/sessions/${encodeURIComponent(sessionId)}/questions/${encodeURIComponent(questionId)}/answer`,
      { method: 'POST', body },
    ),

  // --- traces (Langfuse) ------------------------------------------------
  traceStatus: () => request('/api/traces/status'),
  traces: ({ limit = 50, page = 1, session, user, name } = {}) =>
    request('/api/traces', { query: { limit, page, session, user, name } }),
  trace: (id) => request(`/api/traces/${encodeURIComponent(id)}`),

  // --- openviking (context database) -------------------------------------
  // Every route answers 200 when the integration is off, with
  // `{enable:false, message}`: "not configured" is a state the console renders,
  // not an error it reports. A sync that cannot run answers 200 with
  // `{ok:false, error}` for the same reason, and 409 only when one is already
  // running — which api.js surfaces as an ordinary message.
  openVikingStatus: () => request('/api/openviking/status'),
  syncOpenViking: (full = false) =>
    request('/api/openviking/sync', { method: 'POST', body: { full } }),
  saveOpenVikingDocument: (body) => request('/api/openviking/save', { method: 'POST', body }),
  openVikingDocuments: () => request('/api/openviking/documents'),
  flushOpenViking: () => request('/api/openviking/flush', { method: 'POST' }),

  // --- workspaces (the sidebar's folders) --------------------------------
  // A workspace is a directory the agent may work in, and every conversation
  // belongs to one. The console creates one by *picking an existing directory*,
  // which is why the picker talks to /api/fs/dirs rather than using a file
  // input: a browser cannot hand the server a path it chose.
  //
  // Two contracts worth knowing:
  //   * deleting never deletes files, and moves the folder's conversations to
  //     another workspace — the answer says where;
  //   * renaming changes only the label, never the directory.
  workspaces: () => request('/api/workspaces'),
  workspace: (name) => request(`/api/workspaces/${encodeURIComponent(name)}`),
  createWorkspace: ({ root, name }) =>
    request('/api/workspaces', { method: 'POST', body: { root, name } }),
  renameWorkspace: (name, next) =>
    request(`/api/workspaces/${encodeURIComponent(name)}`, {
      method: 'PATCH',
      body: { name: next },
    }),
  deleteWorkspace: (name) =>
    request(`/api/workspaces/${encodeURIComponent(name)}`, { method: 'DELETE' }),
  // browseDirs lists the subdirectories of a path (the home directory when it is
  // empty), for the picker. A path that cannot be read answers 200 with
  // `{ok:false, error}`: the picker shows the reason in place rather than
  // replacing the view with an error page.
  browseDirs: (path = '') => request('/api/fs/dirs', { query: { path } }),
  // GET/PUT /api/chat/sessions/:id/workspace are deliberately not wrapped here
  // any more: the console has no control that reads or moves a conversation's
  // workspace (it is chosen when the conversation is created, and the sidebar
  // shows it as the folder). The routes still exist server-side for a script
  // that needs them.

  // --- background jobs (the agent's own shell processes) ------------------
  // The list is bounded server-side and keeps finished jobs, so one call is
  // enough to render "运行中" and "已结束" side by side.
  //
  // `output` is a window, not the whole log: omitting `from` returns the tail
  // (plus the byte offset to continue from), and `dropped_bytes` reports how
  // much was discarded before the window — the caller is expected to say so
  // rather than present a partial log as complete.
  //
  // stop and forget are the only two mutations, and each is refused by the
  // server in the state where it would be meaningless: 409 when stopping a job
  // that already ended, and 409 when forgetting one that is still running.
  listJobs: ({ session } = {}) => request('/api/jobs', { query: { session } }),
  jobOutput: (id, { from, maxBytes } = {}) =>
    request(`/api/jobs/${encodeURIComponent(id)}`, { query: { from, max_bytes: maxBytes } }),
  stopJob: (id, { signal = 'term' } = {}) =>
    request(`/api/jobs/${encodeURIComponent(id)}/stop`, { method: 'POST', body: { signal } }),
  forgetJob: (id) => request(`/api/jobs/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  // --- meta -------------------------------------------------------------
  meta: () => request('/api/meta'),

  // --- llm providers / models / capability bindings ----------------------
  // Contract: provider records never carry the API key. `has_api_key` is a
  // boolean and `api_key_hint` is a masked suffix; the key is write-only, sent
  // either in the provider body or through the dedicated /key route, where an
  // empty string clears the stored key.
  llmProviders: () => request('/api/llm/providers'),
  saveLlmProvider: (body) => request('/api/llm/providers', { method: 'POST', body }),
  deleteLlmProvider: (id) =>
    request(`/api/llm/providers/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  setLlmProviderKey: (id, apiKey) =>
    request(`/api/llm/providers/${encodeURIComponent(id)}/key`, {
      method: 'PUT',
      body: { api_key: apiKey },
    }),
  testLlmProvider: (id) =>
    request(`/api/llm/providers/${encodeURIComponent(id)}/test`, { method: 'POST' }),
  refreshLlmProviderModels: (id) =>
    request(`/api/llm/providers/${encodeURIComponent(id)}/models/refresh`, { method: 'POST' }),

  llmModels: (provider) => request('/api/llm/models', { query: { provider } }),
  saveLlmModel: (body) => request('/api/llm/models', { method: 'PUT', body }),
  deleteLlmModel: (providerId, modelId) =>
    request('/api/llm/models', {
      method: 'DELETE',
      body: { provider_id: providerId, model_id: modelId },
    }),
  // 刷新全部: every enabled provider that has a key, in one request. It answers
  // 200 with a per-provider report (`{results: [{provider_id, ok, models_count,
  // error}]}`) even when providers failed, because a broken key is a result the
  // caller asked for rather than an error in the request.
  refreshAllLlmModels: () => request('/api/llm/models/refresh-all', { method: 'POST' }),

  llmBindings: () => request('/api/llm/bindings'),
  saveLlmBinding: (body) => request('/api/llm/bindings', { method: 'PUT', body }),
}
