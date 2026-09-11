<script setup>
// Left sidebar of the 对话 tab: the session list plus per-session actions.
//
// 删除 and 清空 are two-step on purpose — nothing destructive happens on a
// single click.
import { computed, nextTick, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
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
  if (session.id === chat.activeId) return
  selectSession(session.id)
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

const status = computed(() => chat.sessionsStatus)
const error = computed(() => chat.sessionsError)
</script>

<template>
  <div class="chat-side">
    <div class="chat-side-head">
      <div class="card-title">
        <span class="dot" />
        会话
        <span class="card-sub">{{ formatCount(sessions.length) }}</span>
      </div>
      <button
        type="button"
        class="btn primary sm"
        :disabled="chat.creating"
        title="新建一个对话"
        @click="createSession()"
      >
        {{ chat.creating ? '创建中…' : '新建对话' }}
      </button>
    </div>

    <div class="chat-side-body">
      <AsyncBlock
        :state="status"
        :error="error"
        :skeleton-rows="4"
        empty-text="暂无数据"
        @retry="loadSessions()"
      >
        <div v-if="!sessions.length" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">还没有任何对话</div>
          <div class="empty-hint">点击「新建对话」开始，消息会保存在服务端</div>
        </div>

        <ul v-else class="session-list">
          <li
            v-for="session in sessions"
            :key="session.id"
            class="session-item"
            :class="{ active: session.id === activeId }"
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
              <div class="row" style="margin-top: 6px">
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
                  <span class="dimmer">· {{ formatCount(session.message_count) }} 条消息</span>
                </span>
              </button>

              <div v-if="confirming.id === session.id" class="session-confirm">
                <span class="dimmer">
                  {{ confirming.kind === 'delete' ? '确认删除该对话？' : '确认清空全部消息？' }}
                </span>
                <button type="button" class="btn sm danger" @click="doConfirm(session)">
                  {{ confirming.kind === 'delete' ? '删除' : '清空' }}
                </button>
                <button type="button" class="btn sm ghost" @click="cancelConfirm">取消</button>
              </div>

              <div v-else class="session-actions">
                <button type="button" class="btn sm ghost" @click="startRename(session)">改名</button>
                <button type="button" class="btn sm ghost" @click="ask(session, 'clear')">清空</button>
                <button type="button" class="btn sm ghost danger-text" @click="ask(session, 'delete')">
                  删除
                </button>
              </div>
            </template>
          </li>
        </ul>
      </AsyncBlock>
    </div>

    <p class="muted-note chat-side-foot">会话与消息保存在服务端，刷新后仍然存在。</p>
  </div>
</template>
