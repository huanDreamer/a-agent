<script setup>
// A single headline metric. `value` is pre-formatted by the caller so each
// view controls whether counts, tokens or costs are shown.
defineProps({
  label: { type: String, required: true },
  value: { type: [String, Number], default: '—' },
  sub: { type: String, default: '' },
  /** '' | 'g' | 'b' | 'a' | 'p' | 'c' — accent colour of the value. */
  tone: { type: String, default: '' },
  title: { type: String, default: '' },
})
</script>

<template>
  <div class="stat" :class="tone" :title="title">
    <div class="n">{{ value }}</div>
    <div class="k">{{ label }}</div>
    <div v-if="sub" class="sub">{{ sub }}</div>
  </div>
</template>
