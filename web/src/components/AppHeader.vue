<script setup>
import { computed, ref } from 'vue'
import { RANGES, TABS, logout, requestRefresh, setRange, setTab, state } from '../state.js'

const busy = ref(false)

const rangeLabel = computed(() => {
  const range = RANGES.find((r) => r.key === state.range)
  return range ? range.label : ''
})

const meta = computed(() => state.meta || {})

async function onRefresh() {
  busy.value = true
  requestRefresh()
  // The actual queries are fired by the active view; this is a short visual
  // acknowledgement so the button never feels dead.
  window.setTimeout(() => {
    busy.value = false
  }, 450)
}
</script>

<template>
  <div class="topbar">
    <div class="topbar-inner">
      <div class="brand">
        <div class="brand-mark" aria-hidden="true">H</div>
        <div>
          <div class="brand-name">huan-agent admin</div>
          <div class="brand-sub">用量统计与可观测性</div>
        </div>
      </div>

      <div class="chips">
        <span v-if="state.metaError" class="chip bad" :title="state.metaError">服务信息不可用</span>
        <template v-else-if="state.meta">
          <span class="chip accent" title="当前 LLM provider / model">
            <b>{{ meta.provider || '—' }}</b> / {{ meta.model || '—' }}
          </span>
          <span class="chip" title="服务版本">v{{ meta.version || 'dev' }}</span>
          <span v-if="meta.metrics_enabled" class="chip ok" title="Prometheus metrics 已启用">
            metrics
          </span>
          <span v-if="meta.feishu_enabled" class="chip ok" title="飞书接入已启用">飞书</span>
        </template>
        <span v-else class="chip">读取服务信息…</span>
      </div>

      <div class="spacer" />

      <div class="seg" role="group" aria-label="时间范围">
        <button
          v-for="range in RANGES"
          :key="range.key"
          type="button"
          :class="{ active: state.range === range.key }"
          :title="`统计范围：${range.label}`"
          @click="setRange(range.key)"
        >
          {{ range.label }}
        </button>
      </div>

      <button type="button" class="btn" :disabled="busy" title="重新加载当前页面数据" @click="onRefresh">
        {{ busy ? '刷新中…' : '刷新' }}
      </button>

      <span class="chip" :title="`当前登录用户：${state.username || 'admin'}`">
        {{ state.username || 'admin' }}
      </span>

      <button type="button" class="btn danger" title="退出登录" @click="logout()">退出</button>
    </div>

    <nav class="tabs" aria-label="主导航">
      <button
        v-for="tab in TABS"
        :key="tab.key"
        type="button"
        class="tab"
        :class="{ active: state.tab === tab.key }"
        :aria-current="state.tab === tab.key ? 'page' : undefined"
        @click="setTab(tab.key)"
      >
        {{ tab.label }}
      </button>
      <span class="spacer" />
      <span class="tab range-note" style="cursor: default; color: var(--text-dimmer)">
        {{ rangeLabel }}
      </span>
    </nav>
  </div>
</template>
