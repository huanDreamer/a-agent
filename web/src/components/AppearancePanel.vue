<script setup>
// 外观 — the preferences that belong to this browser rather than to the server.
//
// Only the theme lives here today. It is a sub-tab of its own (rather than a
// card among the server settings) because it is the one setting that changes
// nothing an operator has to reason about: it applies immediately and is stored
// in localStorage, with no request and nothing to save.
import Icon from './Icon.vue'
import { THEMES, setTheme, theme } from '../theme.js'
</script>

<template>
  <div class="stack">
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          外观
          <span class="card-sub">当前：{{ theme.resolved === 'dark' ? '暗色' : '亮色' }}</span>
        </div>
      </div>

      <div class="set-row">
        <div class="set-text">
          <div class="set-name">主题</div>
          <div class="set-desc">
            选择会立即生效并保存在本机浏览器里（localStorage），不影响服务端，也不影响其他设备。
            「跟随系统」会随操作系统的深浅色设置切换。
          </div>
        </div>
        <div class="seg" role="group" aria-label="主题">
          <button
            v-for="item in THEMES"
            :key="item.key"
            type="button"
            :class="{ active: theme.choice === item.key }"
            :title="`主题：${item.label}`"
            :aria-pressed="theme.choice === item.key"
            @click="setTheme(item.key)"
          >
            <Icon :name="item.icon" :size="14" />
            {{ item.label }}
            <Icon v-if="theme.choice === item.key" name="check" :size="13" />
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
