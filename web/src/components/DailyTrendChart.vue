<script setup>
// Hand-written SVG line/area chart for the daily usage trend.
//
// No chart library: the geometry is plain arithmetic over the rows returned by
// `/api/usage/by-day`. A ResizeObserver keeps 1 SVG user unit == 1 CSS pixel so
// axis labels stay legible instead of being scaled down on a 380px phone.
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { formatCompact, formatCount, formatDayLong, formatDayShort, formatMoney } from '../format.js'

const props = defineProps({
  /** Rows from /api/usage/by-day: {day, calls, total_tokens, cost:{...}}. */
  rows: { type: Array, default: () => [] },
})

const METRICS = [
  {
    key: 'calls',
    label: '调用次数',
    color: 'var(--chart-1)',
    pick: (row) => Number(row.calls) || 0,
    value: (v) => formatCount(v),
    axis: (v) => formatCompact(v),
  },
  {
    key: 'tokens',
    label: '总 token',
    color: 'var(--chart-2)',
    pick: (row) => Number(row.total_tokens) || 0,
    value: (v) => formatCompact(v),
    axis: (v) => formatCompact(v),
  },
  {
    key: 'cost',
    label: '费用',
    color: 'var(--chart-3)',
    pick: (row) => (row.cost && row.cost.priced !== false ? Number(row.cost.total) || 0 : 0),
    value: (v) => formatMoney(v),
    axis: (v) => (v === 0 ? '0' : `$${v < 0.1 ? v.toFixed(3) : v.toFixed(2)}`),
  },
]

const metricKey = ref('calls')
const metric = computed(() => METRICS.find((m) => m.key === metricKey.value) || METRICS[0])

/** Rows for which no price-table entry matched: cost is unknown, not zero. */
const unpricedRows = computed(() =>
  props.rows.filter((row) => row && row.cost && row.cost.priced === false).length,
)

// ---------------------------------------------------------------- geometry --
const wrap = ref(null)
const width = ref(820)

let observer = null
onMounted(() => {
  if (!wrap.value || typeof ResizeObserver === 'undefined') return
  observer = new ResizeObserver((entries) => {
    for (const entry of entries) {
      const w = Math.round(entry.contentRect.width)
      if (w > 0) width.value = w
    }
  })
  observer.observe(wrap.value)
  const initial = Math.round(wrap.value.getBoundingClientRect().width)
  if (initial > 0) width.value = initial
})
onBeforeUnmount(() => {
  if (observer) observer.disconnect()
})

const compact = computed(() => width.value < 520)
const height = computed(() => (compact.value ? 214 : 264))
const pad = computed(() =>
  compact.value
    ? { top: 12, right: 10, bottom: 30, left: 42 }
    : { top: 14, right: 16, bottom: 34, left: 58 },
)

const plotW = computed(() => Math.max(10, width.value - pad.value.left - pad.value.right))
const plotH = computed(() => Math.max(10, height.value - pad.value.top - pad.value.bottom))

const points = computed(() =>
  props.rows.map((row) => ({
    day: row.day,
    raw: row,
    value: metric.value.pick(row),
  })),
)

/** Round a maximum up to a friendly value so the top gridline is a round number. */
function niceCeil(value) {
  if (!(value > 0)) return 1
  const exponent = Math.floor(Math.log10(value))
  const base = 10 ** exponent
  const n = value / base
  let step
  if (n <= 1) step = 1
  else if (n <= 1.5) step = 1.5
  else if (n <= 2) step = 2
  else if (n <= 2.5) step = 2.5
  else if (n <= 5) step = 5
  else step = 10
  return step * base
}

const dataMax = computed(() => points.value.reduce((max, p) => Math.max(max, p.value), 0))
/** All-zero series: keep a flat baseline and say so, instead of dividing by 0. */
const isAllZero = computed(() => !(dataMax.value > 0))
const yMax = computed(() => (isAllZero.value ? 1 : niceCeil(dataMax.value * 1.05)))

const yTicks = computed(() => {
  if (isAllZero.value) return [0]
  const steps = 4
  const out = []
  for (let i = 0; i <= steps; i += 1) out.push((yMax.value / steps) * i)
  return out.reverse()
})

function xAt(index) {
  const n = points.value.length
  if (n <= 1) return pad.value.left + plotW.value / 2
  return pad.value.left + (plotW.value * index) / (n - 1)
}

function yAt(value) {
  const ratio = yMax.value > 0 ? value / yMax.value : 0
  return pad.value.top + plotH.value * (1 - Math.min(1, Math.max(0, ratio)))
}

const baseline = computed(() => pad.value.top + plotH.value)

const linePath = computed(() => {
  if (!points.value.length) return ''
  return points.value
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${xAt(i).toFixed(1)},${yAt(p.value).toFixed(1)}`)
    .join(' ')
})

const areaPath = computed(() => {
  if (!points.value.length) return ''
  const first = xAt(0)
  const last = xAt(points.value.length - 1)
  return `${linePath.value} L${last.toFixed(1)},${baseline.value.toFixed(1)} L${first.toFixed(
    1,
  )},${baseline.value.toFixed(1)} Z`
})

/** Show at most 7 x-axis labels, evenly sampled to avoid collisions. */
const xLabels = computed(() => {
  const n = points.value.length
  if (!n) return []
  const maxLabels = compact.value ? 4 : 7
  const step = Math.max(1, Math.ceil(n / maxLabels))
  const out = []
  for (let i = 0; i < n; i += step) out.push({ i, x: xAt(i), text: formatDayShort(points.value[i].day) })
  const lastIndex = n - 1
  if (out.length && out[out.length - 1].i !== lastIndex) {
    out.push({ i: lastIndex, x: xAt(lastIndex), text: formatDayShort(points.value[lastIndex].day) })
  }
  return out
})

/** Column hit-areas carrying native <title> tooltips. */
const hoverColumns = computed(() => {
  const n = points.value.length
  if (!n) return []
  const columnWidth = n > 1 ? plotW.value / (n - 1) : plotW.value
  return points.value.map((p, i) => {
    const x = xAt(i) - columnWidth / 2
    return {
      key: `${p.day}-${i}`,
      x: Math.max(pad.value.left - 4, x),
      width: Math.max(6, columnWidth),
      title:
        `${formatDayLong(p.day)} · ${metric.value.label} ${metric.value.value(p.value)}` +
        ` · 调用 ${formatCount(p.raw.calls)} 次 · ${formatCompact(
          p.raw.total_tokens,
        )} tokens`,
    }
  })
})

/** Markers only when they would not turn into visual noise. */
const markerPoints = computed(() =>
  points.value.length > 0 && points.value.length <= 31
    ? points.value.map((p, i) => ({ ...p, index: i }))
    : [],
)

const gradientId = `trend-grad-${Math.random().toString(36).slice(2, 8)}`

const summary = computed(() => {
  const n = points.value.length
  if (!n) return ''
  const total = points.value.reduce((sum, p) => sum + p.value, 0)
  const peak = points.value.reduce((best, p) => (p.value > best.value ? p : best), points.value[0])
  return `${n} 天 · 合计 ${metric.value.value(total)} · 峰值 ${metric.value.value(peak.value)}（${
    peak.day || '—'
  }）`
})
</script>

<template>
  <div class="card">
    <div class="card-head">
      <div class="card-title">
        <span class="dot" />
        每日趋势
        <span class="card-sub">{{ summary }}</span>
      </div>
      <div class="seg" role="group" aria-label="趋势指标">
        <button
          v-for="m in METRICS"
          :key="m.key"
          type="button"
          :class="{ active: metricKey === m.key }"
          @click="metricKey = m.key"
        >
          {{ m.label }}
        </button>
      </div>
    </div>

    <div ref="wrap" class="chart-wrap">
      <svg
        class="chart-svg"
        :viewBox="`0 0 ${width} ${height}`"
        :height="height"
        role="img"
        :aria-label="`每日趋势图：${metric.label}`"
      >
        <defs>
          <linearGradient :id="gradientId" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" :stop-color="metric.color" stop-opacity="0.34" />
            <stop offset="100%" :stop-color="metric.color" stop-opacity="0.02" />
          </linearGradient>
        </defs>

        <!-- horizontal grid + y axis labels -->
        <g>
          <template v-for="(tick, i) in yTicks" :key="`y-${i}`">
            <line
              :x1="pad.left"
              :x2="pad.left + plotW"
              :y1="yAt(tick)"
              :y2="yAt(tick)"
              :stroke="tick === 0 ? 'var(--border-strong)' : 'var(--border)'"
              stroke-width="1"
              :stroke-dasharray="tick === 0 ? '0' : '3 4'"
            />
            <text
              :x="pad.left - 8"
              :y="yAt(tick) + 3.5"
              text-anchor="end"
              fill="var(--muted-foreground)"
              font-size="10.5"
              font-family="ui-monospace, SFMono-Regular, Menlo, monospace"
            >
              {{ metric.axis(tick) }}
            </text>
          </template>
        </g>

        <!-- x axis labels -->
        <g>
          <text
            v-for="label in xLabels"
            :key="`x-${label.i}`"
            :x="label.x"
            :y="height - 10"
            text-anchor="middle"
            fill="var(--muted-foreground)"
            font-size="10.5"
          >
            {{ label.text }}
          </text>
        </g>

        <template v-if="points.length">
          <path :d="areaPath" :fill="`url(#${gradientId})`" />
          <path
            :d="linePath"
            fill="none"
            :stroke="metric.color"
            stroke-width="2"
            stroke-linejoin="round"
            stroke-linecap="round"
          />
          <circle
            v-for="p in markerPoints"
            :key="`m-${p.index}`"
            :cx="xAt(p.index)"
            :cy="yAt(p.value)"
            r="3"
            fill="var(--card)"
            :stroke="metric.color"
            stroke-width="2"
          >
            <title>{{ `${formatDayLong(p.day)} · ${metric.label} ${metric.value(p.value)}` }}</title>
          </circle>
        </template>

        <!-- native tooltips over each day column -->
        <rect
          v-for="col in hoverColumns"
          :key="col.key"
          :x="col.x"
          :y="pad.top"
          :width="col.width"
          :height="plotH"
          fill="transparent"
        >
          <title>{{ col.title }}</title>
        </rect>
      </svg>
    </div>

    <div v-if="!points.length" class="empty">
      <div class="empty-ico" aria-hidden="true">◍</div>
      <div class="empty-text">暂无数据</div>
      <div class="empty-hint">当前时间范围内没有按天聚合的调用记录</div>
    </div>

    <template v-else>
      <div v-if="isAllZero" class="banner info" style="margin-top: 12px">
        <span aria-hidden="true">◌</span>
        <span class="banner-text">该区间内每天的{{ metric.label }}均为 0。</span>
      </div>

      <div v-if="metricKey === 'cost' && unpricedRows" class="banner info" style="margin-top: 12px">
        <span aria-hidden="true">◌</span>
        <span class="banner-text">
          有 {{ unpricedRows }} 天没有匹配到价格表条目，这些天的费用按 0 计入，总费用可能偏低。
        </span>
      </div>

      <div class="chart-legend">
        <span><i class="legend-key" :style="{ background: metric.color }" />{{ metric.label }}（按天）</span>
      </div>
    </template>
  </div>
</template>
