<script setup>
// Observation waterfall for one trace — hand-written, no chart library.
//
// Every observation's bar is positioned and sized by its own start/end time
// relative to the trace's overall span (min start … max end), and rows are
// indented by their depth in the parent_id tree. Geometry is defensive: a
// missing timestamp, a zero-duration span or a parent cycle degrades to a
// full-width bar or a root row instead of a NaN width.
import { computed, reactive } from 'vue'
import JsonBlock from './JsonBlock.vue'
import {
  formatCompact,
  formatDuration,
  formatExactTokens,
  formatJson,
  formatSeconds,
} from '../format.js'

const props = defineProps({
  /** Observation array from GET /api/traces/{id}. */
  observations: { type: Array, default: () => [] },
})

/** Bar colours by observation type; ERROR overrides the colour. */
const TYPE_CLASS = { SPAN: 'span', GENERATION: 'generation', EVENT: 'event' }
const TYPE_LABEL = { SPAN: 'SPAN', GENERATION: 'GENERATION', EVENT: 'EVENT' }
const INDENT_PX = 12
const MAX_INDENT = 8

/** Expanded rows, keyed by observation id (kept out of the API payload). */
const openRows = reactive({})

function numberOrNull(value) {
  if (typeof value === 'number') return Number.isFinite(value) ? value : null
  if (typeof value === 'string' && value.trim() !== '') {
    const n = Number(value)
    return Number.isFinite(n) ? n : null
  }
  return null
}

function timeOf(value) {
  if (!value) return null
  const t = Date.parse(value)
  return Number.isFinite(t) ? t : null
}

function clamp(value, min, max) {
  if (!Number.isFinite(value)) return min
  return Math.min(max, Math.max(min, value))
}

/**
 * One computed builds the whole layout: timed nodes, parent/child links,
 * roots, depth-first row order and the trace span. Cycles and unknown parents
 * are turned into roots so every observation stays visible.
 */
const layout = computed(() => {
  const list = Array.isArray(props.observations) ? props.observations : []

  const nodes = list.map((raw, index) => {
    const obs = raw && typeof raw === 'object' ? raw : {}
    const id = String(obs.id || `obs-${index}`)
    const latencyMs = numberOrNull(obs.latency)
    let start = timeOf(obs.start_time)
    let end = timeOf(obs.end_time)
    if (start === null && end !== null && latencyMs !== null) start = end - latencyMs * 1000
    if (end === null && start !== null && latencyMs !== null) end = start + latencyMs * 1000
    if (start !== null && end !== null && end < start) end = start
    return {
      id,
      obs,
      start,
      end,
      latencyMs,
      type: String(obs.type || '').toUpperCase(),
      level: String(obs.level || 'DEFAULT').toUpperCase(),
      parentId: String(obs.parent_id || ''),
      parentKey: '',
      depth: 0,
      children: [],
      order: index,
    }
  })

  const byId = new Map()
  for (const node of nodes) if (!byId.has(node.id)) byId.set(node.id, node)

  for (const node of nodes) {
    // Walk up the parent chain: a cycle (or a chain longer than the node
    // count) makes the node a root instead of looping forever.
    const seen = new Set([node.id])
    const chain = []
    let cursor = node.parentId ? byId.get(node.parentId) : null
    let cyclic = false
    while (cursor) {
      if (seen.has(cursor.id)) {
        cyclic = true
        break
      }
      seen.add(cursor.id)
      chain.push(cursor.id)
      if (chain.length > nodes.length) {
        cyclic = true
        break
      }
      cursor = cursor.parentId ? byId.get(cursor.parentId) : null
    }
    if (cyclic) {
      node.parentKey = ''
      node.depth = 0
    } else {
      node.parentKey = chain.length ? chain[0] : ''
      node.depth = Math.min(chain.length, MAX_INDENT)
    }
  }

  const byParent = new Map()
  for (const node of nodes) {
    const key = node.parentKey && byId.has(node.parentKey) ? node.parentKey : ''
    if (!byParent.has(key)) byParent.set(key, [])
    byParent.get(key).push(node)
  }

  const byStart = (a, b) => {
    const left = a.start === null ? Number.POSITIVE_INFINITY : a.start
    const right = b.start === null ? Number.POSITIVE_INFINITY : b.start
    if (left !== right) return left - right
    return a.order - b.order
  }
  for (const bucket of byParent.values()) bucket.sort(byStart)

  const roots = (byParent.get('') || []).slice()
  const rows = []
  const visited = new Set()
  const visit = (node) => {
    if (!node || visited.has(node.id)) return
    visited.add(node.id)
    rows.push(node)
    const children = byParent.get(node.id) || []
    for (const child of children) visit(child)
  }
  for (const root of roots) visit(root)
  // Safety net: anything left (should not happen) is still rendered.
  for (const node of nodes.slice().sort(byStart)) visit(node)

  let spanStart = null
  let spanEnd = null
  for (const node of nodes) {
    if (node.start !== null) spanStart = spanStart === null ? node.start : Math.min(spanStart, node.start)
    if (node.end !== null) spanEnd = spanEnd === null ? node.end : Math.max(spanEnd, node.end)
  }
  if (spanStart !== null && spanEnd === null) spanEnd = spanStart
  if (spanStart === null && spanEnd !== null) spanStart = spanEnd
  const hasTiming = spanStart !== null && spanEnd !== null
  const durationMs = hasTiming ? Math.max(0, spanEnd - spanStart) : 0

  return { rows, nodes, hasTiming, spanStart, spanEnd, durationMs }
})

/** Bars are percentages of the trace span; never NaN, never zero-width. */
function barStyle(node) {
  const { hasTiming, spanStart, durationMs } = layout.value
  if (!hasTiming || node.start === null || node.end === null || durationMs <= 0) {
    return { left: '0%', width: '100%' }
  }
  const left = clamp(((node.start - spanStart) / durationMs) * 100, 0, 100)
  const width = clamp(((node.end - node.start) / durationMs) * 100, 0.6, 100 - left)
  return { left: `${left.toFixed(2)}%`, width: `${width.toFixed(2)}%` }
}

function barTitle(node) {
  const { hasTiming, spanStart } = layout.value
  const parts = [node.obs.name || node.id]
  if (hasTiming && node.start !== null) {
    parts.push(`开始 +${formatDuration(node.start - spanStart)}`)
  }
  if (node.start !== null && node.end !== null) {
    parts.push(`耗时 ${formatDuration(node.end - node.start)}`)
  }
  return parts.join(' · ')
}

/** Five evenly spaced axis labels showing the offset from the trace start. */
const axisTicks = computed(() => {
  const { durationMs } = layout.value
  const ticks = []
  for (let i = 0; i <= 4; i += 1) ticks.push(formatDuration((durationMs / 4) * i))
  return ticks
})

function toggle(id) {
  openRows[id] = !openRows[id]
}

function hasJson(value) {
  return formatJson(value) !== ''
}

function typeLabel(node) {
  return TYPE_LABEL[node.type] || node.type || 'SPAN'
}

function barClass(node) {
  if (node.level === 'ERROR') return 'bad'
  return TYPE_CLASS[node.type] || 'span'
}

/** Exact token counts behind the compact ↑/↓ numbers. */
function usageTitle(usage) {
  if (!usage) return ''
  return (
    `${formatExactTokens(usage.input)} in / ${formatExactTokens(usage.output)} out` +
    ` / 合计 ${formatExactTokens(usage.total)}`
  )
}
</script>

<template>
  <div v-if="!layout.rows.length" class="empty">
    <div class="empty-ico" aria-hidden="true">◍</div>
    <div class="empty-text">该 trace 没有 observation</div>
    <div class="empty-hint">链路里只记录了 trace 本身，没有 span / generation / event 明细</div>
  </div>

  <div v-else class="wf">
    <div class="wf-head">
      <span class="muted-note">
        共 {{ layout.rows.length }} 个 observation ·
        总时长
        <b>{{ formatDuration(layout.durationMs) }}</b>
      </span>
      <span class="spacer" />
      <span v-if="!layout.hasTiming" class="chip warn" title="observation 缺少可用的时间戳">
        时间戳缺失，条形按满宽显示
      </span>
      <span v-else-if="layout.durationMs <= 0" class="chip warn" title="所有 observation 的起止时间相同">
        时长为 0，条形按满宽显示
      </span>
    </div>

    <div class="wf-axis" aria-hidden="true">
      <span v-for="(tick, index) in axisTicks" :key="index" class="wf-tick">{{ tick }}</span>
    </div>

    <div class="wf-rows">
      <div
        v-for="node in layout.rows"
        :key="node.id"
        class="wf-row"
        :class="{ open: openRows[node.id], error: node.level === 'ERROR' }"
      >
        <button
          type="button"
          class="wf-meta"
          :style="{ paddingLeft: `${node.depth * INDENT_PX + 2}px` }"
          :aria-expanded="Boolean(openRows[node.id])"
          @click="toggle(node.id)"
        >
          <span class="json-caret" aria-hidden="true">{{ openRows[node.id] ? '▾' : '▸' }}</span>
          <span class="wf-name mono" :title="node.obs.name || node.id">
            {{ node.obs.name || '（未命名）' }}
          </span>
          <span class="tag" :class="node.type === 'GENERATION' ? 'purple' : node.type === 'EVENT' ? 'blue' : ''">
            {{ typeLabel(node) }}
          </span>
          <span v-if="node.level === 'ERROR'" class="tag bad">ERROR</span>
          <span v-if="node.obs.model" class="dimmer mono wf-model">{{ node.obs.model }}</span>
          <span class="spacer" />
          <span class="dimmer nowrap">{{ formatSeconds(node.obs.latency) }}</span>
          <span v-if="node.obs.usage" class="dimmer mono nowrap" :title="usageTitle(node.obs.usage)">
            ↑{{ formatCompact(node.obs.usage.input) }} ↓{{ formatCompact(node.obs.usage.output) }}
          </span>
        </button>

        <div class="wf-track" @click="toggle(node.id)">
          <div class="wf-bar" :class="barClass(node)" :style="barStyle(node)" :title="barTitle(node)" />
        </div>

        <div v-if="openRows[node.id]" class="wf-detail">
          <div v-if="node.obs.status_message" class="banner error" role="alert">
            <span aria-hidden="true">⚠</span>
            <span class="banner-text">{{ node.obs.status_message }}</span>
          </div>
          <div v-if="!hasJson(node.obs.input) && !hasJson(node.obs.output)" class="muted-note">
            该 observation 没有 input / output 数据。
          </div>
          <JsonBlock :value="node.obs.input" label="input" :open="true" :toggle="false" />
          <JsonBlock :value="node.obs.output" label="output" :open="true" :toggle="false" />
          <div class="muted-note mono">
            id {{ node.id }}
            <template v-if="node.obs.parent_id"> · parent {{ node.obs.parent_id }}</template>
          </div>
        </div>
      </div>
    </div>

    <div class="chart-legend">
      <span><i class="legend-key wf-key-span" />SPAN</span>
      <span><i class="legend-key wf-key-generation" />GENERATION</span>
      <span><i class="legend-key wf-key-event" />EVENT</span>
      <span><i class="legend-key wf-key-error" />ERROR（level）</span>
      <span class="dimmer">条形长度 = 该 observation 的耗时，横向位置 = 相对 trace 起点的偏移</span>
    </div>
  </div>
</template>
