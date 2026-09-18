<script setup>
// 服务与工具 — everything about the running server the console can only report.
//
// It is the read-only half of 设置: provider/model/version/feature flags from
// GET /api/meta, and the tool set the agent actually has (builtin + MCP +
// skill) from the same catalog endpoint 对话 uses. Nothing here can be changed
// from the UI — the note under each card says where the change would be made.
//
// The tool list is re-read when the panel opens rather than only at boot: it is
// the thing that changes when an MCP server is saved or switched off in the MCP
// sub-tab, and a cached list would show the tools the process had a minute ago.
import { computed, onMounted } from 'vue'
import Icon from './Icon.vue'
import { chat, loadCatalog, maxSteps } from '../chatStore.js'
import { formatCount } from '../format.js'
import { loadMeta, state } from '../state.js'

const meta = computed(() => state.meta || {})
const tools = computed(() => (chat.catalog && chat.catalog.tools) || [])

/**
 * The version line, in a shape a person can act on.
 *
 * The server reports `e254199-dirty (commit e254199, built 2026-09-18T23:25:47Z)`,
 * and both halves of that mislead at a glance. The build time is UTC, so a build
 * made at 07:25 this morning reads as 23:25 — yesterday evening — to anyone east
 * of Greenwich; and the version is `git describe`, so it does NOT move until the
 * work is committed, which makes a freshly restarted service look stale. So the
 * time is rendered in the reader's own timezone with a relative hint, and a
 * `-dirty` build says out loud that the running code is not in any commit.
 */
const version = computed(() => parseVersion(meta.value.version))

function parseVersion(raw) {
  const text = typeof raw === 'string' && raw.trim() !== '' ? raw.trim() : 'dev'
  // "name (commit abc1234, built 2026-09-18T23:25:47Z)" — anything missing stays
  // missing rather than being invented.
  const built = text.match(/built\s+([0-9T:+-]+Z?)/)
  const commit = text.match(/commit\s+([0-9a-f]{4,40})/i)
  const name = text.split(' (')[0]
  const dirty = /-dirty\b/.test(name)

  let at = null
  if (built) {
    const parsed = new Date(built[1])
    if (!Number.isNaN(parsed.getTime())) at = parsed
  }
  return {
    name: dirty ? name.replace(/-dirty$/, '') : name,
    dirty,
    commit: commit ? commit[1] : '',
    at,
    when: at ? whenText(at) : '',
  }
}

/** "今天 07:25" / "昨天 23:10" / "9月17日 07:25" — local time, with a hint. */
function whenText(at) {
  const now = new Date()
  const clock = `${String(at.getHours()).padStart(2, '0')}:${String(at.getMinutes()).padStart(2, '0')}`
  const days = Math.floor(
    (new Date(now.getFullYear(), now.getMonth(), now.getDate()) -
      new Date(at.getFullYear(), at.getMonth(), at.getDate())) /
      86400000,
  )
  if (days === 0) return `今天 ${clock}`
  if (days === 1) return `昨天 ${clock}`
  return `${at.getMonth() + 1}月${at.getDate()}日 ${clock}`
}

onMounted(() => {
  // Quiet: keep whatever is on screen while the refresh is in flight.
  loadCatalog({ quiet: true })
})
</script>

<template>
  <div class="stack">
    <!-- server / provider information -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          服务信息
          <span class="card-sub">只读 · 来自 <code class="md-code">GET /api/meta</code></span>
        </div>
        <button
          type="button"
          class="btn ghost sm"
          :disabled="!state.metaError && !state.meta"
          @click="loadMeta()"
        >
          <Icon name="refresh" :size="15" />
          重新读取
        </button>
      </div>

      <div v-if="state.metaError" class="banner error" role="alert">
        <Icon name="circle-alert" :size="16" />
        <span class="banner-text">无法读取服务信息：{{ state.metaError }}</span>
        <button type="button" class="btn sm" @click="loadMeta()">重试</button>
      </div>

      <div v-else-if="!state.meta" class="stack" aria-busy="true" aria-live="polite">
        <span class="skeleton" style="height: 14px; width: 38%" />
        <span class="skeleton" style="height: 13px; width: 62%" />
        <span class="muted-note">正在读取服务信息…</span>
      </div>

      <dl v-else class="kv">
        <div class="kv-row">
          <dt>默认 provider</dt>
          <dd class="mono">{{ meta.provider || '—' }}</dd>
        </div>
        <div class="kv-row">
          <dt>默认 model</dt>
          <dd class="mono">{{ meta.model || '—' }}</dd>
        </div>
        <div class="kv-row">
          <dt>服务版本</dt>
          <dd class="mono">
            <span>{{ version.name }}</span>
            <template v-if="version.when"> · 构建于 {{ version.when }}</template>
            <span v-if="version.commit" class="muted-note"> · {{ version.commit }}</span>
            <br />
            <span v-if="version.dirty" class="muted-note">
              工作区有未提交改动：这个版本号不会变，重启也看不出差别——提交之后才会
            </span>
            <span v-else class="muted-note">已提交的构建</span>
          </dd>
        </div>
        <div class="kv-row">
          <dt>Prometheus metrics</dt>
          <dd>{{ meta.metrics_enabled ? '已启用' : '未启用' }}</dd>
        </div>
        <div class="kv-row">
          <dt>飞书接入</dt>
          <dd>{{ meta.feishu_enabled ? '已启用' : '未启用' }}</dd>
        </div>
      </dl>
    </div>

    <!-- workspace + tools (informational only) -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          工作区与工具
          <span class="card-sub">只读 · 工具集来自服务端配置；工作区在左侧列表里管理</span>
        </div>
      </div>

      <p class="set-desc">
        文件与命令工具在<strong>工作区</strong>里运行：一个工作区就是一个本机目录，每个对话属于一个，
        在左侧列表里以文件夹的形式出现（新建、重命名、删除都在那里，不需要到设置里找）。
        是否允许写文件、是否允许执行命令仍是<strong>全局</strong>配置 ——
        <code class="md-code">tools.read_only</code> / <code class="md-code">tools.enable_bash</code>，
        对所有工作区一视同仁。
      </p>
      <p class="set-desc">
        这张列表是「模型真正能调用什么」的上限：内置工具、<strong>MCP</strong> 服务器提供的工具、以及
        <code class="md-code">skill</code>（读取 <strong>技能</strong> 的指令）都在其中。
        MCP 工具会在 MCP 分区保存后立刻出现在这里，不需要重启。
      </p>

      <div class="chips tools-chips">
        <template v-if="tools.length">
          <span v-for="name in tools" :key="name" class="chip mono">{{ name }}</span>
        </template>
        <span v-else class="chip warn">当前没有注册任何工具</span>
      </div>

      <p class="muted-note card-foot">
        共 {{ formatCount(tools.length) }} 个工具 · 单轮最多 {{ formatCount(maxSteps) }} 步 ·
        对话里不展示该列表
      </p>
    </div>
  </div>
</template>
