<script setup>
// Thin context header of the main area: view title, optional subtitle, and a
// right-hand slot for the view's own tools (range selector, refresh, …).
//
// At ≤900px it also carries the drawer button, because the sidebar is gone.
import DrawerButton from './DrawerButton.vue'

defineProps({
  title: { type: String, required: true },
  note: { type: String, default: '' },
})
</script>

<template>
  <header class="view-head">
    <DrawerButton />
    <div class="view-head-text">
      <h1 class="view-title">{{ title }}</h1>
      <p v-if="note" class="view-note">{{ note }}</p>
    </div>
    <div class="view-head-tools">
      <slot name="tools" />
    </div>
  </header>
</template>
