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
// are the only sidebar menu entries.
//
// Which screen comes first is the boot probe's call (GET /api/me): a deployment
// that requires a password (admin.require_login: true) gets 登录 before any panel
// is mounted, and the login-free default goes straight to the shell. The probe
// costs one request, and it is the difference between drawing the console and
// drawing panels that would each answer 401.
import { computed, onMounted, watch } from 'vue'
import AppSidebar from './components/AppSidebar.vue'
import ChatView from './components/ChatView.vue'
import LoginView from './components/LoginView.vue'
import MonitorView from './components/MonitorView.vue'
import SettingsView from './components/SettingsView.vue'
import Icon from './components/Icon.vue'
import { ensureLoaded } from './chatStore.js'
import { bootstrap, loadMeta, needsLogin, state } from './state.js'
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
  // Nothing to load while the login form is up: those requests would carry a
  // session the operator does not have yet.
  if (!needsLogin.value) ensureLoaded()
}

onMounted(boot)

/**
 * After a successful login the console has loaded nothing — the gate kept it
 * from firing requests it had no session for — so it does that now.
 *
 * A session that expired mid-use can leave rows from the previous session in the
 * stores; on a single-user console those are the same operator's reads, and 退出
 * 登录 is the path that clears them by reloading the page.
 */
function onSignedIn() {
  loadMeta()
  ensureLoaded()
}

watch(
  () => state.tab,
  (tab) => {
    if (tab === 'chat') ensureLoaded()
  },
)
</script>

<template>
  <!-- Before the probe answers, neither screen is right: the shell would mount
       every panel and fire the requests a login-required deployment answers with
       401, and the login form would flash on every deployment that needs no
       password. One round trip to localhost decides; until then, say so. -->
  <div v-if="!state.auth.probed" class="boot-splash" role="status">
    <span class="spinner" aria-hidden="true" />
    <span>正在连接服务端…</span>
  </div>

  <LoginView v-else-if="needsLogin" @signed-in="onSignedIn" />

  <div v-else class="app">
    <div class="side" :class="{ open: ui.drawerOpen }">
      <AppSidebar @select="ui.drawerOpen = false" />
    </div>
    <div v-if="ui.drawerOpen" class="drawer-backdrop" @click="ui.drawerOpen = false" />

    <main class="main">
      <!-- A 401 on a deployment whose own probe reports no login requirement:
           nothing in this console can produce it, so something in front of the
           server is asking for credentials. There is no form to offer here —
           say what it is instead. -->
      <div v-if="state.denied" class="banner error main-banner" role="alert">
        <Icon name="circle-alert" :size="16" />
        <span class="banner-text">
          服务端返回了 HTTP 401，但本部署报告未开启
          <code class="md-code">admin.require_login</code>。可能是反向代理在要求认证，或服务端刚换了配置；
          刷新页面再试。
        </span>
        <button type="button" class="btn sm" @click="boot">重试</button>
      </div>

      <!-- 对话 owns the full height: its own context header + pinned composer.
           设置 and 统计监控 render a context header and scroll internally. -->
      <component :is="activeView" :key="state.tab" />
    </main>
  </div>
</template>
