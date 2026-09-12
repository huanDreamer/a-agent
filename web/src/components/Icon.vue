<script setup>
// Inline icon set — hand-written, no icon library (the dependency list is
// frozen: vue / vite / @vitejs/plugin-vue / marked / dompurify).
//
// The geometry lives in ../icons.js because markdown.js also needs it: it
// builds the copy/check glyph for the code-block copy button inside the
// sanitized HTML it returns, and both places must draw the same icon.
import { ICON_PATHS as PATHS, ICON_RECT_FIRST as RECT_FIRST } from '../icons.js'

const props = defineProps({
  /** Key of PATHS. An unknown name renders nothing rather than throwing. */
  name: { type: String, required: true },
  /** Rendered size in CSS px (16 inline, 20 for a primary action). */
  size: { type: [Number, String], default: 16 },
})

const shape = () => {
  const raw = PATHS[props.name]
  if (!raw) return []
  const segments = raw.split('|')
  const rect = Boolean(RECT_FIRST[props.name])
  return (rect ? segments.slice(1) : segments).map((d) => ({ d }))
}

const hasRect = () => Boolean(RECT_FIRST[props.name])
</script>

<template>
  <svg
    class="ico"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <rect v-if="hasRect()" x="3" y="3" width="18" height="18" rx="2" />
    <path v-for="(item, index) in shape()" :key="index" :d="item.d" />
  </svg>
</template>
