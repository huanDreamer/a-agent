// Chat state and turn streaming for the 对话 tab.
//
// It lives outside the component on purpose: a half-finished stream must
// survive switching to another tab and back, and the views stay a pure render
// of this store. Everything in here mirrors the API contract of
// internal/server/chat.go.

import { computed, reactive } from 'vue'
import { api, attachChatTurn, startChatTurn, stopChatTurn } from './api.js'
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
import { applyApprovalOutcome, approvalFromEvent } from './approval.js'
import { normalize as normalizeAttachments } from './attachments.js'
import { normalizePlan, planResumable } from './plan.js'
import { normalizeStats } from './sessionStats.js'
import { formatCompact, formatDuration } from './format.js'
import { noteJobsToolRan } from './jobsStore.js'
import { asText, normalizeSteps, normalizeTool, normalizeTools } from './steps.js'
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
  /**
   * The conversation's aggregated numbers (turns / model calls / tool calls /
   * tokens), as the server reports them on GET /api/chat/sessions/{id}.
   *
   * They are read from the store rather than counted in the browser so the header
   * survives a reload and cannot disagree between two clients. They are refreshed
   * by the same authoritative reload a finished turn already triggers.
   */
  stats: null,
  /**
   * 模型为这一轮建立的任务计划，归一化后的形状（见 plan.js）；null 表示没有计划。
   *
   * 它属于会话而不是某一轮的消息：计划跨越多轮（中断后续跑还是同一份），所以
   * 看板读的是这里，而不是从消息里推出来的东西。
   */
  plan: null,
  /** 看板是否被折叠。跟着当前浏览的会话走，与主题一样只存在本浏览器里。 */
  planCollapsed: false,
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
export { asText }

/**
 * 从累计字段的尾部回退掉一段文字。
 *
 * 流式增量是同时追加到"这一步"和"整轮"两个字段上的，所以被丢弃的那次调用留下
 * 的内容正好是累计值的尾部；不是尾部（状态被别的事件改过）就原样返回——猜着删
 * 比多留一段更糟。
 */
function rollback(total, part) {
  if (!part) return total
  return total.endsWith(part) ? total.slice(0, total.length - part.length) : total
}

/**
 * 一行重试说明。`attempt` 是**本次即将进行的尝试**的序号，所以第 2 次尝试读作
 * "第 2/3 次尝试"；原因带上，否则用户只知道重试了、不知道在等什么。
 */
function retryNoticeText(event) {
  const attempt = numberOrNull(event.attempt)
  const max = numberOrNull(event.max_attempts)
  const delay = numberOrNull(event.delay_ms)
  const reason = typeof event.error === 'string' && event.error !== '' ? event.error : ''

  let head = '正在重试'
  if (attempt !== null && max !== null && max > 0) head = `第 ${attempt}/${max} 次尝试`
  else if (attempt !== null && attempt > 0) head = `第 ${attempt} 次尝试`

  // delay_ms 为 0 或缺失是"立刻重试"：写"0ms 后重试"比不写更让人困惑。
  const wait = delay !== null && delay > 0 ? `${formatDuration(delay)} 后重试` : '立即重试'
  return reason ? `${head}，${wait}（${reason}）` : `${head}，${wait}`
}

/**
 * The one-line explanation of a stop that was not the model's own answer.
 *
 * `reason` is the server's vocabulary (see internal/chat/budget.go). Losing one
 * of these branches is not cosmetic: a reason the page does not know was
 * reported as "步数上限", which told the reader the wrong thing about a turn the
 * loop guard stopped after five identical calls.
 */
function budgetStopText(event) {
  const reason = typeof event.reason === 'string' ? event.reason : ''
  if (reason === 'tokens') return '本轮达到 token 预算，回答可能不完整 · 回复「继续」可接着做'
  if (reason === 'deadline') return '本轮达到时间上限，回答可能不完整 · 回复「继续」可接着做'
  if (reason === 'loop') return '本轮在重复调用同一个工具，已被提前结束 · 回复「继续」可以让它换个做法'
  if (reason === 'idle') return '本轮连续多步只查看、没有推进，已被提前结束 · 回复「继续」并指明要改哪里'
  return '本轮达到步数上限，回答可能不完整 · 回复「继续」可接着做'
}

/** One line about a steering message: what the model was told, and about what. */
function steerNoticeText(event) {
  const kind = typeof event.steer_kind === 'string' ? event.steer_kind : ''
  if (kind === 'repeat') return '同一个工具调用重复了，已提醒模型换个做法'
  if (kind === 'reread') return '反复读同一个文件，已提醒模型只取需要的部分'
  if (kind === 'idle') return '连续多步只查看没有动手，已提醒模型开始推进'
  return '已提醒模型调整做法'
}

// ------------------------------------------------------- render item model --

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
      const tools = normalizeTools(row.tool_calls, true)
      items.push({
        key: `assistant-${row.id ?? nextKey('assistant')}`,
        role: 'assistant',
        text: row.content || '',
        reasoning: row.reasoning || '',
        // The turn's process, step by step: each step's thinking next to the tool
        // calls it asked for. This is what the bubble renders — the flat `tools`
        // list and the `reasoning` blob below are what it falls back from for a
        // message written before steps were stored.
        steps: normalizeSteps(row.steps, { tools, reasoning: row.reasoning || '' }),
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
      const tool = normalizeTool({ id, name: row.tool_name, result: row.content }, last.tools.length, true)
      last.tools.push(tool)
      // The step list is a second view of the same calls, so a folded-in result
      // has to reach it too — otherwise the card under a step would show a call
      // with no output while the flat list has one.
      const step = last.steps.length ? last.steps[last.steps.length - 1] : null
      if (step) step.tools.push(tool)
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

// ------------------------------------------------------- turn budget ----

/**
 * The per-turn budget (设置 → 对话预算).
 *
 * It is loaded from GET /api/chat/budget rather than derived from the catalog's
 * max_steps, because the panel needs more than the effective number: which
 * dimension the console overrode, what 恢复默认 would restore, and whether
 * in-turn compression is off (the half of "run longer" that a console change
 * cannot fix).
 */
export const budget = reactive({
  snapshot: null,
  status: 'loading', // loading | error | ready
  error: '',
  saving: false,
})

/** loadBudget reads the effective budget and its defaults. */
export async function loadBudget({ quiet = false } = {}) {
  if (!quiet && !budget.snapshot) budget.status = 'loading'
  try {
    budget.snapshot = await api.chatBudget()
    budget.error = ''
    budget.status = 'ready'
  } catch (err) {
    if (err && err.status === 401) return
    budget.error = errorText(err, '无法读取对话预算')
    if (!budget.snapshot) budget.status = 'error'
  }
}

/**
 * saveBudget writes the fields the panel filled in and adopts the server's
 * answer, so the panel renders what was accepted rather than what was asked for.
 * A validation failure on the server (a 400) surfaces as this function throwing
 * with the server's own message, which the panel shows as-is.
 */
export async function saveBudget(body) {
  budget.saving = true
  try {
    budget.snapshot = await api.saveChatBudget(body)
    budget.error = ''
    budget.status = 'ready'
    // The composer and 模型 panel read max_steps from the catalog, so refetch it:
    // otherwise the new ceiling would live only in this panel and the page would
    // show two different numbers.
    await loadCatalog({ quiet: true })
    return budget.snapshot
  } finally {
    budget.saving = false
  }
}

/** resetBudget drops every override and hands the three dimensions back. */
export function resetBudget() {
  return saveBudget({ reset_all: true })
}

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

/**
 * Read the session list (GET /api/chat/sessions), select the newest one, and
 * report whether the list was actually read.
 *
 * The boolean is what lets ensureLoaded tell "loaded" from "was refused": a 401
 * (no session yet, or one that expired) leaves the store as empty as a fresh
 * boot, and a caller that treated that as loaded would never try again.
 */
export async function loadSessions({ quiet = false, select = true } = {}) {
  if (!quiet && !chat.sessions.length) chat.sessionsStatus = 'loading'
  try {
    const res = await api.chatSessions(SESSION_LIMIT)
    const list = res && Array.isArray(res.sessions) ? res.sessions : []
    chat.sessions = list
    chat.sessionsError = ''
    chat.sessionsStatus = 'ready'
    // The list is where "is anything generating" comes from, so the poll that
    // keeps that answer fresh is started and stopped from the same place.
    syncStreamPoll()
    // The sidebar groups these by workspace, so the folder data has to be
    // current whenever the list is: a conversation filed under a workspace the
    // page has not heard of would otherwise be invisible.
    if (!chat.workspaces.length) await loadWorkspaces({ quiet: true })

    if (!select) return true
    const active = list.find((s) => s && s.id === chat.activeId)
    if (active) {
      // Keep the header (model, title, count) in sync with server state.
      if (chat.session) chat.session = { ...chat.session, ...active }
      return true
    }
    const newest = list[0]
    if (newest && newest.id) await selectSession(newest.id)
    else resetSelection()
    return true
  } catch (err) {
    if (err && err.status === 401) return false
    chat.sessionsError = errorText(err, '无法读取会话列表')
    if (!chat.sessions.length) chat.sessionsStatus = 'error'
    return false
  }
}

/** The in-flight ensureLoaded, so concurrent callers share one load. */
let booting = null

/**
 * Boot the view: catalog + sessions, then the newest conversation.
 *
 * `booted` is set **after** the list was really read, not before the request:
 * the request fails on a deployment that requires a password and on a server
 * that is not up, and in both cases the next call must be allowed to try again.
 * Setting it up front is what made the console load nothing until a full page
 * reload — the shell's probe-less first attempt consumed the one shot, the login
 * that followed found `booted` already true, and the session and workspace lists
 * stayed empty (they are only read from loadSessions). It also silently disabled
 * every 重试 button in the failed states, which call this again.
 */
export async function ensureLoaded() {
  // The two requests are independent, and booted-vs-catalog can disagree: a boot
  // whose session list was read but whose catalog request failed is `booted` with
  // no catalog, which is exactly the state the 模型目录 banner reports. Its 重试
  // button calls this function, so the guard has to let that case through instead
  // of returning on `booted` alone.
  if (chat.booted && chat.catalog) return
  // The shell, the chat view and the tab watcher all ask for this on mount; the
  // first caller starts the load and the rest await it instead of firing a
  // second round of requests.
  if (booting) return booting
  booting = (async () => {
    if (!chat.catalog) await loadCatalog()
    if (chat.booted) return
    // Only a list that was actually read counts as booted; a 401 or a transport
    // failure leaves this false so the caller can retry once the session exists.
    if (await loadSessions()) chat.booted = true
  })()
  try {
    await booting
  } finally {
    booting = null
  }
}

function resetSelection() {
  chat.activeId = ''
  chat.session = null
  chat.items = []
  // 没有会话就没有计划：留着上一个会话的清单会让它挂在新标题下面。
  chat.plan = null
  chat.messagesError = ''
  chat.messagesStatus = 'ready'
}

export async function selectSession(id, { quiet = false } = {}) {
  if (!id) return
  // Stop *reading* the previous conversation's turn, never the turn itself: it
  // belongs to that conversation, and a reader who comes back must find it still
  // running rather than cut short by their own click.
  detachTurn()
  chat.activeId = id
  chat.actionError = ''
  if (!quiet) {
    // Never show the previous conversation's bubbles under a new title.
    chat.items = []
    // 同理：上一个会话的计划不能留在新会话的看板上。
    chat.plan = null
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
    chat.stats = normalizeStats(res && res.stats)
    // 计划随会话一起读：它是服务端那份的唯一副本，看板不自己攒。
    chat.plan = normalizePlan(res && res.plan)
    chat.messagesError = ''
    chat.messagesStatus = 'ready'
    // A turn may be running for this conversation: started here before the
    // reader left, started in another tab, or still going after a reload. This
    // is the line that makes an unfinished answer visible again — the stored
    // messages above are only what has been written down so far.
    if (chat.session && chat.session.streaming) {
      attachTurn(id)
    }
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
      chat.stats = normalizeStats(null)
      // 新会话还没有任何一轮，所以它不可能有计划：上一个会话的清单留在这里，
      // 看板会挂在一个空对话的输入框上方，看起来像"这个新对话有一堆活没干"。
      // 位置在成功分支里（不在请求之前）：新建失败时人还留在原会话，那份计划
      // 是他的「继续执行」接续点，不能因为一次失败的创建就丢掉。
      chat.plan = null
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
      chat.stats = normalizeStats(null)
      // 清空连同计划一起（服务端也这么删）：看得见的历史没了，看板却还挂着
      // 一份没人能对上的清单，只会让人以为清空没生效。
      chat.plan = null
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

// ------------------------------------------------------------- 任务看板 --

/**
 * 折叠 / 展开看板。
 *
 * 状态放在 store 里而不是组件里：面板会在切标签页时被卸载重挂，一个"折叠起来
 * 好继续干活"的选择不该因为去看了一眼设置就没了。
 */
export function togglePlanCollapsed() {
  chat.planCollapsed = !chat.planCollapsed
}

// --------------------------------------------------------------- the stream --

/**
 * The turn this tab is reading, or null.
 *
 * The reader's interest in a turn is not the view's: this handle lives at module
 * scope, so leaving 对话 for 设置, or opening another conversation, does not end
 * it — and a reload does not lose it either, because the turn is on the server
 * and this handle can simply be built again from it. What it takes to show an
 * unfinished answer is the ability to *attach* to the turn, which is what
 * `attachTurn` does.
 */
let attachment = null

/**
 * The bubble the last attachment drew, so attaching again to the same turn
 * continues it instead of drawing it twice.
 *
 * A re-attach replays the turn from its first event — that is what makes a
 * reload work — so an existing bubble has to be emptied first, or the answer
 * would appear twice over. It is forgotten as soon as the conversation is
 * reloaded, which is what happens whenever the reader leaves and comes back.
 */
let liveItem = null

/** A turn waiting to be filled in by events, as the reader sees it. */
function newLiveTurn() {
  // Wrapped in `reactive` explicitly: events mutate these fields after the item
  // was pushed, and mutating a raw object stored in a reactive array would not
  // trigger a re-render.
  return reactive({
    key: nextKey('turn'),
    role: 'assistant',
    text: '',
    reasoning: '',
    // The turn's process as it happens: one entry per step, each holding that
    // step's thinking, what it said, and the tool cards it asked for. Events are
    // routed here by their `step`, which is what keeps a thought next to the call
    // it prompted instead of in one blob at the end.
    steps: [],
    tools: [],
    // Cards the model raises while this turn runs, in the order it asks them.
    asks: [],
    // Write and exec requests the turn is blocked on, in the order they arrived.
    // They are live-only (see approval.js), so this list starts empty on a reload
    // and is never rebuilt from the stored conversation.
    approvals: [],
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
}

/**
 * Empty a live turn back to "nothing has arrived yet".
 *
 * It is what makes attaching idempotent: the server always replays a turn from
 * its first event, so a bubble that already holds the answer has to be cleared
 * before the replay fills it in again.
 */
function resetLiveTurn(turn) {
  turn.text = ''
  turn.reasoning = ''
  turn.steps = []
  turn.tools = []
  turn.asks = []
  turn.notices = []
  turn.usage = null
  turn.error = ''
  turn.stopReason = ''
  turn.streaming = true
}

/**
 * The step the next event belongs to, opening it when the stream moves on.
 *
 * The stream's `step` is authoritative: a `step_start` opens the step, and any
 * event that arrives for it (or without one, when an older emitter is in play)
 * lands in the newest step rather than inventing one.
 */
function openStep(turn, step) {
  const n = numberOrNull(step)
  const current = turn.steps.length ? turn.steps[turn.steps.length - 1] : null
  if (!current || (n !== null && n > current.index)) {
    const next = reactive({
      index: n === null ? (current ? current.index + 1 : 1) : n,
      reasoning: '',
      text: '',
      tools: [],
      // A live step starts open: the point of showing the process is to see it
      // while it runs. It is folded up when the answer lands, unless the reader
      // has taken over this step by clicking it.
      open: true,
      touched: false,
    })
    turn.steps.push(next)
    return next
  }
  return current
}

/** The tool card with this id, wherever in the turn it lives. */
function findTool(turn, id) {
  for (const step of turn.steps) {
    const found = step.tools.find((tool) => tool.id === id)
    if (found) return found
  }
  return turn.tools.find((tool) => tool.id === id) || null
}

/** How many times a dropped attach stream is re-read before giving up. */
const ATTACH_RETRIES = 3
/** Backoff step between those attempts, multiplied by the attempt number. */
const ATTACH_RETRY_MS = 800

/**
 * Whether the pane should treat the conversation as generating.
 *
 * Only the *active* conversation counts: a turn running in another one is shown
 * in the sidebar, and must not disable this conversation's composer.
 */
function syncStreaming() {
  chat.streaming = Boolean(attachment) && attachment.sessionId === chat.activeId
  chat.streamTick += 1
  syncStreamPoll()
}

/**
 * Stop reading the current turn, without stopping the turn.
 *
 * The server has no idea this happened: the run carries on and stores its answer
 * whether or not anyone is reading, which is what makes leaving the page during
 * a long turn safe.
 */
export function detachTurn() {
  if (!attachment) return
  const current = attachment
  attachment = null
  current.controller.abort()
  syncStreaming()
}

/** Whether this tab is currently reading a turn for that conversation. */
export function attachedTo(sessionId) {
  return Boolean(attachment) && attachment.sessionId === sessionId
}

/**
 * Read a conversation's running turn, from its first event to its last.
 *
 * The server replays what already happened and then follows it live, so this is
 * the same call whether the turn was just started here or started minutes ago in
 * another tab — which is exactly why a reload, a page switch and a conversation
 * switch all end the same way: the answer is on screen, including the part of it
 * that has not arrived yet.
 *
 * Resolves `true` when a turn was read to its end, `false` when there was
 * nothing to read or the reader detached first.
 */
export async function attachTurn(sessionId, { show = true } = {}) {
  detachTurn()
  if (!sessionId) return false

  const controller = new AbortController()
  // Continue the bubble this turn already has, when it still has one: a retry or
  // a poll-driven re-attach must not stack a second copy of the same answer.
  const previous = liveItem && liveItem.sessionId === sessionId ? liveItem.item : null
  const reuse = Boolean(previous) && chat.items.includes(previous)
  const turn = reuse ? previous : newLiveTurn()
  const handle = { sessionId, controller, turn }
  attachment = handle
  liveItem = { sessionId, item: turn }

  // Pushed only when the reader is looking at that conversation: a turn started
  // elsewhere must not appear in this transcript, and an unattached turn is
  // still read so its completion can refresh the list.
  const visible = show && chat.activeId === sessionId
  if (visible && !reuse) chat.items.push(turn)
  syncStreaming()

  let idle = false
  let failure = null
  for (let attempt = 0; ; attempt += 1) {
    // Every attempt replays the turn from its first event, so the bubble starts
    // empty each time rather than showing what the failed attempt delivered.
    resetLiveTurn(turn)
    try {
      const outcome = await attachChatTurn(sessionId, {
        signal: controller.signal,
        onEvent: (event) => {
          if (attachment !== handle) return false // superseded: stop reading
          return handleEvent(turn, event)
        },
      })
      if (attachment !== handle) return false
      idle = Boolean(outcome && outcome.idle)
      if (outcome && outcome.aborted) return false
      break
    } catch (err) {
      if (attachment !== handle) return false
      if (err && err.status === 401) return false
      failure = err
      // Reading a running turn is safe to retry: the server replays it from the
      // start, so a dropped connection costs a wait and nothing else. Without
      // this, a flaky moment mid-turn leaves the reader with nothing to do but
      // reload by hand — the very thing this is meant to spare them.
      if (attempt >= ATTACH_RETRIES) break
      await new Promise((resolve) => window.setTimeout(resolve, ATTACH_RETRY_MS * (attempt + 1)))
      if (attachment !== handle) return false
    }
  }

  if (failure) {
    if (turn.text || turn.reasoning || turn.tools.length || turn.steps.length) {
      // The turn produced visible output before the connection died: keep it and
      // mark it, instead of throwing the answer away.
      turn.error = errorText(failure, '连接中断，本轮可能未完整保存')
    } else if (visible) {
      chat.items = chat.items.filter((item) => item !== turn)
    }
  }

  if (attachment !== handle) return false

  attachment = null
  turn.streaming = false
  if (!turn.createdAt) turn.createdAt = new Date().toISOString()
  syncStreaming()

  if (idle) return false

  // The turn is over and stored. Reload it when the reader is still there (the
  // stored row carries the ids, tool results, summed usage and the auto title);
  // when they have moved on, only the sidebar needs updating — dragging the view
  // back to a conversation they left is how an answer looks like it vanished.
  await loadSessions({ quiet: true, select: false })
  if (chat.activeId === sessionId) {
    await selectSession(sessionId, { quiet: true })
  }
  return true
}

/**
 * Send one message.
 *
 * The message is accepted by the server and the turn starts there; the answer is
 * read by attaching to it. Nothing about the turn depends on this call staying
 * open, so the reader is free to leave the page, the conversation or even the
 * browser — attaching again is all it takes to see it.
 *
 * `content` may be empty when `attachments` carries at least one uploaded asset
 * — an image with no caption is a normal message. The attachment records are the
 * ones the upload route returned, so the ids are already real and only the ids
 * travel in the request body.
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

  // 一条新消息＝一件新事：上一轮已经收尾的计划（所有任务都 done / skipped）
  // 属于上一件事，不该继续挂在输入框上方让人以为还有活没干。服务端在新一轮开始
  // 时也会清掉它并广播一个空计划（见 clearFinishedPlan），这里先清是为了让看板
  // 在点击发送的瞬间就消失，而不是等一个来回。
  //
  // 还有未完成项的计划**保留**：它是「继续执行」的接续点，清掉等于把用户还能
  // 接着做的那件事也一起抹了。
  if (!planResumable(chat.plan)) chat.plan = null

  // The user's own message is shown at once: the server is asked to store it a
  // moment later, and waiting for that round trip to draw a bubble the user just
  // typed would only make the composer feel slow.
  const userItem = reactive({
    key: nextKey('user-live'),
    role: 'user',
    text: content,
    attachments: normalizeAttachments(files),
    createdAt: new Date().toISOString(),
    error: '',
  })
  chat.items.push(userItem)
  chat.streamTick += 1

  try {
    await startChatTurn(sessionId, content, { attachments: files.map((file) => file.id) })
  } catch (err) {
    if (err && err.status === 401) return
    chat.items = chat.items.filter((item) => item !== userItem)
    if (err && err.status === 409) {
      // A turn is already running for this conversation — started from another
      // tab, or before this one was reloaded. The message was refused, so it is
      // offered again rather than silently dropped, and the running turn is
      // shown instead.
      chat.pendingContent = content
      chat.pendingAttachments = files
      chat.actionError = '这个对话已有一轮正在生成，先停止它或等它结束'
    } else {
      chat.pendingContent = content
      chat.pendingAttachments = files
      chat.actionError = errorText(err, '发送失败，请重试')
    }
    chat.streamTick += 1
    const running = await sessionIsStreaming(sessionId)
    if (running) attachTurn(sessionId)
    return
  }

  // The list is re-read now that this conversation is generating: the sidebar's
  // 生成中 mark lives there, and so does the poll that keeps it — and every other
  // conversation the reader has open — up to date while the turn runs.
  await loadSessions({ quiet: true, select: false })

  // Fire and forget: the attachment outlives this call by design, and awaiting it
  // would block the composer for as long as the model takes.
  attachTurn(sessionId)
}

/**
 * 接着上一轮的中断处再跑一轮（`POST .../resume`）。
 *
 * 与 sendMessage 是同一套流程，只是端点不同：先用一条本地用户气泡说明发生了什么，
 * 再让服务端开一轮；被拒绝时（409 / 400）撤掉那条气泡并说明原因——界面上绝不能
 * 留下一条其实没有发出去的消息。
 *
 * 与 sendMessage 不同的是它**不动 pendingContent**：那份待发草稿不是这次操作消费
 * 的，清掉只会让失败横幅上的「重试」凭空消失。
 */
export async function resumeTurn() {
  if (chat.streaming) return
  if (!chat.activeId) {
    chat.actionError = '请先选择或新建一个对话'
    return
  }

  const sessionId = chat.activeId
  chat.actionError = ''

  // 气泡立刻出现：它晚一点才在服务端落库没关系，但点了按钮没有任何反应会让人
  // 再点一次——而这一轮的代价是钱。
  const userItem = reactive({
    key: nextKey('user-live'),
    role: 'user',
    text: '继续执行',
    attachments: [],
    createdAt: new Date().toISOString(),
    error: '',
  })
  chat.items.push(userItem)
  chat.streamTick += 1

  try {
    await api.resumeChatTurn(sessionId)
  } catch (err) {
    if (err && err.status === 401) return
    chat.items = chat.items.filter((item) => item !== userItem)
    if (err && err.status === 409) {
      // 已经有一轮在跑（另一个标签页开的，或者刷新前就在跑）。这不是失败，而是
      // "你要等的那一轮就是它"，所以顺手接上去。
      chat.actionError = '这个对话已有一轮正在生成，先停止它或等它结束'
    } else {
      // 400 的正文里就是服务端给出的可读原因（没有可接续的东西之类），直接用。
      chat.actionError = errorText(err, '无法继续执行，请重试')
    }
    chat.streamTick += 1
    const running = await sessionIsStreaming(sessionId)
    if (running) attachTurn(sessionId)
    return
  }

  // 与发消息一样：先刷新列表（侧栏的生成中标记），答案则由 attach 读回来。
  await loadSessions({ quiet: true, select: false })
  attachTurn(sessionId)
}

/**
 * Ask the server whether a conversation has a turn in flight.
 *
 * It is read from the conversation itself rather than from the attachment,
 * because the interesting case is the one this tab knows nothing about: a turn
 * another tab started, or one that was already running before a reload.
 */
async function sessionIsStreaming(sessionId) {
  try {
    const res = await api.chatSession(sessionId)
    const session = res && res.session ? res.session : null
    if (session && chat.session && chat.session.id === sessionId) {
      chat.session = { ...chat.session, ...session }
    }
    return Boolean(session && session.streaming)
  } catch (err) {
    return false
  }
}

/** Stop the running turn. The server keeps what it produced and stores it. */
export async function stopStreaming() {
  const sessionId = chat.activeId
  if (!sessionId) return
  setNotice('已停止生成')
  try {
    await stopChatTurn(sessionId)
  } catch (err) {
    if (err && err.status === 401) return
    chat.actionError = errorText(err, '停止失败，请重试')
  }
}

// ------------------------------------------ watching the turns in flight --

/**
 * How often the conversation list is re-read while something is generating.
 *
 * A turn can be running in a conversation this tab is not reading — started from
 * another tab, or before a reload — so the sidebar's 生成中 marks and a turn's
 * completion are not things this tab can infer from its own stream. When nothing
 * is generating the timer is off, so an idle console is never polling.
 */
const STREAM_POLL_MS = 5000
let streamPoll = null

/** Start or stop the poll according to whether anything is generating. */
function syncStreamPoll() {
  const generating = chat.sessions.some((session) => session && session.streaming)
  if (generating && streamPoll === null) {
    streamPoll = window.setInterval(pollTurnsInFlight, STREAM_POLL_MS)
  } else if (!generating && streamPoll !== null) {
    window.clearInterval(streamPoll)
    streamPoll = null
  }
}

/**
 * Re-read the list, and pick up a turn that started for the conversation on
 * screen while this tab was not watching it.
 */
async function pollTurnsInFlight() {
  await loadSessions({ quiet: true, select: false })
  const active = chat.sessions.find((session) => session && session.id === chat.activeId)
  if (active && active.streaming && !attachedTo(chat.activeId)) {
    attachTurn(chat.activeId)
  }
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

/**
 * Submit one decision on a write or exec the turn is blocked on.
 *
 * Mirrors submitAsk, with the one difference the whole gate turns on: a failure
 * is not "try again". If the request is gone (404 — it timed out, another tab
 * decided, or the turn ended), the card is settled as refused, because that is
 * what the server will have done. Leaving it editable would invite a second click
 * on a decision that has already been made.
 */
export async function submitApproval(approvalId, { kind, reason = '' } = {}) {
  const sessionId = chat.activeId
  const card = findApproval(approvalId)
  if (!card || !sessionId) return
  if (card.status !== APPROVAL_PENDING) return

  const decision = typeof kind === 'string' ? kind : ''
  if (!decision) return
  card.status = APPROVAL_SUBMITTING
  card.error = ''
  chat.streamTick += 1

  try {
    await api.answerApproval(sessionId, approvalId, { decision, reason: typeof reason === 'string' ? reason : '' })
    // Settled locally as well as from the stream: the model may take a while to
    // use the decision, and a card that keeps offering buttons in the meantime
    // invites a second click on a request that has already been answered.
    card.status = 'settled'
    card.decision = decision
    card.reason = decision === 'deny' ? (reason || '') : ''
    card.source = 'human'
  } catch (err) {
    if (err && err.status === 401) return
    if (err && err.status === 404) {
      card.status = 'settled'
      card.decision = 'deny'
      card.source = 'timeout'
      card.error = ''
      chat.streamTick += 1
      return
    }
    card.error = errorText(err, '提交失败，请重试')
    card.status = APPROVAL_PENDING
  }
  chat.streamTick += 1
}

/** The approval with this id, wherever it currently lives in the conversation. */
function findApproval(id) {
  for (let i = chat.items.length - 1; i >= 0; i--) {
    const item = chat.items[i]
    const list = item && Array.isArray(item.approvals) ? item.approvals : []
    const found = list.find((card) => card && card.id === id)
    if (found) return found
  }
  return null
}

/**
 * Attach a nested event to the card of the call that spawned it.
 *
 * Consecutive deltas of the same kind extend one entry, so a subagent's paragraph
 * is one block rather than one entry per token — the same coalescing the server
 * does, for the same reason.
 */
function appendNested(turn, event, kind) {
  const parentID = String(event.parent_tool_call_id || '')
  const parent = findTool(turn, parentID)
  if (!parent) return
  if (!Array.isArray(parent.nested)) parent.nested = []

  if (kind === 'tool') {
    parent.nested.push({
      kind: 'tool',
      name: String(event.tool_name || ''),
      id: String(event.tool_call_id || ''),
      args: typeof event.tool_args === 'string' ? event.tool_args : '',
      result: '',
      error: '',
    })
    return
  }
  const text = typeof event.text === 'string' ? event.text : ''
  if (!text) return
  const last = parent.nested[parent.nested.length - 1]
  if (last && last.kind === kind) {
    last.text = (last.text || '') + text
    return
  }
  parent.nested.push({ kind, text })
}

/** Attach a nested tool's result to its own entry. */
function closeNested(turn, event) {
  const parent = findTool(turn, String(event.parent_tool_call_id || ''))
  if (!parent || !Array.isArray(parent.nested)) return
  const id = String(event.tool_call_id || '')
  for (const entry of parent.nested) {
    if (entry.kind === 'tool' && entry.id === id) {
      entry.result = typeof event.tool_result === 'string' ? event.tool_result : asText(event.tool_result)
      entry.error = typeof event.tool_error === 'string' ? event.tool_error : ''
      return
    }
  }
}

function handleEvent(turn, event) {
  if (!event || typeof event !== 'object') return true
  chat.streamTick += 1

  switch (event.type) {
    case 'step_start':
      openStep(turn, event.step)
      return true

    case 'reasoning_delta': {
      if (typeof event.text !== 'string') return true
      // A subagent's thinking: on its card, not in the turn's reasoning.
      if (event.parent_tool_call_id) {
        appendNested(turn, event, 'reasoning')
        return true
      }
      const step = openStep(turn, event.step)
      step.reasoning += event.text
      // The turn's whole thinking is still kept: the footer and the "did this
      // turn produce anything" question are about the turn, not about a step.
      turn.reasoning += event.text
      return true
    }

    case 'text_delta': {
      if (typeof event.text !== 'string') return true
      // A subagent's own words. They go on the card of the call that spawned it and
      // never into the answer: the reason to spawn one is that its working-out stays
      // out of the parent's context, and an answer containing it would be text the
      // model never produced.
      if (event.parent_tool_call_id) {
        appendNested(turn, event, 'text')
        return true
      }
      const step = openStep(turn, event.step)
      step.text += event.text
      // The answer is only settled when the turn ends: text that arrives before
      // a tool call turns out to be that step's process, and `step_end` says so.
      // Until then it is shown as the answer, so the reader is never looking at
      // an empty bubble while the model types.
      turn.text += event.text
      return true
    }

    // One step's model call is over and it is going to act: its text was a
    // preamble, not the answer. Keeping it in the step is what stops a
    // conversation's copy buffer from filling up with "我先看一下这个文件。".
    case 'step_end': {
      const step = openStep(turn, event.step)
      if (typeof event.text === 'string') step.text = event.text
      // Every step that closed this way was process, and the turn's answer is
      // what the last step produces — so the answer is empty again until the
      // model starts writing it.
      turn.text = ''
      return true
    }

    case 'tool_call': {
      // A nested call is a subagent's, not this turn's: it belongs under the card
      // of the call that spawned it, not in the step's own tool list. Putting it in
      // the step would make a plan look like it did work it delegated, and the
      // reader would count steps that the model never took.
      if (event.parent_tool_call_id) {
        appendNested(turn, event, 'tool')
        return true
      }
      // ask_user is not rendered as a tool row: the case it belongs to is the
      // card, and pushing it here would show the same exchange twice — and, for
      // the moment between the call and the question, as a card nobody answered.
      if (isAskTool(event.tool_name)) return true
      const step = openStep(turn, event.step)
      const tool = normalizeTool(
        { id: event.tool_call_id, name: event.tool_name, args: event.tool_args },
        step.tools.length,
        false,
      )
      step.tools.push(tool)
      // The flat list is what the ask cards and the notice machinery read; the
      // steps are what the bubble renders. Both are views of the same calls.
      turn.tools.push(tool)
      return true
    }

    case 'tool_result': {
      if (event.parent_tool_call_id) {
        closeNested(turn, event)
        return true
      }
      if (isAskTool(event.tool_name)) return true
      const id = String(event.tool_call_id || '')
      const tool = findTool(turn, id)
      if (!tool) {
        // A result for a call this reader never saw announced, which a late
        // attach can produce. It is shown rather than dropped.
        const step = openStep(turn, event.step)
        const late = normalizeTool({ id, name: event.tool_name }, step.tools.length, false)
        step.tools.push(late)
        turn.tools.push(late)
      }
      const target = tool || turn.tools[turn.tools.length - 1]
      if (!target) return true
      target.result = typeof event.tool_result === 'string' ? event.tool_result : asText(event.tool_result)
      target.error = typeof event.tool_error === 'string' ? event.tool_error : ''
      target.durationMs = numberOrNull(event.duration_ms)
      target.status = target.error ? 'failed' : 'ok'
      // A tool that manages background processes just ran, so the count in this
      // conversation's header is now stale. This is the only signal that a job
      // appeared while nothing was running to poll for: the store's interval
      // exists only while something is alive.
      noteJobsToolRan(target.name)
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

    // A write or exec waiting for a decision, or the outcome of one already on
    // screen. Both shapes share the event type; `approval` marks the
    // announcement, exactly like the ask card next door.
    case 'approval': {
      const card = approvalFromEvent(event)
      if (card) {
        turn.approvals.push(card)
        return true
      }
      const id = typeof event.approval_id === 'string' ? event.approval_id : ''
      const existing = id ? turn.approvals.find((item) => item.id === id) : null
      if (existing) {
        applyApprovalOutcome(existing, event)
        // The server's word replaces the local one: a decision that timed out
        // while the reader was deciding must not leave buttons on screen.
        existing.error = ''
      }
      return true
    }

    case 'usage':
      turn.usage = addUsage(turn.usage, event.usage)
      return true

    // 模型改了计划：看板立刻跟着动。它不属于某一轮的消息（计划跨轮存在），
    // 所以落点是 store 上的 chat.plan，而不是这个 turn。
    case 'plan':
      chat.plan = normalizePlan(event.plan)
      return true

    // 这一步的模型调用失败了，服务端马上要重跑它。已经吐出来的半截文字必须
    // 丢掉：留着的话，重试后的答案会接在失败的那半截后面，读起来像模型自己
    // 重复了一遍。历史不变（失败的那次没有落库），所以只是把界面退回去。
    case 'step_retry': {
      const step = openStep(turn, event.step)
      // turn.text 是"当前这一步的临时答案"（step_end 已经清过它），所以这里
      // 清空就等于回退到这一步开始之前。
      turn.text = ''
      // turn.reasoning 是整轮的累计，不能整个清掉：前面几步的思考是真的发生过
      // 的事。只有这一步收到的这段属于被丢弃的那次调用，所以按长度从尾部回退。
      turn.reasoning = rollback(turn.reasoning, step.reasoning)
      step.text = ''
      step.reasoning = ''
      turn.notices.push({
        kind: 'retry',
        step: numberOrNull(event.step),
        text: retryNoticeText(event),
      })
      return true
    }

    // The window was condensed to stay inside the token budget. The page stays
    // quiet about it on purpose: it is a detail of how a long turn is run, not
    // something the reader can act on, so a line per compression only adds
    // noise above an answer that is still coming. The fact is not lost — the
    // server writes it to the session's log, which is where the window size of
    // a long turn gets investigated anyway.
    case 'context_compressed':
      return true

    // The harness told the model how it was running the turn: it is repeating a
    // call, rereading one file, or looking without ever acting. It is a notice
    // rather than a failure — the model gets the same sentence as a message and
    // usually changes course — but a turn that was steered is worth a line, or
    // the reader cannot tell "slow" from "going in circles".
    case 'steer':
      turn.notices.push({
        kind: 'steer',
        step: numberOrNull(event.step),
        steerKind: typeof event.steer_kind === 'string' ? event.steer_kind : '',
        text: steerNoticeText(event),
        detail: typeof event.text === 'string' ? event.text : '',
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

