<script setup>
// 按模型 — table with a cost column plus the horizontal bar list.
import { computed, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import BarList from './BarList.vue'
import TotalsTable from './TotalsTable.vue'
import { api } from '../api.js'
import { BAR_METRICS } from '../metrics.js'
import { currentFilters, state } from '../state.js'
import { useResource } from '../useResource.js'

const { data, status, error, reload } = useResource(() => api.usageByModel(currentFilters(), 50))

const rows = computed(() => (data.value && data.value.rows) || [])
const barMetricKey = ref('calls')
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div class="page-title">按模型</div>
      <div class="page-note">
        {{ state.range === 'all' ? '全部时间' : '当前时间范围' }} · 共 {{ rows.length }} 个模型
      </div>
    </div>

    <AsyncBlock :state="status" :error="error" :skeleton-rows="6" @retry="reload">
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            模型用量明细
            <span class="card-sub">费用列来自价格表估算，未匹配时显示 —</span>
          </div>
        </div>
        <TotalsTable
          :rows="rows"
          key-label="模型"
          empty-text="该区间内没有按模型聚合的数据"
        />
      </div>

      <div class="card" v-if="rows.length">
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
