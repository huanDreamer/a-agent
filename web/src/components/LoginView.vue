<script setup>
// The login screen, rendered only on a deployment that set
// admin.require_login: true — the one way a session can be missing.
//
// The account is single-user (`admin.username`), so there is no username field:
// one password, and the server's bcrypt hash is the only thing that decides.
// The password goes to POST /api/login; the session token comes back as an
// httpOnly cookie the browser stores and sends on its own, so this app never
// holds the session itself.
//
// The screen is not styled as a failure: the deployment is configured correctly
// and this is how it is meant to start. Only a rejected attempt — the server's
// own message, whether a wrong password or the login throttle — gets the error
// banner.
import { onMounted, ref } from 'vue'
import Icon from './Icon.vue'
import { signIn, state } from '../state.js'

const emit = defineEmits(['signed-in'])

const password = ref('')
const error = ref('')
const busy = ref(false)

// Function ref, as in AppSidebar: a plain `let` captured by the template is the
// pattern this project avoids (see scripts/check-component-bindings.mjs).
let field = null
function setField(el) {
  field = el
}

onMounted(() => {
  if (field) field.focus()
})

async function submit() {
  if (busy.value) return
  if (!password.value) {
    error.value = '请输入密码'
    return
  }
  busy.value = true
  error.value = ''
  const result = await signIn(password.value)
  busy.value = false
  if (!result.ok) {
    error.value = result.error
    // Cleared rather than kept: retyping is the whole next action, and leaving a
    // rejected password in the box invites resubmitting it.
    password.value = ''
    return
  }
  emit('signed-in')
}
</script>

<template>
  <div class="login-gate">
    <form class="login-card" @submit.prevent="submit">
      <div class="login-head">
        <Icon name="lock" :size="18" />
        <h1 class="login-title">huan-agent 控制台</h1>
      </div>

      <p class="login-note">
        该部署开启了 <code class="md-code">admin.require_login</code>，需要密码才能进入。
      </p>

      <!-- The session lives in the server's memory, so the usual reason to be
           back here without having just arrived is a restart. -->
      <p v-if="state.auth.expired" class="banner warn login-banner" role="status">
        <Icon name="circle-alert" :size="15" />
        <span class="banner-text">登录状态已失效（服务端重启会清空会话），请重新登录。</span>
      </p>

      <div class="field">
        <label class="field-label" for="admin-password">密码</label>
        <input
          id="admin-password"
          :ref="setField"
          v-model="password"
          class="input"
          type="password"
          name="password"
          autocomplete="current-password"
          :disabled="busy"
          placeholder="admin.password_hash 对应的密码"
        />
        <span class="field-hint">
          忘记密码就重设一个：<code class="md-code">huan-agent admin set-password</code>
          会写入配置文件，重启服务后生效。
        </span>
      </div>

      <p v-if="error" class="banner error login-banner" role="alert">
        <Icon name="circle-alert" :size="15" />
        <span class="banner-text">{{ error }}</span>
      </p>

      <button type="submit" class="btn primary block" :disabled="busy || !password">
        <Icon name="lock" :size="15" />
        {{ busy ? '登录中…' : '登录' }}
      </button>

      <p class="login-foot">
        单用户控制台：只有一个密码，没有账号体系。会话保存在服务端内存里，服务重启后需要重新登录。
      </p>
    </form>
  </div>
</template>
