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
import { computed, onMounted, watch } from 'vue'
import AppSidebar from './components/AppSidebar.vue'
import LoginView from './components/LoginView.vue'
import ChatView from './components/ChatView.vue'
import DashboardView from './components/DashboardView.vue'
import ByModelView from './components/ByModelView.vue'
import ByUserView from './components/ByUserView.vue'
import RecentView from './components/RecentView.vue'
import AuditView from './components/AuditView.vue'
import SkillsView from './components/SkillsView.vue'
import TraceView from './components/TraceView.vue'
import { ensureLoaded } from './chatStore.js'
import { checkSession, state } from './state.js'
import { ui } from './ui.js'

const VIEWS = {
  chat: ChatView,
  dashboard: DashboardView,
  model: ByModelView,
  user: ByUserView,
  recent: RecentView,
  audit: AuditView,
  skills: SkillsView,
  traces: TraceView,
}

/** The sidebar session list must be live even before 对话 was ever opened. */
const activeView = computed(() => VIEWS[state.tab] || ChatView)

onMounted(checkSession)

// The chat store talks to authenticated endpoints, so it is only booted once a
// session cookie exists — a fetch before that would meet a 401 and leave the
// sidebar empty (the store would consider itself already booted).
watch(
  () => state.phase,
  (phase) => {
    if (phase === 'ready') ensureLoaded()
  },
  { immediate: true },
)

watch(
  () => state.tab,
  (tab) => {
    if (tab === 'chat') ensureLoaded()
  },
)
</script>

<template>
  <!-- Boot: we do not know yet whether a session cookie exists. -->
  <div v-if="state.phase === 'checking'" class="login-screen">
    <div class="login-card">
      <div class="login-brand">
        <div class="brand-mark">H</div>
        <div class="login-title">huan-agent admin</div>
      </div>
      <div class="empty">
        <span class="spin" />
        <span class="empty-text">正在检查登录状态…</span>
      </div>
    </div>
  </div>

  <LoginView v-else-if="state.phase === 'login'" />

  <div v-else class="app">
    <div class="side" :class="{ open: ui.drawerOpen }">
      <AppSidebar @select="ui.drawerOpen = false" />
    </div>
    <div v-if="ui.drawerOpen" class="drawer-backdrop" @click="ui.drawerOpen = false" />

    <main class="main">
      <!-- 对话 owns the full height: its own context header + pinned composer.
           Every other view renders a context header and scrolls internally. -->
      <component :is="activeView" :key="state.tab" />
    </main>
  </div>
</template>
