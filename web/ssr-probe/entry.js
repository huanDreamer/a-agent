import { createSSRApp, h } from 'vue'
import { renderToString } from 'vue/server-renderer'
import DirPicker from '../src/components/DirPicker.vue'
import AppSidebar from '../src/components/AppSidebar.vue'
import LoginView from '../src/components/LoginView.vue'
import AskUserCard from '../src/components/AskUserCard.vue'
import { api } from '../src/api.js'
import { askFromEvent, askFromTool, applyAskOutcome, askAnswerLine } from '../src/ask.js'
import { chat } from '../src/chatStore.js'
import { needsLogin, state } from '../src/state.js'

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
