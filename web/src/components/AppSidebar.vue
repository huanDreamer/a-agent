<script setup>
// The app sidebar: brand + theme, 新建对话 + session list, the MANAGEMENT
// navigation, and the account row.
//
// It replaces both the old global header (range selector / 刷新 / 退出 lived
// there) and the standalone chat session sidebar, so the chat pane owns the
// whole main area. Below 900px it becomes an overlay drawer (see styles.css):
// the parent toggles the `open` class from ui.drawerOpen.
//
// Session 删除 / 清空 stay two-step — nothing destructive happens on one click.
import { computed, nextTick, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import ThemeToggle from './ThemeToggle.vue'
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
import { TABS, logout, setTab, state } from '../state.js'

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
const meta = computed(() => state.meta || {})
const navItems = computed(() => TABS.filter((tab) => tab.section === 'management'))

function goChat() {
  emit('select')
  setTab('chat')
}

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

async function onLogout() {
  emit('select')
  await logout()
}
</script>

<template>
  <div class="side-panel">
    <!-- brand + theme -->
    <div class="side-top">
      <div class="brand">
        <div class="brand-mark" aria-hidden="true">H</div>
        <div class="brand-text">
          <div class="brand-name">huan-agent</div>
          <div class="brand-sub">控制台</div>
        </div>
      </div>
      <ThemeToggle />
    </div>

    <!-- current provider / model -->
    <div class="side-meta">
      <span v-if="state.metaError" class="chip bad" :title="state.metaError">服务信息不可用</span>
      <template v-else-if="state.meta">
        <span class="chip" title="当前 LLM provider / model">
          <b>{{ meta.provider || '—' }}</b> / {{ meta.model || '—' }}
        </span>
        <span class="chip" title="服务版本">v{{ meta.version || 'dev' }}</span>
        <span v-if="meta.metrics_enabled" class="chip ok" title="Prometheus metrics 已启用">
          metrics
        </span>
        <span v-if="meta.feishu_enabled" class="chip ok" title="飞书接入已启用">飞书</span>
      </template>
      <span v-else class="chip">读取服务信息…</span>
    </div>

    <!-- conversations -->
    <div class="side-sessions">
      <div class="side-head">
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

      <div class="side-scroll">
        <div class="side-section">
          <span class="side-label">会话</span>
          <button
            type="button"
            class="nav-item"
            :class="{ active: state.tab === 'chat' }"
            :aria-current="state.tab === 'chat' ? 'page' : undefined"
            @click="goChat"
          >
            <Icon name="message" :size="16" />
            <span class="nav-text">对话</span>
          </button>
        </div>

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
                    class="icon-btn sm"
                    title="重命名"
                    aria-label="重命名该对话"
                    @click="startRename(session)"
                  >
                    <Icon name="pencil" :size="14" />
                  </button>
                  <button
                    type="button"
                    class="icon-btn sm"
                    title="清空该对话的消息"
                    aria-label="清空该对话的消息"
                    @click="ask(session, 'clear')"
                  >
                    <Icon name="eraser" :size="14" />
                  </button>
                  <button
                    type="button"
                    class="icon-btn sm danger-text"
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
    </div>

    <!-- management navigation -->
    <nav class="side-nav" aria-label="管理页面">
      <span class="side-label">管理</span>
      <button
        v-for="item in navItems"
        :key="item.key"
        type="button"
        class="nav-item"
        :class="{ active: state.tab === item.key }"
        :aria-current="state.tab === item.key ? 'page' : undefined"
        @click="setTab(item.key)"
      >
        <Icon :name="item.icon" :size="16" />
        <span class="nav-text">{{ item.label }}</span>
      </button>
    </nav>

    <!-- account -->
    <div class="side-foot">
      <span class="side-user" :title="`当前登录用户：${state.username || 'admin'}`">
        <span class="avatar" aria-hidden="true">{{ (state.username || 'admin').charAt(0).toUpperCase() }}</span>
        <span class="nav-text">{{ state.username || 'admin' }}</span>
      </span>
      <button type="button" class="btn ghost sm" title="退出登录" @click="onLogout">
        <Icon name="logout" :size="15" />
        退出
      </button>
    </div>
  </div>
</template>
