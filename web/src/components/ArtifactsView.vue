<script setup>
// 产物中心 panel — every session's artifacts, from /api/artifacts (no `session`).
//
// A content panel of 统计监控, like 审计日志: bare .stack, no page head of its own
// (the page supplies 刷新; a time range would mean nothing here, since an artifact
// list is bounded by count rather than by date).
//
// It is deliberately not the conversation drawer with a wider query. A drawer
// answers "what did this conversation produce"; this answers "what is on this
// server, and from where" — which is a question about the deployment, so each
// row has to name its owning session, and the ones that cannot (a one-shot run
// with no session) have to say so rather than showing a blank cell.
import { computed, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import { api, artifactUrl } from '../api.js'
import { artifactLabel, extensionOf, kindIcon, kindLabel } from '../artifactsChip.js'
import { filterArtifacts, orphanCount, sessionLabel, totalBytes } from '../artifactsView.js'
import { formatBytes, formatCount, formatRelative, formatAbsolute, truncate } from '../format.js'
import { useResource } from '../useResource.js'

const KINDS = [
  { key: '', label: '全部' },
  { key: 'html', label: '网页' },
  { key: 'document', label: '文档' },
  { key: 'image', label: '图片' },
  { key: 'other', label: '其他' },
]

const kind = ref('')
const session = ref('')
/** The row whose delete is armed; one at a time, and only by asking. */
const confirmDelete = ref('')
const busy = ref('')
const failure = ref('')

// The listing is a single call for everything: the filter below is client-side
// on purpose, because the whole list is already here and a round trip per
// keystroke would be a slower way to look at the same bytes.
const { data, status, error, reload } = useResource(() => api.listArtifacts({}), { watchRange: false })

const enabled = computed(() => !data.value || data.value.enabled !== false)
const message = computed(() => (data.value && data.value.message) || '')
const all = computed(() => (data.value && data.value.artifacts) || [])
const rows = computed(() => filterArtifacts(all.value, { kind: kind.value, session: session.value }))

// Two facts a reader of a deployment-wide list needs and cannot reconstruct from
// the rows: how much disk this is, and how much of it belongs to no conversation.
const bytes = computed(() => totalBytes(rows.value))
const orphans = computed(() => orphanCount(all.value))
const filterActive = computed(() => kind.value !== '' || session.value.trim() !== '')

function clearFilters() {
  kind.value = ''
  session.value = ''
}

function setKind(value) {
  kind.value = value
}

function armDelete(id) {
  confirmDelete.value = id
}

function href(artifact) {
  return artifactUrl(artifact)
}

async function runDelete(artifact) {
  busy.value = artifact.id
  confirmDelete.value = ''
  failure.value = ''
  try {
    await api.deleteArtifact(artifact.id)
    await reload()
  } catch (err) {
    failure.value = (err && err.message) || '删除失败'
  } finally {
    busy.value = ''
  }
}
</script>

<template>
  <div class="stack">
    <!-- Not wired in this deployment: a state to explain, not a failure. -->
    <div v-if="!enabled" class="card">
      <div class="empty">
        <div class="empty-ico" aria-hidden="true">◍</div>
        <div class="empty-text">当前部署未启用产物</div>
        <div class="empty-hint">
          {{ message || '服务端的产物存储没有打开。' }}
        </div>
      </div>
    </div>

    <template v-else>
      <div class="card">
        <form class="filter-bar" @submit.prevent="reload">
          <div class="field">
            <label for="artifact-session" class="muted-note">会话过滤（标题或 id）</label>
            <input
              id="artifact-session"
              v-model="session"
              class="input mono"
              type="search"
              placeholder="例如 巡检报告 / 9f2c…"
            />
          </div>
          <button type="button" class="btn primary" :disabled="status === 'loading'" @click="reload">
            刷新
          </button>
          <button v-if="filterActive" type="button" class="btn ghost" @click="clearFilters">清除</button>
        </form>
      </div>

      <div class="card">
        <div class="card-head">
          <div class="card-title">
            <span class="dot" />
            产物
            <span class="card-sub">全部会话 · 最新在前 · 文件存在服务端的 artifacts 目录</span>
          </div>
          <div class="row">
            <div class="seg" role="group" aria-label="按类型过滤">
              <button
                v-for="item in KINDS"
                :key="item.key"
                type="button"
                :class="{ active: kind === item.key }"
                @click="setKind(item.key)"
              >
                {{ item.label }}
              </button>
            </div>
          </div>
        </div>

        <div class="row">
          <span class="chip">{{ formatCount(rows.length) }} 个</span>
          <span class="chip">{{ formatBytes(bytes) }}</span>
          <span v-if="orphans" class="chip" title="这些产物来自没有会话的执行（例如一次性 run），所以没有归属的对话">
            {{ formatCount(orphans) }} 个无会话归属
          </span>
          <span v-if="failure" class="chip bad">{{ failure }}</span>
        </div>

        <AsyncBlock :state="status" :error="error" :skeleton-rows="8" @retry="reload">
          <div v-if="!rows.length" class="empty">
            <div class="empty-ico" aria-hidden="true">◍</div>
            <div class="empty-text">暂无数据</div>
            <div class="empty-hint">
              {{ filterActive ? '没有匹配当前过滤条件的产物' : '还没有任何会话保存过产物' }}
            </div>
          </div>

          <div v-else class="table-wrap">
            <table class="data">
              <thead>
                <tr>
                  <th>时间</th>
                  <th>会话</th>
                  <th>产物</th>
                  <th>类型</th>
                  <th class="num">大小</th>
                  <th class="num">文件</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="item in rows" :key="item.id">
                  <td class="nowrap" :title="formatAbsolute(item.created_at)">
                    {{ formatRelative(item.created_at) }}
                  </td>
                  <td>
                    <span :title="item.session_id || '没有归属的会话'">{{ sessionLabel(item) }}</span>
                    <div v-if="item.session_id" class="dimmer mono" :title="item.session_id">
                      {{ shortId(item.session_id, 6, 4) }}
                    </div>
                  </td>
                  <td>
                    <div class="row">
                      <Icon :name="kindIcon(item.kind)" :size="14" />
                      <a
                        :href="href(item)"
                        target="_blank"
                        rel="noopener noreferrer"
                        :title="artifactLabel(item)"
                      >
                        {{ truncate(artifactLabel(item), 42) }}
                      </a>
                    </div>
                    <div class="dimmer mono cell-note" :title="item.path">{{ item.path }}</div>
                  </td>
                  <td>
                    <span class="tag">{{ kindLabel(item.kind) }}</span>
                    <div class="dimmer mono cell-note">{{ item.mime }}</div>
                  </td>
                  <td class="num dim">{{ formatBytes(item.bytes) }}</td>
                  <td class="num dim mono">{{ extensionOf(item.path) || '—' }}</td>
                  <td>
                    <div class="row">
                      <a class="btn ghost sm" :href="href(item)" download :title="`下载 ${artifactLabel(item)}`">
                        <Icon name="download" :size="14" />
                      </a>
                      <template v-if="confirmDelete === item.id">
                        <button
                          type="button"
                          class="btn danger sm"
                          :disabled="busy === item.id"
                          title="同时删除文件，不可撤销"
                          @click="runDelete(item)"
                        >
                          确认删除
                        </button>
                        <button type="button" class="btn ghost sm" @click="confirmDelete = ''">取消</button>
                      </template>
                      <button
                        v-else
                        type="button"
                        class="btn ghost sm"
                        :disabled="busy === item.id"
                        :title="`从列表和硬盘上删除 ${item.path}`"
                        @click="armDelete(item.id)"
                      >
                        <Icon name="trash" :size="14" />
                      </button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </AsyncBlock>
      </div>
    </template>
  </div>
</template>
