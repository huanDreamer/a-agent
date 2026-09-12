<script setup>
// App shell: two columns, exactly one viewport tall.
//
//   ┌──────────────┬──────────────────────────────┐
//   │ sidebar      │ main (context header + view) │
//   │ 264px        │ fills the rest, full height  │
//   └──────────────┴──────────────────────────────┘
//
// Only inner panes scroll — the document itself never does. Below 900px the
// sidebar turns into an overlay drawer driven by ui.drawerOpen.
//
// Three surfaces: 对话 (the primary one), 设置 and 统计监控 — the latter two
// are the only sidebar menu entries. There is no login screen and no redirect:
// the console renders immediately, and GET /api/me is merely a boot probe.
import { computed, onMounted, watch } from 'vue'
import AppSidebar from './components/AppSidebar.vue'
import ChatView from './components/ChatView.vue'
import MonitorView from './components/MonitorView.vue'
import SettingsView from './components/SettingsView.vue'
import Icon from './components/Icon.vue'
import { ensureLoaded } from './chatStore.js'
import { bootstrap, state } from './state.js'
import { ui } from './ui.js'

const VIEWS = {
  chat: ChatView,
  settings: SettingsView,
  monitor: MonitorView,
}

const activeView = computed(() => VIEWS[state.tab] || ChatView)

/** Session list + model catalog, for the whole shell (the sidebar is always up). */
function boot() {
  bootstrap()
  ensureLoaded()
}

onMounted(boot)

watch(
  () => state.tab,
  (tab) => {
    if (tab === 'chat') ensureLoaded()
  },
)
</script>

<template>
  <div class="app">
    <div class="side" :class="{ open: ui.drawerOpen }">
      <AppSidebar @select="ui.drawerOpen = false" />
    </div>
    <div v-if="ui.drawerOpen" class="drawer-backdrop" @click="ui.drawerOpen = false" />

    <main class="main">
      <!-- Only reachable with admin.require_login: true, which this console has
           no flow for. Say so instead of showing half-empty panels. -->
      <div v-if="state.denied" class="banner error main-banner" role="alert">
        <Icon name="circle-alert" :size="16" />
        <span class="banner-text">
          服务端要求登录（HTTP 401）。该部署开启了 admin.require_login，而本控制台不提供登录流程；
          请将其关闭（默认即为关闭）后重启服务。
        </span>
        <button type="button" class="btn sm" @click="boot">重试</button>
      </div>

      <!-- 对话 owns the full height: its own context header + pinned composer.
           设置 and 统计监控 render a context header and scroll internally. -->
      <component :is="activeView" :key="state.tab" />
    </main>
  </div>
</template>
