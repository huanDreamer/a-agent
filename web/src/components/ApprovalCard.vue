<script setup>
// One approval card: what the model wants to do, what it will do it to, and the
// three ways to answer.
//
// It is a card rather than a tool row because the action has not happened yet.
// The whole point of the gate is that a person sees the action *before* it runs,
// so the card has to show enough to judge: the command in full, or the file and
// what would change in it.
//
// The card owns only what the reader is doing right now (which button is
// highlighted, what has been typed as a reason). Whether it is still open, and how
// it ended, come from the server's events — so a countdown that runs out while the
// server has already refused the request does not leave buttons offering a
// decision nobody is listening for.
import { computed, onUnmounted, ref } from 'vue'
import Icon from './Icon.vue'
import {
  APPROVAL_ALLOW_ONCE,
  APPROVAL_ALLOW_TURN,
  APPROVAL_DENY,
  APPROVAL_SUBMITTING,
  approvalHint,
  approvalOutcomeLine,
  approvalSecondsLeft,
  isApprovalOpen,
} from '../approval.js'

const props = defineProps({
  /** Card model from approval.js. */
  card: { type: Object, required: true },
})

const emit = defineEmits(['decide'])

const reason = ref('')
const denying = ref(false)

/** A tick for the countdown. It runs only while a card is open, so an idle
    conversation is not waking up every second for nothing. */
const now = ref(Date.now())
let ticker = null
function startTicker() {
  if (ticker) return
  ticker = window.setInterval(() => {
    now.value = Date.now()
  }, 1000)
}
function stopTicker() {
  if (ticker) {
    window.clearInterval(ticker)
    ticker = null
  }
}
startTicker()
onUnmounted(stopTicker)

const open = computed(() => isApprovalOpen(props.card))
const submitting = computed(() => props.card.status === APPROVAL_SUBMITTING)
const preview = computed(() => (Array.isArray(props.card.preview) ? props.card.preview : []))
const secondsLeft = computed(() => approvalSecondsLeft(props.card, now.value))
const outcome = computed(() => approvalOutcomeLine(props.card))
const hint = computed(() => approvalHint(props.card))

/** The capability decides what the card warns about. */
const tone = computed(() => {
  if (!open.value) return props.card.decision === APPROVAL_DENY ? 'warn' : 'ok'
  return props.card.capability === 'exec' ? 'warn' : 'blue'
})

const capabilityLabel = computed(() =>
  props.card.capability === 'exec' ? '执行命令' : props.card.capability === 'write' ? '写入文件' : '操作',
)

function decide(kind) {
  if (!open.value || submitting.value) return
  // 拒绝 can carry a reason, so it opens the box instead of submitting at once.
  if (kind === APPROVAL_DENY && !denying.value) {
    denying.value = true
    return
  }
  emit('decide', { kind, reason: kind === APPROVAL_DENY ? reason.value.trim() : '' })
}

function confirmDeny() {
  if (!open.value || submitting.value) return
  emit('decide', { kind: APPROVAL_DENY, reason: reason.value.trim() })
}

function cancelDeny() {
  denying.value = false
  reason.value = ''
}
</script>

<template>
  <div class="approval-card" :class="`tone-${tone}`" role="group" aria-label="需要确认的操作">
    <div class="approval-head">
      <Icon name="shield-alert" :size="14" />
      <span class="approval-title">需要确认：{{ capabilityLabel }}</span>
      <span class="spacer" />
      <span v-if="open && secondsLeft !== null" class="dimmer nowrap" title="等待上限；超时按拒绝处理">
        {{ secondsLeft }}s 后按拒绝处理
      </span>
    </div>

    <div class="approval-summary" :title="card.summary">{{ card.summary }}</div>

    <pre v-if="preview.length" class="approval-preview mono"><code
    ><span
      v-for="(line, i) in preview"
      :key="i"
      class="approval-line"
      :class="`kind-${line.kind}`"
    >{{ line.kind === 'add' ? '+ ' : line.kind === 'del' ? '- ' : '  ' }}{{ line.text }}
</span></code></pre>

    <div v-if="open" class="approval-actions">
      <template v-if="!denying">
        <button type="button" class="btn sm primary" :disabled="submitting" @click="decide(APPROVAL_ALLOW_ONCE)">
          允许一次
        </button>
        <button type="button" class="btn sm" :disabled="submitting" @click="decide(APPROVAL_ALLOW_TURN)">
          本轮都允许
        </button>
        <button type="button" class="btn sm ghost danger-text" :disabled="submitting" @click="decide(APPROVAL_DENY)">
          拒绝
        </button>
        <span class="muted-note">{{ hint }}</span>
      </template>
      <template v-else>
        <input
          v-model="reason"
          class="input approval-reason"
          type="text"
          maxlength="500"
          placeholder="拒绝的理由（会交给模型，让它换个做法；可留空）"
          aria-label="拒绝的理由"
          @keyup.enter="confirmDeny"
          @keyup.esc="cancelDeny"
        />
        <button type="button" class="btn sm danger" :disabled="submitting" @click="confirmDeny">确认拒绝</button>
        <button type="button" class="btn sm ghost" :disabled="submitting" @click="cancelDeny">取消</button>
      </template>
    </div>

    <div v-else class="approval-outcome" :class="card.decision === APPROVAL_DENY ? 'refused' : 'allowed'">
      {{ outcome }}
    </div>

    <div v-if="card.error" class="approval-error" role="alert">{{ card.error }}</div>
  </div>
</template>

<style scoped>
/* Styles live in the shared stylesheet (styles.css), like every other card: the
   project keeps component CSS out of the components so one place decides what a
   card looks like. */
</style>
