<script setup>
// 对话 — the primary surface: a streaming conversation that owns the full
// height of the main area. The session list, the brand and the navigation all
// live in the app sidebar (AppSidebar.vue), so the chat pane has nothing above
// it but a thin context header.
//
// All state and the SSE turn machinery live in chatStore.js, so a running
// stream survives leaving and re-entering this view.
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import ChatComposer from './ChatComposer.vue'
import ChatMessage from './ChatMessage.vue'
import DrawerButton from './DrawerButton.vue'
import Icon from './Icon.vue'
import {
  chat,
  chatTools,
  clearSession,
  createSession,
  dismissActionError,
  ensureLoaded,
  maxSteps,
  modelGroups,
  renameSession,
  selectSession,
  sendMessage,
  setModel,
  stopStreaming,
} from '../chatStore.js'
import { formatCount } from '../format.js'
import { setDrawer } from '../ui.js'

const editingTitle = ref(false)
const titleDraft = ref('')
const titleInput = ref(null)
const confirmClear = ref(false)
const scroller = ref(null)
const composer = ref(null)
/** Auto-scroll is suspended as soon as the user scrolls up. */
const pinned = ref(true)

const session = computed(() => chat.session)
const items = computed(() => chat.items)
const streaming = computed(() => chat.streaming)

// ------------------------------------------------------------ model selector --

/**
 * Grouped select options. The value is the option's own key rather than a
 * "provider/model" string, because neither part is guaranteed to be free of
 * the separator.
 */
const optionGroups = computed(() => {
  const groups = modelGroups.value.map((group) => ({ provider: group.provider, options: [] }))
  const byProvider = new Map(groups.map((group) => [group.provider, group]))

  const flat = []
  for (const group of modelGroups.value) {
    const bucket = byProvider.get(group.provider)
    for (const entry of group.models) {
      const label =
        `${entry.provider || '—'} / ${entry.model || '—'}` +
        (entry.isDefault ? '（默认）' : '') +
        (entry.hasKey ? '' : '（未配置 API Key）')
      const option = {
        key: `catalog-${flat.length}`,
        provider: entry.provider,
        model: entry.model,
        label,
        disabled: !entry.hasKey,
      }
      bucket.options.push(option)
      flat.push(option)
    }
  }

  const current = session.value
  if (current && !flat.some((o) => o.provider === current.provider && o.model === current.model)) {
    // Never leave the select blank: the session may point at a model that is no
    // longer offered (or the catalog may be empty).
    const option = {
      key: 'current',
      provider: current.provider || '',
      model: current.model || '',
      label: `${current.provider || '—'} / ${current.model || '—'}（不在目录中）`,
      disabled: false,
    }
    groups.unshift({ provider: '当前会话', options: [option] })
    flat.unshift(option)
  }
  return groups
})

const modelOptions = computed(() => optionGroups.value.flatMap((group) => group.options))

const selectedModelKey = computed(() => {
  const current = session.value
  if (!current) return ''
  const found = modelOptions.value.find(
    (o) => o.provider === current.provider && o.model === current.model,
  )
  return found ? found.key : ''
})

/** The catalog entry for the active session's model, when it is known. */
const currentCatalogEntry = computed(() => {
  const current = session.value
  if (!current) return null
  return (
    modelGroups.value
      .flatMap((group) => group.models)
      .find((entry) => entry.provider === current.provider && entry.model === current.model) || null
  )
})

function onModelChange(event) {
  const option = modelOptions.value.find((o) => o.key === event.target.value)
  if (!option) return
  if (option.provider === session.value?.provider && option.model === session.value?.model) return
  setModel(option.provider, option.model)
}

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

async function onClear(confirmed = true) {
  if (!session.value) return
  if (confirmed && !confirmClear.value) {
    // Two-step: the first click only asks.
    confirmClear.value = true
    return
  }
  confirmClear.value = false
  await clearSession(session.value.id)
}

// -------------------------------------------------------------- scrolling --

function onScroll() {
  const el = scroller.value
  if (!el) return
  pinned.value = el.scrollHeight - el.scrollTop - el.clientHeight < 64
}

function scrollToBottom(smooth = false) {
  const el = scroller.value
  if (!el) return
  el.scrollTo({ top: el.scrollHeight, behavior: smooth ? 'smooth' : 'auto' })
  pinned.value = true
}

let lastSessionId = chat.activeId
watch(
  [() => chat.activeId, () => chat.items.length, () => chat.streamTick],
  () => {
    if (lastSessionId !== chat.activeId) {
      lastSessionId = chat.activeId
      pinned.value = true // a freshly opened conversation starts at the end
    }
    if (pinned.value) nextTick(() => scrollToBottom())
  },
  { flush: 'post' },
)

// ---------------------------------------------------------------- actions --

async function onCreate() {
  confirmClear.value = false
  const created = await createSession()
  if (created) nextTick(() => composer.value?.focus())
}

function onSend(text) {
  confirmClear.value = false
  sendMessage(text)
}

function reloadMessages() {
  if (chat.activeId) selectSession(chat.activeId)
}

/** Text of the closest preceding user message, for the failed-turn retry. */
function previousUserText(index) {
  for (let i = index - 1; i >= 0; i -= 1) {
    const item = chat.items[i]
    if (item && item.role === 'user' && item.text) return item.text
  }
  return ''
}

const toolNames = computed(() => chatTools.value.filter((name) => typeof name === 'string' && name))

onMounted(() => {
  ensureLoaded()
})
</script>

<template>
  <div class="chat">
    <div v-if="chat.actionError" class="banner error" role="alert">
      <Icon name="circle-alert" :size="16" />
      <span class="banner-text">{{ chat.actionError }}</span>
      <button
        v-if="chat.pendingContent"
        type="button"
        class="btn sm"
        @click="sendMessage(chat.pendingContent)"
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
      <!-- context header: what the conversation is, and the tools to steer it -->
      <header class="chat-head">
        <button
          type="button"
          class="btn ghost sm icon-btn drawer-btn"
          title="显示导航与会话列表"
          aria-label="显示导航与会话列表"
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

        <select
          class="input chat-model"
          :value="selectedModelKey"
          aria-label="选择模型"
          :disabled="streaming"
          @change="onModelChange"
        >
          <optgroup v-for="group in optionGroups" :key="group.provider" :label="group.provider">
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

        <span v-if="chat.notice" class="chip ok">{{ chat.notice }}</span>
        <span class="spacer" />

        <template v-if="!confirmClear">
          <button type="button" class="btn ghost sm" title="清空该对话的消息" @click="onClear(false)">
            <Icon name="eraser" :size="15" />
            清空
          </button>
        </template>
        <template v-else>
          <span class="dimmer nowrap">确认清空？</span>
          <button type="button" class="btn sm danger" @click="onClear(true)">清空</button>
          <button type="button" class="btn sm ghost" @click="confirmClear = false">取消</button>
        </template>
      </header>

      <div class="chat-chips">
        <span class="muted-note">可用工具</span>
        <template v-if="toolNames.length">
          <span v-for="name in toolNames" :key="name" class="chip mono">{{ name }}</span>
        </template>
        <span v-else class="chip warn">当前没有注册任何工具</span>
        <span v-if="maxSteps" class="chip" title="单轮最多工具调用步数">最多 {{ maxSteps }} 步</span>
        <span
          v-if="currentCatalogEntry && !currentCatalogEntry.hasKey"
          class="chip warn"
          title="该 provider 没有配置 API Key，发送会失败"
        >
          当前模型未配置 API Key
        </span>
        <span class="spacer" />
        <span class="muted-note">共 {{ formatCount(session.message_count) }} 条消息</span>
      </div>

      <!-- messages take every remaining pixel; the composer is pinned below -->
      <div ref="scroller" class="chat-scroll" @scroll="onScroll">
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
              @retry="onSend"
            />
          </div>
        </AsyncBlock>

        <button
          v-if="!pinned"
          type="button"
          class="btn sm chat-jump"
          title="滚动到最新内容"
          @click="scrollToBottom(true)"
        >
          <Icon name="chevron-down" :size="14" />
          回到最新
        </button>
      </div>

      <ChatComposer
        ref="composer"
        :streaming="streaming"
        :pending="chat.pendingContent"
        @send="onSend"
        @stop="stopStreaming"
      />
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

      <ChatComposer disabled :streaming="false" :pending="''" @send="onSend" @stop="stopStreaming" />
    </template>
  </div>
</template>
