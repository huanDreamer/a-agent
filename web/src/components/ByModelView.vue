<script setup>
// 按模型 panel — table with a cost column plus the horizontal bar list.
// A content panel of 统计监控: bare .stack, no page head of its own.
import { computed, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import BarList from './BarList.vue'
import TotalsTable from './TotalsTable.vue'
import { api } from '../api.js'
import { formatCount } from '../format.js'
import { BAR_METRICS } from '../metrics.js'
import { currentFilters, rangeNote } from '../state.js'
import { useResource } from '../useResource.js'

const { data, status, error, reload } = useResource(() => api.usageByModel(currentFilters(), 50))

const rows = computed(() => (data.value && data.value.rows) || [])
const barMetricKey = ref('calls')
</script>

<template>
  <div class="stack">
    <AsyncBlock :state="status" :error="error" :skeleton-rows="6" @retry="reload">
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            模型用量明细
            <span class="card-sub">
              {{ rangeNote }} · 共 {{ formatCount(rows.length) }} 个模型 · 费用列来自价格表估算，未匹配时显示 —
            </span>
          </div>
        </div>
        <TotalsTable :rows="rows" key-label="模型" empty-text="该区间内没有按模型聚合的数据" />
      </div>

      <div v-if="rows.length" class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            模型对比
          </div>
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
        </div>
        <BarList :rows="rows" :metric="barMetricKey" :label-width="200" />
      </div>
    </AsyncBlock>
  </div>
</template>
