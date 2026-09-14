<script setup>
// 技能 — the skill files the agent can load, and the assistant that writes one.
//
// A skill is one markdown file (YAML frontmatter + instructions) in the
// configured skills directory. Two things follow from that, and both shape this
// panel:
//
//   1. The list is the toggle surface (POST /api/skills/:name), and the editor
//      reads and writes the file itself (GET / PUT / DELETE) — so what the
//      operator edits here is literally what the agent loads.
//   2. Enabling a skill changes the conversation's system prompt: the prompt
//      lists the enabled skills by name and description, and the model pulls in
//      an individual skill's body through the `skill` tool. That is why the
//      panel says so — "启用" is not a no-op, it changes what the model is told
//      on the next message.
//
// The 写作助手 drafts a skill from a description and does not save it: the
// draft fills the editor, and 保存 is a separate, deliberate PUT.
import { computed, reactive, ref, watch } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import { api } from '../api.js'
import { formatBytes, formatCount } from '../format.js'
import { useResource } from '../useResource.js'

const { data, status, error, reload } = useResource(() => api.skills(), { watchRange: false })

/** Local working copy: a toggle flips immediately, then is confirmed/reverted. */
const skills = ref([])
const pending = ref({})
const rowError = ref({})

watch(data, (value) => {
  skills.value = ((value && value.skills) || []).map((skill) => ({
    name: skill.name || '（未命名）',
    description: skill.description || '',
    enabled: Boolean(skill.enabled),
    tools: skill.tools || [],
    file: skill.file || '',
    bytes: skill.bytes || 0,
  }))
  rowError.value = {}
})

const enabledCount = computed(() => skills.value.filter((s) => s.enabled).length)
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

async function toggle(skill) {
  if (pending.value[skill.name]) return
  const next = !skill.enabled
  pending.value = { ...pending.value, [skill.name]: true }
  rowError.value = { ...rowError.value, [skill.name]: '' }
  const previous = skill.enabled
  skill.enabled = next
  try {
    await api.setSkillEnabled(skill.name, next)
  } catch (err) {
    skill.enabled = previous
    if (err && err.status === 401) return
    rowError.value = {
      ...rowError.value,
      [skill.name]: errorText(err, '切换失败，请重试'),
    }
  } finally {
    const copy = { ...pending.value }
    delete copy[skill.name]
    pending.value = copy
  }
}

/* --------------------------------------------------------------- editor -- */

const editor = reactive({
  open: false,
  mode: 'edit', // 'edit' | 'create'
  name: '',
  description: '',
  tools: '',
  body: '',
  error: '',
  notice: '',
  loading: false,
  saving: false,
})

function lines(text) {
  return String(text || '')
    .split(/[\n,，]/)
    .map((line) => line.trim())
    .filter((line) => line !== '')
}

function closeEditor() {
  editor.open = false
  editor.error = ''
  editor.notice = ''
}

async function openSkill(skill) {
  editor.open = true
  editor.mode = 'edit'
  editor.name = skill.name
  editor.error = ''
  editor.notice = ''
  editor.loading = true
  try {
    const result = await api.skillDetail(skill.name)
    const detail = result.skill || {}
    editor.description = detail.description || ''
    editor.tools = (detail.tools || []).join('\n')
    editor.body = detail.body || ''
  } catch (err) {
    if (err && err.status === 401) return
    editor.error = errorText(err, '读取失败，请重试')
  } finally {
    editor.loading = false
  }
}

function openCreate() {
  editor.open = true
  editor.mode = 'create'
  editor.name = ''
  editor.description = ''
  editor.tools = ''
  editor.body = ''
  editor.error = ''
  editor.notice = ''
}

async function saveSkill() {
  if (editor.saving) return
  const name = editor.name.trim()
  if (!name) {
    editor.error = '必须填写技能名'
    return
  }
  if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(name)) {
    editor.error = '技能名只能用小写字母、数字、点、横线、下划线，且以字母或数字开头'
    return
  }
  if (!editor.body.trim()) {
    editor.error = '正文不能为空：没有指令的技能没有任何作用'
    return
  }
  editor.saving = true
  editor.error = ''
  try {
    const result = await api.saveSkill(name, {
      description: editor.description,
      tools: lines(editor.tools),
      body: editor.body,
    })
    if (result.dropped_tools && result.dropped_tools.length) {
      // The server refuses to store a tool name this build does not have, since
      // it would make the skill's allow-list unusable.
      editor.notice = `已保存；以下工具不存在，已从技能里移除：${result.dropped_tools.join('、')}`
    } else {
      editor.notice = '已保存'
    }
    editor.mode = 'edit'
    await reload({ quiet: true })
    setFlash(`${name} 已保存`)
  } catch (err) {
    if (err && err.status === 401) return
    editor.error = errorText(err, '保存失败，请重试')
  } finally {
    editor.saving = false
  }
}

const confirmDelete = ref('')

async function removeSkill(skill) {
  if (pending.value[skill.name]) return
  pending.value = { ...pending.value, [skill.name]: true }
  rowError.value = { ...rowError.value, [skill.name]: '' }
  try {
    await api.deleteSkill(skill.name)
    confirmDelete.value = ''
    if (editor.open && editor.name === skill.name) closeEditor()
    await reload({ quiet: true })
    setFlash(`${skill.name} 已删除`)
  } catch (err) {
    if (err && err.status === 401) return
    rowError.value = {
      ...rowError.value,
      [skill.name]: errorText(err, '删除失败，请重试'),
    }
  } finally {
    const copy = { ...pending.value }
    delete copy[skill.name]
    pending.value = copy
  }
}

/* ------------------------------------------------------------ assistant -- */

const assistant = reactive({
  open: false,
  name: '',
  description: '',
  running: false,
  error: '',
  notes: '',
  warnings: [],
  raw: '',
  showRaw: false,
  model: '',
})

async function runAssistant() {
  if (assistant.running) return
  const description = assistant.description.trim()
  if (!description) {
    assistant.error = '请先描述你想要的技能'
    return
  }
  assistant.running = true
  assistant.error = ''
  assistant.warnings = []
  assistant.notes = ''
  assistant.raw = ''
  try {
    const result = await api.draftSkill({ description, name: assistant.name.trim() })
    const draft = result.draft || {}
    assistant.notes = result.notes || ''
    assistant.warnings = result.warnings || []
    assistant.raw = result.raw || ''
    if (result.model) assistant.model = `${result.model.provider} / ${result.model.model}`

    // Fill the editor so the draft is reviewed (and editable) before it is
    // written to a file.
    editor.open = true
    editor.mode = 'create'
    editor.name = draft.name || ''
    editor.description = draft.description || ''
    editor.tools = (draft.tools || []).join('\n')
    editor.body = draft.body || ''
    editor.error = ''
    editor.notice = '这是模型生成的草稿：确认无误后点「保存技能」写入文件。'
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
    <!-- 1. the AI writer: description in, draft editor out (never saved here) -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          技能写作助手
          <span class="card-sub">
            用大模型把一句话变成一份 SKILL.md 草稿 · 只填进编辑器，保存由你确认
          </span>
        </div>
        <button type="button" class="btn ghost sm" @click="assistant.open = !assistant.open">
          <Icon :name="assistant.open ? 'chevron-down' : 'chevron-right'" :size="14" />
          {{ assistant.open ? '收起' : '展开' }}
        </button>
      </div>

      <template v-if="assistant.open">
        <label class="field">
          <span class="field-label">你想要什么技能？</span>
          <textarea
            v-model="assistant.description"
            class="input"
            rows="2"
            placeholder="例如：每次我让你整理发布说明时，先读 CHANGELOG 和最近的 commit，再按固定格式输出"
            aria-label="技能需求描述"
          />
          <span class="field-hint">
            说清楚什么时候该用这个技能、希望它按什么步骤做，模型会据此写出描述、步骤与用到的工具。
          </span>
        </label>

        <label class="field">
          <span class="field-label">技能名（可选）</span>
          <input
            v-model="assistant.name"
            class="input mono"
            type="text"
            placeholder="留空则由模型命名，例如 release-notes"
            autocomplete="off"
            aria-label="技能名"
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
            {{ assistant.running ? '生成中…（用的是默认对话模型，可能要十几秒）' : '用模型生成技能' }}
          </button>
          <span v-if="assistant.model" class="dimmer nowrap">模型：{{ assistant.model }}</span>
        </div>

        <div v-if="assistant.error" class="banner error" role="alert">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">{{ assistant.error }}</span>
        </div>

        <div v-else-if="assistant.raw" class="mcp-assistant-out" role="status">
          <div class="row row-gap">
            <span class="tag ok">
              <Icon name="circle-check" :size="12" />
              已生成，并填入下面的编辑器
            </span>
            <span class="dimmer">草稿没有写入任何文件；保存前可以任意修改。</span>
          </div>
          <p v-if="assistant.notes" class="mcp-assistant-note">{{ assistant.notes }}</p>
          <ul v-if="assistant.warnings.length" class="mcp-warn-list">
            <li v-for="(warn, i) in assistant.warnings" :key="i">
              <Icon name="circle-alert" :size="12" />
              {{ warn }}
            </li>
          </ul>
          <div class="row row-gap mcp-assistant-row">
            <button type="button" class="btn sm ghost" @click="assistant.showRaw = !assistant.showRaw">
              <Icon :name="assistant.showRaw ? 'chevron-down' : 'chevron-right'" :size="13" />
              {{ assistant.showRaw ? '隐藏模型原始回复' : '查看模型原始回复' }}
            </button>
          </div>
          <pre v-if="assistant.showRaw" class="mcp-raw">{{ assistant.raw }}</pre>
        </div>
      </template>

      <p v-else class="muted-note">
        技能是一份可复用的操作指令。启用的技能会出现在对话的系统提示里（只列名字和描述），
        模型判断任务匹配时会调用 <code class="md-code">skill</code> 工具读取它的完整指令再执行。
      </p>
    </div>

    <!-- 2. the skill list -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          技能
          <span class="card-sub">
            已启用 {{ formatCount(enabledCount) }} / {{ formatCount(skills.length) }} ·
            切换会立即生效并写入配置
          </span>
          <span v-if="flash" class="chip ok">{{ flash }}</span>
        </div>
        <div class="row">
          <button type="button" class="btn ghost sm" :disabled="status === 'loading'" @click="reload()">
            <Icon name="refresh" :size="15" />
            刷新
          </button>
          <button type="button" class="btn primary sm" @click="openCreate">
            <Icon name="plus" :size="15" />
            新建技能
          </button>
        </div>
      </div>

      <AsyncBlock :state="status" :error="error" :skeleton-rows="5" @retry="reload">
        <div v-if="!skills.length" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">暂无技能</div>
          <div class="empty-hint">
            技能来自服务端配置的 <code class="md-code">skills.dir</code> 目录里的 <code class="md-code">*.md</code> 文件。
            可以用上面的写作助手生成一份，或点「新建技能」手写。
          </div>
        </div>

        <div v-else class="skill-list">
          <div v-for="skill in skills" :key="skill.name" class="skill-row">
            <div class="skill-main">
              <div class="skill-name">
                <span class="mono">{{ skill.name }}</span>
                <span class="tag" :class="skill.enabled ? 'ok' : ''">
                  {{ skill.enabled ? '已启用' : '已停用' }}
                </span>
                <span v-if="skill.file" class="tag mono" :title="`文件：${skill.file}`">
                  {{ skill.file }}<template v-if="skill.bytes"> · {{ formatBytes(skill.bytes) }}</template>
                </span>
                <span v-else class="tag warn" title="禁用记录还在，但技能文件已经不存在">
                  文件已删除
                </span>
                <span v-for="name in skill.tools" :key="name" class="tag mono" :title="`该技能声明的工具：${name}`">
                  {{ name }}
                </span>
                <span v-if="pending[skill.name]" class="dimmer">保存中…</span>
              </div>
              <div v-if="skill.description" class="skill-desc">{{ skill.description }}</div>
              <div v-if="rowError[skill.name]" class="cell-err cell-note">
                {{ rowError[skill.name] }}
              </div>
            </div>

            <div class="row skill-actions">
              <button
                type="button"
                class="btn sm"
                :disabled="!skill.file"
                title="查看或修改这个技能的完整内容"
                @click="openSkill(skill)"
              >
                <Icon name="pencil" :size="14" />
                查看 / 编辑
              </button>

              <template v-if="confirmDelete === skill.name">
                <span class="dimmer nowrap">确认删除文件？</span>
                <button type="button" class="btn sm danger" @click="removeSkill(skill)">删除</button>
                <button type="button" class="btn sm ghost" @click="confirmDelete = ''">取消</button>
              </template>
              <button
                v-else
                type="button"
                class="btn sm ghost danger-text"
                :disabled="Boolean(pending[skill.name])"
                title="删除该技能的 markdown 文件"
                @click="confirmDelete = skill.name"
              >
                <Icon name="trash" :size="14" />
                删除
              </button>

              <button
                type="button"
                class="switch"
                :class="{ on: skill.enabled }"
                role="switch"
                :aria-checked="skill.enabled"
                :aria-label="`${skill.enabled ? '停用' : '启用'}技能 ${skill.name}`"
                :disabled="Boolean(pending[skill.name])"
                @click="toggle(skill)"
              >
                <span class="knob" />
              </button>
            </div>
          </div>
        </div>

        <p class="muted-note card-foot">
          启用的技能会被写进对话的系统提示（只列名字与描述），模型按需用 <code class="md-code">skill</code>
          工具读取正文，因此停用某个技能下一轮对话就会立刻生效。
        </p>
      </AsyncBlock>

      <!-- the editor: the same markdown the agent loads -->
      <form v-if="editor.open" class="prov-form" @submit.prevent="saveSkill">
        <div class="prov-form-head">
          {{ editor.mode === 'create' ? '新建技能' : `编辑 ${editor.name}` }}
          <span class="card-sub">
            正文会作为技能指令，frontmatter（name / description / tools）由这里生成
          </span>
          <span class="spacer" />
          <span v-if="editor.saving" class="dimmer nowrap">保存中…</span>
          <button type="button" class="btn sm ghost" @click="closeEditor">取消</button>
        </div>

        <div v-if="editor.loading" class="stack" aria-busy="true">
          <span class="skeleton" style="height: 13px; width: 40%" />
          <span class="skeleton" style="height: 13px; width: 70%" />
          <span class="muted-note">正在读取技能文件…</span>
        </div>

        <template v-else>
          <div class="form-grid">
            <label class="field">
              <span class="field-label">技能名</span>
              <input
                v-model="editor.name"
                class="input mono"
                type="text"
                placeholder="release-notes"
                :disabled="editor.mode === 'edit'"
                autocomplete="off"
                aria-label="技能名"
              />
              <span class="field-hint">
                {{ editor.mode === 'edit' ? '技能名即文件名，保存不会改名' : '会写成 <技能名>.md；只能用小写字母、数字、. - _' }}
              </span>
            </label>

            <label class="field">
              <span class="field-label">描述（模型据此判断何时使用）</span>
              <input
                v-model="editor.description"
                class="input"
                type="text"
                placeholder="当用户要求整理发布说明时使用"
                autocomplete="off"
                aria-label="技能描述"
              />
            </label>

            <label class="field field-wide">
              <span class="field-label">tools（每行一个工具名，可留空）</span>
              <textarea
                v-model="editor.tools"
                class="input mono"
                rows="2"
                placeholder="read_file&#10;grep"
                aria-label="技能声明的工具"
              />
              <span class="field-hint">
                该技能期望使用的工具（写进 frontmatter 的 tools）。不存在的工具会被服务端拒绝并提示。
                留空表示不限制。
              </span>
            </label>

            <label class="field field-wide">
              <span class="field-label">正文（markdown 指令）</span>
              <textarea
                v-model="editor.body"
                class="input mono"
                rows="14"
                placeholder="# 步骤&#10;&#10;1. 用 read_file 读取 …"
                aria-label="技能正文"
              />
              <span class="field-hint">写清适用场景、逐步操作、用到哪个工具、输出格式要求。</span>
            </label>
          </div>

          <div v-if="editor.error" class="banner error" role="alert">
            <Icon name="circle-alert" :size="16" />
            <span class="banner-text">{{ editor.error }}</span>
          </div>

          <div class="row prov-form-foot">
            <button type="submit" class="btn primary sm" :disabled="editor.saving">
              <Icon name="check" :size="14" />
              {{ editor.saving ? '保存中…' : '保存技能' }}
            </button>
            <span v-if="editor.notice" class="chip ok">{{ editor.notice }}</span>
            <span class="spacer" />
            <span class="dimmer">保存会写入技能目录中的 md 文件，下一轮对话即可被模型加载。</span>
          </div>
        </template>
      </form>
    </div>
  </div>
</template>
