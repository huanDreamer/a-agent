// The model's questions, normalised.
//
// One card can come from two places, and both must render identically:
//
//   - live, from the `ask_user` event on the stream, which carries the question
//     the moment the model asks it — this is the one that blocks the turn;
//   - from a reloaded conversation, where the question is the arguments of a
//     persisted `ask_user` tool call and the answer is its result.
//
// The server persists both (they are the tool call's arguments and its result),
// so a card survives a refresh without a table of its own. This module is the
// single place that knows how to read either, which is what keeps a reloaded
// card from rendering differently from a live one.

/** The tool whose calls are rendered as cards rather than as tool rows. */
export const ASK_TOOL = 'ask_user'

/** Card states. `pending` is the only one that accepts input. */
export const ASK_PENDING = 'pending'
export const ASK_SUBMITTING = 'submitting'
export const ASK_ANSWERED = 'answered'
export const ASK_TIMEOUT = 'timeout'
export const ASK_CANCELLED = 'cancelled'

/** True for the tool calls that become cards. */
export function isAskTool(name) {
  return String(name || '') === ASK_TOOL
}

function asRecord(value) {
  return value && typeof value === 'object' && !Array.isArray(value) ? value : null
}

function asString(value) {
  return typeof value === 'string' ? value : value === null || value === undefined ? '' : String(value)
}

/** Parse a JSON column that may be a string, an object or (when broken) nothing. */
function parseJSON(value) {
  if (value === null || value === undefined || value === '') return null
  if (typeof value === 'object') return value
  if (typeof value !== 'string') return null
  try {
    return JSON.parse(value)
  } catch (err) {
    // A half-written column must never break the conversation.
    return null
  }
}

/** Normalise the offered options; entries without a usable label are dropped. */
function normalizeOptions(raw) {
  const list = Array.isArray(raw) ? raw : []
  const out = []
  const seen = new Set()
  for (const entry of list) {
    const source = asRecord(entry)
    const label = asString(source ? source.label : entry).trim()
    if (!label || seen.has(label)) continue
    seen.add(label)
    out.push({ label, description: asString(source && source.description).trim() })
  }
  return out
}

function normalizeSelected(raw) {
  const list = Array.isArray(raw) ? raw : []
  return list.map((v) => asString(v).trim()).filter(Boolean)
}

/**
 * Build a card from an `ask_user` stream event.
 *
 * Returns null for the outcome shape: that one updates a card that is already on
 * screen (see `applyAskOutcome`), which is why it carries an id and no question.
 */
export function askFromEvent(event) {
  if (!event || typeof event !== 'object') return null
  const ask = asRecord(event.ask)
  if (!ask) return null
  const id = asString(ask.id).trim()
  if (!id) return null
  return {
    id,
    header: asString(ask.header).trim(),
    question: asString(ask.text),
    options: normalizeOptions(ask.options),
    multiSelect: Boolean(ask.multi_select),
    // Absent means "allowed": the server always states it, but a card that lost
    // the field must not silently remove the free-text box the model expected.
    allowCustom: ask.allow_custom === undefined ? true : Boolean(ask.allow_custom),
    status: ASK_PENDING,
    selected: [],
    text: '',
    error: '',
    fromHistory: false,
  }
}

/** Apply the outcome event (answered / timeout / cancelled) to a card. */
export function applyAskOutcome(card, event) {
  if (!card || !event || typeof event !== 'object') return false
  const status = asString(event.ask_status)
  if (!status) return false
  const answer = asRecord(event.ask_answer)
  if (answer) {
    card.selected = normalizeSelected(answer.selected)
    card.text = asString(answer.text)
  }
  // "submitting" is a local state with the request in flight; the server's word
  // is the truth once it arrives.
  card.status = status
  return true
}

/**
 * Build a card from a persisted `ask_user` tool call.
 *
 * `args` is the question and `result` is the outcome, both exactly as the tool
 * saw them. A call that never produced a result was a turn that died mid-ask,
 * and it renders as cancelled — the honest reading of a question the server
 * never got an answer for.
 */
export function askFromTool(tool) {
  const source = asRecord(tool)
  if (!source) return null
  const args = asRecord(parseJSON(source.args))
  if (!args) return null
  const question = asString(args.question)
  if (!question) return null

  const output = asRecord(parseJSON(source.result))
  const id = asString(source.id) || `ask-${asString(source.key)}`
  const card = {
    id,
    header: asString(args.header).trim(),
    question,
    options: normalizeOptions(args.options),
    multiSelect: Boolean(args.multi_select),
    allowCustom: args.allow_custom === undefined ? true : Boolean(args.allow_custom),
    status: ASK_ANSWERED,
    selected: [],
    text: '',
    error: '',
    fromHistory: true,
  }

  if (source.error) {
    card.status = ASK_CANCELLED
    card.error = asString(source.error)
    return card
  }
  if (!output) {
    card.status = ASK_CANCELLED
    return card
  }
  card.status = asString(output.status) || ASK_ANSWERED
  card.selected = normalizeSelected(output.selected)
  card.text = asString(output.text)
  return card
}

/** Every card a persisted or live turn carries, in call order. */
export function askCardsFromTools(tools) {
  const list = Array.isArray(tools) ? tools : []
  const out = []
  for (const tool of list) {
    if (!isAskTool(tool && tool.name)) continue
    const card = askFromTool(tool)
    if (card) out.push(card)
  }
  return out
}

/** True when a card still accepts an answer. */
export function isAskOpen(card) {
  return Boolean(card) && card.status === ASK_PENDING
}

/**
 * One line describing what was answered, for the settled card and for the
 * tests. It mirrors what the server renders into the model's `answer` field, so
 * a reader of the conversation and the model are told the same thing.
 */
export function askAnswerLine(card) {
  const selected = normalizeSelected(card && card.selected)
  const text = asString(card && card.text).trim()
  const joined = selected.join('、')
  if (joined && text) return `${joined}（补充：${text}）`
  return joined || text
}

/** The status label shown on the card's head. */
export function askStatusLabel(card) {
  const status = card && card.status
  if (status === ASK_PENDING) return '等待你的回答'
  if (status === ASK_SUBMITTING) return '提交中…'
  if (status === ASK_ANSWERED) return '已提交'
  if (status === ASK_TIMEOUT) return '已超时'
  if (status === ASK_CANCELLED) return '已取消'
  return ''
}

/** One line explaining a settled card that nobody answered. */
export function askClosedNote(card) {
  const status = card && card.status
  if (status === ASK_TIMEOUT) return '等待超时，模型已按自己的判断继续'
  if (status === ASK_CANCELLED) return '本轮已中断，模型没有等到回答'
  return ''
}
