<script setup>
import { onMounted, ref } from 'vue'
import { formatDuration } from '../format.js'
import { api } from '../api.js'
import { login, state } from '../state.js'

const password = ref('')
const error = ref(state.authNotice || '')
const busy = ref(false)
/** 'ok' | 'unauthorized' | 'down' | 'checking' */
const healthState = ref('checking')
const health = ref(null)
const input = ref(null)

onMounted(() => {
  if (input.value) input.value.focus()
  // The health endpoint is unauthenticated: it tells the operator whether the
  // Go server is actually up before they blame their password.
  api
    .health()
    .then((res) => {
      health.value = res
      healthState.value = 'ok'
    })
    .catch((err) => {
      health.value = null
      // A 401 here still proves the server answered — just not with a session.
      healthState.value = err && err.status === 401 ? 'unauthorized' : 'down'
    })
})

async function submit() {
  if (busy.value) return
  if (!password.value) {
    error.value = '请输入管理员密码'
    return
  }
  busy.value = true
  error.value = ''
  state.authNotice = ''
  try {
    await login(password.value)
    password.value = ''
  } catch (err) {
    if (err && err.status === 401) {
      error.value = '密码错误，请重试'
    } else {
      error.value = (err && err.message) || '登录失败，请稍后重试'
    }
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login-screen">
    <form class="login-card" @submit.prevent="submit">
      <div class="login-brand">
        <div class="brand-mark" aria-hidden="true">H</div>
        <div>
          <div class="login-title">huan-agent admin</div>
          <div class="brand-sub">用量统计与可观测性</div>
        </div>
      </div>

      <p class="login-sub">请输入管理员密码以访问控制台。</p>

      <div class="field">
        <label for="admin-password" class="muted-note">管理员密码</label>
        <input
          id="admin-password"
          ref="input"
          v-model="password"
          class="input"
          type="password"
          name="password"
          autocomplete="current-password"
          placeholder="••••••••"
          :disabled="busy"
        />
      </div>

      <div v-if="error" class="banner error" style="margin-top: 14px" role="alert">
        <span aria-hidden="true">⚠</span>
        <span class="banner-text">{{ error }}</span>
      </div>

      <button type="submit" class="btn primary" :disabled="busy">
        <span v-if="busy" class="spin" aria-hidden="true" />{{ busy ? '登录中…' : '登录' }}
      </button>

      <div class="login-foot">
        <span v-if="healthState === 'ok' && health">
          服务在线 · v{{ health.version || 'dev' }} · 已运行
          {{ formatDuration((health.uptime_s || 0) * 1000) }}
        </span>
        <span v-else-if="healthState === 'unauthorized'">服务已响应（需要登录）</span>
        <span v-else-if="healthState === 'checking'">正在检查服务状态…</span>
        <span v-else>未能连接到服务，请确认 huan-agent serve 正在运行</span>
      </div>
    </form>
  </div>
</template>
