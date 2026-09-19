<script setup>
// 对话 — the primary surface: a streaming conversation that owns the full
// height of the main area. The session list lives in the app sidebar
// (AppSidebar.vue), so the chat pane has nothing above it but a thin context
// header: the session title (click to rename), 清空, the conversation's totals
// (turns / model calls / tool calls / tokens, from the server's aggregate) and
// the message count.
// The model picker is inside the composer, and the available tools are not
// listed anywhere in the chat.
//
// The header also carries this conversation's background processes: a count that
// opens JobsDrawer beside the conversation. Those processes are state this
// conversation produced — a dev server one turn started and the turns after it
// watch — so the count belongs here rather than in 设置, which is for things the
// operator turns rather than things the agent did.
//
// All state and the SSE turn machinery live in chatStore.js, so a running
// stream survives leaving and re-entering this view.
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import ArtifactsDrawer from './ArtifactsDrawer.vue'
import ChatComposer from './ChatComposer.vue'
import ChatMessage from './ChatMessage.vue'
import DrawerButton from './DrawerButton.vue'
import Icon from './Icon.vue'
import JobsDrawer from './JobsDrawer.vue'
import SubagentsDrawer from './SubagentsDrawer.vue'
import TaskBoard from './TaskBoard.vue'
import ApprovalCard from './ApprovalCard.vue'
import {
  chat,
  clearSession,
  createSession,
  currentCatalogEntry,
  dismissActionError,
  ensureLoaded,
  renameSession,
  resumeTurn,
  selectSession,
  sendMessage,
  stopStreaming,
  submitApproval,
  submitAsk,
} from '../chatStore.js'
import { artifactChipLabel, artifactHintText } from '../artifactsChip.js'
import {
  artifactsCount,
  artifactsEnabled,
  loadArtifactsFor,
  noteArtifactToolRan,
} from '../artifactsStore.js'
import { pendingApprovals } from '../approval.js'
import { planResumable } from '../plan.js'
import { subagentChipLabel, subagentHintText } from '../subagentsChip.js'
import {
  loadSubagentsFor,
  refreshSubagents,
  subagentsMaxConcurrent,
  subagentsRunning,
  subagentsTotal,
} from '../subagentsStore.js'
import { statsSegments, statsTitle } from '../sessionStats.js'
import {
  loadForSession,
  otherRunning,
  sessionJobs,
  sessionRunning,
  stopJobPolling,
} from '../jobsStore.js'
import { openTrace } from '../state.js'
import { setDrawer } from '../ui.js'

const editingTitle = ref(false)
const titleDraft = ref('')
const titleInput = ref(null)
const confirmClear = ref(false)
const scroller = ref(null)
const composer = ref(null)
/** Auto-scroll is suspended as soon as the *user* scrolls up (see below). */
const pinned = ref(true)
/** Whether the background-process drawer is open beside the conversation. */
const jobsOpen = ref(false)
/** Whether the 产物 drawer is open beside the conversation. */
const artifactsOpen = ref(false)

const session = computed(() => chat.session)
const items = computed(() => chat.items)

/**
 * The header's statistics line: 轮数 · 消息条数 · 模型调用 · 工具调用 · token.
 *
 * The values are the server's aggregate (chat.stats), which the authoritative
 * reload at the end of every turn refreshes — so they are already current by the
 * time a turn's output stops moving, and they are the same numbers after a
 * reload. A conversation that has not run anything yet renders the turn and
 * message counts alone rather than a row of zeros.
 *
 * The message count comes from the session row rather than the aggregate: it is
 * the number the sidebar counts, and it is there even for a conversation the
 * aggregate has nothing to say about.
 */
const messageCount = computed(() => (session.value ? session.value.message_count : undefined))
const statSegments = computed(() => statsSegments(chat.stats, { messageCount: messageCount.value }))
const statsDetail = computed(() => statsTitle(chat.stats, { messageCount: messageCount.value }))

/**
 * The background-process chip in the header.
 *
 * It appears only when it has something to say — this conversation has started a
 * background process at some point, or another conversation in the same
 * workspace has one running — because a permanent "0" is noise about a feature
 * nobody used. The tone follows liveness: something running is worth noticing,
 * leftover records are not.
 */
const jobsCount = computed(() => sessionJobs.value.length)
const jobsRunning = computed(() => sessionRunning.value)
const jobsVisible = computed(() => jobsCount.value > 0 || otherRunning.value > 0 || jobsOpen.value)

/**
 * What this conversation delegated.
 *
 * Same reasoning as the jobs chip next to it: it renders only once there is
 * something to count, and the label leads with liveness — "2 个子 agent 运行中" is
 * the fact worth putting in a header, and a finished record is worth keeping but
 * not worth shouting about.
 */
const subagentsOpen = ref(false)
const subagentsVisible = computed(() => subagentsTotal.value > 0 || subagentsOpen.value)
const subagentsTone = computed(() => (subagentsRunning.value > 0 ? 'ok' : ''))
// The wording lives in subagentsChip.js so it can be tested without rendering the
// view — a chip that reports a stale count under the wrong conversation's title is
// the failure this seam exists to catch.
const subagentsLabel = computed(() => subagentChipLabel())
const subagentsHint = computed(() => subagentHintText())
const jobsTone = computed(() => (jobsRunning.value > 0 ? 'ok' : ''))
const jobsLabel = computed(() => {
  if (jobsRunning.value > 0) return `${jobsRunning.value} 个后台进程`
  if (jobsCount.value > 0) return `${jobsCount.value} 条进程记录`
  // Nothing of this conversation's own, but a neighbour in the same workspace is
  // running something: say so rather than showing a bare zero.
  return `同工作区 ${otherRunning.value} 个后台进程`
})
const jobsHint = computed(() => {
  const parts = [`本会话：运行中 ${jobsRunning.value}，记录 ${jobsCount.value}`]
  if (otherRunning.value > 0) parts.push(`同一工作区还有 ${otherRunning.value} 个在运行`)
  parts.push('点击查看日志与停止')
  return parts.join(' · ')
})

/**
 * 产物 — what this conversation produced.
 *
 * It sits beside the subagent chip because it answers the same shape of question
 * about the same conversation, with one difference that decides its rendering
 * rule: an artifact is finished the moment it exists, so there is no liveness to
 * lead with and no reason for a permanent zero. It appears once the conversation
 * has one (or once the reader has opened it), exactly like the two chips next to
 * it.
 */
const artifactsVisible = computed(
  () => (artifactsEnabled.value && artifactsCount.value > 0) || artifactsOpen.value,
)
const artifactsLabel = computed(() => artifactChipLabel())
const artifactsHint = computed(() => artifactHintText())

// The chip and the drawer read the same store, and the store reads whichever
// conversation is selected: switching sessions must never leave the previous
// conversation's count under the new one's title.
watch(
  () => chat.activeId,
  (id) => {
    if (!id) return
    loadForSession(id)
    // The same rule for the subagent list: it belongs to the conversation on
    // screen, and a run left over from the previous one would be attributed to the
    // wrong reader.
    loadSubagentsFor(id)
    // And again for 产物: the button counts this conversation's artifacts, so a
    // count left over from the previous conversation would be a lie about this one.
    loadArtifactsFor(id)
  },
  { immediate: true },
)

// A turn that delegates must update the chip while it runs, and the only signal the
// client gets is the tool call itself. Watching the streaming turn's tool runs is
// what turns "spawn_agent appeared" into "the chip is now live" — the same trigger
// the background-process chip uses, for the same reason.
watch(
  () => chat.streamTick,
  () => {
    if (!chat.activeId) return
    const turn = chat.items[chat.items.length - 1]
    const tools = turn && Array.isArray(turn.tools) ? turn.tools : []
    // A run is worth a reload when a spawn appeared or one just finished; polling
    // then takes over while anything is live.
    //
    // save_artifact is handled in the same pass rather than in a watcher of its
    // own: the trigger is the same event, and the store has no poller to fall back
    // on, so this is the only signal that an artifact appeared mid-turn.
    for (const tool of tools) {
      if (tool.name === 'spawn_agent') {
        refreshSubagents()
        continue
      }
      noteArtifactToolRan(tool.name)
    }
  },
)

// The store's interval keeps running while any job is alive; leaving the view is
// the moment to stop asking a server nobody is looking at.
onBeforeUnmount(stopJobPolling)

const streaming = computed(() => chat.streaming)

/**
 * The write and exec requests the running turn is blocked on.
 *
 * They are rendered above the composer rather than inside the message flow: the
 * turn is *waiting* on them, so they belong where the reader is already looking,
 * next to the 停止 button. A decision that had to be scrolled to is a decision
 * that delays the turn.
 */
const approvals = computed(() => pendingApprovals(chat.items[chat.items.length - 1]))

function onApprovalDecide(card, decision) {
  submitApproval(card.id, decision)
}

/** Only surfaced when the session's model cannot be called at all. */
const missingKey = computed(
  () => Boolean(currentCatalogEntry.value) && !currentCatalogEntry.value.hasKey,
)

// ------------------------------------------------------------- title editing --

function startTitleEdit() {
  if (!session.value) return
  titleDraft.value = session.value.title || ''
  editingTitle.value = true
  nextTick(() => {
    if (titleInput.value) {
      titleInput.value.focus()
      titleInput.value.select()
    }
  })
}

async function commitTitle() {
  if (!editingTitle.value) return
  editingTitle.value = false
  const title = titleDraft.value.trim()
  if (!title || !session.value || title === (session.value.title || '')) return
  await renameSession(session.value.id, title)
}

function cancelTitle() {
  editingTitle.value = false
  titleDraft.value = ''
}

/**
 * 清空 is two-step, and the two steps are separate functions so neither can be
 * reached by accident: `armClear` asks, `runClear` performs.
 */
function armClear() {
  if (!session.value) return
  confirmClear.value = true
}

async function runClear() {
  confirmClear.value = false
  if (!session.value) return
  await clearSession(session.value.id)
}

// -------------------------------------------------------------- scrolling --
//
// `pinned` means "keep the newest message in view". The template reads it twice
// — it gates the streaming auto-scroll and shows the 回到最新 affordance while
// it is false — and only the reader is allowed to clear it.
//
// A programmatic scroll fires the very same `scroll` event a reader's gesture
// does, so `selfScrollTop` remembers the offset this component scrolled to and
// swallows exactly that one event; everything else is the reader. Without that,
// a jump-to-bottom during a session switch (or during streaming) reads as "the
// reader scrolled away" and silently disables all further following.
//
// Opening a conversation is also not a single `scrollTo`: Markdown, tables,
// code blocks and webfonts change the scroll height *after* the messages are
// already in the DOM. `startSettling()` therefore keeps re-anchoring the bottom
// for a bounded window and stops the moment the reader scrolls away.
const NEAR_BOTTOM_PX = 64
/** How long a freshly opened conversation keeps following the bottom. */
const SETTLE_MS = 1200
/** Unchanged content height for this many frames ends the settle early. */
const SETTLE_STABLE_FRAMES = 4
/** 回到最新 animates; its own scroll events are ignored for this long. */
const SMOOTH_MS = 900

/** Offset of our own last programmatic scroll — never the reader's. */
let selfScrollTop = -1
/** While set, scroll events belong to the 回到最新 animation. */
let smoothUntil = 0
let settleRaf = 0
let settleTimer = 0
let settleUntil = 0
let settleHeight = -1
let settleStable = 0

/** Largest meaningful scrollTop for this element. */
function bottomOffset(el) {
  return Math.max(0, el.scrollHeight - el.clientHeight)
}

function nearBottom(el) {
  return el.scrollHeight - el.scrollTop - el.clientHeight < NEAR_BOTTOM_PX
}

/**
 * Land on the newest message right now.
 *
 * Never smooth: opening a conversation (or following a stream) must appear at
 * the end, not animate a long scroll down from the top. Smooth belongs to the
 * explicit 回到最新 button only — see `scrollToBottom`.
 */
function stickToBottom() {
  const el = scroller.value
  if (!el) return
  const target = bottomOffset(el)
  if (Math.abs(el.scrollTop - target) > 1) {
    el.scrollTo({ top: target, behavior: 'auto' })
    selfScrollTop = el.scrollTop
  }
  pinned.value = true
}

function onScroll() {
  const el = scroller.value
  if (!el) return
  const top = el.scrollTop
  // Consume the one event our own jump produces; anything else is the reader.
  const own = top === selfScrollTop
  selfScrollTop = -1
  if (own) return
  if (performance.now() < smoothUntil) {
    // Inside the 回到最新 animation: it is still ours until it arrives.
    if (bottomOffset(el) - top <= 1) smoothUntil = 0
    return
  }
  pinned.value = nearBottom(el)
  // Scrolling away from the end is the reader taking over: stop following.
  if (!pinned.value) stopSettling()
}

/** A real gesture outranks a 回到最新 animation that is still running. */
function onUserScroll() {
  smoothUntil = 0
}

/** Re-anchor the bottom while the content is still changing height. */
function settleFrame() {
  settleRaf = 0
  if (!settleUntil || performance.now() > settleUntil) return
  const el = scroller.value
  if (el) {
    const height = el.scrollHeight
    if (height !== settleHeight) {
      settleHeight = height
      settleStable = 0
      stickToBottom()
    } else {
      settleStable += 1
    }
    if (settleStable >= SETTLE_STABLE_FRAMES) return
  }
  settleRaf = requestAnimationFrame(settleFrame)
}

function stopSettling() {
  settleUntil = 0
  if (settleRaf) {
    cancelAnimationFrame(settleRaf)
    settleRaf = 0
  }
  if (settleTimer) {
    window.clearTimeout(settleTimer)
    settleTimer = 0
  }
}

/** Open a conversation at its newest message and stay there while it settles. */
function startSettling() {
  stopSettling()
  settleUntil = performance.now() + SETTLE_MS
  settleHeight = -1
  settleStable = 0
  stickToBottom()
  settleRaf = requestAnimationFrame(settleFrame)
  // requestAnimationFrame does not run in a hidden tab, so one bounded timeout
  // guarantees a final anchor even if the settle loop never got a frame.
  settleTimer = window.setTimeout(() => {
    settleTimer = 0
    if (pinned.value) stickToBottom()
    stopSettling()
  }, SETTLE_MS)
}

/** 回到最新 — the only smooth scroll in the pane. */
function scrollToBottom() {
  const el = scroller.value
  if (!el) return
  const target = bottomOffset(el)
  pinned.value = true
  smoothUntil = Math.abs(el.scrollTop - target) > 1 ? performance.now() + SMOOTH_MS : 0
  if (!smoothUntil) return
  el.scrollTo({ top: target, behavior: 'smooth' })
}

let lastSessionId = chat.activeId
watch(
  [() => chat.activeId, () => chat.items.length, () => chat.streamTick],
  () => {
    if (lastSessionId !== chat.activeId) {
      lastSessionId = chat.activeId
      pinned.value = true // a freshly opened conversation starts at the end
      nextTick(startSettling)
      return
    }
    if (pinned.value) nextTick(stickToBottom)
  },
  { flush: 'post' },
)

// ---------------------------------------------------------------- actions --

async function onCreate() {
  confirmClear.value = false
  const created = await createSession()
  if (created) nextTick(() => composer.value?.focus())
}

function onSend(text, attachments) {
  confirmClear.value = false
  sendMessage(text, { attachments: attachments || [] })
}

/**
 * 接着上一轮的中断处继续跑。
 *
 * 它**不经过输入框**：这不是"再发一句话"，而是让服务端带着上一轮的计划与已完成
 * 的步骤新开一轮（气泡由 store 自己补上）。所以看板和失败气泡上的按钮共用这一个
 * 入口。
 */
function onResume() {
  confirmClear.value = false
  resumeTurn()
}

/**
 * 最新一条助手回答的下标，没有则 -1。
 *
 * "继续执行"只出现在它上面：中断的接续点只能是**最后**一轮（更早的失败早已被后
 * 面的回合带过），在同一段对话里给出两个接续点只会让人不知道哪个才算数。
 */
const lastAssistantIndex = computed(() => {
  for (let i = chat.items.length - 1; i >= 0; i -= 1) {
    const item = chat.items[i]
    if (item && item.role === 'assistant') return i
  }
  return -1
})

/**
 * 这一条助手气泡要不要给「继续执行」。
 *
 * 三个条件缺一不可：它是最后一条回答、计划里还有没做完的事、当前没有轮次在跑
 * （有一轮在跑时服务端会拒 409，而按钮摆在"停止"旁边会显得能插队）。
 */
function canResumeAt(index) {
  return index === lastAssistantIndex.value && planResumable(chat.plan) && !chat.streaming
}

function reloadMessages() {
  if (chat.activeId) selectSession(chat.activeId)
}

/**
 * Open the trace behind one answer. The panel consumes the request once and
 * clears it, so a later ordinary visit to 统计监控 is not dragged back here.
 */
function openTraceForTurn(traceId) {
  openTrace({ id: traceId })
}

/** Every trace this conversation recorded, i.e. the same panel with its own
 * session filter filled in. */
function openTracesForSession() {
  if (chat.activeId) openTrace({ session: chat.activeId })
}

/** Text of the closest preceding user message, for the failed-turn retry. */
function previousUserText(index) {
  for (let i = index - 1; i >= 0; i -= 1) {
    const item = chat.items[i]
    if (item && item.role === 'user' && item.text) return item.text
  }
  return ''
}

onMounted(() => {
  ensureLoaded()
  // The workspace list is not loaded here any more: the header no longer shows
  // which workspace this conversation is in, and the sidebar — which does show
  // it, as the folder the conversation is filed under — loads the list itself.
  //
  // The pane is remounted whenever the tab changes (App.vue keys the view by
  // tab), and that leaves a brand-new scroller at offset 0 with no store change
  // to react to — e.g. leaving 统计监控 by clicking the conversation you were
  // already in. Opening a conversation has to anchor from here too.
  if (chat.activeId) nextTick(startSettling)
})

onBeforeUnmount(stopSettling)
</script>

<template>
  <div class="chat">
    <div v-if="chat.actionError" class="banner error" role="alert">
      <Icon name="circle-alert" :size="16" />
      <span class="banner-text">{{ chat.actionError }}</span>
      <button
        v-if="chat.pendingContent || chat.pendingAttachments.length"
        type="button"
        class="btn sm"
        @click="sendMessage(chat.pendingContent, { attachments: chat.pendingAttachments })"
      >
        重试
      </button>
      <button type="button" class="btn sm ghost" @click="dismissActionError">关闭</button>
    </div>

    <!-- catalog problems must not leave the pane blank -->
    <div v-if="chat.catalogStatus === 'error'" class="banner error" role="alert">
      <Icon name="circle-alert" :size="16" />
      <span class="banner-text">无法读取模型目录：{{ chat.catalogError || '请稍后重试' }}</span>
      <button type="button" class="btn sm" @click="ensureLoaded()">重试</button>
    </div>

    <template v-if="session">
      <!-- thin context header: what the conversation is, and the tools to steer it -->
      <header class="chat-head">
        <button
          type="button"
          class="btn ghost sm icon-btn drawer-btn"
          title="显示会话列表"
          aria-label="显示会话列表"
          @click="setDrawer(true)"
        >
          <Icon name="panel-left" :size="16" />
        </button>

        <div class="chat-title-wrap">
          <input
            v-if="editingTitle"
            ref="titleInput"
            v-model="titleDraft"
            class="input chat-title-input"
            type="text"
            maxlength="80"
            aria-label="会话标题"
            @keyup.enter="commitTitle"
            @keyup.esc="cancelTitle"
            @blur="commitTitle"
          />
          <button
            v-else
            type="button"
            class="chat-title"
            title="点击重命名该对话"
            @click="startTitleEdit"
          >
            {{ session.title || '新对话' }}
          </button>
        </div>

        <span v-if="missingKey" class="chip warn" title="该 provider 没有配置 API Key，发送会失败">
          当前模型未配置 API Key
        </span>
        <span v-if="chat.notice" class="chip ok">{{ chat.notice }}</span>
        <span class="spacer" />

        <!-- The conversation's workspace used to be a picker right here. It is
             gone: which directory a conversation works in is already visible in
             the sidebar, where conversations are grouped under their workspace
             folder, and repeating it in the header only took room from what the
             header is actually for. A conversation is now placed in a workspace
             when it is created (the folder's + button) and stays there. -->
        <!-- The conversation's totals, from how much conversation there is
             (轮数 · 消息条数) to what it cost (模型 · 工具 · token) — the numbers a
             long conversation is actually judged by. They are one group rather
             than a group plus a trailing count, so the message count keeps its
             place beside the turn count and each segment carries its own
             explanation on hover, including the one caveat that matters —
             neither duration is the conversation's wall-clock length. -->
        <span class="muted-note nowrap chat-stats" :title="statsDetail">
          <template v-for="(segment, index) in statSegments" :key="segment.key">
            <span v-if="index > 0" class="dimmer" aria-hidden="true">·</span>
            <span>{{ segment.text }}</span>
          </template>
        </span>

        <!-- This conversation's background processes. The count is conversation
             state, so it lives on the conversation's own header rather than in
             设置, and clicking it opens the list, the logs and the stop button
             beside the conversation. It renders only once there is something to
             count: a permanent 0 is noise about a feature nobody used. -->
        <button
          v-if="jobsVisible"
          type="button"
          class="chip chip-btn"
          :class="jobsTone"
          :title="jobsHint"
          :aria-expanded="jobsOpen"
          @click="jobsOpen = !jobsOpen"
        >
          <Icon name="terminal" :size="13" />
          {{ jobsLabel }}
        </button>

        <!-- What this conversation delegated to subagents. It sits beside the
             background-process chip because it answers the same question — what is
             running under this conversation — and a reader who has learned one has
             learned the other. -->
        <button
          v-if="subagentsVisible"
          type="button"
          class="chip chip-btn"
          :class="subagentsTone"
          :title="subagentsHint"
          :aria-expanded="subagentsOpen"
          @click="subagentsOpen = !subagentsOpen"
        >
          <Icon name="git-branch" :size="13" />
          {{ subagentsLabel }}
        </button>

        <!-- What this conversation produced. It sits with the two chips above
             because it is the same kind of fact — state this conversation made
             — and it is the only way to reach an artifact's bytes: they live on
             the server, not in the workspace and not in the transcript. -->
        <button
          v-if="artifactsVisible"
          type="button"
          class="chip chip-btn"
          :title="artifactsHint"
          :aria-expanded="artifactsOpen"
          @click="artifactsOpen = !artifactsOpen"
        >
          <Icon name="package" :size="13" />
          {{ artifactsLabel }}
        </button>

        <!-- The session-level way in: every turn this conversation recorded,
             with the panel's session filter already applied. The per-answer 链路
             button is the precise route; this one is the discoverable one, and it
             is what you reach for when you know a turn misbehaved but not which. -->
        <button
          type="button"
          class="btn ghost sm"
          title="在链路追踪里查看该对话的全部 trace"
          @click="openTracesForSession"
        >
          <Icon name="git-branch" :size="15" />
          链路
        </button>

        <template v-if="!confirmClear">
          <button type="button" class="btn ghost sm" title="清空该对话的消息" @click="armClear()">
            <Icon name="eraser" :size="15" />
            清空
          </button>
        </template>
        <template v-else>
          <span class="dimmer nowrap">确认清空？</span>
          <button type="button" class="btn sm danger" @click="runClear()">清空</button>
          <button type="button" class="btn sm ghost" @click="confirmClear = false">取消</button>
        </template>
      </header>

      <!-- messages take every remaining pixel; the composer is pinned below -->
      <div
        ref="scroller"
        class="chat-scroll"
        @scroll="onScroll"
        @wheel="onUserScroll"
        @touchmove="onUserScroll"
      >
        <AsyncBlock
          :state="chat.messagesStatus"
          :error="chat.messagesError"
          :skeleton-rows="5"
          @retry="reloadMessages"
        >
          <div v-if="!items.length" class="empty">
            <div class="empty-ico" aria-hidden="true">◍</div>
            <div class="empty-text">这个对话还没有消息</div>
            <div class="empty-hint">在下方输入内容开始对话，模型会实时返回思考过程与工具调用</div>
          </div>

          <div v-else class="messages">
            <ChatMessage
              v-for="(item, index) in items"
              :key="item.key"
              :item="item"
              :retry-text="previousUserText(index)"
              :can-resume="canResumeAt(index)"
              @retry="onSend"
              @resume="onResume"
              @open-trace="openTraceForTurn"
              @submit-ask="submitAsk"
            />
          </div>
        </AsyncBlock>

        <button
          v-if="!pinned"
          type="button"
          class="btn sm chat-jump"
          title="滚动到最新内容"
          @click="scrollToBottom()"
        >
          <Icon name="chevron-down" :size="14" />
          回到最新
        </button>
      </div>

      <!-- 任务看板贴在输入框正上方：计划属于"这一轮还没做完的事"，不属于消息
           历史（放进消息流会被新消息推走）。没有计划时它自己渲染成空，所以这里
           不需要额外的 v-if。 -->
      <!-- 审批卡片贴在输入框上方：这一轮正卡在这个决定上，所以它属于读者正在看的
           位置（输入框上方），不属于消息历史。 -->
      <div v-if="approvals.length" class="approval-stack">
        <ApprovalCard
          v-for="card in approvals"
          :key="card.id"
          :card="card"
          @decide="(decision) => onApprovalDecide(card, decision)"
        />
      </div>

      <TaskBoard @resume="onResume" />

      <ChatComposer
        ref="composer"
        :streaming="streaming"
        :pending="chat.pendingContent"
        :pending-attachments="chat.pendingAttachments"
        @send="onSend"
        @stop="stopStreaming"
      />

      <!-- The processes this conversation started, beside it rather than inside
           the message flow: they outlive the turn that started them, so they
           cannot be a card in the transcript. -->
      <div v-if="jobsOpen" class="jobs-backdrop" @click="jobsOpen = false" />
      <JobsDrawer v-if="jobsOpen" @close="jobsOpen = false" />
      <SubagentsDrawer v-if="subagentsOpen" @close="subagentsOpen = false" />
      <!-- The resources this conversation produced. Beside it rather than inside
           the transcript for the same reason the two above are: a 32 MiB page has
           no place in a message, and the file outlives the turn that wrote it. -->
      <div v-if="artifactsOpen" class="jobs-backdrop" @click="artifactsOpen = false" />
      <ArtifactsDrawer v-if="artifactsOpen" @close="artifactsOpen = false" />
    </template>

    <!-- no conversation selected yet -->
    <template v-else>
      <div class="chat-scroll chat-scroll-blank">
        <AsyncBlock
          :state="chat.sessionsStatus"
          :error="chat.sessionsError"
          :skeleton-rows="4"
          @retry="ensureLoaded()"
        >
          <div class="empty chat-blank">
            <div class="empty-ico" aria-hidden="true">◍</div>
            <div class="empty-text">还没有对话</div>
            <div class="empty-hint">
              新建一个对话、选择模型后即可开始。消息由服务端保存，刷新后依然存在。
            </div>
            <button type="button" class="btn primary" :disabled="chat.creating" @click="onCreate">
              {{ chat.creating ? '创建中…' : '新建对话' }}
            </button>
            <div class="muted-note chat-blank-note">
              如果这里一直加载失败，请确认服务端已启用对话接口（配置项
              <code class="md-code">chat.enable</code>，修改后需要重启服务）。
            </div>
          </div>
        </AsyncBlock>
      </div>

      <ChatComposer
        disabled
        :streaming="false"
        :pending="''"
        :pending-attachments="[]"
        @send="onSend"
        @stop="stopStreaming"
      />
    </template>
  </div>
</template>
