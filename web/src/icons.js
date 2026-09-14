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
  // 工作区: a folder, following Lucide's folder geometry. It marks the one
  // control in 对话 that decides which directory the agent may touch.
  folder: 'M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z',
  // 附件: attach (paperclip), the drop/upload glyph, an image and an audio cue.
  paperclip:
    'm16 6-8.414 8.586a2 2 0 0 0 2.829 2.829l8.414-8.586a4 4 0 1 0-5.657-5.657l-8.379 8.551a6 6 0 1 0 8.485 8.485l8.379-8.551',
  upload: 'M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4|m17 8-5-5-5 5|M12 3v12',
  x: 'M18 6 6 18|m6 6 12 12',
  eye: 'M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0|M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6',
  headphones:
    'M3 14h3a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-7a9 9 0 0 1 18 0v7a2 2 0 0 1-2 2h-1a2 2 0 0 1-2-2v-3a2 2 0 0 1 2-2h3',
  // 模型管理: 测试连接 (plug) and a positive test result (circle-check).
  plug: 'M12 22v-5|M9 8V2|M15 8V2|M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z',
  'circle-check': 'M21.801 10A10 10 0 1 1 17 3.335|m9 11 3 3L22 4',
  // 长任务: a clock for "the turn stopped on its budget" and stacked layers for
  // "the window was condensed to stay inside it". Both follow Lucide geometry,
  // like every other icon here.
  clock: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20|M12 6v6l4 2',
  // 后台进程: a terminal window for the drawer a conversation's header opens —
  // everything listed there was started as a shell command.
  terminal: 'm4 17 6-6-6-6|M12 19h8',
  layers:
    'm12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83Z|m6.08 9.5-3.5 1.6a1 1 0 0 0 0 1.81l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9a1 1 0 0 0 0-1.83l-3.5-1.59|m6.08 14.5-3.5 1.6a1 1 0 0 0 0 1.81l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9a1 1 0 0 0 0-1.83l-3.5-1.59',
  // 链路追踪: Lucide git-branch — one thing descending from another, which is
  // what the link from an answer to its trace means.
  'git-branch': 'M6 3v12|M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6|M6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6|M18 9a9 9 0 0 1-9 9',
  // ask_user 卡片: Lucide circle-help — the model is asking, not telling.
  'help-circle': 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20|M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3|M12 17h.01',
  // 配置助手: the AI-generation entry point of 设置 → MCP and 设置 → 技能.
  // Lucide sparkles — a small burst plus two sparks.
  sparkles:
    'M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z|M20 3v4|M22 5h-4|M4 17v2|M5 18H3',
  // 登录 / 退出登录. Lucide lock and log-out, drawn as plain paths: ICON_RECT_FIRST
  // can only draw this set's one fixed square, while the lock body is 18×11.
  lock: 'M7 11V7a5 5 0 0 1 10 0v4|M5 11h14a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-7a2 2 0 0 1 2-2z',
  'log-out': 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4|m16 17 5-5-5-5|M21 12H9',
}

/** Icons whose first segment is a rounded rect instead of a path. */
export const ICON_RECT_FIRST = { 'panel-left': true, square: true, cpu: true }
