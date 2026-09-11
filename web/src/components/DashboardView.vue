<script setup>
// Dashboard: headline summary cards + the daily trend chart + a by-model
// breakdown. Every panel has its own loading / empty / error state.
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
import { currentFilters, setTab, state, trendDays } from '../state.js'
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

function retryAll() {
  reloadSummary()
  reloadTrend()
  reloadModel()
}
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <div class="page-title">总览</div>
        <div class="page-note">
          统计范围：<b>{{ state.range === 'all' ? '全部时间' : state.range === '24h' ? '最近 24 小时' : state.range === '7d' ? '最近 7 天' : '最近 30 天' }}</b>
        </div>
      </div>
      <button type="button" class="btn sm" @click="retryAll">重新加载</button>
    </div>

    <AsyncBlock
      :state="summaryStatus"
      :error="summaryError"
      :skeleton-rows="3"
      @retry="reloadSummary"
    >
      <div v-if="!summary" class="empty">
        <div class="empty-ico" aria-hidden="true">◍</div>
        <div class="empty-text">暂无数据</div>
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
          <button type="button" class="btn sm" @click="setTab('model')">查看全部</button>
        </div>
      </div>

      <AsyncBlock :state="modelStatus" :error="modelError" :skeleton-rows="4" @retry="reloadModel">
        <BarList :rows="modelRows" :metric="barMetricKey" empty-text="该区间内没有按模型聚合的数据" />
      </AsyncBlock>
    </div>
  </div>
</template>
