<script setup>
// 统计监控 — the five usage views plus tracing, as sub-tabs inside one page.
//
// The page head carries the sub-tab strip and, for the usage sub-tabs, the
// time-range selector + 刷新 (ViewToolbar). Those controls drive the `since` /
// `days` query parameters of the requests each sub-view makes, so they have to
// stay mounted with the usage panels — 链路追踪 keeps its own filters instead.
//
// Each sub-view is a plain content panel: it renders its own loading / empty /
// error states, and this page owns the head and the single scroll container.
import { computed } from 'vue'
import AuditView from './AuditView.vue'
import ByModelView from './ByModelView.vue'
import ByUserView from './ByUserView.vue'
import DashboardView from './DashboardView.vue'
import RecentView from './RecentView.vue'
import TraceView from './TraceView.vue'
import ViewHead from './ViewHead.vue'
import ViewToolbar from './ViewToolbar.vue'
import { MONITOR_TABS, rangeNote, setMonitor, state } from '../state.js'

const PANELS = {
  dashboard: DashboardView,
  model: ByModelView,
  user: ByUserView,
  recent: RecentView,
  audit: AuditView,
  traces: TraceView,
}

/** Sub-title per sub-tab; the usage ones name the active range. */
const NOTES = {
  dashboard: () => `用量统计 · ${rangeNote.value} · 数据直接来自本机 API`,
  model: () => `${rangeNote.value} · 模型维度的用量与费用估算`,
  user: () => `${rangeNote.value} · 用户维度用量（key 为飞书 open_id）`,
  recent: () => `${rangeNote.value} · 最新在前 · 悬停时间可查看绝对时间`,
  audit: () => '工具调用审计 · 最新在前 · 可按工具名与用户过滤',
  traces: () => '对话与工具调用的 trace 来自 Langfuse，经本机 API 代理读取',
}

const active = computed(() => PANELS[state.monitor] || DashboardView)
const note = computed(() => {
  const build = NOTES[state.monitor] || NOTES.dashboard
  return build()
})

/**
 * The time range only means something for the usage queries
 * (/api/usage/*?since=…&days=…): 审计日志 has no time filter of its own and
 * 链路追踪 uses session / name / user instead, so both get 刷新 alone.
 */
const usageSubTab = computed(() =>
  ['dashboard', 'model', 'user', 'recent'].includes(state.monitor),
)
</script>

<template>
  <div class="page">
    <ViewHead title="统计监控" :note="note">
      <template #tools>
        <ViewToolbar v-if="state.monitor !== 'traces'" :range="usageSubTab" />
      </template>
    </ViewHead>

    <div class="subtabs" role="tablist" aria-label="统计监控分区">
      <button
        v-for="tab in MONITOR_TABS"
        :key="tab.key"
        type="button"
        role="tab"
        class="subtab"
        :class="{ active: state.monitor === tab.key }"
        :aria-selected="state.monitor === tab.key"
        @click="setMonitor(tab.key)"
      >
        {{ tab.label }}
      </button>
    </div>

    <div class="page-scroll">
      <component :is="active" :key="state.monitor" />
    </div>
  </div>
</template>
