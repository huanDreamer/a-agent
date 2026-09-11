// Server-Sent Events framing, by hand.
//
// The chat endpoint is a POST that answers `text/event-stream`, so EventSource
// (GET-only) cannot be used and the response body must be read as a stream
// while it is still being written. This module owns only the byte-level part:
// it turns arbitrary chunks into complete frames.
//
// It deliberately touches no browser API, so it can be exercised directly from
// plain Node (see the fetch-based test in the repo README / verification notes).
//
// Wire format accepted — a superset of what the Go server writes:
//   "data: {json}\n\n"   one JSON payload
//   ": ping\n\n"         heartbeat comment, ignored
// Frames are separated by a blank line, and a chunk may split a frame anywhere
// — including in the middle of a line — hence the internal buffer.

/**
 * Create a streaming parser.
 *
 * `push(chunk)` returns every frame that became complete with this chunk;
 * `flush()` returns a trailing frame that never got its blank-line terminator
 * (a server that closes the response right after the last `data:` line).
 */
export function createSseParser() {
  let buffer = ''

  function toFrame(raw) {
    if (raw.trim() === '') return null
    const data = []
    for (const line of raw.split('\n')) {
      // A line starting with ':' is a comment — the server's `: ping` heartbeat.
      if (line === '' || line.startsWith(':')) continue
      if (!line.startsWith('data:')) continue // event: / id: / retry: are unused
      data.push(line.slice(5).replace(/^ /, ''))
    }
    if (!data.length) return null
    return { data: data.join('\n') }
  }

  return {
    push(chunk) {
      buffer += chunk
      // Normalised *after* appending, because one chunk may end with "\r" and
      // the next may start with "\n".
      if (buffer.indexOf('\r') >= 0) buffer = buffer.replace(/\r\n/g, '\n')

      const frames = []
      let cut = buffer.indexOf('\n\n')
      while (cut >= 0) {
        const raw = buffer.slice(0, cut)
        buffer = buffer.slice(cut + 2)
        const frame = toFrame(raw)
        if (frame) frames.push(frame)
        cut = buffer.indexOf('\n\n')
      }
      return frames
    },

    flush() {
      const raw = buffer
      buffer = ''
      const frame = toFrame(raw)
      return frame ? [frame] : []
    },
  }
}

/** Decode one `data:` payload; a malformed frame yields null instead of throwing. */
export function parseSsePayload(frame) {
  if (!frame || typeof frame.data !== 'string') return null
  try {
    return JSON.parse(frame.data)
  } catch (err) {
    return null
  }
}
