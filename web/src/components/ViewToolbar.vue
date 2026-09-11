<script setup>
// Time-range selector + 刷新.
//
// These used to live in the global header; they belong to the usage views only
// (总览 / 按模型 / 按用户 / 调用记录 / 审计日志), because `state.range` drives
// their `since` filter. 对话 and 技能 never mount this, and 链路追踪 has its own
// filters.
import { computed, ref } from 'vue'
import Icon from './Icon.vue'
import { RANGES, requestRefresh, setRange, state } from '../state.js'

/** Short visual acknowledgement — the queries themselves are fired by the view. */
const busy = ref(false)

const rangeLabel = computed(() => {
  const found = RANGES.find((r) => r.key === state.range)
  return found ? found.label : ''
})

function onRefresh() {
  busy.value = true
  requestRefresh()
  window.setTimeout(() => {
    busy.value = false
  }, 450)
}
</script>

<template>
  <div class="row head-tools">
    <div class="seg" role="group" aria-label="统计时间范围">
      <button
        v-for="range in RANGES"
        :key="range.key"
        type="button"
        :class="{ active: state.range === range.key }"
        :title="`统计范围：${range.label}`"
        :aria-pressed="state.range === range.key"
        @click="setRange(range.key)"
      >
        {{ range.label }}
      </button>
    </div>

    <button
      type="button"
      class="btn ghost sm"
      :disabled="busy"
      :title="`重新加载当前页面数据（当前范围：${rangeLabel}）`"
      @click="onRefresh"
    >
      <Icon name="refresh" :size="15" />
      {{ busy ? '刷新中' : '刷新' }}
    </button>
  </div>
</template>
