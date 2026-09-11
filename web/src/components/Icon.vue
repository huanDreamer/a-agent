<script setup>
// Inline icon set — hand-written, no icon library (the dependency list is
// frozen: vue / vite / @vitejs/plugin-vue only).
//
// The geometry and the SVG attribute convention follow Lucide, the icon set the
// DeepSeek harness bundles: viewBox "0 0 24 24", fill "none", stroke
// "currentColor", stroke-width 2, round caps and joins. Every icon therefore
// inherits its colour from the surrounding text and flips with the theme.
const PATHS = {
  plus: 'M5 12h14|M12 5v14',
  'panel-left': 'M3 3h18v18H3z|M9 3v18',
  send: 'M12 19V5|m5 12 7-7 7 7',
  square: 'M5 5h14v14H5z',
  copy: 'M20 8h-6a2 2 0 0 0-2 2v6|M8 16H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v2',
  check: 'M20 6 9 17l-5-5',
  trash: 'M3 6h18|M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6|M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2|M10 11v6|M14 11v6',
  pencil: 'M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z|m15 5 4 4',
  eraser: 'M21 21H8a2 2 0 0 1-1.42-.587l-3.994-3.999a2 2 0 0 1 0-2.828l10-10a2 2 0 0 1 2.829 0l5.999 6a2 2 0 0 1 0 2.828L12.834 21|m5.082 11.09 8.826 8.826',
  'chevron-down': 'm6 9 6 6 6-6',
  'chevron-right': 'm9 18 6-6-6-6',
  'chevron-left': 'm15 18-6-6 6-6',
  'arrow-left': 'm12 19-7-7 7-7|M19 12H5',
  'circle-alert': 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20|M12 8v4|M12 16h.01',
  'external-link': 'M15 3h6v6|M10 14 21 3|M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6',
  sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8|M12 2v2|M12 20v2|m4.93 4.93 1.41 1.41|m17.66 17.66 1.41 1.41|M2 12h2|M20 12h2|m6.34 17.66-1.41 1.41|m19.07 4.93-1.41 1.41',
  moon: 'M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9',
  monitor: 'M4 3h16a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1|M8 21h8|M12 17v4',
  refresh: 'M21 12a9 9 0 1 1-3-6.7L21 8|M21 3v5h-5',
  logout: 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4|m16 17 5-5-5-5|M21 12H9',
  message: 'M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z',
  activity: 'M22 12h-4l-3 9L9 3l-3 9H2',
  cpu: 'M4 4h16v16H4z|M9 9h6v6H9z|M9 2v2|M15 2v2|M9 20v2|M15 20v2|M2 9h2|M2 15h2|M20 9h2|M20 15h2',
  users: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2|M9 7a4 4 0 1 0 0 8 4 4 0 0 0 0-8|M22 21v-2a4 4 0 0 0-3-3.87|M16 3.13a4 4 0 0 1 0 7.75',
  list: 'M8 6h13|M8 12h13|M8 18h13|M3 6h.01|M3 12h.01|M3 18h.01',
  'scroll-text': 'M15 12h-5|M15 8h-5|M8 5v10a2 2 0 0 1-2 2 2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11a2 2 0 0 1 2 2v12a3 3 0 0 1-3 3H7',
  sparkles: 'M12 3v4|M12 17v4|M3 12h4|M17 12h4|M5.6 5.6l2.8 2.8|M15.6 15.6l2.8 2.8|M18.4 5.6l-2.8 2.8|M8.4 15.6l-2.8 2.8',
  route: 'M6 19a3 3 0 1 0 0-6 3 3 0 0 0 0 6|M18 11a3 3 0 1 0 0-6 3 3 0 0 0 0 6|M9 16h6a3 3 0 0 0 3-3v-2',
}

/** Icons whose first segment is a rounded rect instead of a path. */
const RECT_FIRST = { 'panel-left': true, square: true, cpu: true }

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
