<script setup>
// MCP — the MCP servers the agent can call, and the assistant that writes one.
//
// Three things live here, all against /api/mcp/*:
//
//   1. 配置助手 (the top card): a description in, a draft definition out. The
//      draft fills the form below and is NOT saved — the model proposes, the
//      operator confirms. That split is the whole design: a plausible but wrong
//      package name is the most likely model failure, and it must not be able
//      to reach a working configuration on its own.
//   2. the server list: each row is the stored definition plus its live state
//      (`runtime`), so "已连接 / 未连接 / 已停用" and the tool list come from the
//      process that actually holds the connection rather than from the form.
//   3. the form: create or edit, with 测试连接 that dials the definition as
//      typed (POST /api/mcp/probe) so a broken server never reaches the runtime.
//
// A server declared in the config file (mcp.servers) is re-synced at every
// start, so its row is read-only apart from its enabled flag — the same rule
// 模型管理 applies to a config provider, and the row says why.
import { computed, reactive, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import { api } from '../api.js'
import { formatAbsolute, formatCount, formatRelative } from '../format.js'
import { useResource } from '../useResource.js'

const { data, status, error, loading, reload } = useResource(() => api.mcpServers(), {
  watchRange: false,
})

const servers = computed(() => (data.value && data.value.servers) || [])
const transports = computed(() => (data.value && data.value.transports) || ['stdio', 'sse', 'http'])
/** Whether the server has a runtime at all (false when chat.enable is off). */
const runtimeAvailable = computed(() => Boolean(data.value && data.value.runtime))

const connectedCount = computed(() => servers.value.filter((s) => s.runtime && s.runtime.connected).length)
const toolCount = computed(() =>
  servers.value.reduce((sum, s) => sum + ((s.runtime && s.runtime.tools) || []).length, 0),
)

const TRANSPORT_LABEL = { stdio: 'stdio 本地进程', sse: 'sse 远程', http: 'http 远程' }
const TRANSPORT_SHORT = { stdio: 'stdio', sse: 'sse', http: 'http' }

/* ------------------------------------------------------------- row state -- */

/** Per-server action state: 'test' | 'toggle' | 'delete'. */
const busy = reactive({})
const rowError = reactive({})
const testResult = reactive({})
/** A server id awaiting a second click (destructive actions are two-step). */
const confirmDelete = ref('')
const flash = ref('')
let flashTimer = null

function setFlash(text) {
  flash.value = text
  if (flashTimer) window.clearTimeout(flashTimer)
  flashTimer = window.setTimeout(() => {
    flashTimer = null
    flash.value = ''
  }, 3200)
}

function errorText(err, fallback) {
  if (err && typeof err.message === 'string' && err.message !== '') return err.message
  return fallback
}

function isConfig(server) {
  return server.source === 'config'
}

function isOn(server) {
  return server.enabled !== false
}

function isConnected(server) {
  return Boolean(server.runtime && server.runtime.connected)
}

/** The summary line under a row's name: what would actually be executed. */
function endpoint(server) {
  if (server.transport === 'stdio' || !server.transport) {
    const parts = [server.command, ...(server.args || [])].filter(Boolean)
    return parts.length ? parts.join(' ') : '未填写命令'
  }
  return server.url || '未填写地址'
}

function statusLabel(server) {
  if (!isOn(server)) return '已停用'
  if (!runtimeAvailable.value) return '未运行'
  if (isConnected(server)) return '已连接'
  return '未连接'
}

function statusClass(server) {
  if (!isOn(server)) return ''
  if (isConnected(server)) return 'ok'
  return runtimeAvailable.value ? 'bad' : 'warn'
}

/* --------------------------------------------------------------- actions -- */

async function testServer(server) {
  if (busy[server.id]) return
  busy[server.id] = 'test'
  rowError[server.id] = ''
  try {
    const result = await api.testMcpServer(server.id)
    testResult[server.id] = result
  } catch (err) {
    if (err && err.status === 401) return
    delete testResult[server.id]
    rowError[server.id] = errorText(err, '测试失败，请重试')
  } finally {
    busy[server.id] = ''
  }
}

async function toggleServer(server) {
  if (busy[server.id]) return
  busy[server.id] = 'toggle'
  rowError[server.id] = ''
  try {
    await api.saveMcpServer({ id: server.id, enabled: !isOn(server) })
    await reload({ quiet: true })
    setFlash(`${server.name || server.id} 已${isOn(server) ? '停用' : '启用'}`)
  } catch (err) {
    if (err && err.status === 401) return
    rowError[server.id] = errorText(err, '切换失败，请重试')
  } finally {
    busy[server.id] = ''
  }
}

async function removeServer(server) {
  if (busy[server.id]) return
  busy[server.id] = 'delete'
  rowError[server.id] = ''
  try {
    await api.deleteMcpServer(server.id)
    confirmDelete.value = ''
    delete testResult[server.id]
    await reload({ quiet: true })
    setFlash(`${server.name || server.id} 已删除`)
  } catch (err) {
    if (err && err.status === 401) return
    rowError[server.id] = errorText(err, '删除失败，请重试')
  } finally {
    busy[server.id] = ''
  }
}

/** 重连: reconnect everything. The retry for a transport that died. */
const reloading = ref('')
async function reconnectAll() {
  if (reloading.value) return
  reloading.value = 'all'
  try {
    await api.reloadMcp()
    await reload({ quiet: true })
    setFlash('已重新连接所有 MCP 服务器')
  } catch (err) {
    if (err && err.status !== 401) setFlash(errorText(err, '重连失败'))
  } finally {
    reloading.value = ''
  }
}

/* ---------------------------------------------------------------- form --- */

const form = reactive({
  open: false,
  mode: 'add',
  id: '',
  name: '',
  transport: 'stdio',
  command: '',
  args: '',
  env: '',
  url: '',
  headers: '',
  enabled: true,
})
const formErrors = reactive({ id: '', name: '', command: '', url: '' })
const formError = ref('')
const savedNotice = ref('')
const saving = ref(false)
const probing = ref(false)
const probeResult = ref(null)

function resetForm() {
  form.id = ''
  form.name = ''
  form.transport = 'stdio'
  form.command = ''
  form.args = ''
  form.env = ''
  form.url = ''
  form.headers = ''
  form.enabled = true
  formErrors.id = ''
  formErrors.name = ''
  formErrors.command = ''
  formErrors.url = ''
  formError.value = ''
  probeResult.value = null
}

function openAdd() {
  resetForm()
  form.mode = 'add'
  form.open = true
  savedNotice.value = ''
}

function openEdit(server) {
  resetForm()
  form.mode = 'edit'
  form.open = true
  form.id = server.id
  form.name = server.name || ''
  form.transport = server.transport || 'stdio'
  form.command = server.command || ''
  form.args = (server.args || []).join('\n')
  form.env = (server.env || []).join('\n')
  form.url = server.url || ''
  form.headers = (server.headers || []).join('\n')
  form.enabled = isOn(server)
  savedNotice.value = ''
}

function closeForm() {
  form.open = false
  savedNotice.value = ''
}

const isStdio = computed(() => form.transport === 'stdio')

/** Split a textarea into entries, one per line, dropping blanks. */
function lines(text) {
  return String(text || '')
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
}

/** The request body for the current form contents. */
function formBody(extra = {}) {
  const body = {
    id: form.id.trim(),
    name: form.name.trim(),
    transport: form.transport,
    enabled: form.enabled,
    ...extra,
  }
  if (isStdio.value) {
    body.command = form.command.trim()
    body.args = lines(form.args)
    body.env = lines(form.env)
    body.url = ''
    body.headers = []
  } else {
    body.url = form.url.trim()
    body.headers = lines(form.headers)
    body.command = ''
    body.args = []
    body.env = []
  }
  return body
}

function validateForm() {
  formErrors.id = ''
  formErrors.command = ''
  formErrors.url = ''
  if (!form.id.trim()) {
    formErrors.id = '必须填写 id（URL 与数据库主键都用它）'
  } else if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(form.id.trim())) {
    formErrors.id = '只能用小写字母、数字、点、横线、下划线，且以字母或数字开头'
  }
  if (isStdio.value && !form.command.trim()) {
    formErrors.command = 'stdio 服务器必须填写要执行的命令'
  }
  if (!isStdio.value && !form.url.trim()) {
    formErrors.url = '远程服务器必须填写地址'
  }
  return !formErrors.id && !formErrors.command && !formErrors.url
}

async function probeForm() {
  if (probing.value) return
  if (!validateForm()) return
  probing.value = true
  formError.value = ''
  probeResult.value = null
  try {
    // probe (not test): the definition is dialed as typed and never saved, so a
    // broken server cannot reach the runtime through the form.
    probeResult.value = await api.probeMcpServer(formBody())
  } catch (err) {
    if (err && err.status === 401) return
    formError.value = errorText(err, '测试失败，请重试')
  } finally {
    probing.value = false
  }
}

async function saveForm() {
  if (saving.value) return
  if (!validateForm()) return
  saving.value = true
  formError.value = ''
  try {
    await api.saveMcpServer(formBody({ overwrite: form.mode === 'edit' }))
    await reload({ quiet: true })
    const verb = form.mode === 'add' ? '已创建' : '已保存'
    savedNotice.value = `${form.name || form.id} ${verb}`
    setFlash(`${form.name || form.id} ${verb}`)
    form.open = false
  } catch (err) {
    if (err && err.status === 401) return
    formError.value = errorText(err, '保存失败，请重试')
  } finally {
    saving.value = false
  }
}

/* ------------------------------------------------------------ assistant -- */

const assistant = reactive({
  open: false,
  description: '',
  document: '',
  showDocument: false,
  showRaw: false,
  running: false,
  error: '',
  notes: '',
  warnings: [],
  raw: '',
  model: '',
})
/** Fill the form from an AI draft, so the operator reviews before saving. */
function applyDraft(draft) {
  openAdd()
  form.id = draft.id || ''
  form.name = draft.name || ''
  form.transport = draft.transport || 'stdio'
  form.command = draft.command || ''
  form.args = (draft.args || []).join('\n')
  form.env = (draft.env || []).join('\n')
  form.url = draft.url || ''
  form.headers = (draft.headers || []).join('\n')
  form.enabled = draft.enabled !== false
}

async function runAssistant() {
  if (assistant.running) return
  const description = assistant.description.trim()
  if (!description && !assistant.document.trim()) {
    assistant.error = '请先描述你想接入的 MCP 服务，或粘贴一段文档'
    return
  }
  assistant.running = true
  assistant.error = ''
  assistant.warnings = []
  assistant.notes = ''
  assistant.raw = ''
  try {
    const result = await api.draftMcpServer({
      description,
      document: assistant.showDocument ? assistant.document : '',
    })
    assistant.notes = result.notes || ''
    assistant.warnings = result.warnings || []
    assistant.raw = result.raw || ''
    if (result.model) assistant.model = `${result.model.provider} / ${result.model.model}`
    applyDraft(result.draft || {})
  } catch (err) {
    if (err && err.status === 401) return
    assistant.error = errorText(err, '生成失败，请重试')
  } finally {
    assistant.running = false
  }
}
</script>

<template>
  <div class="stack">
    <!-- 1. the AI assistant: description in, draft out (never saved here) -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          配置助手
          <span class="card-sub">
            用大模型把一句话或一段文档变成 MCP 配置 · 只生成草稿，保存由你确认
          </span>
        </div>
        <button
          type="button"
          class="btn ghost sm"
          @click="assistant.open = !assistant.open"
        >
          <Icon :name="assistant.open ? 'chevron-down' : 'chevron-right'" :size="14" />
          {{ assistant.open ? '收起' : '展开' }}
        </button>
      </div>

      <template v-if="assistant.open">
        <label class="field">
          <span class="field-label">你想要接入什么？</span>
          <textarea
            v-model="assistant.description"
            class="input"
            rows="2"
            placeholder="例如：帮我接入 GitHub 的 MCP，可以读仓库、提 issue"
            aria-label="MCP 需求描述"
          />
          <span class="field-hint">
            说清楚要接入哪个服务、做什么用；越具体越准。
          </span>
        </label>

        <div class="row row-gap mcp-assistant-row">
          <button
            type="button"
            class="btn sm ghost"
            @click="assistant.showDocument = !assistant.showDocument"
          >
            <Icon :name="assistant.showDocument ? 'chevron-down' : 'chevron-right'" :size="13" />
            {{ assistant.showDocument ? '不粘贴文档' : '粘贴文档 / README（可选，更准）' }}
          </button>
        </div>

        <label v-if="assistant.showDocument" class="field">
          <span class="field-label">文档内容</span>
          <textarea
            v-model="assistant.document"
            class="input mono"
            rows="6"
            placeholder="粘贴安装说明、README 片段或配置示例；里面有真实命令时结果最准。"
            aria-label="MCP 文档内容"
          />
        </label>

        <div class="row row-gap mcp-assistant-row">
          <button
            type="button"
            class="btn primary sm"
            :disabled="assistant.running"
            @click="runAssistant"
          >
            <Icon :name="assistant.running ? 'refresh' : 'sparkles'" :size="14" />
            {{ assistant.running ? '生成中…（用的是默认对话模型，可能要十几秒）' : '用模型生成配置' }}
          </button>
          <span v-if="assistant.model" class="dimmer nowrap">模型：{{ assistant.model }}</span>
        </div>

        <div v-if="assistant.error" class="banner error" role="alert">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">{{ assistant.error }}</span>
        </div>

        <template v-else-if="assistant.raw || assistant.warnings.length || assistant.notes">
          <div class="mcp-assistant-out" role="status">
            <div class="row row-gap">
              <span class="tag ok">
                <Icon name="circle-check" :size="12" />
                已生成，并填入下面的表单
              </span>
              <span class="dimmer">确认无误后再点「保存并连接」；草稿没有写入任何配置。</span>
            </div>
            <p v-if="assistant.notes" class="mcp-assistant-note">{{ assistant.notes }}</p>
            <ul v-if="assistant.warnings.length" class="mcp-warn-list">
              <li v-for="(warn, i) in assistant.warnings" :key="i">
                <Icon name="circle-alert" :size="12" />
                {{ warn }}
              </li>
            </ul>
          </div>
          <div class="row row-gap mcp-assistant-row">
            <button type="button" class="btn sm ghost" @click="assistant.showRaw = !assistant.showRaw">
              <Icon :name="assistant.showRaw ? 'chevron-down' : 'chevron-right'" :size="13" />
              {{ assistant.showRaw ? '隐藏模型原始回复' : '查看模型原始回复' }}
            </button>
          </div>
          <pre v-if="assistant.showRaw" class="mcp-raw">{{ assistant.raw }}</pre>
        </template>
      </template>

      <p v-else class="muted-note">
        描述一次即可生成一份可编辑的配置草稿：模型负责给出命令、参数与环境变量，
        密钥一律留空由你填写。展开后可以粘贴 README 让结果更准。
      </p>
    </div>

    <!-- 2. the server list + 3. the form -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          MCP 服务器
          <span class="card-sub">
            {{ formatCount(servers.length) }} 个 · 已连接 {{ formatCount(connectedCount) }} ·
            提供 {{ formatCount(toolCount) }} 个工具
          </span>
          <span v-if="flash" class="chip ok">{{ flash }}</span>
        </div>
        <div class="row">
          <button
            type="button"
            class="btn sm"
            :disabled="Boolean(reloading) || !runtimeAvailable"
            title="断开并重新连接所有已启用的服务器"
            @click="reconnectAll"
          >
            <Icon name="refresh" :size="15" />
            {{ reloading ? '重连中…' : '重连' }}
          </button>
          <button type="button" class="btn ghost sm" :disabled="loading" @click="reload()">
            <Icon name="refresh" :size="15" />
            重新读取
          </button>
          <button type="button" class="btn primary sm" @click="openAdd">
            <Icon name="plus" :size="15" />
            新建服务器
          </button>
        </div>
      </div>

      <!-- A deployment with chat.enable = false has no tool registry, so nothing
           is ever connected. Say so instead of showing a permanent 未连接. -->
      <div v-if="!runtimeAvailable" class="banner info">
        <Icon name="circle-alert" :size="16" />
        <span class="banner-text">
          当前服务的对话功能未启用（<code class="md-code">chat.enable = false</code>），
          因此没有可注册工具的运行时：服务器可以在这里保存和测试，但不会被连接。
          启用对话后重启服务即可生效。
        </span>
      </div>

      <AsyncBlock :state="status" :error="error" :skeleton-rows="3" @retry="reload">
        <div v-if="!servers.length" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">还没有配置任何 MCP 服务器</div>
          <div class="empty-hint">
            用上面的配置助手描述一句「我想接入什么」生成一份草稿，或点「新建服务器」手工填写。
            保存后会立即连接，模型在下一轮对话里就能调用它提供的工具。
          </div>
        </div>

        <div v-else class="mcp-list">
          <div
            v-for="server in servers"
            :key="server.id"
            class="mcp-row"
            :class="{ off: !isOn(server) }"
          >
            <div class="mcp-main">
              <div class="mcp-name">
                <span>{{ server.name || server.id }}</span>
                <span v-if="server.name && server.name !== server.id" class="tag mono">
                  {{ server.id }}
                </span>
                <span class="tag" :class="isConfig(server) ? 'blue' : 'purple'">
                  {{ isConfig(server) ? '配置' : '网页' }}
                </span>
                <span class="tag mono">
                  {{ TRANSPORT_SHORT[server.transport] || server.transport || 'stdio' }}
                </span>
                <span class="tag" :class="statusClass(server)">{{ statusLabel(server) }}</span>
                <span
                  v-if="isConnected(server) && server.runtime.tools.length"
                  class="tag"
                  :title="server.runtime.tools.join('、')"
                >
                  {{ formatCount(server.runtime.tools.length) }} 个工具
                </span>
              </div>
              <div class="mcp-endpoint mono">{{ endpoint(server) }}</div>
              <div v-if="server.env && server.env.length" class="mcp-env">
                <span class="dimmer">环境变量：</span>
                <span v-for="item in server.env" :key="item" class="tag mono">{{ item }}</span>
              </div>
              <div v-if="server.headers && server.headers.length" class="mcp-env">
                <span class="dimmer">请求头：</span>
                <span v-for="item in server.headers" :key="item" class="tag mono">{{ item }}</span>
              </div>
              <div v-if="server.runtime && server.runtime.tools && server.runtime.tools.length" class="mcp-tools">
                <span class="dimmer">工具：</span>
                <span v-for="name in server.runtime.tools" :key="name" class="chip mono">{{ name }}</span>
              </div>

              <div v-if="isConfig(server)" class="prov-note cell-note">
                <Icon name="circle-alert" :size="13" />
                来自配置文件（<code class="md-code">mcp.servers</code>），每次启动都会按配置文件重新同步，
                所以这里只能切换启停；请改配置文件后重启服务。
              </div>

              <div v-if="server.last_error" class="cell-err mcp-full">
                最近一次连接失败：{{ server.last_error }}
              </div>

              <div
                v-if="testResult[server.id]"
                class="prov-test"
                :class="testResult[server.id].ok ? 'ok' : 'bad'"
                role="status"
              >
                <Icon :name="testResult[server.id].ok ? 'circle-check' : 'circle-alert'" :size="14" />
                <template v-if="testResult[server.id].ok">
                  连接成功，发现 {{ formatCount(testResult[server.id].count) }} 个工具<template
                    v-if="testResult[server.id].tools && testResult[server.id].tools.length"
                  >
                    ：{{ testResult[server.id].tools.map((t) => t.name).join('、') }}</template
                  >
                </template>
                <template v-else>
                  连接失败：{{ testResult[server.id].error || '请检查命令或地址' }}
                </template>
              </div>

              <div v-if="rowError[server.id]" class="cell-err mcp-full">{{ rowError[server.id] }}</div>
            </div>

            <div class="prov-actions">
              <button
                type="button"
                class="switch"
                :class="{ on: isOn(server) }"
                role="switch"
                :aria-checked="isOn(server)"
                :aria-label="`${isOn(server) ? '停用' : '启用'} ${server.name || server.id}`"
                :disabled="Boolean(busy[server.id])"
                @click="toggleServer(server)"
              >
                <span class="knob" />
              </button>

              <button
                type="button"
                class="btn sm"
                :disabled="Boolean(busy[server.id])"
                title="连接并列出该服务器提供的工具，不影响正在运行的连接"
                @click="testServer(server)"
              >
                <Icon :name="busy[server.id] === 'test' ? 'refresh' : 'plug'" :size="14" />
                测试连接
              </button>

              <template v-if="!isConfig(server)">
                <button type="button" class="btn sm" @click="openEdit(server)">
                  <Icon name="pencil" :size="14" />
                  编辑
                </button>
                <template v-if="confirmDelete === server.id">
                  <span class="dimmer nowrap">确认删除？</span>
                  <button type="button" class="btn sm danger" @click="removeServer(server)">删除</button>
                  <button type="button" class="btn sm ghost" @click="confirmDelete = ''">取消</button>
                </template>
                <button
                  v-else
                  type="button"
                  class="btn sm ghost danger-text"
                  @click="confirmDelete = server.id"
                >
                  <Icon name="trash" :size="14" />
                  删除
                </button>
              </template>
            </div>
          </div>
        </div>
      </AsyncBlock>

      <!-- create / edit form -->
      <form v-if="form.open" class="prov-form" @submit.prevent="saveForm">
        <div class="prov-form-head">
          {{ form.mode === 'add' ? '新建 MCP 服务器' : `编辑 ${form.id}` }}
          <span class="card-sub">
            {{
              form.mode === 'add'
                ? 'id 是数据库主键，保存后用它标识别这个服务器'
                : '保存后会断开旧连接并重新连接'
            }}
          </span>
          <span class="spacer" />
          <span v-if="saving" class="dimmer nowrap">保存中…</span>
          <button type="button" class="btn sm ghost" @click="closeForm">取消</button>
        </div>

        <div class="form-grid">
          <label class="field">
            <span class="field-label">id</span>
            <input
              v-model="form.id"
              class="input mono"
              type="text"
              placeholder="github"
              :disabled="form.mode === 'edit'"
              autocomplete="off"
              aria-label="MCP 服务器 id"
            />
            <span v-if="formErrors.id" class="field-error">{{ formErrors.id }}</span>
            <span v-else class="field-hint">小写字母、数字、. - _；创建后不可修改</span>
          </label>

          <label class="field">
            <span class="field-label">名称</span>
            <input
              v-model="form.name"
              class="input"
              type="text"
              placeholder="显示名称，留空则用 id"
              autocomplete="off"
              aria-label="MCP 服务器名称"
            />
          </label>

          <div class="field field-wide">
            <span class="field-label">传输方式</span>
            <div class="seg" role="group" aria-label="传输方式">
              <button
                v-for="item in transports"
                :key="item"
                type="button"
                :class="{ active: form.transport === item }"
                :aria-pressed="form.transport === item"
                @click="form.transport = item"
              >
                {{ TRANSPORT_LABEL[item] || item }}
              </button>
            </div>
          </div>

          <template v-if="isStdio">
            <label class="field">
              <span class="field-label">command</span>
              <input
                v-model="form.command"
                class="input mono"
                type="text"
                placeholder="npx"
                autocomplete="off"
                aria-label="MCP 服务器命令"
              />
              <span v-if="formErrors.command" class="field-error">{{ formErrors.command }}</span>
              <span v-else class="field-hint">可执行文件，例如 npx / uvx / node / python3</span>
            </label>

            <label class="field">
              <span class="field-label">args（每行一个参数）</span>
              <textarea
                v-model="form.args"
                class="input mono"
                rows="4"
                placeholder="-y&#10;@modelcontextprotocol/server-github"
                aria-label="MCP 服务器参数"
              />
              <span class="field-hint">按顺序传给命令；包含空格的参数整行填写即可</span>
            </label>

            <label class="field field-wide">
              <span class="field-label">env（每行一个 KEY=value）</span>
              <textarea
                v-model="form.env"
                class="input mono"
                rows="3"
                placeholder="GITHUB_PERSONAL_ACCESS_TOKEN=ghp_xxx"
                aria-label="MCP 服务器环境变量"
              />
              <span class="field-hint">
                会合并到进程环境里；密钥只存在本机数据库，界面按你填写的内容原样展示。
              </span>
            </label>
          </template>

          <template v-else>
            <label class="field field-wide">
              <span class="field-label">url</span>
              <input
                v-model="form.url"
                class="input mono"
                type="text"
                placeholder="https://example.com/sse"
                autocomplete="off"
                aria-label="MCP 服务器地址"
              />
              <span v-if="formErrors.url" class="field-error">{{ formErrors.url }}</span>
              <span v-else class="field-hint">
                {{ form.transport === 'sse' ? 'SSE 端点地址' : 'streamable HTTP 端点地址' }}
              </span>
            </label>

            <label class="field field-wide">
              <span class="field-label">headers（每行一个 Name: value）</span>
              <textarea
                v-model="form.headers"
                class="input mono"
                rows="3"
                placeholder="Authorization: Bearer xxx"
                aria-label="MCP 服务器请求头"
              />
              <span class="field-hint">token 放在这里，例如 <code class="md-code">Authorization: Bearer …</code></span>
            </label>
          </template>

          <div class="field field-inline">
            <span class="field-label">启用</span>
            <div class="row">
              <button
                type="button"
                class="switch"
                :class="{ on: form.enabled }"
                role="switch"
                :aria-checked="form.enabled"
                aria-label="启用该 MCP 服务器"
                @click="form.enabled = !form.enabled"
              >
                <span class="knob" />
              </button>
              <span class="dimmer">{{ form.enabled ? '保存后立即连接' : '只保存定义，不连接' }}</span>
            </div>
          </div>
        </div>

        <div v-if="formError" class="banner error" role="alert">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">{{ formError }}</span>
        </div>

        <div
          v-if="probeResult"
          class="prov-test"
          :class="probeResult.ok ? 'ok' : 'bad'"
          role="status"
        >
          <Icon :name="probeResult.ok ? 'circle-check' : 'circle-alert'" :size="14" />
          <template v-if="probeResult.ok">
            连接成功，发现 {{ formatCount(probeResult.count) }} 个工具<template
              v-if="probeResult.tools.length"
            >
              ：{{ probeResult.tools.map((t) => t.name).join('、') }}</template
            >
          </template>
          <template v-else>连接失败：{{ probeResult.error || '请检查命令或地址' }}</template>
        </div>

        <div class="row prov-form-foot">
          <button type="button" class="btn sm" :disabled="probing || saving" @click="probeForm">
            <Icon :name="probing ? 'refresh' : 'plug'" :size="14" />
            {{ probing ? '测试中…' : '测试连接' }}
          </button>
          <button type="submit" class="btn primary sm" :disabled="saving || probing">
            <Icon name="check" :size="14" />
            {{ saving ? '保存中…' : form.mode === 'add' ? '保存并连接' : '保存并重连' }}
          </button>
          <span v-if="savedNotice" class="chip ok">{{ savedNotice }}</span>
          <span class="spacer" />
          <span class="dimmer">
            测试连接只拨号一次、不保存；保存后工具会立刻出现在「服务与工具」的工具列表里。
          </span>
        </div>
      </form>
    </div>
  </div>
</template>
