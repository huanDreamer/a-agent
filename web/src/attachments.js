// Attachment metadata for the 对话 surface.
//
// The API hands out attachment metadata in two very different shapes:
//
//   1. POST /api/chat/sessions/{id}/attachments → the full record (id, kind,
//      mime, bytes, name, path, url). That is what the composer holds while a
//      file is pending, and what the live turn renders.
//   2. the persisted user message's `attachments` column → a JSON *string*
//      holding an array of asset ids, exactly like `tool_calls` and `usage`.
//      A reloaded conversation therefore knows the ids and nothing else.
//
// This module is the single place that reconciles the two. `normalize()` parses
// the column defensively (a half-written value must never break the
// conversation), and `ensure()` fills in the missing kind/mime for an id-only
// attachment by looking at the Content-Type of the very bytes the server serves
// — the only description of an asset the contract exposes.

import { reactive } from 'vue'
import { assetUrl } from './api.js'

/** id -> { id, url, mime, kind, name, bytes }, shared by every view. */
const attachmentMeta = reactive({})

/** ids already being resolved, so N bubbles never issue N requests. */
const inflight = new Map()

/** Keep only what both shapes can carry; `url` is always derivable from an id. */
function record(id, source) {
  const meta = {
    id,
    url: assetUrl({ id, url: source && source.url }),
    mime: (source && source.mime) || '',
    kind: (source && source.kind) || '',
    name: (source && source.name) || '',
    bytes: typeof (source && source.bytes) === 'number' ? source.bytes : null,
  }
  if (!meta.kind) meta.kind = kindFromMime(meta.mime)
  return meta
}

/** The documented kinds are image / audio; anything else is treated as a file. */
function kindFromMime(mime) {
  const value = String(mime || '').toLowerCase()
  if (value.startsWith('image/')) return 'image'
  if (value.startsWith('audio/')) return 'audio'
  return ''
}

/**
 * Remember what an upload described. Called for every returned record, so the
 * pending chip and the live bubble share one entry.
 */
export function remember(attachment) {
  if (!attachment || !attachment.id) return null
  const meta = record(attachment.id, attachment)
  attachmentMeta[attachment.id] = meta
  return meta
}

/** Merge metadata mined from the server (mime sniffing) without losing names. */
function absorb(id, patch) {
  const current = attachmentMeta[id] || record(id, null)
  attachmentMeta[id] = { ...current, ...patch, url: current.url }
  return attachmentMeta[id]
}

/**
 * Look up an attachment, resolving the mime type on a cache miss.
 *
 * The GET returns the raw bytes; only the headers are needed, so the body is
 * cancelled immediately — a 20 MB image must not be downloaded twice just to
 * learn that it is an image.
 */
export function ensure(id) {
  if (!id) return Promise.resolve(null)
  const known = attachmentMeta[id]
  if (known && known.mime) return Promise.resolve(known)
  if (inflight.has(id)) return inflight.get(id)

  const meta = known || record(id, null)
  attachmentMeta[id] = meta

  const task = fetch(meta.url, { credentials: 'same-origin', method: 'GET' })
    .then((response) => {
      const mime = (response.headers.get('Content-Type') || '').split(';')[0].trim()
      const length = Number(response.headers.get('Content-Length'))
      const patch = { mime, kind: kindFromMime(mime) }
      if (Number.isFinite(length) && length > 0) patch.bytes = length
      // Read nothing: the headers are the whole point.
      if (response.body && typeof response.body.cancel === 'function') {
        response.body.cancel().catch(() => {})
      }
      return absorb(id, patch)
    })
    .catch(() => {
      // Unknown type is not an error the reader needs: it renders as a file
      // chip with its id, which is still better than an empty bubble.
      return absorb(id, {})
    })
    .finally(() => {
      inflight.delete(id)
    })

  inflight.set(id, task)
  return task
}

/** The reactive record for an id, or null when it was never seen. */
export function metaOf(id) {
  return attachmentMeta[id] || null
}

/**
 * Normalise the `attachments` field of a message (or the composer's pending
 * list) into renderable records.
 *
 * Accepts the JSON string the database stores, an already-parsed array, plain
 * id strings, and full records — a server that starts persisting objects
 * instead of ids keeps working.
 */
export function normalize(raw) {
  if (!raw) return []
  let list = raw
  if (typeof raw === 'string') {
    const trimmed = raw.trim()
    if (trimmed === '') return []
    try {
      list = JSON.parse(trimmed)
    } catch (err) {
      return [] // defensive: never let a broken column blank the message
    }
  }
  if (!Array.isArray(list)) return []
  const out = []
  for (const entry of list) {
    if (typeof entry === 'string') {
      const id = entry.trim()
      if (id) out.push(attachmentMeta[id] || record(id, null))
      continue
    }
    if (entry && typeof entry === 'object' && entry.id) {
      out.push(attachmentMeta[entry.id] || record(entry.id, entry))
    }
  }
  return out
}
