// 产物中心 — the rules behind the table, kept out of the component.
//
// The panel is a list plus a filter, and every way it can mislead is in the
// reading of the payload rather than in the markup: a session cell that shows
// nothing where the server could not resolve a title, an orphan artifact (one
// from a one-shot run, with no session) counted as if it belonged somewhere, a
// filter that drops rows it should keep. All of that is asserted by
// scripts/check-artifacts.mjs without a browser.
import { shortId } from './format.js'

/**
 * Narrow the list to what the controls ask for.
 *
 * The filter is client-side because the whole list is already in the response:
 * a round trip per keystroke would be a slower way to look at the same bytes.
 * Both dimensions are case-insensitive substring matches, and an empty control
 * is "no opinion" rather than "match nothing".
 */
export function filterArtifacts(rows, { kind = '', session = '' } = {}) {
  const list = Array.isArray(rows) ? rows : []
  const needle = String(session || '').trim().toLowerCase()
  return list.filter((item) => {
    if (!item) return false
    if (kind && item.kind !== kind) return false
    if (needle) {
      const title = String(item.session_title || '').toLowerCase()
      const id = String(item.session_id || '').toLowerCase()
      if (!title.includes(needle) && !id.includes(needle)) return false
    }
    return true
  })
}

/**
 * The session cell.
 *
 * A title when the server resolved one, the shortened id when it did not, and an
 * honest "—" when there is no session at all. The last case is not an error: an
 * artifact from a one-shot `huan-agent run` has no conversation to belong to, and
 * showing a blank cell for it would read as a bug in the console.
 */
export function sessionLabel(item) {
  if (!item || !item.session_id) return '—'
  return item.session_title || shortId(item.session_id, 8, 4)
}

/** Total stored bytes of the rows on screen. */
export function totalBytes(rows) {
  const list = Array.isArray(rows) ? rows : []
  return list.reduce((sum, item) => sum + Number((item && item.bytes) || 0), 0)
}

/** How many of the rows have no owning session. */
export function orphanCount(rows) {
  const list = Array.isArray(rows) ? rows : []
  return list.filter((item) => !item || !item.session_id).length
}

/**
 * The local calendar day of an artifact, as "YYYY-MM-DD", or '' when unknown.
 *
 * Local rather than UTC, because the day an artifact belongs to is the day the
 * person reading the list had, not the day the server's clock was on: a file
 * saved at 23:30 in Shanghai would otherwise be filed under tomorrow. The stored
 * path's date segment is UTC and is a storage detail; this is the grouping the
 * console shows.
 */
export function dayKey(iso) {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const pad = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/**
 * Group rows by local day, newest day first and newest row first within a day.
 *
 * The rows arrive newest-first from the server, so this preserves their order
 * rather than re-sorting: the group is a way to read the same list, and a
 * re-sort here would be a second opinion about the order the server already
 * decided. A row whose timestamp cannot be parsed lands in a trailing bucket
 * keyed '', which is rendered as a plain "未知时间" head rather than dropped —
 * losing a row to a bad timestamp would be worse than showing it unlabelled.
 */
export function groupByDay(rows) {
  const list = Array.isArray(rows) ? rows : []
  const buckets = new Map()
  for (const item of list) {
    if (!item) continue
    const key = dayKey(item.created_at)
    if (!buckets.has(key)) buckets.set(key, [])
    buckets.get(key).push(item)
  }
  const dated = [...buckets.keys()].filter((k) => k !== '').sort().reverse()
  const groups = dated.map((day) => ({ day, rows: buckets.get(day) }))
  if (buckets.has('')) groups.push({ day: '', rows: buckets.get('') })
  return groups
}
