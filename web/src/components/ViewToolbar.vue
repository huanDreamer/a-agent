<script setup>
// Time-range selector + 刷新 for the usage sub-tabs of 统计监控.
//
// `state.range` is what every usage query turns into `since` (and the trend
// chart into `days`), so this control — not a global header — is what makes the
// range work. 审计日志 mounts it with :range="false": that view has no time
// filter of its own, but 刷新 is still useful there. 链路追踪 never mounts it.
import { computed, ref } from 'vue'
import Icon from './Icon.vue'
import { RANGES, requestRefresh, setRange, state } from '../state.js'

const props = defineProps({
  /** Render the 24小时 / 7天 / 30天 / 全部 segment. */
  range: { type: Boolean, default: true },
})

/** Short visual acknowledgement — the queries themselves are fired by the view. */
const busy = ref(false)

const rangeLabel = computed(() => {
  const found = RANGES.find((r) => r.key === state.range)
  return found ? found.label : ''
})

const refreshTitle = computed(() =>
  props.range
    ? `重新加载当前页面数据（当前范围：${rangeLabel.value}）`
    : '重新加载当前页面数据',
)

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
    <div v-if="range" class="seg" role="group" aria-label="统计时间范围">
      <button
        v-for="item in RANGES"
        :key="item.key"
        type="button"
        :class="{ active: state.range === item.key }"
        :title="`统计范围：${item.label}`"
        :aria-pressed="state.range === item.key"
        @click="setRange(item.key)"
      >
        {{ item.label }}
      </button>
    </div>

    <button type="button" class="btn ghost sm" :disabled="busy" :title="refreshTitle" @click="onRefresh">
      <Icon name="refresh" :size="15" />
      {{ busy ? '刷新中' : '刷新' }}
    </button>
  </div>
</template>
