<script setup>
// 审计日志 — tool invocations from /api/audit, with a tool-name filter.
// The API also accepts `user`, so an open_id filter is offered next to it.
import { computed, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import ViewHead from './ViewHead.vue'
import ViewToolbar from './ViewToolbar.vue'
import { api } from '../api.js'
import {
  formatAbsolute,
  formatCount,
  formatDuration,
  formatRelative,
  shortId,
  truncate,
} from '../format.js'
import { useResource } from '../useResource.js'

const LIMITS = [50, 100, 200]

const tool = ref('')
const user = ref('')
const limit = ref(50)

const { data, status, error, reload } = useResource(() =>
  api.audit({ limit: limit.value, tool: tool.value.trim(), user: user.value.trim() }),
)

const rows = computed(() => (data.value && data.value.rows) || [])
const failureCount = computed(() => rows.value.filter((row) => row.err).length)
const filterActive = computed(() => tool.value.trim() !== '' || user.value.trim() !== '')

function clearFilters() {
  tool.value = ''
  user.value = ''
  reload()
}

function setLimit(value) {
  if (limit.value === value) return
  limit.value = value
  reload()
}
</script>

<template>
  <div class="page">
    <ViewHead
      title="审计日志"
      :note="filterActive ? `命中 ${formatCount(rows.length)} 条` : `最新在前 · 共 ${formatCount(rows.length)} 条`"
    >
      <template #tools>
        <span v-if="failureCount" class="chip bad">{{ formatCount(failureCount) }} 条失败</span>
        <ViewToolbar />
      </template>
    </ViewHead>

    <div class="page-scroll">
      <div class="stack">
        <div class="card">
          <form class="filter-bar" @submit.prevent="reload()">
            <div class="field">
              <label for="audit-tool" class="muted-note">工具名称过滤</label>
              <input
                id="audit-tool"
                v-model="tool"
                class="input mono"
                type="search"
                placeholder="例如 time / web_search"
                @keyup.enter="reload()"
              />
            </div>
            <div class="field">
              <label for="audit-user" class="muted-note">用户 open_id 过滤</label>
              <input
                id="audit-user"
                v-model="user"
                class="input mono"
                type="search"
                placeholder="ou_xxx"
                @keyup.enter="reload()"
              />
            </div>
            <button type="submit" class="btn primary" :disabled="status === 'loading'">查询</button>
            <button v-if="filterActive" type="button" class="btn ghost" @click="clearFilters">
              清除
            </button>
          </form>
        </div>

        <div class="card">
          <div class="card-head">
            <div class="card-title">
              <span class="dot" />
              工具调用
              <span class="card-sub">最新在前 · 失败记录会显示错误信息</span>
            </div>
            <div class="seg" role="group" aria-label="显示条数">
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

          <AsyncBlock :state="status" :error="error" :skeleton-rows="8" @retry="reload">
            <div v-if="!rows.length" class="empty">
              <div class="empty-ico" aria-hidden="true">◍</div>
              <div class="empty-text">暂无数据</div>
              <div class="empty-hint">
                {{ filterActive ? '没有匹配当前过滤条件的审计记录' : '还没有任何工具调用记录' }}
              </div>
            </div>

            <div v-else class="table-wrap">
              <table class="data">
                <thead>
                  <tr>
                    <th>时间</th>
                    <th>用户</th>
                    <th>工具</th>
                    <th>参数</th>
                    <th class="num">耗时</th>
                    <th>结果</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="row in rows" :key="row.id">
                    <td class="nowrap" :title="formatAbsolute(row.created_at)">
                      {{ formatRelative(row.created_at) }}
                    </td>
                    <td>
                      <span class="mono" :title="row.user_id || ''">{{ shortId(row.user_id) }}</span>
                      <div class="dimmer mono" :title="row.session_id || ''">
                        {{ shortId(row.session_id, 5, 3) }}
                      </div>
                    </td>
                    <td>
                      <span class="tag purple">{{ row.tool_name || '—' }}</span>
                    </td>
                    <td class="mono dim" :title="row.arguments || ''">
                      {{ truncate(row.arguments, 48) || '—' }}
                    </td>
                    <td class="num dim">{{ formatDuration(row.duration_ms) }}</td>
                    <td>
                      <template v-if="row.err">
                        <span class="tag bad">失败</span>
                        <div class="cell-err" :title="row.err">{{ truncate(row.err, 120) }}</div>
                      </template>
                      <template v-else>
                        <span class="tag ok">成功</span>
                        <div v-if="row.result" class="dimmer mono cell-note" :title="row.result">
                          {{ truncate(row.result, 60) }}
                        </div>
                      </template>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </AsyncBlock>
        </div>
      </div>
    </div>
  </div>
</template>
