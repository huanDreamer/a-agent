import { createSSRApp, h } from 'vue'
import { renderToString } from 'vue/server-renderer'
import BudgetPanel from '../src/components/BudgetPanel.vue'
import ChatMessage from '../src/components/ChatMessage.vue'
import ChatView from '../src/components/ChatView.vue'
import SubagentsDrawer from '../src/components/SubagentsDrawer.vue'
import DirPicker from '../src/components/DirPicker.vue'
import AppSidebar from '../src/components/AppSidebar.vue'
import LoginView from '../src/components/LoginView.vue'
import AskUserCard from '../src/components/AskUserCard.vue'
import ArtifactsDrawer from '../src/components/ArtifactsDrawer.vue'
import ModelPanel from '../src/components/ModelPanel.vue'
import ArtifactsView from '../src/components/ArtifactsView.vue'
import ApprovalCard from '../src/components/ApprovalCard.vue'
import ClaudeCodePanel from '../src/components/ClaudeCodePanel.vue'
import TaskBoard from '../src/components/TaskBoard.vue'
import { api } from '../src/api.js'
import { askFromEvent, askFromTool, applyAskOutcome, askAnswerLine, isAskTool } from '../src/ask.js'
import {
  applyApprovalOutcome,
  approvalFromEvent,
  approvalHint,
  approvalOutcomeLine,
  approvalSecondsLeft,
  isApprovalOpen,
  pendingApprovals,
} from '../src/approval.js'
import { budget, buildItems, chat, createSession, ensureLoaded } from '../src/chatStore.js'
import {
  normalizePlan,
  planGroups,
  planHeadline,
  planProgress,
  planResumable,
} from '../src/plan.js'
import { normalizeNested } from '../src/steps.js'
import {
  foldSteps,
  normalizeSteps,
  processShouldBeOpen,
  stepFailed,
  stepSummary,
  stepToolMs,
  visibleSteps,
} from '../src/steps.js'
import { claudeCode, needsLogin, state } from '../src/state.js'
import {
  subagentsRunning,
  subagentsStoreState as subagentState,
  subagentsTotal,
} from '../src/subagentsStore.js'
import { subagentChipLabel, subagentHintText } from '../src/subagentsChip.js'
import { artifactChipLabel, artifactHintText, artifactLabel } from '../src/artifactsChip.js'
import { artifactsStoreState as artifactState } from '../src/artifactsStore.js'
import { filterArtifacts, sessionLabel, totalBytes } from '../src/artifactsView.js'

// The dialog is teleported to <body>, and SSR puts teleport content in its own
// buffer rather than in the returned HTML — so both halves are concatenated, or
// the assertions would look at an empty string.
async function renderWithTeleports(component, props) {
  const ctx = {}
  const html = await renderToString(createSSRApp({ render: () => h(component, props) }), ctx)
  const teleported = Object.values(ctx.teleports || {}).join('')
  return html + teleported
}

export async function renderDirPicker(props) {
  return renderWithTeleports(DirPicker, props)
}

export async function renderSidebar() {
  return renderWithTeleports(AppSidebar, {})
}

export async function renderLogin() {
  return renderWithTeleports(LoginView, {})
}

/**
 * Render 设置 → 模型 with a seeded catalog.
 *
 * The point of the probe is the column the user asked for: a window size and a
 * capability set are only useful if they are visible where the models are listed,
 * and "I added it but it is on another sub-tab" is exactly the failure a
 * screenshot-free check can catch. 模型管理 below the table fetches on mount (which
 * SSR does not run), so this asserts on the catalog table — the part this screen
 * opens with.
 */
export async function renderModelPanel({ catalog: seeded }) {
  Object.assign(chat, { catalog: seeded, catalogStatus: 'ready', catalogError: '' })
  return renderWithTeleports(ModelPanel, {})
}

export function setChatState(patch) {
  Object.assign(chat, patch)
}

/**
 * Set the session state the shell branches on. The login screen and the sidebar
 * read `state.auth` directly — it is a module singleton, not a prop — so a probe
 * has to set it exactly as the boot probe does.
 */
export function setAuth(patch) {
  Object.assign(state.auth, patch)
}

/** What the shell's gate decides right now. */
export function gateNeedsLogin() {
  return needsLogin.value
}

export function chatState() {
  return {
    sessions: chat.sessions.length,
    workspaces: chat.workspaces.length,
    available: chat.workspacesAvailable,
  }
}

/* ------------------------------------------------- conversation header -- */

/**
 * Render 对话's header with a given session and stats.
 *
 * The header reads module singletons (chat.session, chat.items, chat.stats)
 * rather than props, so the probe sets them exactly as selectSession does.
 */
export async function renderChatHeader({ session, stats, items }) {
  Object.assign(chat, {
    session,
    stats,
    items: items || [],
    streaming: false,
    catalogStatus: 'ready',
    actionError: '',
    notice: '',
  })
  return renderWithTeleports(ChatView, {})
}

/* ------------------------------------------------------- 对话预算 panel -- */

/**
 * Render 设置 → 对话预算 against a snapshot.
 *
 * The panel reads a module singleton rather than props — that is what lets it
 * survive a tab switch with the text still in the box — so the probe sets the
 * store exactly as loadBudget would, then renders.
 */
export async function renderBudgetPanel(snapshot) {
  Object.assign(budget, { snapshot, status: 'ready', error: '', saving: false })
  return renderWithTeleports(BudgetPanel, {})
}

/** The budget state the panel would read right now. */
export function budgetState() {
  return { status: budget.status, has: Boolean(budget.snapshot) }
}

/* ------------------------------------------------------- ask_user card -- */

/** Render one card, from whatever model shape the probe passes in. */
export async function renderAskUserCard(ask) {
  return renderWithTeleports(AskUserCard, { ask })
}

/** A card built from the live `ask_user` event. */
export function askCardFromEvent(event) {
  return askFromEvent(event)
}

/** A card built from a persisted `ask_user` tool call. */
export function askCardFromTool(tool) {
  return askFromTool(tool)
}

/** Apply an outcome event to a card and return it, for the settled probes. */
export function applyAskEvent(card, event) {
  applyAskOutcome(card, event)
  return card
}

/** The line a settled card shows, computed the way the component does. */
export function askCardAnswer(card) {
  return askAnswerLine(card)
}

/**
 * Submit an answer with `fetch` stubbed out, and report what was actually sent.
 *
 * The answer travels on a URL the server defines and the client builds by hand,
 * and the two are only ever checked against each other at runtime — a typo in
 * the path would look exactly like a question that timed out. This captures the
 * real request so the probe can pin it.
 */
export async function captureAnswerRequest(sessionId, questionId, body) {
  const original = globalThis.fetch
  let captured = null
  globalThis.fetch = async (url, init) => {
    captured = { url: String(url), init: init || {} }
    return {
      ok: true,
      status: 200,
      text: async () => JSON.stringify({ ok: true }),
    }
  }
  try {
    await api.answerQuestion(sessionId, questionId, body)
  } finally {
    globalThis.fetch = original
  }
  return captured
}

/* ------------------------------------------------- turn steps -- */

/**
 * Render one assistant bubble from a stored message row.
 *
 * It goes through buildItems — the same call selectSession makes — so what the
 * probe asserts on is what a reloaded conversation actually renders, not a
 * fixture assembled to please the assertion.
 *
 * `live` marks the turn as one that is still running. That is a state of the
 * turn rather than of the stored row — buildItems always builds settled items —
 * so it is set here the way chatStore sets it on a turn it is reading, and with
 * `pending` deciding whether the model has started writing the answer yet.
 */
export async function renderMessageBubble(message, { live = false, pending = false } = {}) {
  const [item] = buildItems([message])
  if (!item) return ''
  if (live) {
    item.streaming = true
    if (pending) item.text = ''
  }
  return renderWithTeleports(ChatMessage, { item })
}

/** The steps a bubble would render from a stored message row. */
export function messageSteps(message) {
  const [item] = buildItems([message])
  return item ? item.steps : []
}

/** See steps.js: the fold/collapse, summary and orphan branches. */
export const stepHelpers = {
  normalizeSteps,
  visibleSteps,
  stepSummary,
  stepFailed,
  stepToolMs,
  foldSteps,
  isAskTool,
  processShouldBeOpen,
}

/* --------------------------------------------------------- 任务看板 -- */

/**
 * Render 任务看板 against a plan and a streaming flag.
 *
 * The board reads module singletons (chat.plan / chat.planCollapsed /
 * chat.streaming) rather than props — that is what makes it follow a running
 * turn without the view passing anything down — so the probe sets the store the
 * way handleEvent and selectSession do, then renders.
 *
 * `null` is the whole "no plan" case: the component must render nothing at all,
 * which is only observable by rendering it.
 */
export async function renderTaskBoard({ plan = null, collapsed = false, streaming = false } = {}) {
  Object.assign(chat, { plan, planCollapsed: collapsed, streaming })
  return renderWithTeleports(TaskBoard, {})
}

/** The board state a render would read right now. */
export function planState() {
  return {
    has: Boolean(chat.plan),
    tasks: chat.plan ? chat.plan.tasks.length : 0,
    collapsed: Boolean(chat.planCollapsed),
    streaming: Boolean(chat.streaming),
  }
}

/**
 * 新建对话之后，输入框上方还有没有任务看板？
 *
 * 这是"新建对话不该显示任务看板"的回归探针。看板读的是 chat.plan（模块单例），
 * 而 createSession 在服务端建完之后把界面切到新会话——它一路上重置了 items /
 * stats / workspace，唯独漏掉 plan 的话，上一个对话的任务清单就挂在一个空对话的
 * 输入框上方，看起来像"这个刚建的对话有一堆活没干"。
 *
 * 断言必须同时看 store 和渲染结果：store 是原因，HTML 是用户看到的东西。
 *
 * `status` 不是 200 时走的是失败分支——那时人还留在原会话上，那份计划是他的
 * 「继续执行」接续点，不能被一次失败的创建清掉。
 */
export async function createSessionWithPlan({ existing = null, status = 200 } = {}) {
  const original = globalThis.fetch
  const paths = []
  globalThis.fetch = async (url) => {
    const path = String(url)
    paths.push(path)
    if (status !== 200 && path.endsWith('/api/chat/sessions')) {
      return { ok: false, status, text: async () => JSON.stringify({ error: 'boom' }) }
    }
    let body = {}
    if (path.endsWith('/api/chat/sessions')) body = { session: { id: 's-new', title: '', message_count: 0 } }
    else if (path.includes('/api/chat/sessions')) body = { sessions: [] }
    else if (path.includes('/api/workspaces')) body = { workspaces: [{ name: 'proj', root: '/tmp/proj' }] }
    return { ok: true, status: 200, text: async () => JSON.stringify(body) }
  }
  chat.plan = existing
  chat.planCollapsed = false
  chat.activeId = 's-old'
  chat.session = { id: 's-old' }
  chat.items = []
  chat.creating = false
  chat.actionError = ''
  try {
    const created = await createSession()
    return {
      created: Boolean(created),
      plan: chat.plan,
      activeId: chat.activeId,
      // 从探针自己的那份 store 读回来，而不是让调用方去读另一个模块实例：
      // run.mjs 里的 chatStore 与打包进探针的那份是两份。
      actionError: chat.actionError,
      html: await renderWithTeleports(TaskBoard, {}),
      paths,
    }
  } finally {
    globalThis.fetch = original
  }
}

/** See plan.js: the wire normalisation, the grouping and the one-line summary. */
export const planHelpers = {
  normalizePlan,
  planGroups,
  planProgress,
  planResumable,
  planHeadline,
}


/* ------------------------------------------------- approval gate -- */

/** Render one approval card from a model built by approval.js. */
export async function renderApprovalCard(card) {
  return renderWithTeleports(ApprovalCard, { card })
}

/** Build a card from a live `approval` event. */
export function approvalCardFromEvent(event) {
  return approvalFromEvent(event)
}

/** Apply the server's outcome to a card. */
export function applyApprovalEvent(card, event) {
  return applyApprovalOutcome(card, event)
}

/** The requests in a turn that still need a decision. */
export function approvalPending(turn) {
  return pendingApprovals(turn)
}

export const approvalHelpers = {
  approvalHint,
  approvalOutcomeLine,
  approvalSecondsLeft,
  isApprovalOpen,
}

/**
 * Capture the request submitApproval makes.
 *
 * The decision travels on a URL the server defines and the client builds by hand,
 * and the two only ever meet at runtime — a typo in the path would look exactly
 * like a request that was refused. This captures the real request so the probe can
 * pin it.
 */
export async function captureApprovalRequest(sessionId, approvalId, body) {
  const original = globalThis.fetch
  let captured = null
  globalThis.fetch = async (url, init) => {
    captured = { url: String(url), init: init || {} }
    return { ok: true, status: 200, text: async () => JSON.stringify({ ok: true }) }
  }
  try {
    await api.answerApproval(sessionId, approvalId, body)
  } finally {
    globalThis.fetch = original
  }
  return captured
}


/* ------------------------------------------------ nested subagent work -- */

/** Normalise a call's nested entries, for the probe. */
export function nestedOf(raw) {
  return normalizeNested(raw)
}


/* ------------------------------------------------ delegated subagents -- */

/**
 * Load the subagent store with a run list.
 *
 * The store is a module singleton that normally fills itself from the API, so a
 * probe sets it directly — the same thing the existing probes do for chat state.
 */
export function setSubagents(runs, { running = 0, maxConcurrent = 0, runningAll = 0 } = {}) {
  subagentState.runs = runs
  subagentState.running = running
  subagentState.maxConcurrent = maxConcurrent
  subagentState.runningAll = runningAll
  subagentState.total = runs.length
  subagentState.loaded = true
}

/** The facts the header chip renders from. */
export function subagentChip() {
  return {
    running: subagentsRunning.value,
    total: subagentsTotal.value,
    label: subagentChipLabel(),
    hint: subagentHintText(),
  }
}

/** Render the subagents drawer, so the probe sees what a reader sees. */
export async function renderSubagentsDrawer() {
  return renderWithTeleports(SubagentsDrawer, {})
}


/* --------------------------------------------------------------- 产物 -- */

/**
 * Load the artifact store with a list, the way a session load would.
 *
 * The store is a module singleton that normally fills itself from the API, so a
 * probe sets it directly — the same thing setSubagents does.
 */
export function setArtifacts(items, { enabled = true, message = '', sessionId = 'sess-1' } = {}) {
  artifactState.sessionId = sessionId
  artifactState.items = items
  artifactState.enabled = enabled
  artifactState.message = message
  artifactState.error = ''
  artifactState.loading = false
  artifactState.loaded = true
}

/** The facts the header button renders from. */
export function artifactChip() {
  return {
    count: artifactState.items.length,
    label: artifactChipLabel(),
    hint: artifactHintText(),
  }
}

/** Render the 产物 drawer, so the probe sees what a reader sees. */
export async function renderArtifactsDrawer() {
  return renderWithTeleports(ArtifactsDrawer, {})
}

/** Render 产物中心. It loads from the API, so the probe asserts on the empty state. */
export async function renderArtifactsView() {
  return renderWithTeleports(ArtifactsView, {})
}

/** The listing rules, for the probe's assertions. */
export function artifactHelpers() {
  return { filterArtifacts, sessionLabel, totalBytes, artifactLabel }
}


/* ------------------------------------------ ClaudeCode 兼容模式 --------- */

/**
 * Render 设置 → ClaudeCode against a status payload.
 *
 * The panel reads the module singleton rather than props — that is what makes the
 * sidebar badge and the panel one answer instead of two reads that can disagree —
 * so the probe seeds it exactly as `loadClaudeCode` does, then renders.
 *
 * 运行记录 has two sources by design: the status payload's own `hook_log` (on screen
 * the moment the panel opens) and GET /api/claudecode/events, which SSR cannot run.
 * So what this renders is the hook_log half, which is the half a reader sees first.
 */
export async function renderClaudeCodePanel(snapshot, { status = 'ready', error = '' } = {}) {
  // `undefined` is the store's own initial state — no snapshot, i.e. loading — so a
  // caller that wants the panel's error screen asks for it by name.
  Object.assign(claudeCode, {
    snapshot: snapshot === undefined ? null : snapshot,
    status: snapshot === undefined && status === 'ready' ? 'loading' : status,
    error,
    busy: false,
  })
  return renderWithTeleports(ClaudeCodePanel, {})
}

/**
 * Seed the mode store for the sidebar badge, which is the other half of the same
 * store — and the one that must render *nothing* until it has an answer.
 */
export function setClaudeCode(snapshot, { status = 'ready', error = '' } = {}) {
  Object.assign(claudeCode, { snapshot, status, error, busy: false })
}


/* ------------------------------------------------------- boot retry ---- */

/**
 * Drive the store through a scripted boot sequence, and report what happened.
 *
 * This is the regression probe for the bug where the console stayed empty after
 * login until a full page reload. The sequence is the real one, in one store
 * with nothing reset in between:
 *
 *   1. the shell mounts and calls ensureLoaded with no session yet — a
 *      login-required deployment answers 401 on every request;
 *   2. the operator signs in and App.vue calls ensureLoaded again.
 *
 * Step 2 is the one that used to do nothing, because `booted` was set before the
 * request in step 1 rather than after it. `statusFor(step)` decides the HTTP
 * status of each step, so `[401, 200]` is exactly that story.
 */
export async function bootSequence({ statuses = [200] } = {}) {
  const original = globalThis.fetch
  let step = 0
  const perStep = []
  globalThis.fetch = async (url) => {
    const path = String(url)
    perStep[step] = perStep[step] || []
    perStep[step].push(path)
    const status = statuses[Math.min(step, statuses.length - 1)]
    if (status !== 200) {
      return { ok: false, status, text: async () => JSON.stringify({ error: 'unauthorized' }) }
    }
    let body = {}
    if (path.includes('/api/chat/models')) body = { models: [], providers: [], tools: [] }
    else if (path.includes('/api/chat/sessions/')) {
      body = { session: { id: 's-1', title: 'one' }, messages: [], stats: {}, plan: null }
    } else if (path.includes('/api/chat/sessions')) {
      body = { sessions: [{ id: 's-1', title: 'one', updated_at: '2026-01-01T00:00:00Z' }] }
    } else if (path.includes('/api/workspaces')) {
      body = { workspaces: [{ name: 'proj', root: '/tmp/proj' }] }
    }
    return { ok: true, status: 200, text: async () => JSON.stringify(body) }
  }
  // Exactly the state a page load starts from.
  chat.booted = false
  chat.catalog = null
  chat.catalogStatus = 'loading'
  chat.sessions = []
  chat.sessionsStatus = 'loading'
  chat.sessionsError = ''
  chat.workspaces = []
  chat.activeId = ''
  chat.session = null

  const steps = []
  try {
    for (step = 0; step < statuses.length; step++) {
      await ensureLoaded()
      steps.push({
        booted: chat.booted,
        sessions: chat.sessions.length,
        workspaces: chat.workspaces.length,
        requested: (perStep[step] || []).length,
      })
    }
  } finally {
    globalThis.fetch = original
  }
  return steps
}

/**
 * Call ensureLoaded three times concurrently, as the shell, the chat view and
 * the tab watcher do on mount, and report how many requests actually went out.
 */
export async function bootConcurrently() {
  const original = globalThis.fetch
  let hits = 0
  globalThis.fetch = async (url) => {
    hits++
    const path = String(url)
    let body = {}
    if (path.includes('/api/chat/models')) body = { models: [], providers: [], tools: [] }
    else if (path.includes('/api/chat/sessions/')) body = { session: { id: 's-1' }, messages: [], stats: {}, plan: null }
    else if (path.includes('/api/chat/sessions')) body = { sessions: [{ id: 's-1', updated_at: '2026-01-01T00:00:00Z' }] }
    else if (path.includes('/api/workspaces')) body = { workspaces: [{ name: 'proj', root: '/tmp/proj' }] }
    return { ok: true, status: 200, text: async () => JSON.stringify(body) }
  }
  chat.booted = false
  chat.catalog = null
  chat.sessions = []
  chat.workspaces = []
  chat.activeId = ''
  chat.session = null
  try {
    await Promise.all([ensureLoaded(), ensureLoaded(), ensureLoaded()])
  } finally {
    globalThis.fetch = original
  }
  return { hits, booted: chat.booted, sessions: chat.sessions.length }
}

/**
 * Boot with the model catalog refused but the session list readable, then retry
 * as the 模型目录 banner's 重试 button does.
 *
 * The two requests can disagree: booted says "the session list was read", and the
 * catalog is a separate load. A guard that returned on `booted` alone would make
 * that banner's 重试 a no-op and leave the pane without a model list forever —
 * which is why this probe asserts on the second call's request count, not just
 * on `booted`.
 */
export async function bootCatalogRetry() {
  const original = globalThis.fetch
  let step = 0
  const perStep = []
  globalThis.fetch = async (url) => {
    const path = String(url)
    perStep[step] = perStep[step] || []
    perStep[step].push(path)
    // Only the first attempt's catalog request fails; the retry gets it.
    if (path.includes('/api/chat/models') && step === 0) {
      return { ok: false, status: 500, text: async () => JSON.stringify({ error: 'boom' }) }
    }
    let body = {}
    if (path.includes('/api/chat/models')) body = { models: [{ id: 'm-1' }], providers: [], tools: [] }
    else if (path.includes('/api/chat/sessions/')) body = { session: { id: 's-1' }, messages: [], stats: {}, plan: null }
    else if (path.includes('/api/chat/sessions')) body = { sessions: [{ id: 's-1', updated_at: '2026-01-01T00:00:00Z' }] }
    else if (path.includes('/api/workspaces')) body = { workspaces: [{ name: 'proj', root: '/tmp/proj' }] }
    return { ok: true, status: 200, text: async () => JSON.stringify(body) }
  }
  chat.booted = false
  chat.catalog = null
  chat.catalogStatus = 'loading'
  chat.catalogError = ''
  chat.sessions = []
  chat.sessionsStatus = 'loading'
  chat.workspaces = []
  chat.activeId = ''
  chat.session = null
  const steps = []
  try {
    for (step = 0; step < 2; step++) {
      await ensureLoaded()
      steps.push({
        booted: chat.booted,
        catalog: chat.catalog ? (chat.catalog.models || []).length : 0,
        catalogStatus: chat.catalogStatus,
        requested: (perStep[step] || []).length,
      })
    }
  } finally {
    globalThis.fetch = original
  }
  return steps
}
