<script setup>
// One ask_user card: the model's question, its choices, a free-text box and the
// 提交 button.
//
// The card is rendered from a model built by ask.js, which normalises the live
// `ask_user` event and a reloaded `ask_user` tool call into the same shape — so
// this component has exactly one input, and a card looks the same whether the
// reader watched the turn or reopened the conversation tomorrow.
//
// It owns only what the user is doing right now (which option is highlighted,
// what has been typed); whether the card is still open, and what was finally
// answered, come from the model. That split is what makes the settled state
// authoritative: the server's outcome event can land while the user is still
// looking at the card, and the card must follow it rather than its own guess.
import { computed, nextTick, ref, watch } from 'vue'
import Icon from './Icon.vue'
import {
  ASK_ANSWERED,
  ASK_CANCELLED,
  ASK_PENDING,
  ASK_SUBMITTING,
  ASK_TIMEOUT,
  askAnswerLine,
  askClosedNote,
  askStatusLabel,
  isAskOpen,
} from '../ask.js'

const props = defineProps({
  /** Card model from ask.js (live event or persisted tool call). */
  ask: { type: Object, required: true },
})

const emit = defineEmits(['submit'])

/** The person's own words, before submission. */
const custom = ref('')
const composing = ref(false)
const area = ref(null)

/** Resting height is one line; the box grows with what is typed. */
const MAX_HEIGHT = 140

const open = computed(() => isAskOpen(props.ask))
const submitting = computed(() => props.ask.status === ASK_SUBMITTING)
/** The choices stay on screen while the answer is in flight: hiding them the
    instant 提交 is pressed would make the card flicker into its settled look a
    moment before the server confirms anything. */
const inputsVisible = computed(() => open.value || submitting.value)
const multi = computed(() => Boolean(props.ask.multiSelect))
const options = computed(() => (Array.isArray(props.ask.options) ? props.ask.options : []))
const selected = computed(() => (Array.isArray(props.ask.selected) ? props.ask.selected : []))
/** A card with no options is free text only, so the box has to be there. */
const showCustom = computed(() => props.ask.allowCustom !== false || options.value.length === 0)
const answerLine = computed(() => askAnswerLine(props.ask))
const statusLabel = computed(() => askStatusLabel(props.ask))
const closedNote = computed(() => askClosedNote(props.ask))
const hasAnswer = computed(() => selected.value.length > 0 || custom.value.trim() !== '')
const canSubmit = computed(() => open.value && !submitting.value && hasAnswer.value)

const tone = computed(() => {
  if (props.ask.status === ASK_PENDING || props.ask.status === ASK_SUBMITTING) return 'blue'
  if (props.ask.status === ASK_ANSWERED) return 'ok'
  if (props.ask.status === ASK_TIMEOUT) return 'warn'
  return ''
})

const hint = computed(() => {
  if (props.ask.status === ASK_SUBMITTING) return '正在提交…'
  if (!open.value) return ''
  if (options.value.length && showCustom.value) {
    return multi.value ? '可多选，也可以自己补充说明' : '选一个，或自己输入'
  }
  if (options.value.length) return multi.value ? '可多选' : '选一个'
  return '请输入你的回答'
})

// Only a *settled* card fills the box from the model: doing it while the answer
// is being submitted would wipe what the user just typed. A card that arrives
// already answered (a reloaded conversation) is settled from the start, so its
// own text shows up here and nowhere else.
watch(
  () => [props.ask.id, props.ask.text, props.ask.status],
  () => {
    const status = props.ask.status
    if (status === ASK_ANSWERED || status === ASK_TIMEOUT || status === ASK_CANCELLED) {
      custom.value = props.ask.text || ''
    }
  },
  { immediate: true },
)

function isPicked(label) {
  return selected.value.includes(label)
}

function autosize() {
  const el = area.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = `${Math.min(MAX_HEIGHT, el.scrollHeight)}px`
}

/**
 * Clicking a choice.
 *
 * Single-choice is exclusive with the text box: one question has one answer, and
 * leaving both filled would submit a combination the model has to guess the
 * meaning of. Multi-select keeps both, because "these two, and please also…" is
 * a real answer.
 */
function toggleOption(option) {
  if (!open.value) return
  const label = option.label
  const next = [...selected.value]
  const at = next.indexOf(label)
  if (multi.value) {
    if (at >= 0) next.splice(at, 1)
    else next.push(label)
  } else if (at >= 0) {
    next.length = 0
  } else {
    next.length = 0
    next.push(label)
    custom.value = ''
  }
  props.ask.selected = next
}

function onInput() {
  // Typing is choosing, in single-choice cards: an answer is either the option
  // or the text, never both.
  if (!multi.value && custom.value.trim() !== '' && selected.value.length) {
    props.ask.selected = []
  }
  nextTick(autosize)
}

function onKeydown(event) {
  if (event.key !== 'Enter' || event.shiftKey) return
  // Enter inside an IME composition confirms a candidate, not the answer.
  if (composing.value || event.isComposing) return
  event.preventDefault()
  submit()
}

function submit() {
  if (!canSubmit.value) return
  emit('submit', { selected: [...selected.value], text: custom.value.trim() })
}
</script>

<template>
  <section class="ask-card" :class="{ 'ask-live': open }">
    <header class="ask-head">
      <Icon name="help-circle" :size="15" />
      <span class="ask-title">{{ ask.header || '需要你决定' }}</span>
      <span v-if="statusLabel" class="tag" :class="tone">{{ statusLabel }}</span>
      <span v-if="submitting" class="spin" aria-hidden="true" />
    </header>

    <!-- The question is plain text on purpose, even though the answer below the
         tools is Markdown. MarkdownText is the only component that reaches
         v-html, and markdown.js installs DOMPurify's DOM hooks at import time —
         so a card that rendered through it could not be exercised by the SSR
         probes at all, and this card is exactly the kind of interactive surface
         those probes exist to check. Newlines are preserved by CSS. -->
    <div class="ask-question">{{ ask.question }}</div>

    <!-- Buttons with aria-pressed rather than radios: the choices are toggles the
         user can take back before submitting, not a form the page will read. -->
    <div v-if="options.length" class="ask-options" role="group">
      <button
        v-for="option in options"
        :key="option.label"
        type="button"
        class="ask-option"
        :class="{ picked: isPicked(option.label) }"
        :disabled="!open"
        :aria-pressed="isPicked(option.label)"
        @click="toggleOption(option)"
      >
        <span class="ask-mark" aria-hidden="true">
          <Icon v-if="isPicked(option.label)" name="check" :size="13" />
        </span>
        <span class="ask-option-text">
          <span class="ask-option-label">{{ option.label }}</span>
          <span v-if="option.description" class="ask-option-desc">{{ option.description }}</span>
        </span>
      </button>
    </div>

    <textarea
      v-if="showCustom && inputsVisible"
      ref="area"
      v-model="custom"
      class="input ask-input"
      rows="1"
      :placeholder="options.length ? '也可以自己输入…' : '输入你的回答…'"
      :disabled="!open"
      @input="onInput"
      @keydown="onKeydown"
      @compositionstart="composing = true"
      @compositionend="composing = false"
    />

    <!-- The settled card states the answer in the same words the model was given,
         so a reader of the conversation and the model agree on what was decided. -->
    <div v-if="!open && answerLine" class="ask-answer">
      <Icon name="check" :size="14" />
      <span class="ask-answer-text">{{ answerLine }}</span>
    </div>

    <div v-if="closedNote" class="ask-closed">{{ closedNote }}</div>
    <div v-if="ask.error" class="ask-error" role="alert">{{ ask.error }}</div>

    <div v-if="inputsVisible" class="ask-actions">
      <span class="dimmer ask-hint">{{ hint }}</span>
      <button type="button" class="btn primary sm" :disabled="!canSubmit" @click="submit">
        {{ submitting ? '提交中…' : '提交答案' }}
      </button>
    </div>
  </section>
</template>
