<script setup>
// Collapsible, null-safe JSON viewer.
//
// Trace input/output payloads are arbitrary JSON (string, object, array or
// absent), so the value is pretty-printed and truncated by format.js rather
// than rendered as-is. `toggle: false` makes the block follow the `open` prop
// only, which lets a parent row own the expand/collapse (the trace waterfall).
import { computed, ref, watch } from 'vue'
import { formatJson } from '../format.js'

const props = defineProps({
  value: { type: [String, Object, Array, Number, Boolean], default: null },
  label: { type: String, default: 'JSON' },
  /** Initial (or, with toggle=false, authoritative) expanded state. */
  open: { type: Boolean, default: false },
  /** Render the expand/collapse control. */
  toggle: { type: Boolean, default: true },
  maxLength: { type: Number, default: 4000 },
})

const internal = ref(Boolean(props.open))
watch(
  () => props.open,
  (value) => {
    internal.value = Boolean(value)
  },
)

const text = computed(() => formatJson(props.value, props.maxLength))
const expanded = computed(() => (props.toggle ? internal.value : Boolean(props.open)))
const size = computed(() => text.value.length)
</script>

<template>
  <div v-if="text" class="json-block">
    <button
      v-if="toggle"
      type="button"
      class="json-toggle"
      :aria-expanded="expanded"
      @click="internal = !internal"
    >
      <span class="json-caret" aria-hidden="true">{{ expanded ? '▾' : '▸' }}</span>
      <span>{{ label }}</span>
      <span v-if="!expanded" class="dimmer">{{ size.toLocaleString('en-US') }} 字符</span>
    </button>
    <div v-else class="json-label">{{ label }}</div>
    <pre v-if="expanded" class="json">{{ text }}</pre>
  </div>
</template>
