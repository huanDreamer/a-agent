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
 * the line's anchor.
 */
export function statsSegments(raw) {
  const s = normalizeStats(raw)
  const segments = [
    {
      key: 'turns',
      text: `轮 ${formatCount(s.turns)}`,
      title: `共 ${formatCount(s.turns)} 轮：你发起的请求数`,
    },
  ]
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
        '（来自工具调用审计日志）',
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
export function statsTitle(raw) {
  return [
    ...statsSegments(raw).map((segment) => segment.title),
    '两个耗时都不是这段对话的墙钟长度：排队、步与步之间的思考、以及你本人停顿的时间都不计在内。',
  ].join('\n')
}
