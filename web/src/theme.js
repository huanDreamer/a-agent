// Theme controller: 跟随系统 / 亮色 / 暗色.
//
// Resolution order — an explicit choice persisted in localStorage wins, then the
// operating system preference, then light. `initTheme()` runs while the module
// is first imported (before the app mounts) so the `dark` class is already on
// <html> for the first paint.
//
// styles.css carries the same dark palette twice: inside
// `@media (prefers-color-scheme: dark)` for the no-JS / first-paint case, and
// again under `html.dark` — which comes last, so an explicit 亮色 choice still
// wins on a dark machine.

import { computed, reactive } from 'vue'

const KEY = 'huan-agent.theme'

/** 'system' | 'light' | 'dark' */
const CHOICES = ['system', 'light', 'dark']

export const THEMES = [
  { key: 'system', label: '跟随系统', icon: 'monitor' },
  { key: 'light', label: '亮色', icon: 'sun' },
  { key: 'dark', label: '暗色', icon: 'moon' },
]

function systemQuery() {
  if (typeof window === 'undefined' || !window.matchMedia) return null
  return window.matchMedia('(prefers-color-scheme: dark)')
}

/** The persisted choice, or 'system' when there is none (or it is malformed). */
function stored() {
  try {
    const raw = window.localStorage.getItem(KEY)
    return CHOICES.includes(raw) ? raw : 'system'
  } catch (err) {
    // Private mode / disabled storage: fall back to following the system.
    return 'system'
  }
}

export const theme = reactive({
  /** What the user picked. */
  choice: 'system',
  /** The scheme actually in effect: 'light' | 'dark'. */
  resolved: 'light',
  systemDark: false,
})

function apply() {
  const dark = theme.choice === 'dark' || (theme.choice === 'system' && theme.systemDark)
  theme.resolved = dark ? 'dark' : 'light'
  const root = document.documentElement
  root.classList.toggle('dark', dark)
  // Both flags are written: `dark` is the shadcn convention, `data-theme` is
  // what lets the `@media (prefers-color-scheme: dark)` fallback in styles.css
  // step aside when the user explicitly chose 亮色 on a dark machine.
  root.setAttribute('data-theme', dark ? 'dark' : 'light')
  // Lets native form controls (select, textarea, scrollbars) match too.
  root.style.colorScheme = dark ? 'dark' : 'light'
}

/** Persist a choice and re-render. Unknown values are ignored. */
export function setTheme(choice) {
  if (!CHOICES.includes(choice)) return
  theme.choice = choice
  try {
    // 'system' is stored explicitly: it is a choice too, not a missing value.
    window.localStorage.setItem(KEY, choice)
  } catch (err) {
    // Storage unavailable — the choice still applies for this session.
  }
  apply()
}

/** Re-read the OS preference (also the media-query listener). */
function onSystemChange(event) {
  theme.systemDark = Boolean(event.matches)
  if (theme.choice === 'system') apply()
}

export function initTheme() {
  theme.choice = stored()
  const query = systemQuery()
  theme.systemDark = query ? query.matches : false
  apply()
  if (query) {
    if (typeof query.addEventListener === 'function') query.addEventListener('change', onSystemChange)
    else if (typeof query.addListener === 'function') query.addListener(onSystemChange)
  }
}

export const themeLabel = computed(() => {
  const found = THEMES.find((item) => item.key === theme.choice)
  return found ? found.label : '跟随系统'
})

initTheme()
