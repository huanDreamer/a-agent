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
