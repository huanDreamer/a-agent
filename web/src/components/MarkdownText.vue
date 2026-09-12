<script setup>
// Renders assistant answer / reasoning text as GitHub-flavoured Markdown.
//
// The HTML comes from renderMarkdown() in ../markdown.js — the single
// parse-and-sanitize entry point. Nothing else in this component builds markup:
// `v-html` is fed only that function's output, and the interactive chrome it
// contains (per-code-block copy buttons) is handled here by delegation, so the
// button markup never needs a second renderer.
import { computed, ref } from 'vue'
import { renderMarkdown } from '../markdown.js'

const props = defineProps({
  text: { type: String, default: '' },
  /** True while the turn is still arriving: keeps partial markdown stable. */
  streaming: { type: Boolean, default: false },
})

const host = ref(null)

// Memoized by input inside renderMarkdown, and Vue only recomputes when the
// text or the streaming flag actually changes.
const html = computed(() => renderMarkdown(props.text, { streaming: props.streaming }))

/** Feedback window, matching the per-message 复制 button. */
const RESET_MS = 1800

async function onClick(event) {
  const button = event.target instanceof Element ? event.target.closest('.md-copy') : null
  if (!button || !host.value || !host.value.contains(button)) return

  const code = button.closest('.md-code-block')?.querySelector('pre > code')
  if (!code) return

  let state = 'ok'
  try {
    if (!navigator.clipboard || typeof navigator.clipboard.writeText !== 'function') {
      throw new Error('clipboard unavailable')
    }
    await navigator.clipboard.writeText(code.textContent)
  } catch (err) {
    state = 'fail'
  }
  button.dataset.copied = state
  window.setTimeout(() => {
    button.dataset.copied = '0'
  }, RESET_MS)
}
</script>

<template>
  <div v-if="html" ref="host" class="md" @click="onClick" v-html="html" />
</template>
