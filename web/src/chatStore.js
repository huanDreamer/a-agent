// Chat state and turn streaming for the 对话 tab.
//
// It lives outside the component on purpose: a half-finished stream must
// survive switching to another tab and back, and the views stay a pure render
// of this store. Everything in here mirrors the API contract of
// internal/server/chat.go.

import { computed, reactive } from 'vue'
import { api, streamChatTurn } from './api.js'
import {
  ASK_ANSWERED,
  ASK_CANCELLED,
  ASK_PENDING,
  ASK_SUBMITTING,
  applyAskOutcome,
  askCardsFromTools,
  askFromEvent,
  isAskTool,
} from './ask.js'
import { normalize as normalizeAttachments } from './attachments.js'
import { formatCompact, formatDuration } from './format.js'
import { noteJobsToolRan } from './jobsStore.js'
import {
  catalogView,
  modelOptionHint,
  modelOptionLabel,
  providerStatsById,
  staleProviders,
} from './llm.js'

const SESSION_LIMIT = 100
const NOTICE_MS = 2600

export const chat = reactive({
  // --- model catalog: GET /api/chat/models ------------------------------
  catalog: null,
  catalogStatus: 'loading', // loading | error | ready
  catalogError: '',

  // --- session list: GET /api/chat/sessions -----------------------------
  /** True once the initial catalog + session load ran (see ensureLoaded). */
  booted: false,
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
  /** Attachments of that failed send: already uploaded, so they are reused. */
  pendingAttachments: [],
  /** Transient confirmation line ("模型已切换"). */
  notice: '',

  // --- workspaces: the sidebar's folders ---------------------------------
  /**
   * The workspace this conversation works in, as the server reports it:
   * {name, root, session_count}. Null when the deployment has no workspace
   * layer.
   *
   * Per conversation on purpose: the header shows which directory this one is
   * pointed at, and two conversations may be in two different ones.
   */
  workspace: null,
  /** Every workspace: GET /api/workspaces. */
  workspaces: [],
  workspacesStatus: 'ready',
  workspacesError: '',
  /** Note the workspaces list carries, shown once above the folders. */
  workspacesNote: '',
  /** Workspaces whose directory is gone, as warnings from the server. */
  workspaceWarnings: [],
  /** The workspace a new conversation would start in. */
  defaultWorkspace: '',
  /** Home directory, the picker's starting point. */
  homeDir: '',
  /**
   * Whether this server has a workspace layer.
   *
   * False when /api/workspaces answers 404, which is a supported deployment (the
   * layer is optional). The sidebar then falls back to the flat session list:
   * showing "no workspaces yet" plus a create button that can only fail would be
   * worse than showing no folders at all.
   */
  workspacesAvailable: true,
  /** Workspace names with an operation in flight, so their controls can lock. */
  pendingWorkspace: '',
  /** Workspace names whose folder is folded. */
  collapsedWorkspaces: {},
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
        // The persisted column is a JSON string of asset ids, exactly like
        // `tool_calls` and `usage`; normalize() parses it defensively.
        attachments: normalizeAttachments(row.attachments),
        createdAt: row.created_at || '',
        error: '',
      })
      continue
    }
    if (row.role === 'assistant') {
      const tools = parseToolCalls(row.tool_calls, true)
      items.push({
        key: `assistant-${row.id ?? nextKey('assistant')}`,
        role: 'assistant',
        text: row.content || '',
        reasoning: row.reasoning || '',
        reasoningOpen: false,
        reasoningTouched: false,
        tools,
        // The model's questions, rebuilt from the persisted ask_user calls: the
        // tool call carries the question and its result carries the answer, so a
        // reloaded conversation shows the card without a table of its own.
        asks: askCardsFromTools(tools),
        usage: normalizeUsage(row.usage),
        error: row.error || '',
        createdAt: row.created_at || '',
        // The trace that produced this answer, when one was recorded. Empty for
        // a turn that ran with tracing off and for messages written before the
        // column existed; the bubble then offers no link at all rather than one
        // that leads nowhere.
        traceId: row.trace_id || '',
        // Why the turn ended when it was not the model's own answer ("steps",
        // "tokens" or "deadline"). Persisted, so a reloaded conversation still
        // shows that the answer is what fitted in the budget.
        stopReason: row.stop_reason || '',
        notices: [],
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

/**
 * The model catalog, normalised once.
 *
 * chatStore holds whatever GET /api/chat/models answered; every view reads the
 * shaped form from here so 对话 and 设置 cannot disagree about grouping, naming or
 * the "no API key" marking (the shaping itself lives in llm.js, shared with the
 * 设置 panels).
 */
export const catalog = computed(() => catalogView(chat.catalog))

/** Per-provider summaries (counts, freshness, last error) from the same response. */
export const catalogProviders = computed(() => catalog.value.providers)

/** Catalog entries grouped by provider, for the <optgroup> select. */
export const modelGroups = computed(() =>
  catalog.value.groups.map((group) => ({
    provider: group.provider,
    providerName: group.providerName,
    models: group.models,
  })),
)

export const defaultModel = computed(
  () => catalog.value.models.find((entry) => entry.isDefault) || catalog.value.models[0] || null,
)

export const maxSteps = computed(() => {
  const value = chat.catalog && chat.catalog.max_steps
  const n = numberOrNull(value)
  return n && n > 0 ? n : null
})

// ------------------------------------------------------ model selector --

/**
 * Select options grouped by provider, for the composer's model picker.
 *
 * The value is the option's own key rather than a "provider/model" string,
 * because neither part is guaranteed to be free of the separator.
 *
 * A model whose provider has no API key is listed but disabled, with the reason
 * in its label: it cannot work yet, and 设置 is where the key is added. Every
 * model the catalog offers is listed — including the ones a provider keeps even
 * though none of its models declares 对话 — because that list already reflects
 * the server's filtering (enabled providers, enabled models).
 */
export const modelOptionGroups = computed(() => {
  const groups = modelGroups.value.map((group) => ({
    provider: group.provider,
    label: group.providerName,
    options: [],
  }))
  const byProvider = new Map(groups.map((group) => [group.provider, group]))
  const flat = []

  for (const group of modelGroups.value) {
    const bucket = byProvider.get(group.provider)
    for (const entry of group.models) {
      const option = {
        key: `catalog-${flat.length}`,
        provider: entry.provider,
        model: entry.model,
        providerName: entry.providerName,
        label: modelOptionLabel(entry),
        hint: modelOptionHint(entry),
        disabled: !entry.hasKey,
      }
      bucket.options.push(option)
      flat.push(option)
    }
  }

  const current = chat.session
  if (current && !flat.some((o) => o.provider === current.provider && o.model === current.model)) {
    // Never leave the picker blank: the session may point at a model that is no
    // longer offered (or the catalog may be empty).
    const option = {
      key: 'current',
      provider: current.provider || '',
      model: current.model || '',
      providerName: current.provider || '—',
      label: `${current.model || '—'}（不在目录中）`,
      hint: '该会话使用的模型不在当前模型目录里：可能已被停用或删除',
      disabled: false,
    }
    groups.unshift({ provider: '当前会话', label: '当前会话', options: [option] })
    flat.unshift(option)
  }
  return groups
})

export const modelOptions = computed(() =>
  modelOptionGroups.value.flatMap((group) => group.options),
)

/** Key of the option matching the active session, or '' when there is none. */
export const selectedModelKey = computed(() => {
  const current = chat.session
  if (!current) return ''
  const found = modelOptions.value.find(
    (o) => o.provider === current.provider && o.model === current.model,
  )
  return found ? found.key : ''
})

/** The catalog entry for the active session's model, when it is known. */
export const currentCatalogEntry = computed(() => {
  const current = chat.session
  if (!current) return null
  return (
    modelGroups.value
      .flatMap((group) => group.models)
      .find((entry) => entry.provider === current.provider && entry.model === current.model) || null
  )
})

/** Select handler for the composer's picker. */
export function changeModel(key) {
  const option = modelOptions.value.find((o) => o.key === key)
  if (!option) return
  const current = chat.session
  if (current && option.provider === current.provider && option.model === current.model) return
  setModel(option.provider, option.model)
}

/**
 * Providers whose model list is stale, from the catalog the server sent.
 *
 * Exported so 设置 can decide whether opening it should trigger a refresh
 * without re-deriving staleness from a second source.
 */
export const staleCatalogProviders = computed(() => staleProviders(chat.catalog))

/** Per-provider catalog stats, keyed by provider id (counts, freshness, error). */
export const catalogProviderStats = computed(() => providerStatsById(chat.catalog))

/**
 * Providers this page load has already tried to refresh automatically.
 *
 * It lives here rather than in the panel so it survives leaving and re-entering
 * 设置: the automatic pass fires once per provider per page load, however many
 * times the panel is mounted. A reload starts fresh, which is what a user
 * reopening the console expects.
 */
const autoRefreshClaimed = new Set()

/**
 * Claim the stale providers an automatic refresh should cover, or an empty array
 * when there is nothing new to do.
 *
 * Claiming is what makes the automatic pass idempotent: the callers mark the
 * providers as handled *before* the request, so a provider that fails (and stays
 * stale) is never retried in a loop.
 */
export function claimAutoRefresh() {
  const pending = []
  for (const provider of staleProviders(chat.catalog)) {
    if (autoRefreshClaimed.has(provider.id)) continue
    autoRefreshClaimed.add(provider.id)
    pending.push(provider)
  }
  return pending
}

// ------------------------------------------------------------------ loading --

export async function loadCatalog({ quiet = false } = {}) {
  if (!quiet && !chat.catalog) chat.catalogStatus = 'loading'
  try {
    const res = await api.chatModels()
    chat.catalog = res && typeof res === 'object' ? res : { models: [], providers: [], tools: [] }
    chat.catalogError = ''
    chat.catalogStatus = 'ready'
  } catch (err) {
    if (err && err.status === 401) return
    chat.catalogError = errorText(err, '无法读取模型目录')
    if (!chat.catalog) chat.catalogStatus = 'error'
  }
}

/**
 * Refresh every enabled provider's model list (POST /api/llm/models/refresh-all)
 * and reload the catalog it changes.
 *
 * The reload is what makes a refresh visible in 对话 as well as in 设置: the two
 * surfaces read the same endpoint, so refetching the list once updates both.
 *
 * Returns the per-provider results — `{provider_id, ok, models_count, error}` —
 * so the caller can report successes and failures; a provider that failed is
 * part of the report, not an exception, so a partial pass still resolves.
 */
export async function refreshAllModels() {
  chat.actionError = ''
  const res = await api.refreshAllLlmModels()
  const results = res && Array.isArray(res.results) ? res.results : []
  // Both surfaces read the catalog, so one reload updates both.
  await loadCatalog({ quiet: true })
  return results
}

export async function loadSessions({ quiet = false, select = true } = {}) {
  if (!quiet && !chat.sessions.length) chat.sessionsStatus = 'loading'
  try {
    const res = await api.chatSessions(SESSION_LIMIT)
    const list = res && Array.isArray(res.sessions) ? res.sessions : []
    chat.sessions = list
    chat.sessionsError = ''
    chat.sessionsStatus = 'ready'
    // The sidebar groups these by workspace, so the folder data has to be
    // current whenever the list is: a conversation filed under a workspace the
    // page has not heard of would otherwise be invisible.
    if (!chat.workspaces.length) await loadWorkspaces({ quiet: true })

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

/** Boot the view: catalog + sessions, then the newest conversation. */
export async function ensureLoaded() {
  if (!chat.catalog) await loadCatalog()
  // The sidebar shows the session list on every view, so the first caller wins
  // and later calls must not re-select a conversation over the user's choice.
  if (chat.booted) return
  chat.booted = true
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
    if (!chat.session || chat.session.id !== id) {
      chat.session = null
      chat.workspace = null
    }
    chat.messagesStatus = 'loading'
    chat.messagesError = ''
  }
  try {
    const res = await api.chatSession(id)
    if (chat.activeId !== id) return // a newer selection won
    chat.session = res && res.session ? res.session : null
    // The workspace travels with the conversation: a reload must show the one
    // this conversation is in, not the one the last conversation used.
    chat.workspace = (res && res.workspace) || null
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

/**
 * Create a conversation.
 *
 * `workspace` files it under that workspace immediately (the sidebar shows it
 * there before the first message); omitted, it starts in the workspace the last
 * conversation used, which is what the sidebar's own 新建对话 button relies on.
 */
export async function createSession({ workspace = '' } = {}) {
  if (chat.creating) return null
  chat.creating = true
  chat.actionError = ''
  try {
    // No provider/model: the server decides, preferring the model the last
    // conversation used and falling back to the catalog default. Sending the
    // client's default here would override that and snap every new
    // conversation back to the default.
    const res = await api.createChatSession({ title: '', workspace })
    const session = res && res.session ? res.session : null
    if (session && session.id) {
      replaceSession(session)
      chat.sessions = chat.sessions.slice().sort(byRecency)
      chat.activeId = session.id
      chat.session = session
      chat.workspace = null
      chat.items = []
      chat.messagesError = ''
      chat.messagesStatus = 'ready'
      // The folder counts changed, and the grouping depends on them.
      await loadWorkspaces({ quiet: true })
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
 * `content` may be empty when `attachments` carries at least one uploaded asset
 * — an image with no caption is a normal message. The attachment records are
 * the ones the upload route returned, so the ids are already real and only the
 * ids travel in the request body.
 *
 * On `done` the server's text replaces what was streamed (it is authoritative:
 * the ReAct loop may have produced a preamble before calling a tool). On
 * `error` the failure becomes a warning bubble in the conversation. When the
 * stream ends — normally, on 停止, or on a dropped connection — the session is
 * reloaded from the server, which is the only place with the persisted ids,
 * tool results and summed usage.
 */
export async function sendMessage(rawContent, { attachments = [] } = {}) {
  const content = String(rawContent === null || rawContent === undefined ? '' : rawContent).trim()
  const files = (Array.isArray(attachments) ? attachments : []).filter((a) => a && a.id)
  if (content === '' && files.length === 0) return
  if (chat.streaming) return
  if (!chat.activeId) {
    chat.actionError = '请先选择或新建一个对话'
    return
  }

  const sessionId = chat.activeId
  chat.actionError = ''
  chat.pendingContent = ''
  chat.pendingAttachments = []
  chat.notice = ''

  // The live items are wrapped in `reactive` explicitly: the stream appends to
  // their fields after they were pushed, and mutating a raw object stored in a
  // reactive array would not trigger a re-render.
  const userItem = reactive({
    key: nextKey('user-live'),
    role: 'user',
    text: content,
    attachments: normalizeAttachments(files),
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
    // Cards the model raises while this turn runs, in the order it asks them.
    asks: [],
    usage: null,
    error: '',
    // Notices are the turn's own remarks about itself — a condensed window, a
    // budget that ran out. They are kept apart from `text` because the answer
    // is the model's words and these are the runner's.
    notices: [],
    stopReason: '',
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
      attachments: files.map((file) => file.id),
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
      // and offer the text (and the already-uploaded attachments) again in the
      // composer.
      chat.items = chat.items.filter((item) => item !== userItem && item !== turn)
      chat.pendingContent = content
      chat.pendingAttachments = files
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

/**
 * One line explaining why a turn stopped on its budget.
 *
 * The server already appends an explanation to the answer text; this is the
 * same fact as a rendered notice, so it can be styled apart from the model's
 * own words and so a reader who scrolled past the text still sees it.
 */
function budgetStopText(event) {
  const reason = typeof event.reason === 'string' ? event.reason : ''
  const steps = numberOrNull(event.step)
  const tokens = numberOrNull(event.tokens)
  const elapsed = numberOrNull(event.elapsed_ms)

  let headline = '本轮已达到步数上限'
  if (reason === 'tokens') headline = '本轮已达到 token 预算'
  else if (reason === 'deadline') headline = '本轮已达到时间上限'

  const parts = [headline]
  if (steps) parts.push(`共 ${steps} 步`)
  if (tokens) parts.push(`已用 ${formatCompact(tokens)} tokens`)
  if (elapsed !== null) parts.push(`耗时 ${formatDuration(elapsed)}`)
  parts.push('工作区改动已落盘，回复「继续」可接着做')
  return parts.join(' · ')
}

/** Stop the running turn. The server still persists what it produced. */
export function stopStreaming() {
  if (!controller) return
  const pending = controller
  controller = null
  setNotice('已停止生成')
  pending.abort()
}

/** The card with this id, wherever it currently lives in the conversation. */
function findAsk(id) {
  for (let i = chat.items.length - 1; i >= 0; i--) {
    const item = chat.items[i]
    const asks = item && Array.isArray(item.asks) ? item.asks : []
    const found = asks.find((card) => card && card.id === id)
    if (found) return found
  }
  return null
}

/**
 * Submit one card's answer.
 *
 * The answer travels on its own request (the turn's response is busy streaming),
 * so this is a plain POST whose success means the waiting tool has been woken
 * up. The card is settled from what was submitted immediately, because the model
 * may take a while to use it — and it is settled *only* after the POST returns,
 * so a failure leaves the choices editable rather than pretending they were sent.
 */
export async function submitAsk(askId, { selected = [], text = '' } = {}) {
  const sessionId = chat.activeId
  const card = findAsk(askId)
  if (!card || !sessionId) return
  // Already answered (another tab, or a double click): the first submission wins.
  if (card.status !== ASK_PENDING) return

  const picked = Array.isArray(selected) ? [...selected] : []
  const typed = typeof text === 'string' ? text.trim() : ''
  card.status = ASK_SUBMITTING
  card.error = ''
  chat.streamTick += 1

  try {
    await api.answerQuestion(sessionId, askId, { selected: picked, text: typed })
    card.selected = picked
    card.text = typed
    card.status = ASK_ANSWERED
  } catch (err) {
    if (err && err.status === 401) return
    card.error = errorText(err, '提交失败，请重试')
    // 404 means the question is no longer waiting — answered elsewhere, timed
    // out, or the turn is over. Offering the card again would only fail again.
    card.status = err && err.status === 404 ? ASK_CANCELLED : ASK_PENDING
  }
  chat.streamTick += 1
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
      // ask_user is not rendered as a tool row: the case it belongs to is the
      // card, and pushing it here would show the same exchange twice — and, for
      // the moment between the call and the question, as a card nobody answered.
      if (isAskTool(event.tool_name)) return true
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
      if (isAskTool(event.tool_name)) return true
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
      // A tool that manages background processes just ran, so the count in this
      // conversation's header is now stale. This is the only signal that a job
      // appeared while nothing was running to poll for: the store's interval
      // exists only while something is alive.
      noteJobsToolRan(tool.name)
      return true
    }

    // A question the model is waiting on, or the outcome of one that was already
    // on screen. Both shapes share the event type; `ask` marks the announcement.
    case 'ask_user': {
      const card = askFromEvent(event)
      if (card) {
        turn.asks.push(card)
        return true
      }
      const id = typeof event.ask_id === 'string' ? event.ask_id : ''
      const existing = id ? turn.asks.find((item) => item.id === id) : null
      if (existing) {
        applyAskOutcome(existing, event)
        // The server's word replaces the local one, including when it settles a
        // card the user never managed to answer.
        existing.error = ''
      }
      return true
    }

    case 'usage':
      turn.usage = addUsage(turn.usage, event.usage)
      return true

    // The window was condensed to stay inside the token budget. It is a notice
    // rather than a warning: the turn carries on, with a smaller window.
    case 'context_compressed':
      turn.notices.push({
        kind: 'context',
        step: numberOrNull(event.step),
        text: typeof event.text === 'string' ? event.text : '上下文已压缩',
      })
      return true

    // The turn stopped because a budget ran out, not because the model was
    // done. Recorded on the turn (and rendered as its own line) so the answer
    // is never mistaken for a complete one.
    case 'budget_stop':
      turn.stopReason = typeof event.reason === 'string' ? event.reason : 'budget'
      turn.notices.push({
        kind: 'budget',
        step: numberOrNull(event.step),
        tokens: numberOrNull(event.tokens),
        elapsedMs: numberOrNull(event.elapsed_ms),
        text: budgetStopText(event),
      })
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

/** Dismiss the action-error banner (and forget the pending retry). */
export function dismissActionError() {
  chat.actionError = ''
  chat.pendingContent = ''
  chat.pendingAttachments = []
}

// ------------------------------------------------------- workspace (per conversation) --

/**
 * Load the workspaces and the sidebar's grouping data.
 *
 * One request answers the whole sidebar: the workspaces with their conversation
 * counts, which one a new conversation would start in, and the home directory
 * the picker opens at. A failure is recorded rather than thrown — a deployment
 * without the workspace layer answers 404, and that must leave the conversation
 * usable rather than putting an error banner over a working chat.
 */
export async function loadWorkspaces({ quiet = false } = {}) {
  if (!quiet) chat.workspacesStatus = 'loading'
  try {
    const res = await api.workspaces()
    chat.workspaces = (res && res.workspaces) || []
    chat.defaultWorkspace = (res && res.default) || ''
    chat.homeDir = (res && res.home) || ''
    chat.workspacesNote = (res && res.note) || ''
    chat.workspaceWarnings = (res && res.warnings) || []
    chat.workspacesError = ''
    chat.workspacesStatus = 'ready'
    return true
  } catch (err) {
    if (err && err.status === 401) return false
    if (err && err.status === 404) {
      // No workspace layer on this server: a state the sidebar renders as a
      // plain list, not an error to complain about.
      chat.workspacesAvailable = false
      chat.workspaces = []
      chat.workspacesStatus = 'ready'
      return false
    }
    chat.workspacesError = errorText(err, '无法读取工作区列表')
    chat.workspaces = []
    chat.workspacesStatus = 'ready'
    return false
  }
}

/**
 * The sidebar's folders: every workspace, with the conversations that belong to
 * it, in the order the server sent them.
 *
 * A conversation whose workspace is unknown (deleted while the page was open)
 * is not dropped: it appears under a final "未归类" folder, because losing a
 * conversation from the list is worse than showing it in the wrong place.
 */
export const workspaceGroups = computed(() => {
  if (!chat.workspacesAvailable) {
    // One group with no workspace: the sidebar renders its sessions without a
    // folder head, which is exactly the flat list this console had before
    // workspaces existed.
    return [{ workspace: null, sessions: chat.sessions, flat: true }]
  }
  const groups = chat.workspaces.map((ws) => ({
    workspace: ws,
    sessions: chat.sessions.filter((session) => session.workspace === ws.name),
  }))
  const known = new Set(chat.workspaces.map((ws) => ws.name))
  const orphans = chat.sessions.filter((session) => !known.has(session.workspace))
  if (orphans.length) {
    groups.push({
      workspace: { name: '未归类', root: '', session_count: orphans.length, missing: true },
      sessions: orphans,
      orphan: true,
    })
  }
  return groups
})

/** Fold or unfold a workspace folder. Local to this browser, like the theme. */
export function toggleWorkspaceFold(name) {
  chat.collapsedWorkspaces = {
    ...chat.collapsedWorkspaces,
    [name]: !chat.collapsedWorkspaces[name],
  }
}

function workspaceErrorMessage(err, fallback) {
  if (err && err.status === 0) return errorText(err, fallback)
  return errorText(err, fallback)
}

/**
 * Create a workspace from a directory the user picked.
 *
 * The name is optional: the server defaults it to the directory's base name,
 * which is almost always what a person wants and is what the form pre-fills.
 */
export async function createWorkspace({ root, name }) {
  chat.pendingWorkspace = root || 'new'
  chat.actionError = ''
  try {
    const res = await api.createWorkspace({ root, name })
    if (res && res.ok === false) {
      chat.actionError = res.error || '创建工作区失败'
      return null
    }
    await Promise.all([loadWorkspaces({ quiet: true }), loadSessions({ quiet: true })])
    const created = res && res.workspace
    if (created && created.name) setNotice(`工作区已创建：${created.name}`)
    return created || null
  } catch (err) {
    if (!err || err.status !== 401) chat.actionError = workspaceErrorMessage(err, '创建工作区失败')
    return null
  } finally {
    chat.pendingWorkspace = ''
  }
}

/** Rename a workspace's label. The directory is untouched. */
export async function renameWorkspace(from, to) {
  const name = (to || '').trim()
  if (!name || name === from) return false
  chat.pendingWorkspace = from
  chat.actionError = ''
  try {
    const res = await api.renameWorkspace(from, name)
    if (res && res.ok === false) {
      chat.actionError = res.error || '重命名失败'
      return false
    }
    await Promise.all([loadWorkspaces({ quiet: true }), loadSessions({ quiet: true })])
    // The open conversation may be in the renamed workspace, so its copy of the
    // workspace is refreshed with it. Nothing renders that copy today — the
    // sidebar draws the folders from chat.workspaces — but a stale one would be
    // the next reader's bug.
    if (chat.workspace && chat.workspace.name === from && res && res.workspace) {
      chat.workspace = res.workspace
    }
    setNotice(`工作区已重命名为 ${name}`)
    return true
  } catch (err) {
    if (!err || err.status !== 401) chat.actionError = workspaceErrorMessage(err, '重命名失败')
    return false
  } finally {
    chat.pendingWorkspace = ''
  }
}

/**
 * Delete a workspace.
 *
 * The confirmation is the caller's job (it is destructive-looking even though it
 * never deletes files). The answer's note is surfaced, because what a person
 * needs to know afterwards is where their conversations went.
 */
export async function deleteWorkspace(name) {
  chat.pendingWorkspace = name
  chat.actionError = ''
  try {
    const res = await api.deleteWorkspace(name)
    if (res && res.ok === false) {
      chat.actionError = res.error || '删除工作区失败'
      return false
    }
    await Promise.all([loadWorkspaces({ quiet: true }), loadSessions({ quiet: true })])
    setNotice(res && res.moved_to ? `工作区已删除，对话已移动到 ${res.moved_to}` : '工作区已删除')
    return true
  } catch (err) {
    if (!err || err.status !== 401) chat.actionError = workspaceErrorMessage(err, '删除工作区失败')
    return false
  } finally {
    chat.pendingWorkspace = ''
  }
}

// Pointing an existing conversation at another workspace is deliberately gone
// from the console: the header picker was removed because the sidebar already
// says which workspace a conversation is in, and nothing else offered the move.
// A conversation is placed in a workspace when it is created — the + on a
// workspace folder — and stays there. The server route it used to call
// (PUT /api/chat/sessions/:id/workspace) still exists for a script that needs
// it; the console has no control for it.

