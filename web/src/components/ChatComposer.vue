<script setup>
// Message composer: auto-sizing textarea, Enter to send, Shift+Enter for a
// newline, and 停止 while a turn is streaming.
//
// Enter is ignored while an IME composition is active, otherwise confirming a
// Chinese candidate would send the message.
import { computed, nextTick, ref, watch } from 'vue'

const props = defineProps({
  /** No conversation selected: the composer is inert and explains why. */
  disabled: { type: Boolean, default: false },
  streaming: { type: Boolean, default: false },
  /** Text of a failed send, restored so the user does not retype it. */
  pending: { type: String, default: '' },
})

const emit = defineEmits(['send', 'stop'])

const MAX_HEIGHT = 190

const value = ref('')
const composing = ref(false)
const area = ref(null)

const canSend = computed(() => !props.disabled && !props.streaming && value.value.trim() !== '')
const hint = computed(() => {
  if (props.disabled) return '请先新建或选择一个对话'
  if (props.streaming) return '正在生成，可点击「停止」中断本轮'
  return 'Enter 发送 · Shift+Enter 换行'
})

function autosize() {
  const el = area.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = `${Math.min(MAX_HEIGHT, Math.max(38, el.scrollHeight))}px`
}

function reset() {
  value.value = ''
  nextTick(autosize)
}

function submit() {
  const text = value.value.trim()
  if (!canSend.value) return
  emit('send', text)
  reset()
}

function onKeydown(event) {
  if (event.key !== 'Enter' || event.shiftKey) return
  if (composing.value || event.isComposing) return
  event.preventDefault()
  submit()
}

function focus() {
  if (area.value) area.value.focus()
}

watch(
  () => props.pending,
  (text) => {
    if (!text) return
    value.value = text
    nextTick(() => {
      autosize()
      focus()
    })
  },
)

defineExpose({ focus })
</script>

<template>
  <form class="composer" @submit.prevent="submit">
    <textarea
      ref="area"
      v-model="value"
      class="input composer-input"
      rows="1"
      :placeholder="disabled ? '请先新建或选择一个对话…' : '输入消息…'"
      :disabled="disabled"
      :aria-label="'消息内容'"
      @input="autosize"
      @keydown="onKeydown"
      @compositionstart="composing = true"
      @compositionend="composing = false"
    />
    <div class="composer-actions">
      <span class="composer-hint">{{ hint }}</span>
      <button v-if="streaming" type="button" class="btn danger" @click="emit('stop')">停止</button>
      <button v-else type="submit" class="btn primary" :disabled="!canSend">发送</button>
    </div>
  </form>
</template>
