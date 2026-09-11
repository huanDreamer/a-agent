<script setup>
// The mobile drawer trigger. Rendered inside each view's context header and
// hidden by CSS above 900px, where the sidebar is a permanent column.
import Icon from './Icon.vue'
import { setDrawer } from '../ui.js'
</script>

<template>
  <button
    type="button"
    class="btn ghost sm icon-btn drawer-btn"
    title="显示导航与会话列表"
    aria-label="显示导航与会话列表"
    @click="setDrawer(true)"
  >
    <Icon name="panel-left" :size="16" />
  </button>
</template>
