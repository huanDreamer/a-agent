// Minimal markdown subset for assistant answers — hand-written on purpose
// (no renderer dependency, and nothing that can inject markup).
//
// Supported: fenced code blocks (```lang ... ```), inline `code`, **bold** and
// line breaks. Line breaks are kept inside the text and rendered with
// `white-space: pre-wrap`, so no <br> is needed.
//
// The parser returns plain data; components render it through text nodes, so
// model output can never become HTML.

const FENCE = /^\s*```(.*)$/
// Source (not a shared RegExp instance): parseInline recurses for **bold**, and
// a module-level /g regex would have its lastIndex reset by the inner call,
// making the outer scan re-match the same token forever.
const INLINE_SOURCE = '(`[^`\\n]+`|\\*\\*[^*\\n]+\\*\\*)'
/** Bound recursion: `**` nesting beyond this depth is rendered literally. */
const MAX_DEPTH = 3

/**
 * Split inline text into tokens: {type:'text'|'code'|'strong'}.
 * A strong token carries nested `tokens` so `**bold with `code`**` renders.
 */
export function parseInline(text, depth = 0) {
  const value = text === null || text === undefined ? '' : String(text)
  if (value === '') return []
  if (depth >= MAX_DEPTH) return [{ type: 'text', text: value }]

  const tokens = []
  const pattern = new RegExp(INLINE_SOURCE, 'g')
  let last = 0
  let match
  while ((match = pattern.exec(value)) !== null) {
    if (match.index > last) tokens.push({ type: 'text', text: value.slice(last, match.index) })
    const raw = match[0]
    if (raw.charAt(0) === '`') {
      tokens.push({ type: 'code', text: raw.slice(1, -1) })
    } else {
      tokens.push({ type: 'strong', tokens: parseInline(raw.slice(2, -2), depth + 1) })
    }
    last = match.index + raw.length
  }
  if (last < value.length) tokens.push({ type: 'text', text: value.slice(last) })
  return tokens
}

/**
 * Split text into blocks: {type:'code', lang, code} and
 * {type:'paragraph', tokens}. Blank lines separate paragraphs; an unclosed
 * fence swallows the rest of the text rather than dropping it.
 */
export function parseBlocks(text) {
  const value = text === null || text === undefined ? '' : String(text)
  if (value === '') return []

  const lines = value.split('\n')
  const blocks = []
  let paragraph = []

  function flushParagraph() {
    if (!paragraph.length) return
    const joined = paragraph.join('\n')
    if (joined.trim() !== '') blocks.push({ type: 'paragraph', tokens: parseInline(joined) })
    paragraph = []
  }

  for (let i = 0; i < lines.length; i += 1) {
    const fence = FENCE.exec(lines[i])
    if (fence) {
      flushParagraph()
      const lang = fence[1].trim()
      const code = []
      i += 1
      while (i < lines.length && !FENCE.test(lines[i])) {
        code.push(lines[i])
        i += 1
      }
      // The for-loop increment steps over the closing fence when there is one.
      blocks.push({ type: 'code', lang, code: code.join('\n') })
      continue
    }
    if (lines[i].trim() === '') {
      flushParagraph()
      continue
    }
    paragraph.push(lines[i])
  }
  flushParagraph()
  return blocks
}
