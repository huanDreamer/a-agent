// The conversation header's statistics line.
//
// The numbers come from GET /api/chat/sessions/{id} (`stats`), which the server
// aggregates from what is stored — so they survive a reload and are the same for
// every client. This module only decides how they read: which fields are worth a
// segment, in what order, and what each one's tooltip says.
//
// It is a module of its own, and pure, because that decision is the part worth
// checking without a browser (see scripts/check-stats.mjs) — the panel is then a
// loop over the segments instead of five places that format a number.
import { formatCompact, formatCount, formatDuration } from './format.js'

/**
 * normalizeStats reads a `stats` payload into the fields this module renders.
 *
 * Every field is coerced to a non-negative integer: the header must not print
 * `NaN 次` because a server grew a field this build does not know yet.
 */
export function normalizeStats(raw) {
  const s = raw && typeof raw === 'object' ? raw : {}
  const n = (v) => {
    const value = Number(v)
    return Number.isFinite(value) && value > 0 ? Math.floor(value) : 0
  }
  return {
    messages: n(s.messages),
    turns: n(s.turns),
    llmCalls: n(s.llm_calls),
    llmDurationMs: n(s.llm_duration_ms),
    toolCalls: n(s.tool_calls),
    toolDurationMs: n(s.tool_duration_ms),
    promptTokens: n(s.prompt_tokens),
    completionTokens: n(s.completion_tokens),
    totalTokens: n(s.total_tokens),
  }
}

/**
 * statsSegments turns a normalized payload into the header's segments.
 *
 * A field with nothing to say is left out rather than rendered as a zero: a
 * conversation that has made no model call yet shows "轮 0" and stops there,
 * which is more informative than four zeros competing for the same line.
 *
 * `turns` is always present — it is the one number that is meaningful at 0, and
 * the line's anchor — and `messageCount` (the session's own count, passed in by
 * the header) sits directly behind it: together they say how much conversation
 * there is, before the segments that say what it cost. It is not read from the
 * payload because the session row carries it and it is the number the sidebar
 * counts; a header whose message count disagreed with the list would be worse
 * than one without it.
 */
export function statsSegments(raw, { messageCount } = {}) {
  const s = normalizeStats(raw)
  const segments = [
    {
      key: 'turns',
      text: `轮 ${formatCount(s.turns)}`,
      title: `共 ${formatCount(s.turns)} 轮：你发起的请求数`,
    },
  ]
  // A number is rendered even at 0 — like `turns`, "no messages yet" is a fact
  // worth the four characters. Absent (a caller that has no session) means the
  // segment is not this module's to invent.
  const messages = Number(messageCount)
  if (Number.isFinite(messages) && messages >= 0) {
    segments.push({
      key: 'messages',
      text: `共 ${formatCount(messages)} 条消息`,
      title: `共 ${formatCount(messages)} 条消息：你说的每一句和模型写下的每一段都算一条`,
    })
  }
  if (s.llmCalls > 0) {
    segments.push({
      key: 'llm',
      text: `模型 ${formatCount(s.llmCalls)} 次 · ${formatDuration(s.llmDurationMs)}`,
      title:
        `模型调用 ${formatCount(s.llmCalls)} 次，服务端计时合计 ${formatDuration(s.llmDurationMs)}` +
        '（每次 assistant 消息即一次调用；耗时是 provider 上报的单次耗时之和）',
    })
  }
  if (s.toolCalls > 0) {
    segments.push({
      key: 'tools',
      text: `工具 ${formatCount(s.toolCalls)} 次 · ${formatDuration(s.toolDurationMs)}`,
      title:
        `工具调用 ${formatCount(s.toolCalls)} 次，实测耗时合计 ${formatDuration(s.toolDurationMs)}` +
        '（来自每一轮存下来的过程记录，与每条回答下面那行「执行过程」同一份数据）',
    })
  }
  if (s.totalTokens > 0) {
    segments.push({
      key: 'tokens',
      // The total is the headline; the split is on hover, because it is the
      // detail that only matters once the total looks wrong.
      text: `${formatCompact(s.totalTokens)} tokens`,
      title:
        `prompt ${formatCount(s.promptTokens)} + completion ` +
        `${formatCount(s.completionTokens)} = ${formatCount(s.totalTokens)} tokens（provider 上报）`,
    })
  }
  return segments
}

/**
 * statsTitle is the whole line's tooltip: the segments' own titles, plus the one
 * caveat a reader would otherwise read as a bug.
 */
export function statsTitle(raw, options) {
  return [
    ...statsSegments(raw, options).map((segment) => segment.title),
    '两个耗时都不是这段对话的墙钟长度：排队、步与步之间的思考、以及你本人停顿的时间都不计在内。',
  ].join('\n')
}
