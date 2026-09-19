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
import ApprovalCard from '../src/components/ApprovalCard.vue'
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
import { budget, buildItems, chat } from '../src/chatStore.js'
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
import { needsLogin, state } from '../src/state.js'
import {
  subagentsRunning,
  subagentsStoreState as subagentState,
  subagentsTotal,
} from '../src/subagentsStore.js'
import { subagentChipLabel, subagentHintText } from '../src/subagentsChip.js'

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
