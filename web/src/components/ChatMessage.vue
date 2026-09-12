<script setup>
// One conversation bubble (user or assistant).
//
// The assistant bubble carries everything a turn produces: the collapsible
// 思考过程 panel, one card per tool call, the answer itself (rendered as
// Markdown by MarkdownText) and the turn footer with token usage. The user's
// own message stays plain text — it is never rendered as Markdown.
import { computed, ref } from 'vue'
import Icon from './Icon.vue'
import JsonBlock from './JsonBlock.vue'
import MarkdownText from './MarkdownText.vue'
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

const emit = defineEmits(['retry'])

const copyState = ref('') // '' | 'ok' | 'fail'

const timeLabel = computed(() => formatRelative(props.item.createdAt))
const timeTitle = computed(() => formatAbsolute(props.item.createdAt))
const reasoningSize = computed(() => String(props.item.reasoning || '').length.toLocaleString('en-US'))

const usage = computed(() => props.item.usage || null)
const usageTotalTitle = computed(() =>
  usage.value ? formatExactTokens(usage.value.totalTokens) : '',
)

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
    <div class="bubble bubble-user">{{ item.text }}</div>
    <div class="msg-time" :title="timeTitle">{{ timeLabel }}</div>
  </div>

  <div v-else class="msg msg-assistant">
    <div class="bubble bubble-assistant">
      <div class="msg-head">
        <span class="msg-role">助手</span>
        <span v-if="item.streaming" class="chip accent">
          <span class="spin" aria-hidden="true" />生成中
        </span>
        <span class="spacer" />
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

      <!-- tool calls, in the order the model requested them -->
      <div v-if="item.tools && item.tools.length" class="tools">
        <div v-for="tool in item.tools" :key="tool.key" class="tool-card">
          <div class="tool-head">
            <span class="tag purple mono">{{ tool.name }}</span>
            <span class="tag" :class="statusTone(tool)">
              {{ statusLabel(tool) }}
              <template v-if="tool.status === 'failed' && tool.error">· 已报错</template>
            </span>
            <span v-if="tool.durationMs !== null && tool.durationMs !== undefined" class="dimmer">
              {{ formatDuration(tool.durationMs) }}
            </span>
            <span class="spacer" />
            <button type="button" class="btn sm ghost" @click="tool.open = !tool.open">
              {{ tool.open ? '收起' : '参数/结果' }}
            </button>
          </div>

          <div v-if="tool.error" class="cell-err tool-err">{{ tool.error }}</div>

          <div v-if="tool.open" class="tool-body">
            <JsonBlock :value="tool.args" label="参数" />
            <JsonBlock :value="tool.result" :label="tool.error ? '结果（失败）' : '结果'" />
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

      <div v-if="usage" class="turn-usage">
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
      </div>
    </div>
    <div class="msg-time" :title="timeTitle">{{ item.streaming ? '生成中…' : timeLabel }}</div>
  </div>
</template>
