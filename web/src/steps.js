// The steps of one turn, normalised.
//
// A turn is a ReAct loop: the model thinks, calls tools, sees the results, and
// thinks again. The server persists that loop as a list of steps — each one's
// reasoning, the text it produced, and the tool calls it asked for — and streams
// the same thing as events while it runs (see internal/chat/events.go).
//
// This module is the single place that knows how to read either shape, so a
// reloaded conversation renders exactly like a live one. It is also where a
// message written *before* steps were stored is rebuilt: those rows carry one
// blob of reasoning and a flat list of tool calls, and no reader can say which
// thought asked for which call — that information was never written down, so the
// turn is presented as the single block of process it actually is.

/** Non-empty string helper that also accepts numbers. */
function asString(value) {
  if (value === null || value === undefined) return ''
  return typeof value === 'string' ? value : String(value)
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

function numberOrNull(value) {
  if (typeof value === 'number') return Number.isFinite(value) ? value : null
  if (typeof value === 'string' && value.trim() !== '') {
    const n = Number(value)
    return Number.isFinite(n) ? n : null
  }
  return null
}

/** Tool args/results are strings that usually contain JSON; keep them textual. */
export function asText(value) {
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
 * The persisted `tool_calls`/`steps` columns are JSON strings written by
 * json.Marshal over chat.ToolRun, so both the documented lowercase keys and Go's
 * default capitalised field names are accepted: a rename on the server side
 * cannot break the conversation.
 *
 * `finished` says whether the call is over. A persisted call always is; a live
 * one is not until its result arrives, which is what the status badge shows.
 */
export function normalizeTool(raw, index, finished) {
  const source = raw && typeof raw === 'object' ? raw : {}
  const id = asString(pick(source, ['id', 'ID', 'tool_call_id', 'ToolCallID']) ?? `tool-${index}`)
  const name = asString(pick(source, ['name', 'Name', 'tool_name', 'ToolName']) ?? '工具')
  const args = asText(pick(source, ['args', 'Args', 'arguments', 'Arguments']))
  const result = asText(pick(source, ['result', 'Result', 'tool_result', 'ToolResult']))
  const error = asString(pick(source, ['err', 'Err', 'error', 'Error']))
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
    // What this call spawned — a subagent's own actions. The server stores it with
    // the turn, so a reloaded conversation shows the same thing a live one did.
    nested: normalizeNested(pick(source, ['nested', 'Nested'])),
  }
}

/**
 * Normalise a call's nested entries.
 *
 * The shape is small on purpose: a reader sees a line per action, not a second
 * conversation. A subagent's full transcript belongs in its own trace.
 */
export function normalizeNested(raw) {
  const list = Array.isArray(raw) ? raw : []
  const out = []
  for (const entry of list) {
    const source = entry && typeof entry === 'object' ? entry : null
    if (!source) continue
    const kind = asString(pick(source, ['kind', 'Kind']))
    if (kind !== 'text' && kind !== 'reasoning' && kind !== 'tool') continue
    out.push({
      kind,
      text: asText(pick(source, ['text', 'Text'])),
      name: asString(pick(source, ['name', 'Name'])),
      id: asString(pick(source, ['id', 'ID'])),
      result: asText(pick(source, ['result', 'Result'])),
      error: asString(pick(source, ['error', 'Error', 'err', 'Err'])),
    })
  }
  return out
}

/** Parse a persisted JSON column that may be a string, an array or nothing. */
export function parseJSONList(raw) {
  if (raw === null || raw === undefined || raw === '') return []
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
  return Array.isArray(list) ? list : []
}

/** Normalise the tool calls of one step (or of a legacy flat list). */
export function normalizeTools(raw, finished) {
  return parseJSONList(raw).map((entry, index) => normalizeTool(entry, index, finished))
}

/**
 * One render model for a step, from either source.
 *
 * `open` and `touched` belong to the render model, not to the data, and they
 * govern a step's *tool cards* — the reasoning is always rendered. A stored step
 * arrives with its cards folded, because the turn it belongs to is over and its
 * answer is what the reader came for; the thinking stays in the flow so the step
 * still says why it did what the header names. A live step is opened as it starts
 * instead (see chatStore.js), so the cards are visible while the work happens.
 *
 * `touched` records that the reader opened or folded this step themselves, which
 * the automatic fold on the answer must not override.
 */
function stepOf(raw, index) {
  const source = raw && typeof raw === 'object' ? raw : {}
  const number = numberOrNull(pick(source, ['index', 'Index', 'step', 'Step']))
  return {
    index: number === null ? index + 1 : number,
    reasoning: asString(pick(source, ['reasoning', 'Reasoning'])),
    text: asString(pick(source, ['text', 'Text'])),
    tools: normalizeTools(pick(source, ['tools', 'Tools']), true),
    open: false,
    touched: false,
  }
}

/**
 * Rebuild a legacy turn as one step.
 *
 * Rows written before steps were persisted hold the whole turn's reasoning and a
 * flat list of tool calls, with nothing to pair them by. Presenting it as one
 * block of process is therefore not a limitation of the renderer but a statement
 * about the data: the pairing was never recorded. It is deliberately still
 * foldable, and still shows every tool card and the model's thinking.
 */
function legacyStep(tools, reasoning) {
  return {
    index: 1,
    reasoning: asString(reasoning),
    text: '',
    tools: Array.isArray(tools) ? tools : [],
    open: false,
    touched: false,
  }
}

/**
 * Every step of one assistant message, in order.
 *
 * `rawSteps` is the persisted `steps` column, a live step list, or nothing;
 * `fallbackTools` and `fallbackReasoning` are the older columns, used only when
 * there are no steps to read.
 */
export function normalizeSteps(rawSteps, { tools = [], reasoning = '' } = {}) {
  const list = Array.isArray(rawSteps) ? rawSteps : parseJSONList(rawSteps)
  if (list.length) return list.map((entry, index) => stepOf(entry, index))
  // A turn that only answered has no process to show; one that thought but never
  // acted still has something worth folding up.
  if (!tools.length && !asString(reasoning)) return []
  return [legacyStep(tools, reasoning)]
}

/** Whether any step in the list called a tool. */
export function stepsHaveTools(steps) {
  return (Array.isArray(steps) ? steps : []).some((step) => step && step.tools && step.tools.length)
}

/**
 * The steps of one message as the bubble renders them: ask_user calls removed,
 * empty steps dropped.
 *
 * `isAsk` is passed in rather than imported so this module stays free of the
 * ask-card model — the two are separate presentations of the same tool call, and
 * only the component knows both.
 *
 * The result is a filtered *view*: the objects in it are the caller's own step
 * objects, so a fold or an unfold still reaches the state the component holds.
 * What a step shows is derived per call rather than stored, which is why the
 * filter is a function and not a property.
 */
export function visibleSteps(steps, isAsk = () => false) {
  const out = []
  for (const step of Array.isArray(steps) ? steps : []) {
    if (!step) continue
    const tools = visibleTools(step, isAsk)
    if (!tools.length && !asString(step.reasoning) && !asString(step.text)) continue
    out.push(step)
  }
  return out
}

/**
 * The tools of one step as the bubble shows them: the ask_user call is the card
 * above, and leaving the raw call in the step would show the same exchange twice
 * — once as the thing the user actually did, once as machine output.
 */
export function visibleTools(step, isAsk = () => false) {
  return ((step && step.tools) || []).filter((tool) => !isAsk(tool && tool.name))
}

/** How many tool names a folded step's header shows before it summarizes. */
const SUMMARY_TOOLS = 3

/**
 * One line for a step's header: what it did, or that it is still thinking.
 *
 * It is the only thing visible about a folded step, so it has to name the work:
 * "read_file, bash" says what happened without a click, and a step that has
 * produced nothing yet says so rather than looking like an empty one.
 */
export function stepSummary(step, { streaming = false, last = false } = {}) {
  const names = ((step && step.tools) || []).map((tool) => asString(tool && tool.name)).filter(Boolean)
  if (!names.length) {
    if (streaming && last) return '推理中…'
    return step && step.reasoning ? '思考' : '进行中…'
  }
  const shown = names.slice(0, SUMMARY_TOOLS)
  return names.length > shown.length
    ? `${shown.join(', ')} 等 ${names.length} 个工具`
    : shown.join(', ')
}

/** A failed call is worth flagging on a folded step, not only inside it. */
export function stepFailed(step) {
  return ((step && step.tools) || []).some((tool) => tool && tool.status === 'failed')
}

/** Total time a step's tools took, or null when none reported a duration. */
export function stepToolMs(step) {
  let total = 0
  let seen = false
  for (const tool of (step && step.tools) || []) {
    const ms = tool && tool.durationMs
    if (typeof ms !== 'number' || !Number.isFinite(ms)) continue
    total += ms
    seen = true
  }
  return seen ? total : null
}

/**
 * Whether a turn's steps should have their tool cards open right now.
 *
 * The cards are worth seeing while a turn runs — that is the whole point of
 * showing them — and worth folding once its answer lands, because a twenty-step
 * turn of arguments and results would otherwise push its own answer off the
 * screen. A turn that has not answered yet keeps them open: the work is then the
 * only thing there is to read. The thinking is not part of this decision; it is
 * rendered whatever this returns.
 */
export function stepsShouldBeOpen({ answered = false, streaming = false } = {}) {
  if (answered) return false
  return Boolean(streaming)
}

/**
 * Bring the steps in line with that, leaving alone the ones a reader has clicked.
 *
 * Clicking a step is a statement about what the reader wants to see, so a step
 * they opened stays open however the turn ends. Returns whether anything moved,
 * so a caller with no reactivity of its own can tell.
 */
export function foldSteps(steps, { answered = false, streaming = false } = {}) {
  const wantOpen = stepsShouldBeOpen({ answered, streaming })
  let changed = false
  for (const step of Array.isArray(steps) ? steps : []) {
    if (!step || step.touched || step.open === wantOpen) continue
    step.open = wantOpen
    changed = true
  }
  return changed
}

/**
 * Whether the whole process block is open right now.
 *
 * The process is everything the turn did before answering — every step's
 * thinking and what it ran — and it is folded once the turn is over, so the
 * answer is what a finished reply shows. While the turn runs it stays open:
 * the work is the only thing there is to read.
 *
 * `pinned` is the reader's own decision (null = they have not clicked), and it
 * outranks the rule for good: a reader who opened the process mid-turn keeps it
 * open when the answer lands, and one who folded a running turn keeps it shut.
 *
 * The condition is deliberately "is the turn still running" and not "has it
 * produced text yet": a step's text arrives as a preamble and is cleared at
 * `step_end`, so the latter flaps — the process would fold and unfold several
 * times inside one turn.
 */
export function processShouldBeOpen({ streaming = false, pinned = null } = {}) {
  if (pinned !== null && pinned !== undefined) return Boolean(pinned)
  return Boolean(streaming)
}

/**
 * The tool names a step called, for its summary line and the answer's own line
 * ("read_file, bash 等 5 个工具").
 */
export function toolNamesOf(steps, { limit = Infinity } = {}) {
  const names = []
  for (const step of Array.isArray(steps) ? steps : []) {
    for (const tool of (step && step.tools) || []) {
      if (tool && tool.name && names.length < limit) names.push(tool.name)
    }
  }
  return names
}
