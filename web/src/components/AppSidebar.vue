<script setup>
// The app sidebar — four regions, in this order:
//
//   1. 新建对话 — and nothing else. Which mode the next message runs on is not a
//      sidebar fact: it is the whole app's, drawn as the corner ribbon by the
//      shell (ClaudeRibbon.vue), so this slot belongs to the one button that
//      starts a conversation;
//   2. the **workspace folders**: conversations grouped by the directory they
//      run in. A workspace is managed here because this is where it is visible —
//      renaming a label and deleting a folder are list actions, not settings.
//   3. the two pinned menu items 设置 and 统计监控;
//   4. the account and 退出登录, present only where the deployment enforces a
//      password (admin.require_login: true).
//
// Why folders rather than a flat list with a workspace column: the question a
// person asks about a long list is "which project is this?", and grouping answers
// it without reading anything. It is also where switching lives: a conversation
// created from a folder starts in that workspace.
//
// Session rows keep 链路 / 重命名 / 清空 / 删除, shown on hover or focus. 删除 and
// 清空 stay two-step, as does deleting a workspace.
import { computed, nextTick, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import DirPicker from './DirPicker.vue'
import Icon from './Icon.vue'
import {
  chat,
  clearSession,
  createSession,
  createWorkspace,
  deleteSession,
  deleteWorkspace,
  loadSessions,
  loadWorkspaces,
  renameSession,
  renameWorkspace,
  selectSession,
  toggleWorkspaceFold,
  workspaceGroups,
} from '../chatStore.js'
import { formatAbsolute, formatCount, formatRelative } from '../format.js'
import { NAV, openTrace, setTab, signOut, state } from '../state.js'

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

// --- workspace editing --------------------------------------------------
const wsEditing = ref('')
const wsDraft = ref('')
let wsInput = null
function setWsInput(el) {
  wsInput = el
}
/** Pending workspace deletion: the name being confirmed. */
const wsConfirming = ref('')
const pickerOpen = ref(false)
/** Failure from the last attempt to create one, shown inside the dialog. */
const pickerError = ref('')
const creatingWorkspace = ref(false)

const sessions = computed(() => chat.sessions)
const activeId = computed(() => chat.activeId)
const groups = computed(() => workspaceGroups.value)

function pick(session) {
  emit('select')
  if (session.id !== chat.activeId) selectSession(session.id)
  if (state.tab !== 'chat') state.tab = 'chat'
}

/** New conversation in the workspace whose folder was used. */
async function onCreate(workspace = '') {
  emit('select')
  if (state.tab !== 'chat') state.tab = 'chat'
  cancelConfirm()
  cancelWsEdit()
  // createSession refreshes the folder counts itself: the new conversation lands
  // in one of them.
  await createSession({ workspace })
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

/**
 * Open 链路追踪 on everything this conversation recorded.
 *
 * The session is not selected first on purpose: the panel's own session filter
 * is what identifies the conversation there, so jumping sideways should not also
 * change which conversation the chat pane is showing.
 */
function viewTraces(session) {
  openTrace({ session: session.id })
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
  await loadWorkspaces({ quiet: true })
}

// --- workspace actions --------------------------------------------------

function startWsRename(ws) {
  wsConfirming.value = ''
  wsEditing.value = ws.name
  wsDraft.value = ws.name
  nextTick(() => {
    if (wsInput) {
      wsInput.focus()
      wsInput.select()
    }
  })
}

function cancelWsEdit() {
  wsEditing.value = ''
  wsDraft.value = ''
}

async function commitWsRename(ws) {
  if (wsEditing.value !== ws.name) return
  const next = wsDraft.value.trim()
  wsEditing.value = ''
  if (!next || next === ws.name) return
  await renameWorkspace(ws.name, next)
}

/**
 * Delete a workspace after a second click.
 *
 * The confirmation is inline and two-step because it changes where the agent may
 * work — and the note under it says the part that matters most: the files stay,
 * and the conversations inside move to another workspace.
 */
async function confirmDeleteWorkspace(ws) {
  wsConfirming.value = ''
  const ok = await deleteWorkspace(ws.name)
  if (ok) await loadSessions({ quiet: true })
}

// --- session ------------------------------------------------------------

/** Failure of the last 退出登录 attempt, shown in the account block. */
const signOutError = ref('')

/**
 * Log out.
 *
 * Only offered where login is actually enforced (`state.auth.required`): with
 * require_login off there is no account and no session, so a 退出登录 button
 * would be a control that does nothing.
 *
 * A failure is reported instead of reloaded away: if the server never heard the
 * request, the cookie may still be live and pretending otherwise would leave the
 * browser holding a session the operator thinks they closed.
 */
async function onSignOut() {
  signOutError.value = ''
  const done = await signOut()
  if (!done) signOutError.value = '退出失败：无法通知服务端，会话可能仍然有效'
}

/**
 * Create the workspace from the picked directory.
 *
 * The dialog closes only on success: a duplicate label or an unusable directory
 * is something to correct in place, and hiding the form would leave the reason
 * behind the overlay with nothing to fix.
 */
async function onPickedDirectory({ root, name }) {
  pickerError.value = ''
  creatingWorkspace.value = true
  try {
    const created = await createWorkspace({ root, name })
    if (!created) {
      pickerError.value = chat.actionError || '创建工作区失败'
      chat.actionError = ''
      return
    }
    pickerOpen.value = false
    await loadSessions({ quiet: true })
  } finally {
    creatingWorkspace.value = false
  }
}

function openPicker() {
  pickerError.value = ''
  pickerOpen.value = true
}

/** closedFold reports whether a workspace's conversations are hidden. */
function isFolded(name) {
  return Boolean(chat.collapsedWorkspaces[name])
}

// The run-mode badge that used to live here — 兼容模式 as a two-line purple panel
// under 新建对话, rendered whether or not the mode was on — is now the shell's
// corner ribbon (ClaudeRibbon.vue). Nothing about the mode is the sidebar's to
// compute: the state it reads, the model it names and the tooltip all live there.
</script>

<template>
  <div class="side-panel">
    <!-- 1. the only thing above the list -->
    <div class="side-new">
      <button
        type="button"
        class="btn primary block"
        :disabled="chat.creating"
        title="新建一个对话（在当前工作区里）"
        @click="onCreate(chat.defaultWorkspace)"
      >
        <Icon name="plus" :size="16" />
        {{ chat.creating ? '创建中…' : '新建对话' }}
      </button>
    </div>

    <!-- 2. the workspace folders: every remaining pixel, and it scrolls -->
    <div class="side-scroll">
      <div class="side-label">{{ chat.workspacesAvailable ? '工作区' : '会话列表' }}</div>

      <AsyncBlock
        :state="chat.sessionsStatus"
        :error="chat.sessionsError"
        :skeleton-rows="5"
        empty-text="暂无数据"
        @retry="loadSessions()"
      >
        <div v-if="!chat.workspacesAvailable && !sessions.length" class="empty side-empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">还没有任何对话</div>
          <div class="empty-hint">点击「新建对话」开始，消息会保存在服务端</div>
        </div>

        <div v-else-if="chat.workspacesAvailable && !groups.length" class="empty side-empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">还没有工作区</div>
          <div class="empty-hint">选一个本机目录作为工作区，agent 就在那里读写文件。</div>
          <button type="button" class="btn primary sm" @click="openPicker">
            <Icon name="plus" :size="15" />
            选择目录
          </button>
        </div>

        <div
          v-for="group in groups"
          :key="group.workspace ? group.workspace.name : 'all'"
          class="ws-folder"
          :class="{ flat: !group.workspace }"
        >
          <!-- the folder head: fold, name, count, actions. A deployment without
               a workspace layer renders one headless group, i.e. a plain list. -->
          <div
            v-if="group.workspace"
            class="ws-head"
            :class="{ pending: chat.pendingWorkspace === group.workspace.name }"
          >
            <button
              type="button"
              class="ws-fold"
              :aria-expanded="!isFolded(group.workspace.name)"
              :title="isFolded(group.workspace.name) ? '展开' : '折叠'"
              @click="toggleWorkspaceFold(group.workspace.name)"
            >
              <Icon
                :name="isFolded(group.workspace.name) ? 'chevron-right' : 'chevron-down'"
                :size="14"
              />
            </button>

            <template v-if="wsEditing === group.workspace.name">
              <input
                :ref="setWsInput"
                v-model="wsDraft"
                class="input"
                type="text"
                maxlength="60"
                aria-label="工作区名字"
                @keyup.enter="commitWsRename(group.workspace)"
                @keyup.esc="cancelWsEdit"
              />
              <button type="button" class="btn sm primary" @click="commitWsRename(group.workspace)">
                保存
              </button>
              <button type="button" class="btn sm ghost" @click="cancelWsEdit">取消</button>
            </template>

            <template v-else-if="wsConfirming === group.workspace.name">
              <span class="dimmer nowrap">删除该工作区？</span>
              <button
                type="button"
                class="btn sm danger"
                :disabled="chat.pendingWorkspace === group.workspace.name"
                @click="confirmDeleteWorkspace(group.workspace)"
              >
                删除
              </button>
              <button type="button" class="btn sm ghost" @click="wsConfirming = ''">取消</button>
            </template>

            <template v-else>
              <button
                type="button"
                class="ws-name"
                :title="group.workspace.root || '未归类'"
                @click="toggleWorkspaceFold(group.workspace.name)"
              >
                {{ group.workspace.name }}
              </button>
              <span class="ws-count">{{ formatCount(group.sessions.length) }}</span>
              <span class="spacer" />
              <span v-if="!group.orphan" class="ws-actions">
                <button
                  type="button"
                  class="session-btn"
                  title="在该工作区新建对话"
                  aria-label="在该工作区新建对话"
                  @click="onCreate(group.workspace.name)"
                >
                  <Icon name="plus" :size="14" />
                </button>
                <button
                  type="button"
                  class="session-btn"
                  title="重命名工作区（不改目录）"
                  aria-label="重命名工作区"
                  @click="startWsRename(group.workspace)"
                >
                  <Icon name="pencil" :size="14" />
                </button>
                <button
                  type="button"
                  class="session-btn danger-text"
                  title="删除工作区（不删除目录里的文件）"
                  aria-label="删除工作区"
                  @click="wsConfirming = group.workspace.name"
                >
                  <Icon name="trash" :size="14" />
                </button>
              </span>
            </template>
          </div>

          <!-- a workspace whose directory is gone is visible, not silent -->
          <p v-if="group.workspace && group.workspace.missing && !group.orphan" class="ws-warning">
            目录已不存在，需重新指定
          </p>

          <!-- the folder's conversations -->
          <ul
            v-if="!group.workspace || !isFolded(group.workspace.name)"
            class="ws-sessions session-list"
            :class="{ 'session-list-flat': !group.workspace }"
          >
            <li
              v-for="session in group.sessions"
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
                    <!-- A turn running in a conversation the reader is not
                         looking at: the answer is arriving, and this is where
                         they can see that it exists. -->
                    <span v-if="session.streaming" class="session-live">
                      <span class="spin" aria-hidden="true" />
                      <span>生成中</span>
                    </span>
                    <template v-else>
                      <span :title="formatAbsolute(session.updated_at)">
                        {{ formatRelative(session.updated_at) }}
                      </span>
                      <span class="dimmer">· {{ formatCount(session.message_count) }} 条</span>
                    </template>
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
                    title="在链路追踪里查看该对话的全部 trace"
                    aria-label="在链路追踪里查看该对话的全部 trace"
                    @click="viewTraces(session)"
                  >
                    <Icon name="git-branch" :size="14" />
                  </button>
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

            <li v-if="!group.sessions.length" class="ws-empty">
              {{ group.orphan ? '这些对话的工作区已被删除' : '这个工作区还没有对话' }}
            </li>
          </ul>
        </div>
      </AsyncBlock>

      <!-- creating a workspace lives at the end of the list it adds to -->
      <div v-if="chat.workspacesAvailable" class="side-foot">
        <button type="button" class="btn ghost block sm" @click="openPicker">
          <Icon name="folder" :size="15" />
          新建工作区（选择目录）
        </button>
      </div>
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

    <!-- 4. who is logged in — only on a deployment that asks for a password.
         With require_login off there is no account and no session, so the block
         would offer a control with nothing behind it. -->
    <div v-if="state.auth.required" class="side-account">
      <div class="account-line">
        <Icon name="lock" :size="14" />
        <span class="account-name" :title="`已登录：${state.auth.username || 'admin'}`">
          {{ state.auth.username || 'admin' }}
        </span>
        <button
          type="button"
          class="btn ghost sm"
          title="退出登录并回到登录页"
          @click="onSignOut"
        >
          <Icon name="log-out" :size="14" />
          退出
        </button>
      </div>
      <p v-if="signOutError" class="account-error">{{ signOutError }}</p>
    </div>

    <DirPicker
      :open="pickerOpen"
      :start="chat.homeDir"
      :error="pickerError"
      :busy="creatingWorkspace"
      @close="pickerOpen = false"
      @pick="onPickedDirectory"
    />
  </div>
</template>
