<script setup>
// 设置 → ClaudeCode — 兼容模式：这个 agent 读 ~/.claude/settings.json，按 Claude Code
// 用的那份模型配置和 hooks 运行。
//
// Why this panel is a render of one server payload rather than a form: the mode is a
// single boolean, and every other fact on screen — which model, which token, which
// hooks, and which of them this agent can actually dispatch — belongs to
// settings.json, not to the console. So nothing here asks anyone to retype a model
// name; the panel shows what the file resolved to, and 重新读取 settings.json is the
// only way to make an edit to it take effect.
//
// Three sentences the panel exists to say, each of them the surprising half of a fact:
//   * while the mode is on, EVERY conversation runs on model.effective_model — the
//     per-conversation model chosen in 对话 is overridden. It is stated twice (in the
//     switch card and in the model card) because it is the one consequence of this
//     switch that reaches conversations the reader is not looking at;
//   * the token only ever arrives masked (`model.token_masked`). There is no field for
//     the real key anywhere in this console, by design;
//   * only the five events in `hooks.supported_events` have a trigger point here.
//     Anything else configured in settings.json is listed as 已配置但不会派发, never as
//     working — the reader's job is to know which of their hooks are dead, and a list
//     that hid them would answer a different question.
//
// The mode itself lives in state.js, because the sidebar badge shows it next to every
// conversation while this panel switches it; two independent reads could disagree, so
// there is exactly one. This panel renders that object and re-reads it quietly on
// mount (settings.json can change under a console that has been open for a while).
//
// Layout: every wide thing is inside a `.table-wrap`, and every long mono string (a
// path, a command, a base_url) carries `.cc-break` — the console is used on a
// phone-sized window, where one command line with no break opportunity is what pushes
// a card off screen.
import { computed, onMounted, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import { api } from '../api.js'
import { formatAbsolute, formatCount, formatDuration, formatRelative, shortId } from '../format.js'
import { useResource } from '../useResource.js'
import {
  claudeCode,
  loadClaudeCode,
  reloadClaudeCodeSettings,
  setClaudeCodeMode,
} from '../state.js'

/** How many records 运行记录 asks for. The server also sends the same shape in
 *  `hook_log`, so this is "the last 50 in this process", not a page of a log. */
const EVENT_LIMIT = 50

/**
 * What each of the five events means *in this agent*.
 *
 * The wording is the panel's own rather than the file's: "PreToolUse" is Claude
 * Code's name for a moment this agent also has, and the reader needs the moment,
 * not the name. An event outside this map has no trigger point here at all.
 */
const EVENT_NOTES = {
  SessionStart: '新建 / 恢复对话时',
  UserPromptSubmit: '用户消息提交前',
  PreToolUse: '每次工具调用前（可以阻止）',
  PostToolUse: '工具调用之后',
  Stop: '一轮回答结束时',
}

const TYPE_LABELS = {
  command: '命令',
  http: 'HTTP',
  mcp_tool: 'MCP 工具',
  prompt: '提示词',
  agent: '子 agent',
}

/* ------------------------------------------------------- the payload --- */

const snapshot = computed(() => claudeCode.snapshot || {})
const model = computed(() => snapshot.value.model || {})
const native = computed(() => snapshot.value.native || {})
const hooks = computed(() => snapshot.value.hooks || {})
const env = computed(() => (Array.isArray(snapshot.value.env) ? snapshot.value.env : []))

const isCompat = computed(() => Boolean(snapshot.value.compat))
/** false only when there is nothing to turn on (no settings.json / no model). */
const available = computed(() => snapshot.value.available !== false)

/**
 * Whether both segments are locked.
 *
 * `available === false` means there is nothing to turn *on*, so with the mode off
 * there is nothing either segment could do. With the mode already on — the file broke
 * after it was switched — BOTH stay live on purpose: turning it off has to remain
 * possible, or a settings.json edited into a bad state would strand the console in a
 * mode that cannot run a message.
 */
const lockSwitch = computed(() => claudeCode.busy || (!available.value && !isCompat.value))
/** 本机模式's model, in one string, for the "what it goes back to" line. */
const nativeLabel = computed(() => {
  const provider = native.value.provider || ''
  const name = native.value.model || ''
  if (!name) return provider || '默认模型'
  return provider ? `${provider} / ${name}` : name
})
/** What a turn actually runs on right now. */
const effectiveModel = computed(
  () => model.value.effective_model || model.value.model || '（未解析出模型）',
)

/**
 * Why the switch cannot be turned on, in the order the reasons matter: the file is
 * what the mode reads, so a file that could not be read or parsed comes first, then
 * the model that came out of it.
 */
const unavailableReason = computed(() => {
  if (snapshot.value.settings_error) return snapshot.value.settings_error
  if (model.value.problem) return model.value.problem
  if (!snapshot.value.settings_found) {
    return `${snapshot.value.settings_path || '~/.claude/settings.json'} 不存在或读不到`
  }
  return 'settings.json 里没有可用的模型配置'
})

/**
 * The file changed after the last read: the server re-reads it on an mtime change,
 * but a console that has been open across that edit is showing the previous answer,
 * and "why is my hook not running" is usually this. mtime and loaded_at are both
 * RFC3339; a second of slack absorbs the filesystem's timestamp granularity.
 */
const settingsStale = computed(() => {
  const mtime = Date.parse(snapshot.value.settings_mtime || '')
  const loaded = Date.parse(snapshot.value.loaded_at || '')
  if (!Number.isFinite(mtime) || !Number.isFinite(loaded)) return false
  return mtime - loaded > 1000
})

/** The 认证 line: the scheme the requests go out with, and the masked token. */
const authLine = computed(() => {
  const scheme = model.value.auth_header || model.value.auth_style || ''
  if (!model.value.has_token) return scheme ? `${scheme}（没有 token）` : '没有 token'
  return `${scheme ? `${scheme} ` : ''}${model.value.token_masked || '（已隐藏）'}`.trim()
})

/**
 * The value of one env row.
 *
 * A full token never reaches this page — `model.token_masked` is all the server
 * sends — but an env row is a key/value pair copied out of settings.json, so a row
 * marked `secret` is rendered the way the token is: head, ellipsis, tail. A value
 * that already arrives masked (it contains an ellipsis, or is too short to be a key)
 * is passed through rather than masked a second time, and an empty one says so
 * instead of rendering as a blank cell.
 */
function envValue(row) {
  const text = typeof row.value === 'string' ? row.value : ''
  if (!row.secret) return text || '（空）'
  if (text === '') return '（未设置）'
  if (text.includes('…')) return text
  if (text.length <= 12) return '••••••'
  return `${text.slice(0, 6)}…${text.slice(-4)}`
}

/** 786432 -> "786,432 tokens" ; the window is a token count, so it says so. */
const compactWindow = computed(() => {
  const raw = model.value.auto_compact_window
  if (raw === undefined || raw === null || raw === '') return ''
  return `${formatCount(raw)} tokens`
})

/** The model card's rows: label, value, and whether the value is a code-ish one. */
const modelRows = computed(() => [
  { label: 'base_url', value: model.value.base_url, mono: true },
  { label: '协议', value: model.value.kind, mono: true },
  { label: '认证', value: authLine.value, mono: true },
  { label: '主模型', value: model.value.model, mono: true },
  { label: 'Opus 模型', value: model.value.opus_model, mono: true },
  { label: 'Sonnet 模型', value: model.value.sonnet_model, mono: true },
  { label: 'Haiku 模型', value: model.value.haiku_model, mono: true },
  { label: '子 agent 模型', value: model.value.subagent_model, mono: true },
  { label: 'effort', value: model.value.effort },
  { label: 'auto-compact 窗口', value: compactWindow.value },
])

/* ------------------------------------------------------------ hooks ---- */

const supportedEvents = computed(() =>
  Array.isArray(hooks.value.supported_events) && hooks.value.supported_events.length
    ? hooks.value.supported_events
    : Object.keys(EVENT_NOTES),
)

const payloadEvents = computed(() =>
  Array.isArray(hooks.value.events) ? hooks.value.events : [],
)

/**
 * Normalise one event entry into what the template renders.
 *
 * The server always sends `groups[].handlers[]`, but the panel reads each level
 * defensively: a hook block is diagnostic output about a file the operator may have
 * edited a second ago, and an omitted array must render as "no handlers", never as a
 * crash in the middle of the card.
 */
function buildEvent(name, entry) {
  const groups = (entry && Array.isArray(entry.groups) ? entry.groups : []).map((group) => ({
    matcher: typeof group.matcher === 'string' ? group.matcher : '',
    handlers: (Array.isArray(group.handlers) ? group.handlers : []).map((handler) => ({
      type: handler.type || 'command',
      command: handler.command || '',
      url: handler.url || '',
      mcpServer: handler.mcp_server || '',
      mcpTool: handler.mcp_tool || '',
      args: Array.isArray(handler.args) ? handler.args : [],
      timeout: handler.timeout || 0,
      async: Boolean(handler.async),
      condition: handler.if || '',
      summary: handler.summary || '',
      unsupported: handler.unsupported || '',
    })),
  }))
  const handlers = groups.reduce((sum, group) => sum + group.handlers.length, 0)
  // One line per handler of an event that will never run, for the compact listing.
  const summaries = []
  for (const group of groups) {
    for (const handler of group.handlers) {
      const target = handlerCommand(handler)
      const matcher = group.matcher ? `${group.matcher} · ` : ''
      summaries.push(`${matcher}${target || TYPE_LABELS[handler.type] || handler.type}`)
    }
  }
  return {
    event: name,
    note: EVENT_NOTES[name] || '',
    groups,
    handlers,
    summaries,
  }
}

/**
 * The five events this agent can dispatch, in the order the server declares them.
 *
 * They are listed even when settings.json configures nothing for them: the reader's
 * question is "what can this agent do", and 已接入 · 未配置处理程序 is the honest
 * answer for a trigger point nobody hooked.
 */
const dispatchedEvents = computed(() => {
  const byName = new Map(payloadEvents.value.map((entry) => [entry.event, entry]))
  return supportedEvents.value.map((name) => buildEvent(name, byName.get(name)))
})

/**
 * Configured in settings.json, but this agent has no trigger point for them.
 *
 * `supported_events` is the authoritative list of what actually fires here, so an
 * event outside it is inert whatever its own entry claims — that is the contract
 * this list exists to render. Built from both halves of the payload on purpose:
 * `events` when the server described them, and `unsupported_configured` when it only
 * reported the name — either way the name has to be on screen, marked as configured
 * and not dispatched.
 */
const inertEvents = computed(() => {
  const known = new Set(supportedEvents.value)
  const out = []
  for (const entry of payloadEvents.value) {
    if (known.has(entry.event)) continue
    known.add(entry.event)
    out.push(buildEvent(entry.event, entry))
  }
  for (const name of hooks.value.unsupported_configured || []) {
    if (known.has(name)) continue
    known.add(name)
    out.push(buildEvent(name, undefined))
  }
  return out
})

/** What a handler actually runs: the command, the URL, or the MCP tool it calls. */
function handlerCommand(handler) {
  if (handler.command) return handler.command
  if (handler.url) return handler.url
  if (handler.mcpTool || handler.mcpServer) {
    return [handler.mcpServer, handler.mcpTool].filter(Boolean).join(' · ')
  }
  return ''
}

function typeLabel(handler) {
  return TYPE_LABELS[handler.type] || handler.type
}

/** 命令 is the common case and stays neutral; the other two are worth a colour. */
function typeClass(handler) {
  if (handler.unsupported) return 'warn'
  if (handler.type === 'http') return 'blue'
  if (handler.type === 'mcp_tool') return 'purple'
  return ''
}

/* ------------------------------------------------------- 运行记录 ------ */

/**
 * GET /api/claudecode/events — the record of what the hooks did in this process.
 *
 * A resource of its own rather than a field of the status: 刷新 here means "read the
 * log again", which is a different question from "read settings.json again" (the
 * switch card's button), and tying them together would make one button do two things
 * the reader cannot tell apart.
 */
const {
  data: eventsData,
  status: eventsStatus,
  error: eventsError,
  reload: reloadEvents,
} = useResource(() => api.claudeCodeEvents(EVENT_LIMIT), { watchRange: false })

/**
 * The rows, from the fresher of the two places the same records arrive.
 *
 * `hook_log` rides on the status payload, so the table is populated the moment the
 * panel opens; GET /api/claudecode/events is the read that 刷新 drives. Once that
 * request has answered, its answer wins — including when it is empty, which is a
 * fact about this process rather than a missing value.
 */
const logRows = computed(() => {
  const fromRoute = eventsData.value && Array.isArray(eventsData.value.events)
    ? eventsData.value.events
    : null
  if (fromRoute) return fromRoute
  const fromStatus = snapshot.value.hook_log
  return Array.isArray(fromStatus) ? fromStatus : []
})

/**
 * The log section's state, for AsyncBlock.
 *
 * A request still in flight is not a loading state while hook_log rows are already
 * on screen — that would replace a list the reader is looking at with a skeleton.
 * A failure is always reported: a log that silently stops updating is worse than an
 * error, because it reads as "no hooks ran".
 */
const logStatus = computed(() => {
  if (eventsStatus.value === 'error') return logRows.value.length ? 'ready' : 'error'
  if (logRows.value.length) return 'ready'
  return eventsStatus.value === 'loading' ? 'loading' : 'ready'
})

/**
 * How one record ended.
 *
 * Four outcomes, and they are not interchangeable: 后台 is an async handler that was
 * still running when the record was written (`exit_code: -1`, not a failure), 已阻止
 * is a PreToolUse hook that stopped the call (a decision, not a crash), a non-zero
 * exit code is a handler that failed, and 0 is a handler that ran.
 */
function exitTag(row) {
  const code = row.exit_code
  if (code === -1) return { label: '后台', cls: 'blue' }
  if (typeof code !== 'number') return { label: '—', cls: '' }
  if (code > 0) return { label: `失败 exit ${code}`, cls: 'bad' }
  return { label: 'exit 0', cls: 'ok' }
}

/* --------------------------------------------------------- actions ----- */

/** The last mutation's outcome, and its failure — both read as the server's words. */
const notice = ref('')
const actionError = ref('')

function messageOf(err, fallback) {
  return err && typeof err.message === 'string' && err.message !== '' ? err.message : fallback
}

/**
 * Switch the mode.
 *
 * The log is re-read with the switch because hooks are part of what the mode turns
 * on: a record list that still shows the previous mode's runs next to a "已关闭"
 * label would be read as proof that hooks keep running.
 */
async function chooseMode(compat) {
  if (claudeCode.busy || compat === isCompat.value) return
  notice.value = ''
  actionError.value = ''
  try {
    const next = await setClaudeCodeMode(compat)
    notice.value = next.compat
      ? `已打开 ClaudeCode 兼容模式：下一轮消息起，所有对话都跑 ${effectiveModel.value}`
      : `已回到本机模式：下一轮消息起，每个对话跑自己在 对话 里选的模型（默认 ${nativeLabel.value}）`
    await reloadEvents({ quiet: true })
  } catch (err) {
    if (err && err.status === 401) return
    actionError.value = messageOf(err, '切换失败，请重试')
  }
}

/** 重新读取 settings.json — the mtime watcher's manual counterpart. */
async function rereadSettings() {
  if (claudeCode.busy) return
  notice.value = ''
  actionError.value = ''
  try {
    const next = await reloadClaudeCodeSettings()
    const count = (next.hooks && next.hooks.total_handlers) || 0
    notice.value = `已重新读取 ${next.settings_path || 'settings.json'}：${formatCount(count)} 个 hook 处理程序`
    await reloadEvents({ quiet: true })
  } catch (err) {
    if (err && err.status === 401) return
    actionError.value = messageOf(err, '重新读取失败，请重试')
  }
}

/** 运行记录's 刷新: the log only, not the file. */
async function refreshLog() {
  notice.value = ''
  actionError.value = ''
  try {
    await reloadEvents({ quiet: true })
  } catch (err) {
    if (err && err.status === 401) return
    actionError.value = messageOf(err, '读取运行记录失败')
  }
}

// The mode is read once at boot for the sidebar badge; re-read it quietly here, so a
// panel opened after the file was edited shows what the file says now.
onMounted(() => loadClaudeCode({ quiet: true }))
</script>

<template>
  <div class="stack">
    <AsyncBlock
      :state="claudeCode.status"
      :error="claudeCode.error"
      :skeleton-rows="4"
      @retry="loadClaudeCode()"
    >
      <!-- 1. the switch: the only control on this screen -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            运行模式
            <span class="card-sub">
              读 Claude Code 的 <code class="md-code">settings.json</code> 运行 ·
              这里是唯一能开关它的地方
            </span>
          </div>
          <button
            type="button"
            class="btn sm"
            :disabled="claudeCode.busy"
            title="重新读取 settings.json（服务端也会在文件 mtime 变化时自己重读）"
            @click="rereadSettings"
          >
            <Icon name="refresh" :size="15" />
            {{ claudeCode.busy ? '读取中…' : '重新读取 settings.json' }}
          </button>
        </div>

        <div class="cc-mode">
          <div class="seg" role="group" aria-label="运行模式">
            <button
              type="button"
              :class="{ active: !isCompat }"
              :aria-pressed="!isCompat"
              :disabled="lockSwitch"
              @click="chooseMode(false)"
            >
              本机模式
            </button>
            <button
              type="button"
              :class="{ active: isCompat }"
              :aria-pressed="isCompat"
              :disabled="lockSwitch"
              @click="chooseMode(true)"
            >
              ClaudeCode 兼容
            </button>
          </div>
          <span class="cc-state">
            <template v-if="isCompat">
              当前是 <strong>ClaudeCode 兼容模式</strong>：跑
              <code class="md-code">{{ effectiveModel }}</code>。
            </template>
            <template v-else>
              当前是 <strong>本机模式</strong>：每个对话跑自己在 对话 里选的模型（默认
              <code class="md-code">{{ nativeLabel }}</code>）。
            </template>
          </span>
        </div>

        <!-- The one consequence of this switch that reaches conversations the reader
             is not looking at: it overrides the model each of them picked. -->
        <div v-if="isCompat" class="banner warn" role="note">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">
            <strong>所有对话</strong>都跑 <code class="md-code">{{ effectiveModel }}</code>，
            覆盖 对话 里为每个对话选的模型 —— 包括正在跑的、以及你没在看的那些。
            关掉模式即恢复：下一个对话按 <code class="md-code">{{ nativeLabel }}</code> 跑，
            不需要重启。
          </span>
        </div>
        <p v-else class="muted-note card-foot">
          打开后，<strong>所有对话</strong>都会改跑 settings.json 里解析出的模型，覆盖 对话
          里每个对话自己选的模型；关掉就回到
          <code class="md-code">{{ nativeLabel }}</code>，下一个对话立即生效。
        </p>

        <!-- Nothing to turn on: the reason is the server's (the file, or the model). -->
        <div v-if="!available" class="banner error" role="alert">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">
            这个部署暂时无法打开兼容模式：{{ unavailableReason }}
          </span>
        </div>

        <div class="cc-file">
          <Icon name="file-text" :size="13" />
          <span class="cc-break mono">{{ snapshot.settings_path || '~/.claude/settings.json' }}</span>
          <span v-if="snapshot.settings_found" class="tag ok">已读到</span>
          <span v-else class="tag bad">读不到</span>
          <span v-if="snapshot.loaded_at" :title="formatAbsolute(snapshot.loaded_at)">
            最后读取 {{ formatRelative(snapshot.loaded_at) }}
          </span>
        </div>

        <p v-if="settingsStale" class="muted-note">
          文件在最后一次读取之后又改过（mtime
          {{ formatAbsolute(snapshot.settings_mtime) }} > 读取
          {{ formatAbsolute(snapshot.loaded_at) }}）：点「重新读取 settings.json」让它生效。
        </p>
        <p v-if="snapshot.settings_error" class="cell-err cc-break">
          读取 settings.json 失败：{{ snapshot.settings_error }}
        </p>
      </div>

      <!-- 2. the model configuration, as the file resolved it -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            模型（来自 Claude Code）
            <span class="card-sub">
              只读 · 由 <code class="md-code">settings.json</code> 的 env 解析，改文件后重新读取
            </span>
            <span class="tag" :class="model.ready ? 'ok' : 'bad'">
              {{ model.ready ? '可运行' : '不可运行' }}
            </span>
          </div>
        </div>

        <div v-if="model.problem" class="banner error" role="alert">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">{{ model.problem }}</span>
        </div>

        <dl class="kv">
          <div v-for="row in modelRows" :key="row.label" class="kv-row">
            <dt>{{ row.label }}</dt>
            <dd :class="{ mono: row.mono, 'cc-break': row.mono }">{{ row.value || '—' }}</dd>
          </div>
          <div class="kv-row">
            <dt>生效模型</dt>
            <dd class="mono cc-break">
              {{ effectiveModel }}
              <span v-if="isCompat" class="tag purple">所有对话</span>
              <span v-else class="tag">模式关闭，暂未使用</span>
            </dd>
          </div>
        </dl>

        <p class="muted-note card-foot">
          这一份配置<strong>覆盖</strong>的是「每个对话跑哪个模型」：模式打开时，对话里选的
          <code class="md-code">{{ nativeLabel }}</code> 不再生效，所有对话一律跑
          <code class="md-code">{{ effectiveModel }}</code>。
          <template v-if="!isCompat">现在模式是关的，所以对话仍按各自选的模型跑。</template>
        </p>
      </div>

      <!-- 3. the env vars the file sets, which is where all of the above came from -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            环境变量
            <span class="card-sub">
              settings.json 的 env · 共 {{ formatCount(env.length) }} 条，模型与端点就是从这里解析的
            </span>
          </div>
        </div>

        <div v-if="!env.length" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">settings.json 里没有配置 env</div>
          <div class="empty-hint">
            兼容模式的模型配置全部来自这里（<code class="md-code">ANTHROPIC_BASE_URL</code> /
            <code class="md-code">ANTHROPIC_AUTH_TOKEN</code> /
            <code class="md-code">ANTHROPIC_MODEL</code> 等）：没有 env 就没有可跑的模式。
          </div>
        </div>

        <div v-else class="table-wrap">
          <table class="data">
            <thead>
              <tr>
                <th>变量</th>
                <th>值</th>
                <th>用途</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in env" :key="row.key">
                <td class="mono cc-break">{{ row.key }}</td>
                <td class="mono cc-break">
                  {{ envValue(row) }}
                  <span v-if="row.secret" class="tag">已隐藏</span>
                </td>
                <td class="cc-break">
                  {{ row.note || '—' }}
                  <span v-if="row.used === false" class="tag warn">未使用</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p class="muted-note card-foot">
          带 <code class="md-code">secret</code> 的变量只会以掩码形式显示（和
          <code class="md-code">token_masked</code> 同一种写法）：这个页面没有任何地方能填写真实密钥
          —— 要换密钥就改 <code class="md-code">settings.json</code>，再点「重新读取 settings.json」。
        </p>
      </div>

      <!-- 4. hooks: what the file asked for, against what this agent can dispatch -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            Hooks
            <span class="card-sub">
              已接入 {{ formatCount(supportedEvents.length) }} 个事件 ·
              配置了 {{ formatCount(hooks.configured_events) }} 个 ·
              共 {{ formatCount(hooks.total_handlers) }} 个处理程序
            </span>
            <span class="tag" :class="hooks.enabled ? 'ok' : 'warn'">
              {{ hooks.enabled ? '已启用' : '未启用' }}
            </span>
          </div>
        </div>

        <div v-if="hooks.settings_disable_all" class="banner warn" role="note">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">
            settings.json 里写着 <code class="md-code">disableAllHooks: true</code>：
            Claude Code 自己也会跳过全部 hooks，所以即使模式打开，下面这些处理程序一个都不会执行。
          </span>
        </div>
        <div v-else-if="!hooks.enabled" class="banner info" role="note">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">
            兼容模式没打开，所以 hooks 不派发：下面列出的是 settings.json 里配了什么，
            以及本 agent 有没有对应的触发点。打开模式后它们才会执行。
          </span>
        </div>

        <!-- The five trigger points, with what each one means here -->
        <div class="cc-events">
          <div v-for="event in dispatchedEvents" :key="event.event" class="cc-event">
            <div class="cc-event-head">
              <span class="mono">{{ event.event }}</span>
              <span class="tag ok">已接入</span>
              <span v-if="event.handlers" class="tag">
                {{ formatCount(event.handlers) }} 个处理程序
              </span>
              <span v-else class="tag warn">未配置处理程序</span>
            </div>
            <p class="cc-event-note">{{ event.note }}</p>

            <div v-for="(group, gi) in event.groups" :key="gi" class="cc-group">
              <div class="cc-group-head">
                <span class="dimmer">matcher</span>
                <span class="tag mono">{{ group.matcher || '（全部）' }}</span>
              </div>
              <div class="cc-handlers">
                <div
                  v-for="(handler, hi) in group.handlers"
                  :key="hi"
                  class="cc-handler"
                  :class="{ dead: Boolean(handler.unsupported) }"
                >
                  <span class="tag" :class="typeClass(handler)">{{ typeLabel(handler) }}</span>
                  <span v-if="handlerCommand(handler)" class="cc-handler-cmd">
                    {{ handlerCommand(handler) }}
                  </span>
                  <span v-else class="dimmer" :title="handler.summary">
                    {{ handler.summary || '没有可执行的目标' }}
                  </span>
                  <span v-for="arg in handler.args" :key="arg" class="tag mono">{{ arg }}</span>
                  <span v-if="handler.timeout" class="tag">{{ formatCount(handler.timeout) }}s 超时</span>
                  <span v-if="handler.async" class="tag blue">后台执行</span>
                  <span v-if="handler.condition" class="tag mono">if {{ handler.condition }}</span>
                  <span v-if="handler.unsupported" class="tag warn">
                    不会运行：{{ handler.unsupported }}
                  </span>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- Configured in the file, but this agent has no trigger point: never shown
             as working, because "my hook is configured and does nothing" is exactly
             the question this list answers. -->
        <div v-if="inertEvents.length" class="cc-inert">
          <p class="cc-inert-head">
            <Icon name="circle-alert" :size="14" />
            settings.json 里还配了这些事件，但本 agent 没有对应的触发点，永远不会被派发：
          </p>
          <div v-for="event in inertEvents" :key="event.event" class="cc-event">
            <div class="cc-event-head">
              <span class="mono">{{ event.event }}</span>
              <span class="tag warn">未接入（本 agent 无触发点）</span>
              <span v-if="event.handlers" class="tag">
                {{ formatCount(event.handlers) }} 个处理程序已配置
              </span>
            </div>
            <p class="cc-event-note">
              配了也不会生效：这一段只是把「文件里写了、实际不会跑」这件事摆出来。
            </p>
            <div v-if="event.summaries.length" class="cc-summaries">
              <span v-for="(text, i) in event.summaries" :key="i" class="tag mono">{{ text }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- 5. what the hooks did in this process -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            运行记录
            <span class="card-sub">
              本进程内 hook 的执行 · 打开时显示状态里的
              <code class="md-code">hook_log</code>，「刷新」读
              <code class="md-code">GET /api/claudecode/events</code>
            </span>
          </div>
          <button type="button" class="btn ghost sm" :disabled="eventsStatus === 'loading'" @click="refreshLog">
            <Icon name="refresh" :size="15" />
            刷新
          </button>
        </div>

        <div v-if="eventsStatus === 'error' && logRows.length" class="banner error" role="alert">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">
            刷新运行记录失败：{{ eventsError || '请稍后重试' }}（下面是上一次读到的内容）
          </span>
          <button type="button" class="btn sm" @click="refreshLog">重试</button>
        </div>

        <AsyncBlock
          :state="logStatus"
          :error="eventsError"
          :skeleton-rows="3"
          @retry="reloadEvents()"
        >
          <div v-if="!logRows.length" class="empty">
            <div class="empty-ico" aria-hidden="true">◍</div>
            <div class="empty-text">本进程还没有 hook 运行记录</div>
            <div class="empty-hint">
              Hooks 只在对话真正跑起来的时候执行：发一条消息、调用一次工具，这里就会出现一行。
              模式关闭、<code class="md-code">disableAllHooks</code> 为 true、或者 settings.json 里没有
              配置处理程序时，它会一直是空的。
            </div>
          </div>

          <div v-else class="table-wrap">
            <table class="data">
              <thead>
                <tr>
                  <th>时间</th>
                  <th>事件</th>
                  <th>matcher</th>
                  <th>处理程序</th>
                  <th>结果</th>
                  <th class="num">耗时</th>
                  <th>说明</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(row, i) in logRows" :key="i">
                  <td class="nowrap" :title="formatAbsolute(row.at)">
                    {{ formatRelative(row.at) }}
                  </td>
                  <td class="mono">{{ row.event || '—' }}</td>
                  <td class="mono cc-break">{{ row.matcher || '（全部）' }}</td>
                  <td class="cc-break">
                    <span class="tag mono">{{ row.handler || '—' }}</span>
                    <span v-if="row.command" class="cc-handler-cmd">{{ row.command }}</span>
                  </td>
                  <td>
                    <span class="tag" :class="exitTag(row).cls">{{ exitTag(row).label }}</span>
                    <span v-if="row.blocked" class="tag warn">已阻止</span>
                  </td>
                  <td class="num">{{ formatDuration(row.duration_ms) }}</td>
                  <td class="cc-break">
                    <div v-if="row.reason" class="cc-log-reason">{{ row.reason }}</div>
                    <div v-if="row.error" class="cell-err">{{ row.error }}</div>
                    <div v-if="row.tool_name || row.session_id" class="dimmer">
                      <template v-if="row.tool_name">工具 {{ row.tool_name }}</template>
                      <template v-if="row.tool_name && row.session_id"> · </template>
                      <template v-if="row.session_id">会话 {{ shortId(row.session_id) }}</template>
                    </div>
                    <span
                      v-if="!row.reason && !row.error && !row.tool_name && !row.session_id"
                      class="dimmer"
                    >
                      —
                    </span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <p class="muted-note card-foot">
            最多显示最近 {{ formatCount(EVENT_LIMIT) }} 条 · 只记录本进程：服务端重启后从空开始。
          </p>
        </AsyncBlock>
      </div>

      <!-- 6. where the mode stops, in three lines -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            这个模式的边界
          </div>
        </div>
        <ul class="cc-limits">
          <li>
            只有 <code class="md-code">command</code> / <code class="md-code">http</code> /
            <code class="md-code">mcp_tool</code> 三种处理程序会执行；
            <code class="md-code">prompt</code> 与 <code class="md-code">agent</code>
            会照原样列出来，并标注「不会运行」。
          </li>
          <li>
            只有 SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / Stop 五个事件在这个
            agent 里有触发点；settings.json 里配的其他事件只会被列出来，不会被派发。
          </li>
          <li>
            模型、密钥、hooks 都由 <code class="md-code">~/.claude/settings.json</code> 决定：这个页面
            只能开关模式与重新读取文件，改不了文件里的内容。
          </li>
        </ul>
      </div>
    </AsyncBlock>

    <!-- The last action's outcome, under everything it could have changed. -->
    <div v-if="notice" class="banner cc-notice" role="status">
      <Icon name="check" :size="16" />
      <span class="banner-text">{{ notice }}</span>
    </div>
    <div v-if="actionError" class="banner error" role="alert">
      <Icon name="circle-alert" :size="16" />
      <span class="banner-text">{{ actionError }}</span>
    </div>
  </div>
</template>
