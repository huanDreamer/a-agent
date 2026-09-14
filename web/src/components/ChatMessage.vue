<script setup>
// One conversation bubble (user or assistant).
//
// The assistant bubble carries everything a turn produces: the collapsible
// 思考过程 panel, one card per tool call, the answer itself (rendered as
// Markdown by MarkdownText) and the turn footer with token usage. The user's
// own message stays plain text — it is never rendered as Markdown.
import { computed, ref } from 'vue'
import AskUserCard from './AskUserCard.vue'
import AttachmentView from './AttachmentView.vue'
import Icon from './Icon.vue'
import JsonBlock from './JsonBlock.vue'
import MarkdownText from './MarkdownText.vue'
import { askCardsFromTools, isAskTool } from '../ask.js'
import {
  formatAbsolute,
  formatCompact,
  formatDuration,
  formatExactTokens,
  formatRelative,
} from '../format.js'

const props = defineProps({
  /** Render item built by chatStore.buildItems / the live turn. */
  item: { type: Object, required: true },
  /** Text of the last user message, offered again when the turn failed. */
  retryText: { type: String, default: '' },
})

const emit = defineEmits(['retry', 'openTrace', 'submitAsk'])

const copyState = ref('') // '' | 'ok' | 'fail'

const timeLabel = computed(() => formatRelative(props.item.createdAt))
const timeTitle = computed(() => formatAbsolute(props.item.createdAt))
const reasoningSize = computed(() => String(props.item.reasoning || '').length.toLocaleString('en-US'))

const usage = computed(() => props.item.usage || null)
const usageTotalTitle = computed(() =>
  usage.value ? formatExactTokens(usage.value.totalTokens) : '',
)

/**
 * The model's questions, either the ones raised while this turn streamed or the
 * ones rebuilt from the persisted ask_user calls of a reloaded conversation.
 *
 * The fallback derivation exists because a card is a property of the tool call,
 * not of the item: an item assembled without it (a live turn before the first
 * event, a test fixture) must still render what it has.
 */
const askCards = computed(() => {
  const fromItem = Array.isArray(props.item.asks) ? props.item.asks : []
  if (fromItem.length) return fromItem
  return askCardsFromTools(props.item.tools)
})

/**
 * The tool calls that are *not* questions.
 *
 * ask_user is excluded deliberately: an answered question renders as one card,
 * and leaving the raw call in the list would show the same exchange twice — once
 * as the thing the user actually did, once as machine output.
 */
const toolCards = computed(() =>
  (Array.isArray(props.item.tools) ? props.item.tools : []).filter((tool) => !isAskTool(tool.name)),
)

/** The runner's notices about this turn, as opposed to the model's answer. */
const contextNotices = computed(() =>
  (props.item.notices || []).filter((note) => note && note.kind === 'context'),
)

/**
 * The one-line explanation of a budget stop.
 *
 * For a live turn the text comes from the budget_stop event; for a reloaded
 * one only the reason is stored, so it is spelled out here. Both paths end up
 * saying the same thing, which is the point: the marker must not depend on
 * whether the reader was watching when the turn ended.
 */
const stopNote = computed(() => {
  const live = (props.item.notices || []).find((note) => note && note.kind === 'budget')
  if (live) return live.text
  const reason = props.item.stopReason || ''
  if (!reason) return ''
  if (reason === 'tokens') return '本轮已达到 token 预算，回答可能不完整 · 回复「继续」可接着做'
  if (reason === 'deadline') return '本轮已达到时间上限，回答可能不完整 · 回复「继续」可接着做'
  return '本轮已达到步数上限，回答可能不完整 · 回复「继续」可接着做'
})

function toggleReasoning() {
  // Mark it as user-driven so the auto-collapse on the first answer token
  // never overrides an explicit choice.
  props.item.reasoningTouched = true
  props.item.reasoningOpen = !props.item.reasoningOpen
}

function statusLabel(tool) {
  if (tool.status === 'failed') return '失败'
  if (tool.status === 'running') return '运行中'
  return '成功'
}

function statusTone(tool) {
  if (tool.status === 'failed') return 'bad'
  if (tool.status === 'running') return ''
  return 'ok'
}

/** Longest argument preview kept in the DOM; the title keeps it whole. */
const PREVIEW_MAX = 400

/**
 * One-line argument preview.
 *
 * The raw model-produced arguments are JSON text, so newlines and runs of spaces
 * are collapsed: the row is `white-space: nowrap` and a preview that wrapped
 * would make the card grow with the argument. CSS ellipsis does the visual
 * truncation; the cap only keeps a multi-kilobyte argument out of the DOM.
 */
function argsPreview(tool) {
  const text = oneLine(tool.args)
  return text.length > PREVIEW_MAX ? `${text.slice(0, PREVIEW_MAX)}…` : text
}

/** Hover text: the *full* arguments, not the trimmed preview. */
function argsTitle(tool) {
  return oneLine(tool.args)
}

function oneLine(text) {
  return String(text === null || text === undefined ? '' : text)
    .replace(/\s+/g, ' ')
    .trim()
}

async function copy() {
  const text = props.item.text || ''
  if (text === '') return
  try {
    if (!navigator.clipboard || typeof navigator.clipboard.writeText !== 'function') {
      throw new Error('clipboard unavailable')
    }
    await navigator.clipboard.writeText(text)
    copyState.value = 'ok'
  } catch (err) {
    copyState.value = 'fail'
  }
  window.setTimeout(() => {
    copyState.value = ''
  }, 1800)
}
</script>

<template>
  <div v-if="item.role === 'user'" class="msg msg-user">
    <div class="bubble bubble-user">
      <!-- Attachments sit above the text: an image is the message, the caption
           is the annotation. A message may carry attachments and no text. -->
      <div v-if="item.attachments && item.attachments.length" class="attach-row">
        <AttachmentView
          v-for="file in item.attachments"
          :key="file.id"
          :attachment="file"
        />
      </div>
      <div v-if="item.text" class="bubble-text">{{ item.text }}</div>
    </div>
    <div class="msg-time" :title="timeTitle">{{ timeLabel }}</div>
  </div>

  <div v-else class="msg msg-assistant">
    <div class="bubble bubble-assistant">
      <!-- Only the progress chip is left up here: the 助手 label said the same
           thing the bubble's own alignment already says, and an answer's actions
           belong next to the numbers they sit under rather than floating over the
           answer's first line. The head is dropped entirely once the turn is
           done, so a finished answer starts at its own text. -->
      <div v-if="item.streaming" class="msg-head">
        <span class="chip accent">
          <span class="spin" aria-hidden="true" />生成中
        </span>
      </div>

      <!-- 思考过程: expanded while thinking, collapsed once the answer starts -->
      <div v-if="item.reasoning" class="reason">
        <button
          type="button"
          class="reason-head"
          :aria-expanded="Boolean(item.reasoningOpen)"
          @click="toggleReasoning"
        >
          <Icon :name="item.reasoningOpen ? 'chevron-down' : 'chevron-right'" :size="14" />
          <span>思考过程</span>
          <span class="dimmer">
            {{ item.streaming && item.reasoningOpen ? '推理中…' : `${reasoningSize} 字符` }}
          </span>
        </button>
        <!-- Reasoning is model-written prose too, so it renders as Markdown. -->
        <MarkdownText
          v-if="item.reasoningOpen"
          class="reason-body"
          :text="item.reasoning"
          :streaming="item.streaming"
        />
      </div>

      <!-- The questions the model put to the user and the answers it got. They
           sit above the tool calls because a question is what the turn is
           waiting on, while the tools are what it already did. -->
      <div v-if="askCards.length" class="asks">
        <AskUserCard
          v-for="card in askCards"
          :key="card.id"
          :ask="card"
          @submit="emit('submitAsk', card.id, $event)"
        />
      </div>

      <!-- tool calls, in the order the model requested them -->
      <div v-if="toolCards.length" class="tools">
        <div v-for="tool in toolCards" :key="tool.key" class="tool-card">
          <!-- One row: what was called, how it went, and what it was called
               with. The arguments are visible without a click — that is what a
               reader usually wants — and a long argument ellipsizes instead of
               wrapping, so the row never grows. -->
          <div class="tool-head">
            <button
              type="button"
              class="tool-toggle"
              :aria-expanded="Boolean(tool.open)"
              :title="tool.open ? '收起参数与结果' : '展开参数与结果'"
              :aria-label="tool.open ? '收起参数与结果' : '展开参数与结果'"
              @click="tool.open = !tool.open"
            >
              <Icon :name="tool.open ? 'chevron-down' : 'chevron-right'" :size="13" />
            </button>
            <span class="tag purple mono">{{ tool.name }}</span>
            <span class="tag" :class="statusTone(tool)">{{ statusLabel(tool) }}</span>
            <span v-if="tool.durationMs !== null && tool.durationMs !== undefined" class="dimmer nowrap">
              {{ formatDuration(tool.durationMs) }}
            </span>
            <span v-if="tool.args" class="tool-args" :title="argsTitle(tool)">{{ argsPreview(tool) }}</span>
            <span v-if="tool.status === 'running'" class="spin" aria-hidden="true" />
          </div>

          <div v-if="tool.error" class="cell-err tool-err">{{ tool.error }}</div>

          <div v-if="tool.open" class="tool-body">
            <!-- Expanded, both payloads are shown straight away: the chevron is
                 the only affordance, and asking for a second click to see the
                 arguments it just promised would be worse than the preview. -->
            <JsonBlock :value="tool.args" label="参数" open />
            <JsonBlock :value="tool.result" :label="tool.error ? '结果（失败）' : '结果'" open />
          </div>
        </div>
      </div>

      <MarkdownText v-if="item.text" :text="item.text" :streaming="item.streaming" />
      <div v-else-if="item.streaming" class="empty-inline">
        <span class="spin" aria-hidden="true" />
        <span>正在生成…</span>
      </div>

      <div v-if="item.error" class="banner error turn-warn" role="alert">
        <span aria-hidden="true">⚠</span>
        <span class="banner-text">{{ item.error }}</span>
        <button v-if="retryText" type="button" class="btn sm" @click="emit('retry', retryText)">
          重试
        </button>
      </div>

      <!-- The budget stop, kept out of the answer: the model's words and the
           runner's remark about them are different things, and only one of them
           belongs in the copy buffer. The reason survives a reload (it is
           persisted with the message), so this renders for a stored turn too. -->
      <div v-if="stopNote" class="banner notice turn-warn" role="status">
        <Icon name="clock" :size="14" />
        <span class="banner-text">{{ stopNote }}</span>
        <button v-if="retryText" type="button" class="btn sm" @click="emit('retry', '继续')">
          继续
        </button>
      </div>

      <ul v-if="contextNotices.length" class="turn-notices">
        <li v-for="(note, i) in contextNotices" :key="i">
          <Icon name="layers" :size="13" />
          <span>{{ note.text }}</span>
        </li>
      </ul>

      <!-- The turn footer: the token numbers, then the answer's actions at the far
           right. Deliberately not gated on `usage` — a failed turn is exactly when
           someone wants the trace and it is also the turn most likely to have no
           tokens to show, and a turn whose provider reports no usage still has an
           answer to copy. -->
      <div v-if="usage || item.traceId || item.text" class="turn-usage">
        <template v-if="usage">
          <span>
            prompt <b>{{ formatCompact(usage.promptTokens) }}</b>
          </span>
          <span>
            completion <b>{{ formatCompact(usage.completionTokens) }}</b>
          </span>
          <span :title="usageTotalTitle">
            合计 <b>{{ formatCompact(usage.totalTokens) }}</b> tokens
          </span>
          <span v-if="usage.durationMs !== null && usage.durationMs !== undefined">
            耗时 <b>{{ formatDuration(usage.durationMs) }}</b>
          </span>
        </template>
        <div v-if="item.text || item.traceId" class="turn-actions">
          <button
            v-if="item.text"
            type="button"
            class="btn sm ghost"
            :title="'把回答复制到剪贴板'"
            @click="copy"
          >
            <Icon :name="copyState === 'ok' ? 'check' : 'copy'" :size="14" />
            {{ copyState === 'ok' ? '已复制' : copyState === 'fail' ? '复制失败' : '复制' }}
          </button>
          <button
            v-if="item.traceId"
            type="button"
            class="btn sm ghost"
            title="打开这条回答的链路追踪"
            @click="emit('openTrace', item.traceId)"
          >
            <Icon name="git-branch" :size="14" />
            链路
          </button>
        </div>
      </div>
    </div>
    <div class="msg-time" :title="timeTitle">{{ item.streaming ? '生成中…' : timeLabel }}</div>
  </div>
</template>
