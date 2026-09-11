// Metric definitions for the aggregate rows shared by summary/by-model/
// by-user/by-day responses: {calls, prompt_tokens, completion_tokens,
// total_tokens, duration_ms, cost:{prompt_cost, completion_cost, total, priced}}.
//
// A metric knows how to turn a row into (a) a number for bar widths and
// (b) a human string for value labels — including the "cost is unknown" case,
// where the bar is empty and the label is "—" rather than "$0.0000".

import { formatCompact, formatCost, formatCount, formatDuration, DASH } from './format.js'

function num(value) {
  const n = Number(value)
  return Number.isFinite(n) ? n : 0
}

export const BAR_METRICS = [
  {
    key: 'calls',
    label: '调用次数',
    color: '#58a6ff',
    tone: '',
    barValue: (row) => num(row.calls),
    text: (row) => formatCount(row.calls),
  },
  {
    key: 'total_tokens',
    label: '总 token',
    color: '#bc8cff',
    tone: 'p',
    barValue: (row) => num(row.total_tokens),
    text: (row) => formatCompact(row.total_tokens),
  },
  {
    key: 'cost',
    label: '费用',
    color: '#3fb950',
    tone: 'g',
    barValue: (row) => (row.cost && row.cost.priced !== false ? num(row.cost.total) : 0),
    text: (row) => formatCost(row.cost),
    /** Cost is unknown (no price entry) — render the label but no bar. */
    unknown: (row) => !row.cost || row.cost.priced === false,
  },
]

export function barMetric(key) {
  return BAR_METRICS.find((m) => m.key === key) || BAR_METRICS[0]
}

/** Secondary line under a bar's value: calls + tokens + total latency. */
export function rowDetail(row) {
  const parts = []
  if (row && row.calls !== undefined && row.calls !== null) {
    parts.push(`调用 ${formatCount(row.calls)} 次`)
  }
  if (row && row.total_tokens !== undefined && row.total_tokens !== null) {
    parts.push(`${formatCompact(row.total_tokens)} tokens`)
  }
  if (row && row.duration_ms !== undefined && row.duration_ms !== null) {
    parts.push(`耗时 ${formatDuration(row.duration_ms)}`)
  }
  return parts
}
