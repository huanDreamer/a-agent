<script setup>
// Totals table shared by 按模型 / 按用户 (and the dashboard's breakdown card).
// Columns: key, calls, prompt/completion tokens, total tokens, average latency,
// cost. Every cell comes from format.js so a missing field degrades to "—".
import { computed } from 'vue'
import {
  formatAverageDuration,
  formatCompact,
  formatCost,
  formatCount,
  formatExactTokens,
  isCostUnknown,
  shortId,
} from '../format.js'

const props = defineProps({
  rows: { type: Array, default: () => [] },
  keyLabel: { type: String, default: 'Key' },
  shortenKeys: { type: Boolean, default: false },
  emptyText: { type: String, default: '暂无数据' },
})

const items = computed(() =>
  props.rows.map((row, index) => {
    const rawKey = row && row.key !== undefined && row.key !== null && row.key !== '' ? row.key : '—'
    return {
      id: `${rawKey}-${index}`,
      rawKey: String(rawKey),
      label: props.shortenKeys ? shortId(rawKey) : String(rawKey),
      calls: formatCount(row.calls),
      prompt: formatCompact(row.prompt_tokens),
      promptTitle: formatExactTokens(row.prompt_tokens),
      completion: formatCompact(row.completion_tokens),
      completionTitle: formatExactTokens(row.completion_tokens),
      total: formatCompact(row.total_tokens),
      totalTitle: formatExactTokens(row.total_tokens),
      avg: formatAverageDuration(row.duration_ms, row.calls),
      cost: formatCost(row.cost),
      costUnknown: isCostUnknown(row.cost),
    }
  }),
)
</script>

<template>
  <div v-if="!rows.length" class="empty">
    <div class="empty-ico" aria-hidden="true">◍</div>
    <div class="empty-text">{{ emptyText }}</div>
  </div>

  <div v-else class="table-wrap">
    <table class="data">
      <thead>
        <tr>
          <th>{{ keyLabel }}</th>
          <th class="num">调用次数</th>
          <th class="num">Prompt</th>
          <th class="num">Completion</th>
          <th class="num">总 token</th>
          <th class="num">平均延迟</th>
          <th class="num">费用</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="item in items" :key="item.id">
          <td class="mono" :title="item.rawKey">{{ item.label }}</td>
          <td class="num">{{ item.calls }}</td>
          <td class="num" :title="item.promptTitle">{{ item.prompt }}</td>
          <td class="num" :title="item.completionTitle">{{ item.completion }}</td>
          <td class="num" :title="item.totalTitle">{{ item.total }}</td>
          <td class="num dim">{{ item.avg }}</td>
          <td class="num" :class="{ dim: item.costUnknown }">
            <span :title="item.costUnknown ? '没有匹配到价格表条目，费用未知' : '按价格表估算（USD）'">
              {{ item.cost }}
            </span>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
