<script setup>
// OpenViking — the context database the agent remembers through.
//
// Everything here is read-mostly except three actions, because the settings
// themselves live in the server config (openviking.*): base_url, account, user,
// subtree and the memory/document switches are reported, not edited.
//
// The one idea worth stating: a disabled integration is a *state*, not a
// failure. Every /api/openviking route answers 200 with `enable:false` plus a
// message when it is off, so this panel renders that message calmly (banner
// info, no action buttons) instead of as an error the operator cannot clear —
// the fix is a config file and a restart, which the message spells out.
//
// A sync or a save that cannot run is reported the same way on purpose: 200 with
// `ok:false` and a reason ("no workspace configured", a broken connection). That
// reason belongs next to the button that asked for it, which is where it lands.
import { computed, reactive, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import StatCard from './StatCard.vue'
import { api } from '../api.js'
import { formatAbsolute, formatBytes, formatCount, formatRelative } from '../format.js'
import { useResource } from '../useResource.js'

/** How many rows of the synced-document list are rendered at most. */
const DOC_LIMIT = 50

const { data, status, error, loading, reload } = useResource(() => api.openVikingStatus(), {
  watchRange: false,
})

// The document list is its own resource so its loading and error states stay
// separate from the counters': a list that fails to load must not blank out the
// connection card above it.
const {
  data: docsData,
  status: docsStatus,
  error: docsError,
  loading: docsLoading,
  reload: reloadDocs,
} = useResource(() => api.openVikingDocuments(), { watchRange: false })

const info = computed(() => data.value || {})
const enabled = computed(() => info.value.enable === true)
const memory = computed(() => info.value.memory || {})
const stats = computed(() => memory.value.stats || {})
const docsInfo = computed(() => info.value.documents || {})
const mcp = computed(() => info.value.mcp || {})

/**
 * A timestamp the server never set serializes as Go's zero time (year 1) —
 * `omitempty` does not drop a struct — so it is treated as absence, like every
 * other missing value in this console, rather than rendered as "2026 年前".
 */
function stamp(iso) {
  if (!iso) return ''
  const t = Date.parse(iso)
  if (!Number.isFinite(t) || new Date(t).getFullYear() < 2000) return ''
  return iso
}

/** "已连接 · version 1.2 · auth token" — the parts that exist, in that order. */
const connectionLabel = computed(() => {
  const parts = ['已连接']
  if (info.value.version) parts.push(`version ${info.value.version}`)
  if (info.value.auth_mode) parts.push(`auth ${info.value.auth_mode}`)
  return parts.join(' · ')
})

const settledSync = computed(() => docsInfo.value.last_sync || null)

/** The last report's error lines, shown behind a toggle because there can be many. */
const showSyncErrors = ref(false)

/* --------------------------------------------------------------- actions -- */

/** '' | 'sync' | 'full' | 'flush' | 'save' — at most one request at a time. */
const busy = ref('')
/** `{ ok, report?, error? }` of the last sync, rendered inline. */
const syncResult = ref(null)
/** `{ ok, text }` of the last flush. */
const flushResult = ref(null)
/** `{ ok, uri?, error? }` of the last save. */
const saveResult = ref(null)

const form = reactive({ title: '', content: '', tags: '' })
const canSave = computed(() => form.title.trim() !== '' && form.content.trim() !== '')

function errorText(err, fallback) {
  if (err && typeof err.message === 'string' && err.message !== '') return err.message
  return fallback
}

/** The reason a 200 `{ok:false}` carries; `message` is the disabled answer. */
function reasonOf(payload, fallback) {
  if (payload && typeof payload.error === 'string' && payload.error !== '') return payload.error
  if (payload && typeof payload.message === 'string' && payload.message !== '') return payload.message
  return fallback
}

/** "a, b，c" -> ['a','b','c'] — half-width and full-width commas both split. */
function parseTags(text) {
  const seen = new Set()
  const out = []
  for (const raw of String(text || '').split(/[,，]/)) {
    const tag = raw.trim()
    if (tag === '' || seen.has(tag)) continue
    seen.add(tag)
    out.push(tag)
  }
  return out
}

async function runSync(full) {
  if (busy.value) return
  busy.value = full ? 'full' : 'sync'
  syncResult.value = null
  try {
    const result = await api.syncOpenViking(full)
    syncResult.value = result && result.ok
      ? { ok: true, report: result.report || null }
      : { ok: false, report: null, error: reasonOf(result, '同步失败') }
    // A sync rewrites the state file the counters and the list are read from, so
    // both are stale the moment it returns.
    await Promise.all([reload({ quiet: true }), reloadDocs({ quiet: true })])
  } catch (err) {
    if (err && err.status === 401) return
    // 409 ("a sync is already running") arrives here as an ordinary message.
    syncResult.value = { ok: false, report: null, error: errorText(err, '同步失败，请重试') }
  } finally {
    busy.value = ''
  }
}

async function flushMemory() {
  if (busy.value) return
  busy.value = 'flush'
  flushResult.value = null
  try {
    await api.flushOpenViking()
    flushResult.value = { ok: true, text: '已提交缓冲区中的记忆，计数会在下一轮对话后更新' }
    await reload({ quiet: true })
  } catch (err) {
    if (err && err.status === 401) return
    flushResult.value = { ok: false, text: errorText(err, '刷新记忆失败，请重试') }
  } finally {
    busy.value = ''
  }
}

async function saveDocument() {
  if (busy.value || !canSave.value) return
  busy.value = 'save'
  saveResult.value = null
  try {
    const result = await api.saveOpenVikingDocument({
      title: form.title.trim(),
      content: form.content,
      tags: parseTags(form.tags),
      source: 'console',
    })
    if (result && result.ok) {
      saveResult.value = { ok: true, uri: result.uri || '' }
      // The fields are cleared only on success: a rejected save must keep what
      // was typed so it can be corrected and retried.
      form.title = ''
      form.content = ''
      form.tags = ''
      await Promise.all([reloadDocs({ quiet: true }), reload({ quiet: true })])
    } else {
      saveResult.value = { ok: false, error: reasonOf(result, '保存失败') }
    }
  } catch (err) {
    if (err && err.status === 401) return
    saveResult.value = { ok: false, error: errorText(err, '保存失败，请重试') }
  } finally {
    busy.value = ''
  }
}

/* ------------------------------------------------------------------- list -- */

const documents = computed(() => (docsData.value && docsData.value.documents) || [])

/**
 * Newest first, capped. The server already sorts by synced_at, but the cap is
 * what decides which rows are dropped, so the ordering it depends on is done
 * here rather than assumed.
 */
const visibleDocuments = computed(() => {
  const sorted = [...documents.value].sort((a, b) => {
    const at = Date.parse(a && a.synced_at) || 0
    const bt = Date.parse(b && b.synced_at) || 0
    if (at !== bt) return bt - at
    return String((a && a.path) || '').localeCompare(String((b && b.path) || ''))
  })
  return sorted.slice(0, DOC_LIMIT)
})

const KIND_LABEL = { workspace: '工作区文件', document: '手动保存' }

function kindLabel(kind) {
  return KIND_LABEL[kind] || kind || '—'
}
</script>

<template>
  <div class="stack">
    <!-- 1. what is configured, and whether it answers -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          OpenViking
          <span class="card-sub">
            上下文库 · 长期记忆与文档 · 连接设置在服务端配置（<code class="md-code">openviking.*</code>）
          </span>
        </div>
        <button type="button" class="btn ghost sm" :disabled="loading" @click="reload()">
          <Icon name="refresh" :size="15" />
          重新读取
        </button>
      </div>

      <AsyncBlock :state="status" :error="error" :skeleton-rows="3" @retry="reload">
        <!-- Not configured: a normal state, so it is styled as information and
             offers nothing but 重新读取 — the switch is in the config file. -->
        <div v-if="!enabled" class="banner info">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">
            {{ info.message || 'OpenViking 未启用（在配置里设置 openviking.enable: true 后重启服务）' }}
          </span>
        </div>

        <template v-else>
          <dl class="kv">
            <div class="kv-row">
              <dt>服务地址</dt>
              <dd class="mono">{{ info.base_url || '—' }}</dd>
            </div>
            <div class="kv-row">
              <dt>账号 / 用户</dt>
              <dd class="mono">{{ info.account || '—' }} / {{ info.user || '—' }}</dd>
            </div>
            <div class="kv-row">
              <dt>子树</dt>
              <dd class="mono">{{ info.subtree || '—' }}</dd>
            </div>
            <div class="kv-row">
              <dt>MCP 注册</dt>
              <dd>
                <span class="tag" :class="mcp.registered ? 'ok' : 'warn'">
                  {{ mcp.registered ? '已注册' : '未注册' }}
                </span>
                <span v-if="mcp.name" class="tag mono">{{ mcp.name }}</span>
              </dd>
            </div>
          </dl>

          <!-- A dead connection is a warning, not a failure of this page: the
               rest of the panel still shows what was configured. -->
          <div v-if="info.connected" class="row row-gap card-foot">
            <span class="tag ok">
              <Icon name="circle-check" :size="12" />
              {{ connectionLabel }}
            </span>
            <span v-if="stamp(info.checked_at)" class="dimmer">
              检查于 {{ formatRelative(stamp(info.checked_at)) }}
            </span>
          </div>
          <div v-else class="banner warn card-foot" role="alert">
            <Icon name="circle-alert" :size="16" />
            <span class="banner-text">
              {{ info.health_error || 'OpenViking 无响应，请检查服务地址、账号与网络' }}
            </span>
          </div>
        </template>
      </AsyncBlock>
    </div>

    <template v-if="enabled">
      <!-- 2. memory: the counters an operator checks before asking "did it learn?" -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            长期记忆
            <span class="card-sub">
              <span class="tag" :class="memory.enable ? 'ok' : ''">
                {{ memory.enable ? '记忆已启用' : '记忆未启用' }}
              </span>
              <span class="tag" :class="memory.commit ? 'ok' : 'warn'">
                {{ memory.commit ? '自动提交' : '仅缓冲，需手动提交' }}
              </span>
            </span>
          </div>
          <button
            type="button"
            class="btn sm"
            :disabled="Boolean(busy)"
            title="把缓冲区里的记忆立刻提交到 OpenViking"
            @click="flushMemory"
          >
            <Icon name="refresh" :size="15" />
            {{ busy === 'flush' ? '提交中…' : '刷新记忆' }}
          </button>
        </div>

        <div class="stat-grid">
          <StatCard label="已提交" tone="b" :value="formatCount(stats.submitted)" />
          <StatCard label="已提取事实" tone="p" :value="formatCount(stats.facts)" />
          <StatCard
            label="待提交"
            tone="a"
            :value="formatCount(stats.pending)"
            :sub="memory.commit ? '由服务自动提交' : '需要手动点「刷新记忆」'"
          />
          <StatCard
            label="失败"
            :tone="stats.failures ? 'a' : 'c'"
            :value="formatCount(stats.failures)"
          />
        </div>

        <div v-if="flushResult" class="prov-test card-foot" :class="flushResult.ok ? 'ok' : 'bad'" role="status">
          <Icon :name="flushResult.ok ? 'circle-check' : 'circle-alert'" :size="14" />
          {{ flushResult.text }}
        </div>

        <div v-if="stats.last_error" class="cell-err card-foot">
          最近一次记忆写入失败
          <template v-if="stamp(stats.last_error_at)">（{{ formatAbsolute(stamp(stats.last_error_at)) }}）</template>
          ：{{ stats.last_error }}
        </div>

        <p class="muted-note card-foot">
          <template v-if="stamp(stats.last_submit_at)">
            最近一次提交：{{ formatRelative(stamp(stats.last_submit_at)) }} ·
          </template>
          计数的口径是本次进程启动以来的累计值。
        </p>
      </div>

      <!-- 3. the workspace: what is tracked, and the two sync actions -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            文档工作区
            <span class="card-sub">
              把工作区文件同步进 OpenViking · 已跟踪 {{ formatCount(docsInfo.tracked) }} 个文件
            </span>
          </div>
          <div class="row">
            <button
              type="button"
              class="btn primary sm"
              :disabled="Boolean(busy)"
              title="只上传内容有变化或尚未上传的文件"
              @click="runSync(false)"
            >
              <Icon :name="busy === 'sync' ? 'refresh' : 'upload'" :size="15" />
              {{ busy === 'sync' ? '同步中…' : '立即同步工作区' }}
            </button>
            <button
              type="button"
              class="btn ghost sm"
              :disabled="Boolean(busy)"
              title="忽略状态文件，把工作区里的每个文件重新上传一遍"
              @click="runSync(true)"
            >
              <Icon name="upload" :size="15" />
              {{ busy === 'full' ? '重传中…' : '全量重传' }}
            </button>
          </div>
        </div>

        <dl class="kv">
          <div class="kv-row">
            <dt>根 URI</dt>
            <dd class="mono">{{ docsInfo.root_uri || '—' }}</dd>
          </div>
          <div class="kv-row">
            <dt>工作区目录</dt>
            <dd class="mono">{{ docsInfo.workspace_dir || '未配置（无法同步）' }}</dd>
          </div>
          <div class="kv-row">
            <dt>随工作区同步</dt>
            <dd>
              <span class="tag" :class="docsInfo.sync_workspace ? 'ok' : 'warn'">
                {{ docsInfo.sync_workspace ? '已开启' : '未开启' }}
              </span>
            </dd>
          </div>
          <div class="kv-row">
            <dt>最近一次同步</dt>
            <dd>
              <template v-if="stamp(docsInfo.last_sync_at)">
                {{ formatAbsolute(stamp(docsInfo.last_sync_at)) }}
              </template>
              <span v-else class="dimmer">尚未同步</span>
            </dd>
          </div>
        </dl>

        <!-- The outcome of the button just pressed. -->
        <div
          v-if="syncResult"
          class="prov-test card-foot"
          :class="syncResult.ok ? 'ok' : 'bad'"
          role="status"
        >
          <Icon :name="syncResult.ok ? 'circle-check' : 'circle-alert'" :size="14" />
          <template v-if="syncResult.ok">
            <template v-if="syncResult.report">
              同步完成：上传 {{ formatCount(syncResult.report.uploaded) }} · 未变化
              {{ formatCount(syncResult.report.unchanged) }} · 跳过
              {{ formatCount(syncResult.report.skipped) }} · 失败
              {{ formatCount(syncResult.report.failed) }}（共扫描
              {{ formatCount(syncResult.report.scanned) }} 个文件）
            </template>
            <template v-else>同步完成</template>
          </template>
          <template v-else>{{ syncResult.error }}</template>
        </div>

        <!-- The report of the last run, from the status payload: it survives a
             page reload, which is what makes it worth showing separately. -->
        <div v-if="settledSync" class="refresh-report">
          <div class="refresh-head">
            <span class="tag" :class="settledSync.failed ? 'warn' : 'ok'">
              {{ settledSync.full ? '全量同步' : '增量同步' }}
            </span>
            <span>
              上次同步 {{ formatAbsolute(stamp(settledSync.started_at)) }}：
              扫描 {{ formatCount(settledSync.scanned) }} · 上传
              {{ formatCount(settledSync.uploaded) }} · 未变化
              {{ formatCount(settledSync.unchanged) }} · 跳过
              {{ formatCount(settledSync.skipped) }} · 失败 {{ formatCount(settledSync.failed) }}
            </span>
          </div>
          <p class="muted-note">
            目标：<code class="md-code">{{ settledSync.root_uri || '—' }}</code>
          </p>
          <div v-if="settledSync.errors && settledSync.errors.length" class="row row-gap">
            <button type="button" class="btn ghost sm" @click="showSyncErrors = !showSyncErrors">
              <Icon :name="showSyncErrors ? 'chevron-down' : 'chevron-right'" :size="13" />
              {{ showSyncErrors ? '收起失败原因' : `查看 ${formatCount(settledSync.errors.length)} 条失败原因` }}
            </button>
          </div>
          <ul
            v-if="showSyncErrors && settledSync.errors && settledSync.errors.length"
            class="refresh-list"
          >
            <li v-for="(item, i) in settledSync.errors" :key="i" class="refresh-item bad">
              <Icon name="circle-alert" :size="12" />
              {{ item }}
            </li>
          </ul>
        </div>
      </div>

      <!-- 4. what OpenViking actually holds -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            已同步文档
            <span class="card-sub">
              来自 <code class="md-code">GET /api/openviking/documents</code>
            </span>
          </div>
          <button type="button" class="btn ghost sm" :disabled="docsLoading" @click="reloadDocs()">
            <Icon name="refresh" :size="15" />
            重新读取
          </button>
        </div>

        <AsyncBlock
          :state="docsStatus"
          :error="docsError"
          :skeleton-rows="3"
          @retry="reloadDocs"
        >
          <div v-if="!documents.length" class="empty">
            <div class="empty-ico" aria-hidden="true">◍</div>
            <div class="empty-text">还没有同步任何文档</div>
            <div class="empty-hint">
              点上面的「立即同步工作区」把配置的工作区目录上传，或用下面的表单手工保存一篇。
            </div>
          </div>

          <template v-else>
            <div class="table-wrap">
              <table class="data">
                <thead>
                  <tr>
                    <th>路径</th>
                    <th>类型</th>
                    <th class="num">大小</th>
                    <th>同步时间</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="doc in visibleDocuments" :key="doc.path || doc.uri">
                    <td class="mono" :title="doc.uri || doc.path">
                      {{ doc.path || '—' }}
                      <div v-if="doc.title" class="cell-note dimmer">{{ doc.title }}</div>
                    </td>
                    <td>
                      <span class="tag" :class="doc.kind === 'document' ? 'blue' : 'purple'">
                        {{ kindLabel(doc.kind) }}
                      </span>
                    </td>
                    <td class="num mono">{{ formatBytes(doc.size) }}</td>
                    <td
                      class="nowrap"
                      :title="formatAbsolute(stamp(doc.synced_at))"
                    >
                      <template v-if="stamp(doc.synced_at)">
                        {{ formatRelative(stamp(doc.synced_at)) }}
                      </template>
                      <span v-else class="dimmer">—</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>

            <p class="muted-note card-foot">
              共 {{ formatCount(documents.length) }} 篇已同步。
              <template v-if="documents.length > visibleDocuments.length">
                只列出最新的 {{ formatCount(DOC_LIMIT) }} 篇，其余请用上面的路径在
                OpenViking 里查看。
              </template>
            </p>
          </template>
        </AsyncBlock>
      </div>

      <!-- 5. one document, written by hand into the context database -->
      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            手动保存文档
            <span class="card-sub">
              直接写入 OpenViking，不经过工作区同步 · 来源记为 <code class="md-code">console</code>
            </span>
          </div>
        </div>

        <div class="form-grid">
          <label class="field">
            <span class="field-label">标题（必填）</span>
            <input
              v-model="form.title"
              class="input"
              type="text"
              placeholder="例如：部署环境的注意事项"
              aria-label="文档标题"
            />
          </label>
          <label class="field">
            <span class="field-label">标签（可选）</span>
            <input
              v-model="form.tags"
              class="input mono"
              type="text"
              placeholder="部署，运维"
              aria-label="文档标签"
            />
            <span class="field-hint">用逗号分隔，会被写进文档的元数据，便于之后检索。</span>
          </label>
          <label class="field field-wide">
            <span class="field-label">正文（必填）</span>
            <textarea
              v-model="form.content"
              class="input"
              rows="6"
              placeholder="写清背景、结论和可直接执行的步骤；这段正文就是之后被检索到的内容。"
              aria-label="文档正文"
            />
          </label>
        </div>

        <div class="row row-gap prov-form-foot">
          <button
            type="button"
            class="btn primary sm"
            :disabled="Boolean(busy) || !canSave"
            @click="saveDocument"
          >
            <Icon name="upload" :size="15" />
            {{ busy === 'save' ? '保存中…' : '保存到 OpenViking' }}
          </button>
          <span v-if="!canSave" class="dimmer nowrap">标题和正文都填写后才能保存</span>
        </div>

        <div
          v-if="saveResult"
          class="prov-test"
          :class="saveResult.ok ? 'ok' : 'bad'"
          role="status"
        >
          <Icon :name="saveResult.ok ? 'circle-check' : 'circle-alert'" :size="14" />
          <template v-if="saveResult.ok">
            已保存<template v-if="saveResult.uri">：<code class="md-code">{{ saveResult.uri }}</code></template>
          </template>
          <template v-else>{{ saveResult.error }}</template>
        </div>
      </div>
    </template>
  </div>
</template>
