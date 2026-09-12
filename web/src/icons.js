// Single source of truth for the hand-written icon set (no icon library —
// the dependency list is frozen to vue / vite / @vitejs/plugin-vue / marked /
// dompurify).
//
// Icon.vue renders these through SVG templates; markdown.js builds the same
// geometry for the code-block copy button it injects into sanitized HTML, so
// the glyph cannot drift between the two places it appears.
//
// The geometry and the SVG attribute convention follow Lucide, the icon set the
// DeepSeek harness bundles: viewBox "0 0 24 24", fill "none", stroke
// "currentColor", stroke-width 2, round caps and joins. Every icon therefore
// inherits its colour from the surrounding text and flips with the theme.

export const ICON_PATHS = {
  plus: 'M5 12h14|M12 5v14',
  'panel-left': 'M3 3h18v18H3z|M9 3v18',
  'arrow-up': 'M12 19V5|m5 12 7-7 7 7',
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
  // 设置: a gear, following Lucide's gear-path + hub geometry.
  settings:
    'M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 0 0 2.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 0 0 1.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 0 0-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 0 0-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 0 0-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 0 0-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 0 0 1.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065|M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6',
  // 统计监控: Lucide bar-chart-3.
  'bar-chart': 'M3 3v18h18|M18 17V9|M13 17V5|M8 17v-3',
  cpu: 'M4 4h16v16H4z|M9 9h6v6H9z|M9 2v2|M15 2v2|M9 20v2|M15 20v2|M2 9h2|M2 15h2|M20 9h2|M20 15h2',
}

/** Icons whose first segment is a rounded rect instead of a path. */

/** Icons whose first segment is a rounded rect instead of a path. */
export const ICON_RECT_FIRST = { 'panel-left': true, square: true, cpu: true }
