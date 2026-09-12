<script setup>
// Message composer — one integrated control, the way a modern chat client does
// it: a single large rounded container holding the textarea, with the model
// selector at its bottom-left and the send (or 停止) button at its bottom-right.
// The container lights up with a focus ring while the textarea has focus.
//
// Enter sends, Shift+Enter inserts a newline; Enter is ignored while an IME
// composition is active, otherwise confirming a Chinese candidate would send
// the message.
import { computed, nextTick, ref, watch } from 'vue'
import Icon from './Icon.vue'
import { changeModel, modelOptionGroups, selectedModelKey } from '../chatStore.js'

const props = defineProps({
  /** No conversation selected: the composer is inert and explains why. */
  disabled: { type: Boolean, default: false },
  streaming: { type: Boolean, default: false },
  /** Text of a failed send, restored so the user does not retype it. */
  pending: { type: String, default: '' },
})

const emit = defineEmits(['send', 'stop'])

/** Resting height ≈ 4 lines; grows with the content, then scrolls. */
const MIN_HEIGHT = 96
const MAX_HEIGHT = 260

const value = ref('')
const composing = ref(false)
const area = ref(null)

const canSend = computed(() => !props.disabled && !props.streaming && value.value.trim() !== '')
const groups = computed(() => modelOptionGroups.value)
const modelKey = computed(() => selectedModelKey.value)
const pickerDisabled = computed(() => props.disabled || props.streaming)
const hint = computed(() => {
  if (props.disabled) return '请先新建或选择一个对话'
  if (props.streaming) return '正在生成，可点击「停止」中断本轮'
  return 'Enter 发送 · Shift+Enter 换行'
})

function autosize() {
  const el = area.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = `${Math.min(MAX_HEIGHT, Math.max(MIN_HEIGHT, el.scrollHeight))}px`
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

function onModelChange(event) {
  changeModel(event.target.value)
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
    <div class="composer-box">
      <textarea
        ref="area"
        v-model="value"
        class="composer-input"
        rows="1"
        :placeholder="disabled ? '请先新建或选择一个对话…' : '输入消息…'"
        :disabled="disabled"
        aria-label="消息内容"
        @input="autosize"
        @keydown="onKeydown"
        @compositionstart="composing = true"
        @compositionend="composing = false"
      />

      <div class="composer-bar">
        <label class="composer-model" :title="pickerDisabled ? '当前不可切换模型' : '选择模型'">
          <Icon name="cpu" :size="14" />
          <select
            class="composer-select"
            :value="modelKey"
            aria-label="选择模型"
            :disabled="pickerDisabled"
            @change="onModelChange"
          >
            <optgroup v-for="group in groups" :key="group.provider" :label="group.provider">
              <option
                v-for="option in group.options"
                :key="option.key"
                :value="option.key"
                :disabled="option.disabled"
              >
                {{ option.label }}
              </option>
            </optgroup>
          </select>
          <Icon name="chevron-down" :size="13" />
        </label>

        <span class="composer-hint">{{ hint }}</span>

        <button v-if="streaming" type="button" class="composer-stop" title="停止生成" @click="emit('stop')">
          <Icon name="square" :size="14" />
          停止
        </button>
        <button
          v-else
          type="submit"
          class="composer-send"
          :disabled="!canSend"
          :title="canSend ? '发送（Enter）' : '输入内容后可发送'"
          aria-label="发送"
        >
          <Icon name="arrow-up" :size="18" />
        </button>
      </div>
    </div>
  </form>
</template>
