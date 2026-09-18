// The approval requests a running turn is waiting on, normalised.
//
// This is the console half of the approval gate, and it is deliberately simpler
// than ask.js, because an approval differs from a question in two ways that both
// cut work:
//
//   - **It is live only.** A question is persisted as a tool call and its result,
//     so a reloaded conversation can render the same card. An approval is not a
//     tool call — it happens *before* one — so a reloaded conversation shows the
//     tool card and its outcome, which is the honest picture: what was asked, and
//     what came of it.
//   - **It has no partial state.** A question can be answered in the person's own
//     words or by choosing; an approval is one of three decisions, and the reason
//     only exists on a refusal.
//
// What it shares with ask.js is the part that matters: the server's outcome event
// is authoritative. A countdown that reaches zero while the server has already
// timed the request out must not leave a card offering buttons.

/** One decision, and what it means on screen. */
export const APPROVAL_ALLOW_ONCE = 'allow_once'
export const APPROVAL_ALLOW_TURN = 'allow_turn'
export const APPROVAL_DENY = 'deny'

/** Card states. Only `pending` accepts input. */
export const APPROVAL_PENDING = 'pending'
export const APPROVAL_SUBMITTING = 'submitting'
export const APPROVAL_SETTLED = 'settled'

function asString(value) {
  return typeof value === 'string' ? value : value === null || value === undefined ? '' : String(value)
}

/** Normalise one line of a request's preview. */
function normalizePreview(raw) {
  const list = Array.isArray(raw) ? raw : []
  const out = []
  for (const entry of list) {
    const text = asString(entry && entry.text).replace(/\r?\n$/, '')
    if (text === '') continue
    const kind = asString(entry && entry.kind)
    out.push({ kind: kind === 'add' || kind === 'del' || kind === 'context' ? kind : 'meta', text })
  }
  return out
}

/**
 * Build the card model from the live `approval` event.
 *
 * Returns null for an event that is not an announcement, so the caller can tell
 * "a new request" from "the outcome of one already on screen" the same way the
 * ask path does.
 */
export function approvalFromEvent(event) {
  const req = event && event.approval
  if (!req || typeof req !== 'object') return null
  const id = asString(req.id)
  if (!id) return null
  return {
    id,
    tool: asString(req.tool),
    capability: asString(req.capability),
    summary: asString(req.summary),
    preview: normalizePreview(req.preview),
    default: asString(req.default),
    // The wait the server is actually applying, so the countdown matches it
    // rather than a number this file invented.
    timeoutMs: Number(req.timeout_ms) > 0 ? Number(req.timeout_ms) : 0,
    status: APPROVAL_PENDING,
    decision: '',
    reason: '',
    source: '',
    error: '',
    announcedAt: Date.now(),
  }
}

/** Apply the server's outcome to a card that is already on screen. */
export function applyApprovalOutcome(card, event) {
  const decision = asString(event && event.approval_decision)
  if (!decision) return card
  card.status = APPROVAL_SETTLED
  card.decision = decision
  card.reason = asString(event && event.approval_reason)
  card.source = asString(event && event.approval_source)
  return card
}

/** True while the card still accepts a decision. */
export function isApprovalOpen(card) {
  return Boolean(card) && (card.status === APPROVAL_PENDING || card.status === APPROVAL_SUBMITTING)
}

/** The requests in a turn that still need an answer. */
export function pendingApprovals(turn) {
  const list = Array.isArray(turn && turn.approvals) ? turn.approvals : []
  return list.filter(isApprovalOpen)
}

/** What the buttons say. */
export function approvalDecisionLabel(decision) {
  switch (decision) {
    case APPROVAL_ALLOW_ONCE:
      return '允许一次'
    case APPROVAL_ALLOW_TURN:
      return '本轮都允许'
    case APPROVAL_DENY:
      return '拒绝'
    default:
      return asString(decision) || '—'
  }
}

/** One line describing how a request ended, for a settled card. */
export function approvalOutcomeLine(card) {
  if (!card || card.status !== APPROVAL_SETTLED) return ''
  const what = approvalDecisionLabel(card.decision)
  if (card.decision !== APPROVAL_DENY) return what
  // A refusal has two very different causes, and they read differently: a person
  // said no, or nobody was there. The second is the one worth spelling out,
  // because it is the gate holding rather than a judgement.
  if (card.source === 'timeout') return '拒绝（等待超时，没有人回应）'
  if (card.source === 'policy') return `拒绝（${card.reason || '按策略处理'}）`
  return card.reason ? `拒绝：${card.reason}` : what
}

/** The hint under the buttons: what each choice means. */
export function approvalHint(card) {
  if (!card) return ''
  if (card.status === APPROVAL_SUBMITTING) return '正在提交…'
  if (!isApprovalOpen(card)) return ''
  return '允许一次只管这一次；本轮都允许在这一次回答结束前不再询问'
}

/**
 * Seconds left on a request, or null when there is no limit to show.
 *
 * It counts down from the wait the server said it is applying, and it never goes
 * below zero: a negative countdown would suggest the request is still alive.
 */
export function approvalSecondsLeft(card, now) {
  if (!card || !isApprovalOpen(card) || !card.timeoutMs) return null
  const elapsed = Math.max(0, (now || Date.now()) - card.announcedAt)
  return Math.max(0, Math.ceil((card.timeoutMs - elapsed) / 1000))
}
