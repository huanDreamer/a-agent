// Chat state and turn streaming for the 对话 tab.
//
// It lives outside the component on purpose: a half-finished stream must
// survive switching to another tab and back, and the views stay a pure render
// of this store. Everything in here mirrors the API contract of
// internal/server/chat.go.

import { computed, reactive } from 'vue'
import { api, streamChatTurn } from './api.js'

const SESSION_LIMIT = 100
const NOTICE_MS = 2600

export const chat = reactive({
  // --- model catalog: GET /api/chat/models ------------------------------
  catalog: null,
  catalogStatus: 'loading', // loading | error | ready
  catalogError: '',

  // --- session list: GET /api/chat/sessions -----------------------------
  sessions: [],
  sessionsStatus: 'loading',
  sessionsError: '',

  // --- the selected conversation ----------------------------------------
  activeId: '',
  session: null,
  /** Render items built from GET /api/chat/sessions/{id} + the live turn. */
  items: [],
  messagesStatus: 'ready',
  messagesError: '',

  // --- turn machinery ---------------------------------------------------
  streaming: false,
  creating: false,
  /** Bumped on every streamed event so views can react (auto-scroll). */
  streamTick: 0,
  /** Failure of a user action (send / rename / clear / delete / model). */
  actionError: '',
  /** Content of a failed send, offered again by the banner's 重试 button. */
  pendingContent: '',
  /** Transient confirmation line ("模型已切换"). */
  notice: '',
})

// ---------------------------------------------------------------- helpers --

let keySeq = 0
function nextKey(prefix) {
  keySeq += 1
  return `${prefix}-${keySeq}`
}

let noticeTimer = null
function setNotice(text) {
  chat.notice = text
  if (noticeTimer) window.clearTimeout(noticeTimer)
  noticeTimer = window.setTimeout(() => {
    noticeTimer = null
    chat.notice = ''
  }, NOTICE_MS)
}

function errorText(err, fallback) {
  if (err && typeof err.message === 'string' && err.message !== '') return err.message
  return fallback
}

function numberOrNull(value) {
  if (typeof value === 'number') return Number.isFinite(value) ? value : null
  if (typeof value === 'string' && value.trim() !== '') {
    const n = Number(value)
    return Number.isFinite(n) ? n : null
  }
  return null
}

/** First non-empty value among `keys` — tolerant of both key casing styles. */
function pick(source, keys) {
  if (!source || typeof source !== 'object') return undefined
  for (const key of keys) {
    const value = source[key]
    if (value !== undefined && value !== null && value !== '') return value
  }
  return undefined
}

/** Tool args/results are strings that usually contain JSON; keep them textual. */
function asText(value) {
  if (value === null || value === undefined) return ''
  if (typeof value === 'string') return value
  try {
    return JSON.stringify(value, null, 2)
  } catch (err) {
    return String(value)
  }
}

/**
 * Normalise one tool call.
 *
 * The persisted `tool_calls` column is a JSON string. It is written by
 * json.Marshal over chat.ToolRun, which currently has no json tags, so both the
 * documented lowercase keys and Go's default capitalised field names are
 * accepted here — a rename on the server side cannot break the UI.
 */
function toolItem(raw, index, finished) {
  const source = raw && typeof raw === 'object' ? raw : {}
  const id = String(pick(source, ['id', 'ID', 'tool_call_id', 'ToolCallID']) ?? `tool-${index}`)
  const name = String(pick(source, ['name', 'Name', 'tool_name', 'ToolName']) ?? '工具')
  const args = asText(pick(source, ['args', 'Args', 'arguments', 'Arguments']))
  const result = asText(pick(source, ['result', 'Result', 'tool_result', 'ToolResult']))
  const error = String(pick(source, ['err', 'Err', 'error', 'Error']) ?? '')
  const durationMs = numberOrNull(pick(source, ['duration_ms', 'DurationMs', 'durationMs']))
  return {
    key: `${id}-${index}`,
    id,
    name,
    args,
    result,
    error,
    durationMs,
    status: error ? 'failed' : finished || result ? 'ok' : 'running',
    open: false,
  }
}

/** Parse the persisted tool_calls JSON string; a broken value is ignored. */
function parseToolCalls(raw, finished) {
  if (!raw) return []
  let list = raw
  if (typeof raw === 'string') {
    const trimmed = raw.trim()
    if (trimmed === '') return []
    try {
      list = JSON.parse(trimmed)
    } catch (err) {
      // Defensive: a half-written column must never break the conversation.
      return []
    }
  }
  if (!Array.isArray(list)) return []
  return list.map((entry, index) => toolItem(entry, index, finished))
}

/** Normalise a usage object (the persisted column is a JSON string). */
function normalizeUsage(raw) {
  let source = raw
  if (typeof raw === 'string') {
    try {
      source = JSON.parse(raw)
    } catch (err) {
      return null
    }
  }
  if (!source || typeof source !== 'object') return null
  const prompt = numberOrNull(pick(source, ['prompt_tokens', 'promptTokens']))
  const completion = numberOrNull(pick(source, ['completion_tokens', 'completionTokens']))
  const total = numberOrNull(pick(source, ['total_tokens', 'totalTokens']))
  const durationMs = numberOrNull(pick(source, ['duration_ms', 'durationMs']))
  if (prompt === null && completion === null && total === null && durationMs === null) return null
  return {
    promptTokens: prompt ?? 0,
    completionTokens: completion ?? 0,
    totalTokens: total ?? (prompt ?? 0) + (completion ?? 0),
    durationMs,
  }
}

/**
 * Accumulate per-step usage events.
 *
 * The server emits one `usage` event per model call (each ReAct step) and
 * persists their sum, so the turn footer adds them up to match the reloaded
 * message.
 */
function addUsage(current, raw) {
  const next = normalizeUsage(raw)
  if (!next) return current
  if (!current) return { ...next }
  return {
    promptTokens: current.promptTokens + next.promptTokens,
    completionTokens: current.completionTokens + next.completionTokens,
    totalTokens: current.totalTokens + next.totalTokens,
    durationMs:
      current.durationMs === null || current.durationMs === undefined
        ? next.durationMs
        : next.durationMs === null || next.durationMs === undefined
          ? current.durationMs
          : current.durationMs + next.durationMs,
  }
}

// ------------------------------------------------------- render item model --

/** Build the conversation view model from the persisted message rows. */
export function buildItems(messages) {
  const items = []
  const list = Array.isArray(messages) ? messages : []
  for (const message of list) {
    const row = message && typeof message === 'object' ? message : {}
    if (row.role === 'user') {
      items.push({
        key: `user-${row.id ?? nextKey('user')}`,
        role: 'user',
        text: row.content || '',
        createdAt: row.created_at || '',
        error: '',
      })
      continue
    }
    if (row.role === 'assistant') {
      items.push({
        key: `assistant-${row.id ?? nextKey('assistant')}`,
        role: 'assistant',
        text: row.content || '',
        reasoning: row.reasoning || '',
        reasoningOpen: false,
        reasoningTouched: false,
        tools: parseToolCalls(row.tool_calls, true),
        usage: normalizeUsage(row.usage),
        error: row.error || '',
        createdAt: row.created_at || '',
        streaming: false,
      })
      continue
    }
    if (row.role === 'tool') {
      // Tool results normally live inside the assistant's tool_calls JSON. A
      // standalone row is folded into the previous turn so nothing is hidden,
      // and ignored when there is nothing to fold it into or it is a duplicate.
      const last = items.length ? items[items.length - 1] : null
      if (!last || last.role !== 'assistant') continue
      const id = row.tool_call_id || `tool-${items.length}`
      if (last.tools.some((tool) => tool.id === id)) continue
      last.tools.push(
        toolItem({ id, name: row.tool_name, result: row.content }, last.tools.length, true),
      )
    }
  }
  return items
}

// ------------------------------------------------------------- derivations --

export const chatModels = computed(() => {
  const models = chat.catalog && chat.catalog.models
  return Array.isArray(models) ? models : []
})

export const chatTools = computed(() => {
  const tools = chat.catalog && chat.catalog.tools
  return Array.isArray(tools) ? tools : []
})

/** Catalog entries grouped by provider, for the <optgroup> select. */
export const modelGroups = computed(() => {
  const groups = new Map()
  for (const entry of chatModels.value) {
    const item = entry && typeof entry === 'object' ? entry : {}
    const provider = item.provider || '默认'
    if (!groups.has(provider)) groups.set(provider, [])
    groups.get(provider).push({
      provider: item.provider || '',
      model: item.model || '',
      isDefault: Boolean(item.default),
      hasKey: item.has_api_key !== false,
    })
  }
  return [...groups].map(([provider, models]) => ({ provider, models }))
})

export const defaultModel = computed(
  () => chatModels.value.find((m) => m && m.default) || chatModels.value[0] || null,
)

export const maxSteps = computed(() => {
  const value = chat.catalog && chat.catalog.max_steps
  const n = numberOrNull(value)
  return n && n > 0 ? n : null
})

export const activeSession = computed(() => chat.session)

// ------------------------------------------------------------------ loading --

export async function loadCatalog({ quiet = false } = {}) {
  if (!quiet && !chat.catalog) chat.catalogStatus = 'loading'
  try {
    const res = await api.chatModels()
    chat.catalog = res && typeof res === 'object' ? res : { models: [], tools: [] }
    chat.catalogError = ''
    chat.catalogStatus = 'ready'
  } catch (err) {
    if (err && err.status === 401) return
    chat.catalogError = errorText(err, '无法读取模型目录')
    if (!chat.catalog) chat.catalogStatus = 'error'
  }
}

export async function loadSessions({ quiet = false, select = true } = {}) {
  if (!quiet && !chat.sessions.length) chat.sessionsStatus = 'loading'
  try {
    const res = await api.chatSessions(SESSION_LIMIT)
    const list = res && Array.isArray(res.sessions) ? res.sessions : []
    chat.sessions = list
    chat.sessionsError = ''
    chat.sessionsStatus = 'ready'

    if (!select) return
    const active = list.find((s) => s && s.id === chat.activeId)
    if (active) {
      // Keep the header (model, title, count) in sync with server state.
      if (chat.session) chat.session = { ...chat.session, ...active }
      return
    }
    const newest = list[0]
    if (newest && newest.id) await selectSession(newest.id)
    else resetSelection()
  } catch (err) {
    if (err && err.status === 401) return
    chat.sessionsError = errorText(err, '无法读取会话列表')
    if (!chat.sessions.length) chat.sessionsStatus = 'error'
  }
}

/** Boot the tab: catalog + sessions, then the newest conversation. */
export async function ensureLoaded() {
  if (!chat.catalog) await loadCatalog()
  await loadSessions()
}

function resetSelection() {
  chat.activeId = ''
  chat.session = null
  chat.items = []
  chat.messagesError = ''
  chat.messagesStatus = 'ready'
}

export async function selectSession(id, { quiet = false } = {}) {
  if (!id) return
  if (!quiet && chat.streaming) stopStreaming()
  chat.activeId = id
  chat.actionError = ''
  if (!quiet) {
    // Never show the previous conversation's bubbles under a new title.
    chat.items = []
    if (!chat.session || chat.session.id !== id) chat.session = null
    chat.messagesStatus = 'loading'
    chat.messagesError = ''
  }
  try {
    const res = await api.chatSession(id)
    if (chat.activeId !== id) return // a newer selection won
    chat.session = res && res.session ? res.session : null
    chat.items = buildItems(res && res.messages)
    chat.messagesError = ''
    chat.messagesStatus = 'ready'
  } catch (err) {
    if (err && err.status === 401) return
    if (chat.activeId !== id) return
    chat.messagesError = errorText(err, '无法读取会话内容')
    if (quiet) chat.actionError = chat.messagesError
    else chat.messagesStatus = 'error'
  }
}

// ------------------------------------------------------------ session edits --

function replaceSession(session) {
  if (!session || !session.id) return
  let found = false
  chat.sessions = chat.sessions.map((item) => {
    if (item && item.id === session.id) {
      found = true
      return { ...item, ...session }
    }
    return item
  })
  if (!found) chat.sessions = [session, ...chat.sessions]
}

export async function createSession() {
  if (chat.creating) return null
  chat.creating = true
  chat.actionError = ''
  const fallback = defaultModel.value || {}
  try {
    const res = await api.createChatSession({
      title: '',
      provider: fallback.provider || '',
      model: fallback.model || '',
    })
    const session = res && res.session ? res.session : null
    if (session && session.id) {
      replaceSession(session)
      chat.sessions = chat.sessions.slice().sort(byRecency)
      chat.activeId = session.id
      chat.session = session
      chat.items = []
      chat.messagesError = ''
      chat.messagesStatus = 'ready'
    }
    return session
  } catch (err) {
    if (err && err.status === 401) return null
    chat.actionError = errorText(err, '新建对话失败，请重试')
    if (!chat.sessions.length) chat.sessionsStatus = 'error'
    return null
  } finally {
    chat.creating = false
  }
}

function byRecency(a, b) {
  const left = Date.parse((a && a.updated_at) || 0) || 0
  const right = Date.parse((b && b.updated_at) || 0) || 0
  return right - left
}

export async function renameSession(id, title) {
  const next = String(title || '').trim()
  if (!next) {
    chat.actionError = '标题不能为空'
    return false
  }
  chat.actionError = ''
  try {
    const res = await api.patchChatSession(id, { title: next })
    const session = res && res.session ? res.session : null
    if (session) {
      replaceSession(session)
      if (chat.session && chat.session.id === id) chat.session = { ...chat.session, ...session }
    }
    setNotice('标题已更新')
    return true
  } catch (err) {
    if (err && err.status === 401) return false
    chat.actionError = errorText(err, '重命名失败，请重试')
    return false
  }
}

export async function setModel(provider, model) {
  const id = chat.activeId
  if (!id) return
  chat.actionError = ''
  try {
    const res = await api.patchChatSession(id, { provider, model })
    const session = res && res.session ? res.session : null
    if (session) {
      // The server resolves the pair, so the response is authoritative.
      chat.session = { ...(chat.session || {}), ...session }
      replaceSession(session)
    }
    setNotice('模型已切换')
  } catch (err) {
    if (err && err.status === 401) return
    chat.actionError = errorText(err, '切换模型失败，请重试')
    await loadSessions({ quiet: true, select: true })
  }
}

export async function clearSession(id) {
  chat.actionError = ''
  try {
    await api.clearChatSession(id)
    if (chat.activeId === id) {
      chat.items = []
      if (chat.session) chat.session = { ...chat.session, message_count: 0 }
    }
    replaceSession({ id, message_count: 0 })
    setNotice('会话已清空')
    return true
  } catch (err) {
    if (err && err.status === 401) return false
    chat.actionError = errorText(err, '清空失败，请重试')
    return false
  }
}

export async function deleteSession(id) {
  chat.actionError = ''
  try {
    await api.deleteChatSession(id)
    chat.sessions = chat.sessions.filter((s) => !s || s.id !== id)
    if (chat.activeId === id) {
      resetSelection()
      const next = chat.sessions[0]
      if (next && next.id) await selectSession(next.id)
    }
    setNotice('会话已删除')
    return true
  } catch (err) {
    if (err && err.status === 401) return false
    chat.actionError = errorText(err, '删除失败，请重试')
    return false
  }
}

// --------------------------------------------------------------- the stream --

let controller = null
let streamSeq = 0

/**
 * Send one message and stream the answer.
 *
 * On `done` the server's text replaces what was streamed (it is authoritative:
 * the ReAct loop may have produced a preamble before calling a tool). On
 * `error` the failure becomes a warning bubble in the conversation. When the
 * stream ends — normally, on 停止, or on a dropped connection — the session is
 * reloaded from the server, which is the only place with the persisted ids,
 * tool results and summed usage.
 */
export async function sendMessage(rawContent) {
  const content = String(rawContent === null || rawContent === undefined ? '' : rawContent).trim()
  if (content === '') return
  if (chat.streaming) return
  if (!chat.activeId) {
    chat.actionError = '请先选择或新建一个对话'
    return
  }

  const sessionId = chat.activeId
  chat.actionError = ''
  chat.pendingContent = ''
  chat.notice = ''

  // The live items are wrapped in `reactive` explicitly: the stream appends to
  // their fields after they were pushed, and mutating a raw object stored in a
  // reactive array would not trigger a re-render.
  const userItem = reactive({
    key: nextKey('user-live'),
    role: 'user',
    text: content,
    createdAt: new Date().toISOString(),
    error: '',
  })
  const turn = reactive({
    key: nextKey('turn'),
    role: 'assistant',
    text: '',
    reasoning: '',
    reasoningOpen: true,
    reasoningTouched: false,
    tools: [],
    usage: null,
    error: '',
    createdAt: '',
    streaming: true,
  })
  chat.items.push(userItem, turn)
  chat.streaming = true
  chat.streamTick += 1

  const seq = ++streamSeq
  controller = new AbortController()
  let aborted = false

  try {
    const outcome = await streamChatTurn(sessionId, content, {
      signal: controller.signal,
      onEvent: (event) => handleEvent(turn, event),
    })
    aborted = Boolean(outcome && outcome.aborted)
  } catch (err) {
    if (err && err.status === 401) return
    if (seq !== streamSeq) return
    if (turn.text || turn.reasoning || turn.tools.length) {
      // The turn produced visible output before the connection died: keep it
      // and mark it, instead of throwing the answer away.
      turn.error = errorText(err, '连接中断，本轮可能未完整保存')
      turn.streaming = false
    } else {
      // Nothing arrived: the optimistic bubbles cannot be trusted, so drop them
      // and offer the text again in the composer.
      chat.items = chat.items.filter((item) => item !== userItem && item !== turn)
      chat.pendingContent = content
      chat.actionError = errorText(err, '发送失败，请重试')
    }
    chat.streamTick += 1
    return
  } finally {
    if (seq === streamSeq) {
      chat.streaming = false
      controller = null
      turn.streaming = false
      chat.streamTick += 1
    }
  }

  if (seq !== streamSeq) return
  if (aborted) {
    // The server persists an aborted turn on client disconnect, but the row can
    // land a moment after the request is cancelled — give it that moment so a
    // stopped turn is not missing from the reload.
    await wait(400)
    if (seq !== streamSeq) return
  }
  // Authoritative reload: ids, persisted tool calls, summed usage, auto title.
  await selectSession(sessionId, { quiet: true })
  await loadSessions({ quiet: true, select: false })
}

const wait = (ms) => new Promise((resolve) => window.setTimeout(resolve, ms))

/** Stop the running turn. The server still persists what it produced. */
export function stopStreaming() {
  if (!controller) return
  const pending = controller
  controller = null
  setNotice('已停止生成')
  pending.abort()
}

function handleEvent(turn, event) {
  if (!event || typeof event !== 'object') return true
  chat.streamTick += 1

  switch (event.type) {
    case 'step_start':
      if (numberOrNull(event.step)) turn.step = event.step
      return true

    case 'reasoning_delta':
      if (typeof event.text === 'string') turn.reasoning += event.text
      return true

    case 'text_delta':
      if (typeof event.text === 'string') {
        turn.text += event.text
        // The reasoning panel is only interesting until the answer starts.
        if (!turn.reasoningTouched && turn.reasoningOpen) turn.reasoningOpen = false
      }
      return true

    case 'tool_call': {
      turn.tools.push(
        toolItem(
          { id: event.tool_call_id, name: event.tool_name, args: event.tool_args },
          turn.tools.length,
          false,
        ),
      )
      return true
    }

    case 'tool_result': {
      const id = String(event.tool_call_id || '')
      let tool = turn.tools.find((item) => item.id === id)
      if (!tool) {
        tool = toolItem({ id, name: event.tool_name }, turn.tools.length, false)
        turn.tools.push(tool)
      }
      tool.result = typeof event.tool_result === 'string' ? event.tool_result : asText(event.tool_result)
      tool.error = typeof event.tool_error === 'string' ? event.tool_error : ''
      tool.durationMs = numberOrNull(event.duration_ms)
      tool.status = tool.error ? 'failed' : 'ok'
      return true
    }

    case 'usage':
      turn.usage = addUsage(turn.usage, event.usage)
      return true

    case 'done':
      if (typeof event.text === 'string' && event.text !== '') {
        turn.text = event.text
        turn.authoritative = true
      }
      return true

    case 'error':
      turn.error = typeof event.error === 'string' && event.error !== '' ? event.error : '本轮执行失败'
      turn.streaming = false
      // Keep reading until the terminating frame: the server persists the
      // failed turn just before it, and that row is what a reload shows.
      return true

    case 'stream_end':
      return false // always last — nothing follows

    default:
      return true
  }
}

/** Drop every trace of the previous login (called on logout). */
export function resetChat() {
  streamSeq += 1
  if (controller) {
    const pending = controller
    controller = null
    pending.abort()
  }
  if (noticeTimer) {
    window.clearTimeout(noticeTimer)
    noticeTimer = null
  }
  chat.catalog = null
  chat.catalogStatus = 'loading'
  chat.catalogError = ''
  chat.sessions = []
  chat.sessionsStatus = 'loading'
  chat.sessionsError = ''
  chat.activeId = ''
  chat.session = null
  chat.items = []
  chat.messagesStatus = 'ready'
  chat.messagesError = ''
  chat.streaming = false
  chat.creating = false
  chat.streamTick = 0
  chat.actionError = ''
  chat.pendingContent = ''
  chat.notice = ''
}

/** Dismiss the action-error banner (and forget the pending retry). */
export function dismissActionError() {
  chat.actionError = ''
  chat.pendingContent = ''
}
