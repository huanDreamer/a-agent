// Formatting helpers shared by every view.
//
// Design rules:
//  - every helper is null-safe and returns DASH ("—") for missing input, so a
//    partial API response never renders as "NaN", "undefined" or a blank cell;
//  - counts get thousands separators, token totals get a compact form ("1.2M");
//  - an unpriced cost is *unknown*, not zero, and is rendered as "—".

export const DASH = '—'

function toNumber(value) {
  if (typeof value === 'number') return Number.isFinite(value) ? value : null
  if (typeof value === 'string' && value.trim() !== '') {
    const n = Number(value)
    return Number.isFinite(n) ? n : null
  }
  return null
}

function stripTrailingZeros(text) {
  return text.includes('.') ? text.replace(/\.?0+$/, '') : text
}

function pad2(n) {
  return String(n).padStart(2, '0')
}

/** 1234 -> "1,234" ; used for call counts, user counts, session counts. */
export function formatCount(value) {
  const n = toNumber(value)
  if (n === null) return DASH
  return Math.round(n).toLocaleString('en-US')
}

/** 1234567 -> "1.2M" ; used for token counts. */
export function formatCompact(value, digits = 1) {
  const n = toNumber(value)
  if (n === null) return DASH
  const abs = Math.abs(n)
  if (abs < 1000) return String(Math.round(n))
  const units = [
    { size: 1e9, suffix: 'B' },
    { size: 1e6, suffix: 'M' },
    { size: 1e3, suffix: 'K' },
  ]
  for (const unit of units) {
    if (abs >= unit.size) {
      const scaled = n / unit.size
      const text = Math.abs(scaled) >= 100 ? scaled.toFixed(0) : scaled.toFixed(digits)
      return `${stripTrailingZeros(text)}${unit.suffix}`
    }
  }
  return String(Math.round(n))
}

/** Exact token count with separators, for `title` tooltips next to a compact value. */
export function formatExactTokens(value) {
  const n = toNumber(value)
  if (n === null) return DASH
  return `${Math.round(n).toLocaleString('en-US')} tokens`
}

/**
 * Cost in USD with 4 decimals. `cost` is the API's cost object; when
 * `cost.priced` is false no price-table entry matched, so the amount is
 * unknown and we render "—" instead of a misleading $0.0000.
 */
export function formatCost(cost) {
  if (!cost) return DASH
  if (cost.priced === false) return DASH
  return formatMoney(cost.total)
}

/** A bare USD amount, or "—" when the amount is missing. */
export function formatMoney(value) {
  const n = toNumber(value)
  if (n === null) return DASH
  if (n === 0) return '$0.0000'
  if (Math.abs(n) < 0.00005) return '< $0.0001'
  return `$${n.toFixed(4)}`
}

/** True when a cost object carries no usable price information. */
export function isCostUnknown(cost) {
  return !cost || cost.priced === false
}

/** 12 -> "12ms", 1234 -> "1.2s", 90000 -> "1m30s". */
export function formatDuration(ms) {
  const n = toNumber(ms)
  if (n === null || n < 0) return DASH
  if (n < 1000) return `${Math.round(n)}ms`
  const seconds = n / 1000
  if (seconds < 60) return `${seconds.toFixed(1)}s`
  const minutes = Math.floor(seconds / 60)
  const rest = Math.round(seconds % 60)
  if (minutes < 60) return `${minutes}m${pad2(rest)}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h${pad2(minutes % 60)}m`
}

/** Average latency = total duration / calls, rendered with formatDuration. */
export function formatAverageDuration(totalMs, calls) {
  const total = toNumber(totalMs)
  const count = toNumber(calls)
  if (total === null || !count) return DASH
  return formatDuration(total / count)
}

/** "3 分钟前" — relative timestamp for recent/audit rows. */
export function formatRelative(iso, now = Date.now()) {
  if (!iso) return DASH
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return String(iso)
  const diff = now - t
  if (diff < 0) return '刚刚'
  const seconds = Math.floor(diff / 1000)
  if (seconds < 10) return '刚刚'
  if (seconds < 60) return `${seconds} 秒前`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} 分钟前`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} 小时前`
  const days = Math.floor(hours / 24)
  if (days < 30) return `${days} 天前`
  const months = Math.floor(days / 30)
  if (months < 12) return `${months} 个月前`
  return `${Math.floor(months / 12)} 年前`
}

/** "2026-09-06 18:00:00" in local time — the `title` of a relative timestamp. */
export function formatAbsolute(iso) {
  if (!iso) return DASH
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return String(iso)
  return (
    `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ` +
    `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`
  )
}

/** "2026-09-06" -> "09-06" for chart axis labels. */
export function formatDayShort(day) {
  if (!day) return ''
  const text = String(day)
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(text)
  return m ? `${m[2]}-${m[3]}` : text
}

/** "2026-09-06" -> "2026-09-06（周六）" for tooltips. */
export function formatDayLong(day) {
  if (!day) return DASH
  const d = new Date(`${day}T00:00:00`)
  if (Number.isNaN(d.getTime())) return String(day)
  const week = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'][d.getDay()]
  return `${day}（${week}）`
}

/** Shorten a long id (Feishu open_id, session id) but keep both ends readable. */
export function shortId(id, head = 6, tail = 4) {
  if (id === null || id === undefined || id === '') return DASH
  const text = String(id)
  if (text.length <= head + tail + 1) return text
  return `${text.slice(0, head)}…${text.slice(-tail)}`
}

/** Truncate free text for a table cell; the full value belongs in a title. */
export function truncate(text, max = 60) {
  if (text === null || text === undefined) return ''
  const value = String(text)
  if (value.length <= max) return value
  return `${value.slice(0, max)}…`
}

/** Share of a total in percent, for bar widths. Never returns NaN. */
export function percentOf(value, total) {
  const v = toNumber(value)
  const t = toNumber(total)
  if (v === null || !t || t <= 0) return 0
  return Math.max(0, Math.min(100, (v / t) * 100))
}
