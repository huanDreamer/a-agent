// What the header button says, and how one artifact is labelled.
//
// It lives outside the components for the reason subagentsChip.js does: getting
// the wording wrong is how a header starts lying — a count that belongs to the
// previous conversation, a title that is empty where the model gave none, a kind
// rendered as a raw value. None of that is visible in a rendered page, so the
// rules are functions here and are asserted by scripts/check-artifacts.mjs.
import { artifactsCount, artifactsEnabled, artifactsMessage } from './artifactsStore.js'

/**
 * The header button's label: a plain count.
 *
 * There is no liveness half to lead with — an artifact is finished the moment it
 * exists — so the label is the count and nothing else. The button is not
 * rendered at all when the count is zero and the drawer is closed (see ChatView),
 * which is why no zero branch is needed here.
 */
export function artifactChipLabel(count, enabled) {
  const n = typeof count === 'number' ? count : artifactsCount.value
  const on = typeof enabled === 'boolean' ? enabled : artifactsEnabled.value
  if (!on) return '产物未启用'
  return `${n} 个产物`
}

/** The button's tooltip: what 产物 means, and where else they are listed. */
export function artifactHintText() {
  const parts = [`本会话：${artifactsCount.value} 个产物`]
  if (!artifactsEnabled.value) {
    parts.push(artifactsMessage.value || '当前部署未启用产物存储')
    return parts.join(' · ')
  }
  parts.push('智能体保存的页面、报告、图片等资源，点击可打开')
  parts.push('全部会话的产物在 统计监控 → 产物中心')
  return parts.join(' · ')
}

/**
 * One artifact's label: its title, or — when the model saved no title — its file
 * name.
 *
 * Falling back to the path rather than to "未命名" matters: the file name is what
 * the model chose to save, so it is a true label, and a list of rows all reading
 * "未命名" tells a reader nothing about which one to open.
 */
export function artifactLabel(artifact) {
  if (!artifact) return ''
  const title = typeof artifact.title === 'string' ? artifact.title.trim() : ''
  if (title) return title
  const path = typeof artifact.path === 'string' ? artifact.path : ''
  const slash = path.lastIndexOf('/')
  return slash >= 0 ? path.slice(slash + 1) : path
}

/** The Chinese name of a kind, for the tag on a row. */
const KIND_LABELS = {
  html: '网页',
  document: '文档',
  image: '图片',
  other: '其他',
}

export function kindLabel(kind) {
  return KIND_LABELS[kind] || KIND_LABELS.other
}

/** The icon a row draws for a kind. Unknown kinds get the generic 产物 glyph. */
const KIND_ICONS = {
  html: 'monitor',
  document: 'file-text',
  image: 'image',
  other: 'package',
}

export function kindIcon(kind) {
  return KIND_ICONS[kind] || KIND_ICONS.other
}

/**
 * The stored file's name: the last segment of the path.
 *
 * This is the name the file actually has on disk, which since artifacts are saved
 * under a title-derived stem is the most readable thing to show about where an
 * artifact lives: "季度用量报告.html" says what it is, where the full path
 * ("sess-1/2026-02-14/季度用量报告.html") only adds the folder it is filed in.
 * The full path belongs in a title attribute, not in a cell.
 */
export function fileName(path) {
  const value = typeof path === 'string' ? path : ''
  if (value === '') return ''
  const trimmed = value.replace(/\/+$/, '')
  const slash = trimmed.lastIndexOf('/')
  return slash >= 0 ? trimmed.slice(slash + 1) : trimmed
}

/** The extension shown after a name: ".html", or '' when the path has none. */
export function extensionOf(path) {
  const value = typeof path === 'string' ? path : ''
  const slash = value.lastIndexOf('/')
  const dot = value.lastIndexOf('.')
  if (dot <= slash || dot === value.length - 1) return ''
  return value.slice(dot)
}
