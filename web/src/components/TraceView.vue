<script setup>
// 链路追踪 — Langfuse trace list + observation waterfall.
//
// Three independent panels, each with its own loading / empty / error state:
// the delivery status strip, the trace list (with filters and paging) and the
// detail of the selected trace. Reading traces goes through the admin API, so
// the Langfuse secret key never reaches the browser.
import { computed, reactive, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import TraceWaterfall from './TraceWaterfall.vue'
import { api } from '../api.js'
import {
  DASH,
  formatAbsolute,
  formatCount,
  formatMoney,
  formatRelative,
  formatSeconds,
  shortId,
  truncate,
} from '../format.js'
import { useResource } from '../useResource.js'

const LIMITS = [20, 50, 100]

const filters = reactive({ session: '', name: '', user: '' })
const limit = ref(50)
const page = ref(1)
const selectedId = ref('')

/** A 502 means Langfuse itself is unreachable — not that tracing is off. */
const listDown = ref(false)
const detailDown = ref(false)

const statusRes = useResource(() => api.traceStatus(), { watchRange: false })

const list = useResource(
  async () => {
    try {
      const res = await api.traces({
        limit: limit.value,
        page: page.value,
        session: filters.session.trim(),
        name: filters.name.trim(),
        user: filters.user.trim(),
      })
      listDown.value = false
      return res
    } catch (err) {
      listDown.value = Boolean(err && err.status === 502)
      throw err
    }
  },
  {
    watchRange: false,
    watch: [() => page.value, () => limit.value],
  },
)

const detail = useResource(
  () => {
    const id = selectedId.value
    if (!id) return Promise.resolve(null)
    return api
      .trace(id)
      .then((res) => {
        detailDown.value = false
        return res
      })
      .catch((err) => {
        detailDown.value = Boolean(err && err.status === 502)
        throw err
      })
  },
  { watchRange: false, watch: [() => selectedId.value] },
)

// ------------------------------------------------------------------ status --

const status = computed(() => (statusRes.data.value && statusRes.data.value) || null)
const traceEnabled = computed(() => {
  const data = status.value || list.data.value || detail.data.value
  return Boolean(data && data.enabled)
})
const host = computed(() => {
  const data = status.value || list.data.value
  return (data && data.host) || ''
})
const stats = computed(() => (status.value && status.value.stats) || null)

/** Delivery counters, with failures pulled out so they can stand out. */
const counters = computed(() => {
  const s = stats.value || {}
  const failure = Number(s.failed) || 0
  const dropped = Number(s.dropped) || 0
  return [
    { key: 'sent', label: 'sent', value: formatCount(s.sent), tone: '' },
    { key: 'failed', label: 'failed', value: formatCount(failure), tone: failure > 0 ? 'bad' : '' },
    {
      key: 'dropped',
      label: 'dropped',
      value: formatCount(s.dropped),
      tone: dropped > 0 ? 'bad' : '',
    },
    { key: 'queued', label: 'queued', value: formatCount(s.queued), tone: '' },
    { key: 'flushes', label: 'flushes', value: formatCount(s.flushes), tone: '' },
  ]
})

const hasDeliveryProblem = computed(() => {
  const s = stats.value || {}
  return (Number(s.failed) || 0) > 0 || (Number(s.dropped) || 0) > 0
})

// -------------------------------------------------------------------- list --

const rows = computed(() => (list.data.value && list.data.value.traces) || [])
const canPrev = computed(() => page.value > 1)
const canNext = computed(() => rows.value.length >= limit.value)
const filterActive = computed(
  () => filters.session.trim() !== '' || filters.name.trim() !== '' || filters.user.trim() !== '',
)

/** Distinct, explicit wording for the two failure modes. */
const listError = computed(() => {
  const message = list.error.value || ''
  if (listDown.value) return `Langfuse 后端不可达：${message || '无法读取链路数据'}`
  return message
})

const detailError = computed(() => {
  const message = detail.error.value || ''
  if (detailDown.value) return `Langfuse 后端不可达：${message || '无法读取链路数据'}`
  return message
})

const detailState = computed(() => {
  if (!selectedId.value) return 'empty'
  const data = detail.data.value
  const loadedId = data && data.trace ? data.trace.id : ''
  if (detail.loading.value && loadedId !== selectedId.value) return 'loading'
  if (detail.error.value && loadedId !== selectedId.value) return 'error'
  return 'ready'
})

const detailTrace = computed(() => {
  const data = detail.data.value
  if (!data || !data.trace) return null
  return data.trace
})

const observations = computed(() => (detailTrace.value && detailTrace.value.observations) || [])

const deepLink = computed(() => {
  const base = (detail.data.value && detail.data.value.host) || host.value
  const id = detailTrace.value && detailTrace.value.id
  if (!base || !id) return ''
  return `${String(base).replace(/\/+$/, '')}/trace/${id}`
})

function applyFilters() {
  page.value = 1
  list.reload()
}

function clearFilters() {
  filters.session = ''
  filters.name = ''
  filters.user = ''
  page.value = 1
  list.reload()
}

function setLimit(value) {
  if (limit.value === value) return
  limit.value = value
  page.value = 1
}

function goPrev() {
  if (!canPrev.value) return
  page.value -= 1
}

function goNext() {
  if (!canNext.value) return
  page.value += 1
}

function selectRow(trace) {
  if (!trace || !trace.id) return
  selectedId.value = trace.id
}

function refreshAll() {
  statusRes.reload()
  list.reload()
  if (selectedId.value) detail.reload()
}

const CONFIG_SNIPPET = `langfuse:
  enable: true
  host: "https://cloud.langfuse.com"
  public_key: "pk-lf-..."
  secret_key: "sk-lf-..."`

function traceNameOf(trace) {
  return trace.name || '（未命名）'
}
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <div class="page-title">链路追踪</div>
        <div class="page-note">对话与工具调用的 trace 来自 Langfuse，经本机 API 代理读取</div>
      </div>
      <div class="row">
        <span v-if="host" class="chip" :title="host">{{ host }}</span>
        <button type="button" class="btn sm" :disabled="list.loading.value" @click="refreshAll">
          重新加载
        </button>
      </div>
    </div>

    <!-- 1. delivery status -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          追踪状态
          <span class="card-sub">上报计数器</span>
        </div>
        <span v-if="statusRes.status.value === 'ready'" class="chip" :class="traceEnabled ? 'ok' : 'warn'">
          {{ traceEnabled ? '已启用' : '未启用' }}
        </span>
      </div>

      <AsyncBlock
        :state="statusRes.status.value"
        :error="statusRes.error.value"
        :skeleton-rows="2"
        @retry="statusRes.reload"
      >
        <div v-if="!status" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">没有返回追踪状态</div>
          <div class="empty-hint">/api/traces/status 未返回内容，请重试或检查服务端日志</div>
        </div>

        <div v-else class="stack">
          <div class="chips">
            <span class="chip" :class="traceEnabled ? 'ok' : 'warn'">
              追踪<b>{{ traceEnabled ? '开启' : '关闭' }}</b>
            </span>
            <span class="chip" :title="host || '未配置 host'">
              host <b>{{ host || DASH }}</b>
            </span>
            <template v-if="stats">
              <span
                v-for="counter in counters"
                :key="counter.key"
                class="chip"
                :class="counter.tone"
                :title="`${counter.label}：${counter.value}`"
              >
                {{ counter.label }} <b>{{ counter.value }}</b>
              </span>
              <span class="chip" :class="stats.enabled ? 'ok' : 'warn'">
                客户端<b>{{ stats.enabled ? '运行中' : '已停用' }}</b>
              </span>
            </template>
          </div>

          <div v-if="hasDeliveryProblem" class="banner error" role="alert">
            <span aria-hidden="true">⚠</span>
            <span class="banner-text">
              有事件上报失败或被丢弃：请检查 langfuse.host 是否可达、public_key / secret_key 是否有效。
            </span>
          </div>
        </div>
      </AsyncBlock>
    </div>

    <!-- 2. trace list -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          Trace 列表
          <span class="card-sub">最新在前 · 点击一行查看瀑布流</span>
        </div>
        <div class="row">
          <span class="muted-note">第 {{ formatCount(page) }} 页</span>
          <div class="seg" role="group" aria-label="每页条数">
            <button
              v-for="value in LIMITS"
              :key="value"
              type="button"
              :class="{ active: limit === value }"
              @click="setLimit(value)"
            >
              {{ value }} 条
            </button>
          </div>
        </div>
      </div>

      <form class="filter-bar" @submit.prevent="applyFilters">
        <div class="field">
          <label for="trace-session" class="muted-note">会话 ID 过滤</label>
          <input
            id="trace-session"
            v-model="filters.session"
            class="input mono"
            type="search"
            placeholder="会话 uuid"
            @keyup.enter="applyFilters"
          />
        </div>
        <div class="field">
          <label for="trace-name" class="muted-note">名称过滤</label>
          <input
            id="trace-name"
            v-model="filters.name"
            class="input mono"
            type="search"
            placeholder="例如 chat.turn"
            @keyup.enter="applyFilters"
          />
        </div>
        <div class="field">
          <label for="trace-user" class="muted-note">用户过滤</label>
          <input
            id="trace-user"
            v-model="filters.user"
            class="input mono"
            type="search"
            placeholder="admin"
            @keyup.enter="applyFilters"
          />
        </div>
        <button type="submit" class="btn primary" :disabled="list.loading.value">查询</button>
        <button v-if="filterActive" type="button" class="btn ghost" @click="clearFilters">清除</button>
      </form>

      <div class="list-toolbar">
        <button type="button" class="btn sm" :disabled="!canPrev || list.loading.value" @click="goPrev">
          上一页
        </button>
        <button type="button" class="btn sm" :disabled="!canNext || list.loading.value" @click="goNext">
          下一页
        </button>
        <span v-if="list.data.value && traceEnabled" class="muted-note">
          本页 {{ formatCount(rows.length) }} 条
        </span>
      </div>

      <!-- A failed reload keeps the previous rows on screen, but never hides
           the failure: an unreachable Langfuse must be visible here. -->
      <div v-if="list.error.value" class="banner error" role="alert">
        <span aria-hidden="true">⚠</span>
        <span class="banner-text">加载失败：{{ listError || '请稍后重试' }}</span>
        <button type="button" class="btn sm" @click="list.reload">重试</button>
      </div>

      <AsyncBlock
        :state="list.status.value"
        :error="listError"
        :skeleton-rows="6"
        @retry="list.reload"
      >
        <!-- tracing off: say exactly which config to set -->
        <div v-if="!traceEnabled" class="empty trace-off">
          <div class="empty-ico" aria-hidden="true">◎</div>
          <div class="empty-text">链路追踪未启用</div>
          <div v-if="list.data.value && list.data.value.message" class="empty-hint">
            {{ list.data.value.message }}
          </div>
          <div class="empty-hint">
            在配置文件（configs/config.yaml）中打开 Langfuse 并填入凭证，然后重启
            huan-agent 服务：
          </div>
          <pre class="json trace-config">{{ CONFIG_SNIPPET }}</pre>
          <div class="empty-hint">
            至少需要 <code class="md-code">langfuse.enable</code>、<code class="md-code">langfuse.host</code>、<code class="md-code">langfuse.public_key</code>、
            <code class="md-code">langfuse.secret_key</code> 四项；配置改动需要重启才会生效。
          </div>
        </div>

        <div v-else-if="!rows.length" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">暂无数据</div>
          <div class="empty-hint">
            {{ filterActive ? '没有匹配当前过滤条件的 trace' : '还没有任何 trace，先去「对话」里发一条消息试试' }}
          </div>
        </div>

        <div v-else class="table-wrap">
          <table class="data">
            <thead>
              <tr>
                <th>时间</th>
                <th>名称</th>
                <th>用户</th>
                <th>会话</th>
                <th class="num">延迟</th>
                <th class="num">费用</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="trace in rows"
                :key="trace.id"
                class="row-click"
                :class="{ selected: trace.id === selectedId }"
                tabindex="0"
                role="button"
                :aria-label="`查看 trace ${trace.id} 的详情`"
                @click="selectRow(trace)"
                @keyup.enter="selectRow(trace)"
              >
                <td class="nowrap" :title="formatAbsolute(trace.timestamp)">
                  {{ formatRelative(trace.timestamp) }}
                </td>
                <td>
                  <span class="mono">{{ traceNameOf(trace) }}</span>
                  <div v-if="trace.tags && trace.tags.length" class="dimmer">
                    {{ truncate(trace.tags.join(' · '), 40) }}
                  </div>
                </td>
                <td class="mono dim" :title="trace.user_id || ''">{{ trace.user_id || DASH }}</td>
                <td class="mono dim" :title="trace.session_id || ''">{{ shortId(trace.session_id, 8, 4) }}</td>
                <td class="num dim">{{ formatSeconds(trace.latency) }}</td>
                <td class="num dim">{{ formatMoney(trace.total_cost) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </AsyncBlock>
    </div>

    <!-- 3. trace detail -->
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          Trace 详情
          <span class="card-sub">observation 瀑布流 · 点击一行展开 input / output</span>
        </div>
        <div class="row">
          <a
            v-if="deepLink"
            class="btn sm"
            :href="deepLink"
            target="_blank"
            rel="noopener noreferrer"
            :title="deepLink"
          >
            在 Langfuse 中打开 ↗
          </a>
          <button
            v-if="selectedId"
            type="button"
            class="btn sm ghost"
            @click="selectedId = ''"
          >
            关闭
          </button>
        </div>
      </div>

      <div v-if="!selectedId" class="empty">
        <div class="empty-ico" aria-hidden="true">◍</div>
        <div class="empty-text">没有选中任何 trace</div>
        <div class="empty-hint">在上方列表中点击一行，即可查看它的时间线与 observation 明细</div>
      </div>

      <div v-else-if="detail.error.value && detailTrace" class="banner error" role="alert">
        <span aria-hidden="true">⚠</span>
        <span class="banner-text">加载失败：{{ detailError || '请稍后重试' }}</span>
        <button type="button" class="btn sm" @click="detail.reload">重试</button>
      </div>

      <AsyncBlock
        v-else
        :state="detailState"
        :error="detailError"
        :skeleton-rows="6"
        @retry="detail.reload"
      >
        <div v-if="!detailTrace" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">该 trace 不存在或已被清理</div>
          <div class="empty-hint">请返回列表重新选择</div>
        </div>

        <div v-else class="stack">
          <div class="chips">
            <span class="chip accent mono shrink" :title="detailTrace.id">{{ detailTrace.id }}</span>
            <span class="chip"><b>{{ traceNameOf(detailTrace) }}</b></span>
            <span class="chip" :title="formatAbsolute(detailTrace.timestamp)">
              {{ formatRelative(detailTrace.timestamp) }}
            </span>
            <span class="chip">用户 <b>{{ detailTrace.user_id || DASH }}</b></span>
            <span class="chip" :title="detailTrace.session_id || ''">
              会话 <b>{{ shortId(detailTrace.session_id, 8, 4) }}</b>
            </span>
            <span class="chip">延迟 <b>{{ formatSeconds(detailTrace.latency) }}</b></span>
            <span class="chip">费用 <b>{{ formatMoney(detailTrace.total_cost) }}</b></span>
            <span v-for="tag in detailTrace.tags || []" :key="tag" class="chip">{{ tag }}</span>
          </div>

          <div v-if="detailTrace.input !== undefined || detailTrace.output !== undefined" class="grid-2">
            <JsonBlock :value="detailTrace.input" label="trace input" :open="true" :toggle="false" />
            <JsonBlock :value="detailTrace.output" label="trace output" :open="true" :toggle="false" />
          </div>

          <TraceWaterfall :observations="observations" />
        </div>
      </AsyncBlock>
    </div>
  </div>
</template>
