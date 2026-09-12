// Markdown → sanitized HTML for assistant answers and reasoning text.
//
// INVARIANT — THE ONLY PATH TO v-html:
//   `renderMarkdown()` below is the single function in this codebase that turns
//   model text into an HTML string: it parses (marked, GFM) AND sanitizes
//   (DOMPurify) in one call. Anything a model writes is untrusted input, so no
//   component may build HTML any other way — do not call marked or DOMPurify
//   directly, do not add a second `v-html`, and do not interpolate model text
//   into a string that becomes markup. The markup this file *adds* to the
//   sanitizer's output (code-block chrome, table wrappers, task-list checkboxes)
//   is built with createElement/textContent from data that is already sanitized,
//   never by concatenating untrusted text.
//
// Shape of the pipeline, per call:
//   1. repairStreaming()  — keep partial SSE text structurally stable;
//   2. marked             — GFM → HTML (raw HTML passes through here);
//   3. DOMPurify          — drop everything dangerous (see PURIFY_CONFIG);
//   4. decorate()         — wrap tables, add per-code-block chrome (label +
//                           copy button), upgrade task markers. All of this
//                           happens *after* sanitizing, on the DOM.
//
// Results are memoized by input, so a message list that renders the same string
// twice (and any other duplicate render) parses it once.

import { Marked } from 'marked'
import DOMPurify from 'dompurify'
import { ICON_PATHS } from './icons.js'

/* -------------------------------------------------------------- 1. parser -- */

/** GFM, and `breaks` because this is a chat: a single newline is a <br>. */
const parser = new Marked({ gfm: true, breaks: true })

parser.use({
  renderer: {
    // The task-list marker is emitted as a <span>, not marked's default
    // <input type="checkbox">: `input` is forbidden in the sanitizer, so a
    // model-authored <input> can never survive (verified in the browser). The
    // placeholder carries the state; decorate() turns it into a real, disabled
    // checkbox after sanitizing.
    checkbox(token) {
      return `<span class="md-task" data-checked="${token.checked ? '1' : '0'}"></span>`
    },
  },
})

/* ----------------------------------------------------------- 2. sanitizer -- */

// Everything here is denied *and* the reasonable defaults stay on, so this list
// is the explicit statement of the requirement rather than the whole policy.
// `img` is denied on purpose: a model-authored `<img src="http://…">` is a
// tracking beacon that fires when the message renders.
const FORBID_TAGS = [
  'script',
  'style',
  'iframe',
  'object',
  'embed',
  'form',
  'input',
  'button',
  'link',
  'meta',
  'base',
  'img',
  'svg',
  'math',
  'frame',
  'frameset',
  'applet',
  'audio',
  'video',
  'source',
  'track',
  'textarea',
  'select',
  'option',
  'template',
  'title',
  'noscript',
]
const FORBID_ATTR = ['style', 'srcset', 'formaction', 'action', 'method', 'xlink:href', 'ping']

// An allow-list of schemes, so `javascript:`, `data:` and `vbscript:` are
// stripped by construction rather than by blocklisting: http, https, mailto,
// plus scheme-less links (#anchor, /path, ./relative, note.md). Everything a
// browser would treat as a script URL fails this.
const ALLOWED_URI_REGEXP = /^(?:(?:https?|mailto):|[^a-z]|[a-z+.-]+(?:[^a-z+.-:]|$))/i

const PURIFY_CONFIG = {
  FORBID_TAGS,
  FORBID_ATTR,
  ALLOWED_URI_REGEXP,
  RETURN_TRUSTED_TYPE: false,
  // SAFE_FOR_TEMPLATES is deliberately NOT set. It exists for apps that hand
  // the sanitized string to a client-side template compiler, and it pays for
  // that by deleting every `{{ … }}` from the text — which would silently
  // corrupt the Jinja/Handlebars/Go-template samples a coding agent writes.
  // Nothing here compiles this HTML: it goes straight to `v-html`, and Vue
  // does not compile the contents of a v-html binding.
}

// Every link gets the same treatment, added after its attributes were sanitized
// (so `href` is already the sanitized one, and a stripped one stays stripped).
function hardenLinks(node) {
  if (node.tagName !== 'A') return
  node.setAttribute('target', '_blank')
  node.setAttribute('rel', 'noopener noreferrer')
  node.setAttribute('class', 'md-link')
}
// removeHook first: this module can be re-evaluated (HMR), and hooks accumulate
// otherwise. There is exactly one hook, and it is this one.
DOMPurify.removeHook('afterSanitizeAttributes')
DOMPurify.addHook('afterSanitizeAttributes', hardenLinks)

/* ------------------------------------------------- 3. streaming stability -- */

/**
 * Rewrite *only* the ambiguous tail of a partial message so the rendered
 * structure does not jump around while a turn streams.
 *
 * The one case worth repairing: GFM needs a table's delimiter row to have a
 * cell per column, and every cell has to look like `:?-+:?`. A model emits
 * `| a | b |` and then types `|---|` … `|---|---|`, so for a few deltas the
 * delimiter row is incomplete or has a half-typed cell (`:`, `-`, ` `). GFM
 * rejects all of those outright, which makes the message flash from a table
 * back to a paragraph of raw `|` pipes and then to a table again. While
 * streaming, finish the row the way the model is about to: pad it to the
 * header's column count and replace any not-yet-valid cell with `---`.
 *
 * Nothing else is rewritten: an unclosed fence, list, blockquote or heading is
 * already parsed as its final structure by CommonMark/GFM (an unterminated
 * fence extends to the end of the text), so those need no help. The repair is
 * applied only while streaming and never changes the final render of a
 * complete message.
 */
function repairStreaming(text, streaming) {
  if (!streaming || text.length < 4) return text

  const lines = text.split('\n')
  const tail = lines[lines.length - 1]
  // A delimiter row still being typed: pipes, dashes, colons and spaces only.
  if (!/^[|\s:-]*$/.test(tail) || !tail.includes('-')) return text

  const header = lines.length >= 2 ? lines[lines.length - 2] : ''
  if (!header.includes('|')) return text

  const columns = cellCount(header)
  const cells = tail
    .replace(/^\s*\|/, '')
    .replace(/\|\s*$/, '')
    .split('|')
    .map((cell) => (/^:?-+:?$/.test(cell.trim()) ? cell.trim() : '---'))
  if (columns < 2 || cells.length > columns) return text

  while (cells.length < columns) cells.push('---')
  lines[lines.length - 1] = `|${cells.join('|')}|`
  return lines.join('\n')
}

/** `| a | b |` → 2. A trailing pipe is a border, not a cell. */
function cellCount(row) {
  const trimmed = row.trim().replace(/^\|/, '').replace(/\|$/, '')
  if (trimmed === '') return 0
  return trimmed.split('|').length
}

/* ----------------------------------------------------------- 4. decoration -- */

/** Same geometry as Icon.vue's copy/check glyphs (see ../icons.js). */
function iconElement(name, className) {
  const NS = 'http://www.w3.org/2000/svg'
  const svg = document.createElementNS(NS, 'svg')
  for (const [key, value] of Object.entries({
    class: `ico ${className}`,
    width: 14,
    height: 14,
    viewBox: '0 0 24 24',
    fill: 'none',
    stroke: 'currentColor',
    'stroke-width': 2,
    'stroke-linecap': 'round',
    'stroke-linejoin': 'round',
    'aria-hidden': 'true',
    focusable: 'false',
  })) {
    svg.setAttribute(key, String(value))
  }
  // copy/check are plain path sets (no <rect> segment), like Icon.vue renders.
  for (const d of String(ICON_PATHS[name] || '').split('|')) {
    const path = document.createElementNS(NS, 'path')
    path.setAttribute('d', d)
    svg.append(path)
  }
  return svg
}

/** Label shown for a fenced/indented code block: its info string. */
function languageOf(code) {
  const match = /(?:^|\s)language-([^\s]+)/.exec(code.className || '')
  if (!match) return 'text'
  // Decoded by the DOM already; shown as-is (textContent), capped so a silly
  // info string cannot stretch the header.
  return match[1].slice(0, 24)
}

function copyButtonElement() {
  const button = document.createElement('button')
  button.type = 'button'
  button.className = 'btn sm ghost md-copy'
  button.setAttribute('aria-label', '复制代码')
  button.setAttribute('data-copied', '0')

  const add = (tag, className, text) => {
    const el = document.createElement(tag)
    el.className = className
    if (text !== undefined) el.textContent = text
    return el
  }
  // Both glyphs ship in the button; CSS shows one or the other from
  // data-copied, so the click handler only flips that attribute — the same
  // '复制 / 已复制 / 复制失败' feedback the message-level button gives.
  button.append(
    iconElement('copy', 'md-i-copy'),
    iconElement('check', 'md-i-done'),
    add('span', 'md-t-idle', '复制'),
    add('span', 'md-t-ok', '已复制'),
    add('span', 'md-t-fail', '复制失败'),
  )
  return button
}

/**
 * Add the code-block header (language + copy) and the scrollable table wrapper
 * to already-sanitized markup. Everything here is built with createElement and
 * textContent from nodes that survived the sanitizer — no untrusted string is
 * ever concatenated into markup. Idempotent: an element that already has its
 * wrapper is left alone.
 */
function decorate(root) {
  for (const code of [...root.querySelectorAll('pre > code')]) {
    const pre = code.parentElement
    if (pre.parentElement && pre.parentElement.classList.contains('md-code-block')) continue

    const block = document.createElement('div')
    block.className = 'md-code-block'
    pre.replaceWith(block)

    const head = document.createElement('div')
    head.className = 'md-code-head'
    const lang = document.createElement('span')
    lang.className = 'md-lang'
    lang.textContent = languageOf(code)
    head.append(lang, copyButtonElement())

    block.append(head, pre)
  }

  for (const table of [...root.querySelectorAll('table')]) {
    if (table.parentElement && table.parentElement.classList.contains('md-table-wrap')) continue
    const wrap = document.createElement('div')
    wrap.className = 'md-table-wrap'
    wrap.setAttribute('role', 'region')
    wrap.setAttribute('aria-label', '表格')
    wrap.setAttribute('tabindex', '0')
    table.replaceWith(wrap)
    wrap.append(table)
  }

  // <span class="md-task" data-checked="…"> → a real disabled checkbox, built
  // here rather than parsed, so `input` can stay forbidden in the sanitizer.
  for (const marker of [...root.querySelectorAll('span.md-task')]) {
    const box = document.createElement('input')
    box.type = 'checkbox'
    box.disabled = true
    box.className = 'md-task-box'
    if (marker.getAttribute('data-checked') === '1') {
      box.checked = true
      box.setAttribute('checked', '')
    }
    marker.replaceWith(box)
  }
}

/** Last-resort render when the parser gives up (see renderMarkdown). */
function plainText(text) {
  const p = document.createElement('p')
  p.className = 'md-p'
  p.textContent = text
  return p.outerHTML
}

/* ------------------------------------------------------------ entry point -- */

const CACHE_LIMIT = 80
/** Input text → rendered HTML. Ordered by insertion, so the oldest is evicted. */
const cache = new Map()

/**
 * Parse and sanitize `text`; returns HTML that is safe to pass to `v-html`.
 *
 * This is the only function allowed to produce markup from model output — see
 * the invariant at the top of this file.
 *
 * @param {string} text markdown, complete or still streaming
 * @param {{streaming?: boolean}} [options] `streaming: true` while the turn is
 *   arriving, which enables the partial-table repair above.
 * @returns {string} sanitized HTML ('' for empty input)
 */
export function renderMarkdown(text, options = {}) {
  const value = text === null || text === undefined ? '' : String(text)
  if (value === '') return ''

  const streaming = options.streaming === true
  const key = `${streaming ? 'S' : 'F'}\u0000${value}`
  const cached = cache.get(key)
  if (cached !== undefined) {
    // Refresh its position so eviction drops a message nobody renders anymore.
    cache.delete(key)
    cache.set(key, cached)
    return cached
  }

  let html
  try {
    html = parser.parse(repairStreaming(value, streaming))
  } catch (err) {
    // Untrusted text can defeat the parser: marked recurses on nesting and
    // throws RangeError on pathological input (thousands of nested `>`), which
    // must not take the message list down mid-stream. Show it as plain text.
    html = plainText(value)
  }

  const safe = DOMPurify.sanitize(html, PURIFY_CONFIG)
  const template = document.createElement('template')
  template.innerHTML = safe
  decorate(template.content)
  const rendered = template.innerHTML

  cache.set(key, rendered)
  if (cache.size > CACHE_LIMIT) cache.delete(cache.keys().next().value)
  return rendered
}
