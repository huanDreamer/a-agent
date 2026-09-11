<script setup>
// Horizontal bar list for by-model / by-user / by-provider rows.
// Bar width is proportional to the *largest* row in the list (not the total),
// so small values stay visible; zero and empty inputs degrade to "暂无数据".
import { computed } from 'vue'
import { barMetric, rowDetail } from '../metrics.js'
import { DASH, shortId } from '../format.js'

const props = defineProps({
  rows: { type: Array, default: () => [] },
  /** 'calls' | 'total_tokens' | 'cost' */
  metric: { type: String, default: 'calls' },
  /** Column width of the label gutter, e.g. 190 for model names. */
  labelWidth: { type: Number, default: 190 },
  /** Shorten keys (open_id / session ids) in the label. */
  shortenKeys: { type: Boolean, default: false },
  emptyText: { type: String, default: '暂无数据' },
})

const active = computed(() => barMetric(props.metric))

const max = computed(() =>
  props.rows.reduce((acc, row) => Math.max(acc, active.value.barValue(row)), 0),
)

const items = computed(() =>
  props.rows.map((row, index) => {
    const value = active.value.barValue(row)
    const unknown = typeof active.value.unknown === 'function' && active.value.unknown(row)
    const rawKey = row && row.key !== undefined && row.key !== null && row.key !== '' ? row.key : DASH
    return {
      id: `${rawKey}-${index}`,
      rawKey: String(rawKey),
      label: props.shortenKeys ? shortId(rawKey) : String(rawKey),
      value,
      unknown,
      width: max.value > 0 ? Math.max(0, Math.min(100, (value / max.value) * 100)) : 0,
      text: active.value.text(row),
      detail: rowDetail(row),
    }
  }),
)

const allZero = computed(() => props.rows.length > 0 && max.value <= 0)
</script>

<template>
  <div v-if="!rows.length" class="empty">
    <div class="empty-ico" aria-hidden="true">◍</div>
    <div class="empty-text">{{ emptyText }}</div>
  </div>

  <div v-else>
    <div class="bars">
      <div v-for="item in items" :key="item.id" class="bar-row">
        <div class="bar-label" :style="{ maxWidth: `${labelWidth}px` }" :title="item.rawKey">
          {{ item.label }}
        </div>
        <div class="bar-body">
          <div class="bar-track">
            <div
              v-if="item.value > 0"
              class="bar-fill"
              :class="active.tone"
              :style="{ width: `${item.width}%` }"
            />
          </div>
          <div class="bar-meta">
            <b>{{ item.text }}</b>
            <span v-if="item.unknown" class="dimmer" title="没有匹配到价格表条目，费用未知">费用未知</span>
            <span v-for="(part, i) in item.detail" :key="i">{{ part }}</span>
          </div>
        </div>
      </div>
    </div>

    <div v-if="allZero" class="banner info" style="margin-top: 14px">
      <span aria-hidden="true">◌</span>
      <span class="banner-text">该区间内所有条目的{{ active.label }}均为 0。</span>
    </div>
  </div>
</template>
