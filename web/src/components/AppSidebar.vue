<script setup>
// The app sidebar — exactly three regions, in this order:
//
//   1. 新建对话 (one button, nothing above it: no brand, no provider line, no
//      theme control, and no account/logout row at the bottom);
//   2. the session list, which takes all the remaining height and scrolls;
//   3. the two pinned menu items 设置 and 统计监控.
//
// Session rows carry 重命名 / 清空 / 删除 actions that only appear while the row
// is hovered or holds keyboard focus (see .session-actions in styles.css). They
// are siblings of the row's pick button, so an action click never also selects
// the session. Below 900px there is no hover, so the actions stay visible.
//
// 删除 / 清空 stay two-step — nothing destructive happens on one click.
import { computed, nextTick, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import {
  chat,
  clearSession,
  createSession,
  deleteSession,
  loadSessions,
  renameSession,
  selectSession,
} from '../chatStore.js'
import { formatAbsolute, formatCount, formatRelative } from '../format.js'
import { NAV, setTab, state } from '../state.js'

const emit = defineEmits(['select'])

const editingId = ref('')
const draft = ref('')
/** Function ref: a plain `ref=` inside v-for would collect an array. */
let draftInput = null
function setDraftInput(el) {
  draftInput = el
}
/** Pending destructive action: {id, kind:'delete'|'clear'}. */
const confirming = ref({ id: '', kind: '' })

const sessions = computed(() => chat.sessions)
const activeId = computed(() => chat.activeId)

function pick(session) {
  emit('select')
  if (session.id !== chat.activeId) selectSession(session.id)
  if (state.tab !== 'chat') state.tab = 'chat'
}

async function onCreate() {
  emit('select')
  if (state.tab !== 'chat') state.tab = 'chat'
  cancelConfirm()
  await createSession()
}

function onNav(key) {
  emit('select')
  setTab(key)
}

function startRename(session) {
  confirming.value = { id: '', kind: '' }
  editingId.value = session.id
  draft.value = session.title || ''
  nextTick(() => {
    if (draftInput) {
      draftInput.focus()
      draftInput.select()
    }
  })
}

function cancelRename() {
  editingId.value = ''
  draft.value = ''
}

async function commitRename(session) {
  if (editingId.value !== session.id) return
  const title = draft.value.trim()
  editingId.value = ''
  if (!title || title === (session.title || '')) return
  await renameSession(session.id, title)
}

function ask(session, kind) {
  cancelRename()
  confirming.value = { id: session.id, kind }
}

function cancelConfirm() {
  confirming.value = { id: '', kind: '' }
}

async function doConfirm(session) {
  const kind = confirming.value.kind
  confirming.value = { id: '', kind: '' }
  if (kind === 'delete') await deleteSession(session.id)
  else await clearSession(session.id)
}
</script>

<template>
  <div class="side-panel">
    <!-- 1. the only thing above the list -->
    <div class="side-new">
      <button
        type="button"
        class="btn primary block"
        :disabled="chat.creating"
        title="新建一个对话"
        @click="onCreate"
      >
        <Icon name="plus" :size="16" />
        {{ chat.creating ? '创建中…' : '新建对话' }}
      </button>
    </div>

    <!-- 2. the session list: takes every remaining pixel and scrolls -->
    <div class="side-scroll">
      <div class="side-label">会话列表</div>

      <AsyncBlock
        :state="chat.sessionsStatus"
        :error="chat.sessionsError"
        :skeleton-rows="5"
        empty-text="暂无数据"
        @retry="loadSessions()"
      >
        <div v-if="!sessions.length" class="empty side-empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">还没有任何对话</div>
          <div class="empty-hint">点击「新建对话」开始，消息会保存在服务端</div>
        </div>

        <ul v-else class="session-list">
          <li
            v-for="session in sessions"
            :key="session.id"
            class="session-item"
            :class="{ active: session.id === activeId && state.tab === 'chat' }"
          >
            <div v-if="editingId === session.id" class="session-edit">
              <input
                :ref="setDraftInput"
                v-model="draft"
                class="input mono"
                type="text"
                maxlength="80"
                aria-label="会话标题"
                @keyup.enter="commitRename(session)"
                @keyup.esc="cancelRename"
              />
              <div class="row session-edit-actions">
                <button type="button" class="btn sm primary" @click="commitRename(session)">
                  保存
                </button>
                <button type="button" class="btn sm ghost" @click="cancelRename">取消</button>
              </div>
            </div>

            <template v-else>
              <button
                type="button"
                class="session-pick"
                :aria-current="session.id === activeId ? 'true' : undefined"
                @click="pick(session)"
              >
                <span class="session-title">{{ session.title || '新对话' }}</span>
                <span class="session-meta">
                  <span :title="formatAbsolute(session.updated_at)">
                    {{ formatRelative(session.updated_at) }}
                  </span>
                  <span class="dimmer">· {{ formatCount(session.message_count) }} 条</span>
                </span>
              </button>

              <div v-if="confirming.id === session.id" class="session-confirm">
                <span class="dimmer nowrap">
                  {{ confirming.kind === 'delete' ? '确认删除？' : '确认清空？' }}
                </span>
                <button type="button" class="btn sm danger" @click="doConfirm(session)">
                  {{ confirming.kind === 'delete' ? '删除' : '清空' }}
                </button>
                <button type="button" class="btn sm ghost" @click="cancelConfirm">取消</button>
              </div>

              <div v-else class="session-actions">
                <button
                  type="button"
                  class="session-btn"
                  title="重命名"
                  aria-label="重命名该对话"
                  @click="startRename(session)"
                >
                  <Icon name="pencil" :size="14" />
                </button>
                <button
                  type="button"
                  class="session-btn"
                  title="清空该对话的消息"
                  aria-label="清空该对话的消息"
                  @click="ask(session, 'clear')"
                >
                  <Icon name="eraser" :size="14" />
                </button>
                <button
                  type="button"
                  class="session-btn danger-text"
                  title="删除该对话"
                  aria-label="删除该对话"
                  @click="ask(session, 'delete')"
                >
                  <Icon name="trash" :size="14" />
                </button>
              </div>
            </template>
          </li>
        </ul>
      </AsyncBlock>
    </div>

    <!-- 3. the two pinned menu items — nothing after them -->
    <nav class="side-nav" aria-label="控制台">
      <button
        v-for="item in NAV"
        :key="item.key"
        type="button"
        class="nav-item"
        :class="{ active: state.tab === item.key }"
        :aria-current="state.tab === item.key ? 'page' : undefined"
        @click="onNav(item.key)"
      >
        <Icon :name="item.icon" :size="16" />
        <span class="nav-text">{{ item.label }}</span>
      </button>
    </nav>
  </div>
</template>
