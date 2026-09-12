<script setup>
// 总览 panel: headline summary cards + the daily trend chart + a by-model
// breakdown. Every panel has its own loading / empty / error state.
//
// It is a content panel of 统计监控 (MonitorView owns the page head, the
// sub-tab strip and the range toolbar), so it renders a bare .stack.
import { computed, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import StatCard from './StatCard.vue'
import DailyTrendChart from './DailyTrendChart.vue'
import BarList from './BarList.vue'
import { api } from '../api.js'
import {
  formatAverageDuration,
  formatCompact,
  formatCost,
  formatCount,
  formatDuration,
  isCostUnknown,
} from '../format.js'
import { BAR_METRICS } from '../metrics.js'
import { currentFilters, rangeNote, setMonitor, trendDays } from '../state.js'
import { useResource } from '../useResource.js'

const { data: summary, status: summaryStatus, error: summaryError, reload: reloadSummary } =
  useResource(() => api.usageSummary(currentFilters()))

const { data: byDay, status: trendStatus, error: trendError, reload: reloadTrend } = useResource(() =>
  api.usageByDay(currentFilters(), trendDays.value),
)

const { data: byModel, status: modelStatus, error: modelError, reload: reloadModel } = useResource(
  () => api.usageByModel(currentFilters(), 8),
)

const barMetricKey = ref('calls')

const trendRows = computed(() => (byDay.value && byDay.value.rows) || [])
const modelRows = computed(() => (byModel.value && byModel.value.rows) || [])
const totals = computed(() => summary.value || {})

const costUnknown = computed(() => isCostUnknown(summary.value && summary.value.cost))
const costValue = computed(() => formatCost(summary.value && summary.value.cost))
const costSub = computed(() =>
  costUnknown.value ? '未匹配到价格表，费用未知' : '按价格表估算（USD）',
)

const tokenSub = computed(() => {
  const s = totals.value
  if (s.total_tokens === undefined) return ''
  return `prompt ${formatCompact(s.prompt_tokens)} · completion ${formatCompact(s.completion_tokens)}`
})

const latencySub = computed(() => {
  const s = totals.value
  if (s.duration_ms === undefined) return ''
  return `总耗时 ${formatDuration(s.duration_ms)}`
})
</script>

<template>
  <div class="stack">
    <AsyncBlock :state="summaryStatus" :error="summaryError" :skeleton-rows="3" @retry="reloadSummary">
      <div v-if="!summary" class="empty">
        <div class="empty-ico" aria-hidden="true">◍</div>
        <div class="empty-text">暂无数据</div>
        <div class="empty-hint">{{ rangeNote }}内没有任何 LLM 调用</div>
      </div>
      <div v-else class="stat-grid">
        <StatCard
          label="调用次数"
          tone="b"
          :value="formatCount(totals.calls)"
          :title="`该区间内共 ${formatCount(totals.calls)} 次 LLM 调用`"
        />
        <StatCard
          label="总 token"
          tone="p"
          :value="formatCompact(totals.total_tokens)"
          :sub="tokenSub"
          :title="`prompt + completion 合计 ${formatCount(totals.total_tokens)} tokens`"
        />
        <StatCard
          label="费用"
          tone="g"
          :value="costValue"
          :sub="costSub"
          :title="costUnknown ? '没有匹配到价格表条目，费用未知' : '按价格表估算（USD）'"
        />
        <StatCard
          label="用户数"
          tone="c"
          :value="formatCount(totals.users)"
          title="该区间内产生调用的去重用户数"
        />
        <StatCard
          label="会话数"
          tone="c"
          :value="formatCount(totals.sessions)"
          title="该区间内产生调用的去重会话数"
        />
        <StatCard
          label="平均延迟"
          tone="a"
          :value="formatAverageDuration(totals.duration_ms, totals.calls)"
          :sub="latencySub"
          title="总耗时 / 调用次数"
        />
      </div>
    </AsyncBlock>

    <AsyncBlock :state="trendStatus" :error="trendError" :skeleton-rows="6" @retry="reloadTrend">
      <DailyTrendChart :rows="trendRows" />
    </AsyncBlock>

    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          按模型分布
          <span class="card-sub">Top {{ modelRows.length }}</span>
        </div>
        <div class="row">
          <div class="seg" role="group" aria-label="排序指标">
            <button
              v-for="m in BAR_METRICS"
              :key="m.key"
              type="button"
              :class="{ active: barMetricKey === m.key }"
              @click="barMetricKey = m.key"
            >
              {{ m.label }}
            </button>
          </div>
          <button type="button" class="btn sm" @click="setMonitor('model')">查看全部</button>
        </div>
      </div>

      <AsyncBlock :state="modelStatus" :error="modelError" :skeleton-rows="4" @retry="reloadModel">
        <BarList :rows="modelRows" :metric="barMetricKey" empty-text="该区间内没有按模型聚合的数据" />
      </AsyncBlock>
    </div>

    <p class="muted-note page-foot">
      统计范围：{{ rangeNote }} · 数据来自
      <code class="md-code">/api/usage/*</code>，未做任何缓存
    </p>
  </div>
</template>
