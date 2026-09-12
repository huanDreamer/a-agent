<script setup>
// 按用户 panel — same shape as 按模型, but the key is a Feishu open_id, so long
// keys are shortened in the table/bar labels with the full value in a title.
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

const { data, status, error, reload } = useResource(() => api.usageByUser(currentFilters(), 50))

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
            用户用量明细
            <span class="card-sub">
              {{ rangeNote }} · 共 {{ formatCount(rows.length) }} 位用户 · key 为飞书 open_id，悬停可查看完整值
            </span>
          </div>
        </div>
        <TotalsTable
          :rows="rows"
          key-label="用户（open_id）"
          shorten-keys
          empty-text="该区间内没有按用户聚合的数据"
        />
      </div>

      <div v-if="rows.length" class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            用户对比
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
        <BarList :rows="rows" :metric="barMetricKey" :label-width="170" shorten-keys />
      </div>
    </AsyncBlock>
  </div>
</template>
