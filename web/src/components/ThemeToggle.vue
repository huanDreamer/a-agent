<script setup>
// 跟随系统 / 亮色 / 暗色. The choice is persisted by theme.js and applied as
// `class="dark"` on <html>.
import Icon from './Icon.vue'
import { THEMES, setTheme, theme } from '../theme.js'
</script>

<template>
  <div class="theme-toggle" role="group" aria-label="主题">
    <button
      v-for="item in THEMES"
      :key="item.key"
      type="button"
      class="theme-btn"
      :class="{ active: theme.choice === item.key }"
      :title="`主题：${item.label}`"
      :aria-label="`主题：${item.label}`"
      :aria-pressed="theme.choice === item.key"
      @click="setTheme(item.key)"
    >
      <Icon :name="item.icon" :size="15" />
    </button>
  </div>
</template>
