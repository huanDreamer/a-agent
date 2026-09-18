<script setup>
// One conversation bubble (user or assistant).
//
// The assistant bubble is a record of *how* the turn was worked out and then of
// what it concluded: the process — one block per step, each holding that step's
// thinking and the tools it ran — comes first, and the answer stands alone
// underneath it. The user's own message stays plain text, never Markdown.
//
// The order is the point. A turn is a ReAct loop, so its reasoning and its tool
// calls alternate; showing all the tool cards and then all the thinking makes the
// reader pair them up by eye, and there is nothing in the data to pair them by
// once they have been shown apart.
//
// What folds is a step's tool cards, not its thinking. Reasoning is what makes
// the process legible — a folded step that showed only a list of tool names said
// what ran but not why — while the tool cards carry the bulk (arguments and
// results run to hundreds of kilobytes in a long turn, reasoning to a few
// paragraphs, and .reason-body caps and scrolls its own). So the thinking stays in
// the flow and the tools fold away under it, still one click from being read.
import { computed, ref, watch } from 'vue'
import AskUserCard from './AskUserCard.vue'
import AttachmentView from './AttachmentView.vue'
import Icon from './Icon.vue'
import JsonBlock from './JsonBlock.vue'
import MarkdownText from './MarkdownText.vue'
import { askCardsFromTools, isAskTool } from '../ask.js'
import {
  foldSteps,
  processShouldBeOpen,
  stepFailed,
  stepSummary,
  stepToolMs,
  visibleSteps,
  visibleTools,
} from '../steps.js'
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
  /**
   * 这个气泡是不是"可以接着跑"的那一个：计划还有未完成项、没有轮次在跑、并且
   * 它是最新的一条回答。判断需要整段对话（最后一条是谁），所以由 ChatView 算，
   * 这里只负责画。
   */
  canResume: { type: Boolean, default: false },
})

const emit = defineEmits(['retry', 'openTrace', 'submitAsk', 'resume'])

const copyState = ref('') // '' | 'ok' | 'fail'

const timeLabel = computed(() => formatRelative(props.item.createdAt))
const timeTitle = computed(() => formatAbsolute(props.item.createdAt))

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
 * event, a test fixture) must still render what it has. The flat `tools` list is
 * read first because that is where the server puts every call; the steps are
 * searched only when it is empty, so a question is never shown twice.
 */
const askCards = computed(() => {
  const fromItem = Array.isArray(props.item.asks) ? props.item.asks : []
  if (fromItem.length) return fromItem
  const fromTools = askCardsFromTools(props.item.tools)
  if (fromTools.length) return fromTools
  return askCardsFromTools(
    (Array.isArray(props.item.steps) ? props.item.steps : []).flatMap((step) =>
      step && Array.isArray(step.tools) ? step.tools : [],
    ),
  )
})

/**
 * The turn's process, one block per step.
 *
 * ask_user is filtered out of the tool lists deliberately: an answered question
 * renders as one card above, and leaving the raw call in a step would show the
 * same exchange twice — once as the thing the user actually did, once as machine
 * output. Steps with nothing left are dropped, which is why this is a derivation
 * rather than a straight read (see steps.js).
 */
const steps = computed(() => visibleSteps(props.item.steps, isAskTool))

/** The newest step: the one still working, while the turn runs. */
const lastIndex = computed(() => steps.value.length - 1)

/**
 * A foldable header line, computed for every step once.
 *
 * `step` is the reactive step itself, never a copy: `open` and `touched` live on
 * it, so a click has to reach that object or the fold would land on a throwaway.
 * `tools` is the narrowed list the block renders — the ask_user call in it is the
 * card above, not a tool row.
 */
const stepViews = computed(() =>
  steps.value.map((step, i) => ({
    step,
    tools: visibleTools(step, isAskTool),
    last: i === lastIndex.value,
    summary: stepSummary(step, { streaming: Boolean(props.item.streaming), last: i === lastIndex.value }),
    failed: stepFailed(step),
    toolMs: stepToolMs(step),
  })),
)

function toggleStep(step) {
  // Mark it as the reader's choice, so the fold that follows the answer never
  // overrides what they asked for.
  step.touched = true
  step.open = !step.open
}

/**
 * The numbers the folded process reports: 工具调用次数 and 消息条数.
 *
 * 消息 counts the turn's steps — one iteration of the ReAct loop is one assistant
 * message exchanged with the model — so the header describes exactly what is
 * behind the fold rather than a second set of numbers to reconcile. The tool
 * count uses the same filtered list the steps render (ask_user excluded: it is
 * the card above, and counting a call the reader cannot see as a row would make
 * the two disagree).
 *
 * The duration is the sum of the per-step tool times, and it is absent when no
 * call reported one — a provider that says nothing is not zero.
 */
const processStats = computed(() => {
  let toolCalls = 0
  let toolMs = 0
  let sawMs = false
  let failed = false
  for (const view of stepViews.value) {
    toolCalls += view.tools.length
    if (view.failed) failed = true
    if (view.toolMs !== null) {
      toolMs += view.toolMs
      sawMs = true
    }
  }
  return {
    toolCalls,
    messages: steps.value.length,
    failed,
    toolMs: sawMs ? toolMs : null,
  }
})

/**
 * Whether the whole process is folded, and who decided.
 *
 * `pinned` is the reader's click (null = they have not clicked); the rule itself
 * — open while the turn runs, folded once it is over — lives in steps.js, where
 * it can be asserted without a browser.
 */
const processPinned = ref(null)
const processOpen = computed(() =>
  processShouldBeOpen({ streaming: Boolean(props.item.streaming), pinned: processPinned.value }),
)

function toggleProcess() {
  processPinned.value = !processOpen.value
}

/**
 * The hover text, which explains the numbers rather than repeating them: the line
 * itself cannot say that "消息" counts model interactions, and a reader who
 * wonders what 5 条消息 means has nowhere else to look.
 *
 * It follows the same rules as the line it explains — the tool count is left out
 * when there were no tool calls — so the two can never disagree.
 */
const processTitle = computed(() => {
  const stats = processStats.value
  const parts = [`本轮 ${stats.messages} 次模型交互（每次对应一条消息）`]
  parts.push(stats.toolCalls > 0 ? `${stats.toolCalls} 次工具调用` : '未调用工具')
  if (stats.toolMs !== null) {
    parts.push(`工具合计 ${formatDuration(stats.toolMs)}`)
  }
  parts.push(processOpen.value ? '点击折叠' : '点击展开全过程（含思考）')
  return parts.join(' · ')
})

/**
 * Follow the turn with the process: open while it works, folded once it answers.
 *
 * Folding is what keeps a twenty-step turn from pushing its own answer off the
 * screen, and it is acceptable only because a step the reader has clicked is left
 * exactly as they left it. See stepsShouldBeOpen for the rule itself.
 *
 * It runs on mount as well: a turn that is still streaming when the reader
 * arrives (a reload, another tab, a conversation switch) is opened here, so
 * "attach to a running turn" and "watch it from the start" look the same.
 */
watch(
  () => [Boolean(props.item.text), Boolean(props.item.streaming)],
  () => {
    foldSteps(steps.value, {
      answered: Boolean(props.item.text),
      streaming: Boolean(props.item.streaming),
    })
  },
  { immediate: true },
)

/**
 * 步骤级重试的说明。
 *
 * 它必须显示出来：重试会把这一步已经流出来的文字清掉，如果界面上不留一行字，
 * 那次"文字忽然变短"看起来就是渲染 bug，而不是"刚才那次调用失败了、正在重来"。
 */
const retryNotices = computed(() =>
  (props.item.notices || []).filter((note) => note && note.kind === 'retry'),
)

/**
 * The loop guard's steering messages.
 *
 * They are shown because a steered turn is otherwise indistinguishable from a
 * slow one: the reader watches an hour of tool calls and no answer, with no way
 * to know the harness had already told the model it was going in circles.
 */
const steerNotices = computed(() =>
  (props.item.notices || []).filter((note) => note && note.kind === 'steer'),
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
  if (reason === 'loop') return '本轮因重复调用同一工具被提前结束 · 回复「继续」可以让它换个做法'
  if (reason === 'idle') return '本轮因长时间只查看没有推进被提前结束 · 回复「继续」并指明要改哪里'
  return '本轮已达到步数上限，回答可能不完整 · 回复「继续」可接着做'
})

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
/**
 * The lines a spawned agent's actions reduce to.
 *
 * A tool entry shows its name and what it returned, trimmed to one line each: the
 * point of showing them is that the card stops looking like a hang, not that the
 * reader audits the subagent's every call (its trace has that).
 */
function nestedLines(tool) {
  const list = Array.isArray(tool && tool.nested) ? tool.nested : []
  return list.map((entry) => {
    if (entry.kind === 'tool') {
      const outcome = entry.error ? `失败：${entry.error}` : entry.result ? firstLine(entry.result) : '运行中…'
      return {
        kind: 'tool',
        text: `${entry.name} → ${outcome}`,
        title: entry.result || entry.error || entry.name,
      }
    }
    const text = firstLine(entry.text)
    return {
      kind: entry.kind,
      text: text || (entry.kind === 'reasoning' ? '(思考中)' : '(输出中)'),
      title: entry.text,
    }
  })
}

function nestedKindLabel(kind) {
  if (kind === 'tool') return '工具'
  if (kind === 'reasoning') return '思考'
  return '输出'
}

function firstLine(text) {
  const value = typeof text === 'string' ? text : ''
  const cut = value.split('\n')[0] || ''
  return cut.length > 160 ? `${cut.slice(0, 160)}…` : cut
}

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

      <!-- 执行过程: everything the turn did before answering, behind one fold.
           Collapsed, it is a single line of numbers — 工具调用次数 / 消息条数 —
           and the answer below it is the whole of what the reader sees, which is
           what a finished reply should look like. Opened, it is the same step by
           step record as before: the model's thinking and what it said, then the
           tool cards it ran (those still fold on their own, per step).

           A running turn keeps it open: the process is what there is to watch
           while the answer has not been written. See processOpen.

           The answer itself is never inside this fold. -->
      <div v-if="stepViews.length" class="steps">
        <button
          type="button"
          class="steps-head"
          :class="{ 'steps-head-open': processOpen }"
          :aria-expanded="processOpen"
          :title="processTitle"
          @click="toggleProcess"
        >
          <Icon :name="processOpen ? 'chevron-down' : 'chevron-right'" :size="14" />
          <span class="steps-title">执行过程</span>
          <span v-if="processStats.toolCalls > 0" class="steps-count">
            {{ processStats.toolCalls }} 次工具调用
          </span>
          <span v-if="processStats.toolCalls > 0" class="dimmer" aria-hidden="true">·</span>
          <span class="steps-count">{{ processStats.messages }} 条消息</span>
          <span v-if="processStats.failed" class="tag bad">有失败</span>
          <span v-if="processStats.toolMs !== null" class="dimmer nowrap">
            {{ formatDuration(processStats.toolMs) }}
          </span>
        </button>

        <div v-if="processOpen" class="steps-body">
        <div v-for="view in stepViews" :key="view.step.index" class="step">
          <!-- The header labels the step and folds its tool cards. A step that
               ran no tools — the one that answered, usually — has nothing to
               fold, so it is a plain row rather than a button that does nothing
               when clicked. -->
          <component
            :is="view.tools.length ? 'button' : 'div'"
            class="step-head"
            :class="{ 'step-head-plain': !view.tools.length }"
            :type="view.tools.length ? 'button' : undefined"
            :aria-expanded="view.tools.length ? Boolean(view.step.open) : undefined"
            @click="view.tools.length ? toggleStep(view.step) : undefined"
          >
            <Icon
              v-if="view.tools.length"
              :name="view.step.open ? 'chevron-down' : 'chevron-right'"
              :size="14"
            />
            <span class="step-no">#{{ view.step.index }}</span>
            <span class="step-what">{{ view.summary }}</span>
            <span v-if="view.failed" class="tag bad">失败</span>
            <span v-if="view.toolMs !== null" class="dimmer nowrap">
              {{ formatDuration(view.toolMs) }}
            </span>
            <span v-if="item.streaming && view.last" class="spin" aria-hidden="true" />
          </component>

          <div class="step-body">
            <!-- 思考: this step's reasoning only, and always shown. It is the
                 model's own account of why the step did what it did, which is
                 what a fold used to hide: a folded step named its tools but
                 never said what they were for. Reasoning is model-written prose,
                 so it renders as Markdown. -->
            <MarkdownText
              v-if="view.step.reasoning"
              class="reason-body"
              :text="view.step.reasoning"
              :streaming="item.streaming"
            />
            <!-- What the step said before acting. It is text the model produced,
                 so it is rendered as Markdown and kept out of the answer. -->
            <MarkdownText
              v-if="view.step.text && !view.last"
              class="step-said"
              :text="view.step.text"
              :streaming="item.streaming"
            />

            <!-- The tools this step ran, with the results it saw. Only these
                 fold: they are the bulky half of a step. -->
            <div v-if="view.tools.length && view.step.open" class="tools">
              <div v-for="tool in view.tools" :key="tool.key" class="tool-card">
                <!-- One row: what was called, how it went, and what it was called
                     with. The arguments are visible without a click — that is what
                     a reader usually wants — and a long argument ellipsizes instead
                     of wrapping, so the row never grows. -->
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

                <!-- 子 agent 做了什么。它单独一行一行地列在卡片里，而不是混进这一轮的
                     工具列表：那些调用不属于这一轮，读者不该把委派出去的工作算成模型
                     自己做的步骤。没有嵌套内容时这一段完全不渲染。 -->
                <div v-if="nestedLines(tool).length" class="tool-nested">
                  <div class="tool-nested-head">
                    <Icon name="git-branch" :size="12" />
                    <span>子 agent 的 {{ nestedLines(tool).length }} 条动作</span>
                  </div>
                  <ol class="tool-nested-list">
                    <li
                      v-for="(line, i) in nestedLines(tool)"
                      :key="`${tool.key}-nested-${i}`"
                      class="tool-nested-line"
                      :class="`kind-${line.kind}`"
                    >
                      <span class="tool-nested-kind">{{ nestedKindLabel(line.kind) }}</span>
                      <span class="tool-nested-text mono" :title="line.title">{{ line.text }}</span>
                    </li>
                  </ol>
                </div>

                <div v-if="tool.open" class="tool-body">
                  <!-- Expanded, both payloads are shown straight away: the chevron
                       is the only affordance, and asking for a second click to see
                       the arguments it just promised would be worse than the
                       preview. -->
                  <JsonBlock :value="tool.args" label="参数" open />
                  <JsonBlock :value="tool.result" :label="tool.error ? '结果（失败）' : '结果'" open />
                </div>
              </div>
            </div>
          </div>
        </div>
        </div>
      </div>

      <!-- The questions the model put to the user and the answers it got. They
           sit between the process and the answer because a question is what the
           turn is waiting on: everything above already happened, nothing below
           has been written yet. -->
      <div v-if="askCards.length" class="asks">
        <AskUserCard
          v-for="card in askCards"
          :key="card.id"
          :ask="card"
          @submit="emit('submitAsk', card.id, $event)"
        />
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
        <!-- 重试是"把原话再发一遍"，继续执行是"接着上一轮的中断处跑"：一次中断
             之后，后者常常是更省的做法——已经做完的步骤不会重做。 -->
        <button
          v-if="canResume"
          type="button"
          class="btn sm"
          title="接着上一轮的中断处继续跑：会带着计划和已完成的步骤，不会从零重来"
          @click="emit('resume')"
        >
          继续执行
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

      <ul v-if="steerNotices.length" class="turn-notices">
        <li
          v-for="(note, i) in steerNotices"
          :key="'steer-' + i"
          class="note-steer"
          :title="note.detail || ''"
        >
          <Icon name="refresh" :size="13" />
          <span>{{ note.text }}</span>
        </li>
      </ul>

      <ul v-if="retryNotices.length" class="turn-notices">
        <li v-for="(note, i) in retryNotices" :key="i" class="note-retry">
          <Icon name="refresh" :size="13" />
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
