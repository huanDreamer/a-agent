<script setup>
// 调用记录 — raw recent LLM calls from /api/usage/recent.
import { computed, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import ViewHead from './ViewHead.vue'
import ViewToolbar from './ViewToolbar.vue'
import { api } from '../api.js'
import {
  formatAbsolute,
  formatCompact,
  formatCount,
  formatDuration,
  formatExactTokens,
  formatRelative,
  shortId,
} from '../format.js'
import { currentFilters, rangeNote } from '../state.js'
import { useResource } from '../useResource.js'

const LIMITS = [20, 50, 100]
const limit = ref(20)

const { data, status, error, reload } = useResource(() =>
  api.usageRecent(currentFilters(), limit.value),
)

const rows = computed(() => (data.value && data.value.rows) || [])

function setLimit(value) {
  if (limit.value === value) return
  limit.value = value
  reload()
}
</script>

<template>
  <div class="page">
    <ViewHead title="调用记录" :note="`${rangeNote} · 最新在前 · 悬停时间可查看绝对时间`">
      <template #tools>
        <ViewToolbar />
      </template>
    </ViewHead>

    <div class="page-scroll">
      <div class="stack">
        <div class="card">
          <div class="card-head">
            <div class="card-title">
              <span class="dot" />
              最近调用
              <span class="card-sub">共 {{ formatCount(rows.length) }} 条</span>
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
              <div class="empty-hint">{{ rangeNote }}内没有调用记录</div>
            </div>

            <div v-else class="table-wrap">
              <table class="data">
                <thead>
                  <tr>
                    <th>时间</th>
                    <th>用户</th>
                    <th>Provider / 模型</th>
                    <th>会话</th>
                    <th class="num">Prompt</th>
                    <th class="num">Completion</th>
                    <th class="num">总 token</th>
                    <th class="num">耗时</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="row in rows" :key="row.id">
                    <td class="nowrap" :title="formatAbsolute(row.created_at)">
                      {{ formatRelative(row.created_at) }}
                    </td>
                    <td>
                      <span class="mono" :title="row.user_id || ''">{{ shortId(row.user_id) }}</span>
                    </td>
                    <td>
                      <span class="tag blue">{{ row.provider || '—' }}</span>
                      <span class="mono row-gap">{{ row.model || '—' }}</span>
                    </td>
                    <td>
                      <span class="mono dim" :title="row.session_id || ''">
                        {{ shortId(row.session_id) }}
                      </span>
                    </td>
                    <td class="num" :title="formatExactTokens(row.prompt_tokens)">
                      {{ formatCompact(row.prompt_tokens) }}
                    </td>
                    <td class="num" :title="formatExactTokens(row.completion_tokens)">
                      {{ formatCompact(row.completion_tokens) }}
                    </td>
                    <td class="num" :title="formatExactTokens(row.total_tokens)">
                      {{ formatCompact(row.total_tokens) }}
                    </td>
                    <td class="num dim">{{ formatDuration(row.duration_ms) }}</td>
                  </tr>
                </tbody>
              </table>
            </div>

            <div v-if="rows.length" class="muted-note table-foot">
              共 {{ formatCount(rows.length) }} 条记录 · 时间窗口 {{ rangeNote }}
            </div>
          </AsyncBlock>
        </div>
      </div>
    </div>
  </div>
</template>
