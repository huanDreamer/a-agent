// A MarkdownText stand-in for the render probe, and only for the probe.
//
// The real component renders through src/markdown.js, which builds DOM (it
// decorates code blocks and tables after sanitizing) and therefore cannot run in
// Node without a DOM. The probe needs to render 对话's message bubble — the one
// component that owns the structure of a turn — and that structure is exactly
// what must be asserted: which step holds which thinking, and that the answer
// stands outside the folded process.
//
// So this renders the same text as a plain block. Every assertion the probe makes
// is therefore about text and structure, and none about markup or sanitizing —
// which the browser build still does through the real component and the real
// sanitizer, the single path to v-html.
import { h } from 'vue'

export default {
  name: 'MarkdownTextProbe',
  props: {
    text: { type: String, default: '' },
    streaming: { type: Boolean, default: false },
  },
  render() {
    if (!this.text) return null
    return h('div', { class: 'md md-probe' }, this.text)
  },
}
