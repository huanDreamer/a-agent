<script setup>
import { onMounted } from 'vue'
import AppHeader from './components/AppHeader.vue'
import LoginView from './components/LoginView.vue'
import DashboardView from './components/DashboardView.vue'
import ByModelView from './components/ByModelView.vue'
import ByUserView from './components/ByUserView.vue'
import RecentView from './components/RecentView.vue'
import AuditView from './components/AuditView.vue'
import SkillsView from './components/SkillsView.vue'
import { TABS, checkSession, state } from './state.js'

const VIEWS = {
  dashboard: DashboardView,
  model: ByModelView,
  user: ByUserView,
  recent: RecentView,
  audit: AuditView,
  skills: SkillsView,
}

onMounted(checkSession)
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

  <template v-else>
    <AppHeader />
    <div class="app-shell">
      <div class="stack">
        <component :is="VIEWS[state.tab]" :key="state.tab" />
      </div>
      <p class="muted-note" style="margin-top: 26px; text-align: center">
        huan-agent admin · 数据来自本机 API，未做任何缓存
      </p>
    </div>
  </template>
</template>
