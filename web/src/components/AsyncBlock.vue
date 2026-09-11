<script setup>
// Shared building blocks: every data view renders these instead of ad-hoc
// markup, which is what keeps loading/empty/error states consistent.
import { computed } from 'vue'

const props = defineProps({
  // 'loading' | 'error' | 'empty' | 'ready'
  state: { type: String, default: 'ready' },
  error: { type: String, default: '' },
  // Number of shimmer rows to draw while loading.
  skeletonRows: { type: Number, default: 5 },
  emptyText: { type: String, default: '暂无数据' },
  emptyHint: { type: String, default: '' },
  /** Render an empty <div class="card"> wrapper around the state. */
  bare: { type: Boolean, default: false },
})

const emit = defineEmits(['retry'])

const rows = computed(() => Math.max(1, Math.min(20, props.skeletonRows)))
</script>

<template>
  <!-- error -->
  <div v-if="state === 'error'" class="banner error" role="alert">
    <span aria-hidden="true">⚠</span>
    <span class="banner-text">加载失败：{{ error || '请稍后重试' }}</span>
    <button type="button" class="btn sm" @click="emit('retry')">重试</button>
  </div>

  <!-- loading -->
  <div v-else-if="state === 'loading'" class="stack" aria-busy="true" aria-live="polite">
    <span class="skeleton" style="height: 14px; width: 34%" />
    <span
      v-for="i in rows"
      :key="i"
      class="skeleton"
      :style="{ height: '13px', width: `${96 - ((i * 7) % 26)}%` }"
    />
    <span class="muted-note">加载中…</span>
  </div>

  <!-- empty -->
  <div v-else-if="state === 'empty'" class="empty">
    <div class="empty-ico" aria-hidden="true">◍</div>
    <div class="empty-text">{{ emptyText }}</div>
    <div v-if="emptyHint" class="empty-hint">{{ emptyHint }}</div>
  </div>

  <!-- ready -->
  <slot v-else />
</template>
