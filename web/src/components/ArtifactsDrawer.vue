<script setup>
// 产物 — what this conversation produced, as a drawer beside it.
//
// Modelled on the subagents drawer, and for the same reason: the header button
// and this list must agree, so both read artifactsStore. What differs is the
// nature of the thing — an artifact is a *resource*, not a process:
//
//   1. it cannot be stopped, because it is already finished. Opening it (or
//      downloading it) is the action, and it is the reason a reader opens this
//      drawer at all.
//   2. its bytes are not in the workspace and not in the transcript. The link is
//      the only way to them, which is why it is on every row rather than in a
//      detail view.
//   3. deleting is the one destructive thing here, so it asks first. The server
//      deletes the row and then the file, and a file that refuses to go is
//      reported rather than silently pretending the artifact is gone.
//
// The list is not grouped by kind: an artifact list is short and newest-first is
// the order a reader wants, while grouping would push a just-saved page below
// older images.
import { computed, ref } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import { artifactUrl } from '../api.js'
import { artifactLabel, extensionOf, kindIcon, kindLabel } from '../artifactsChip.js'
import { formatBytes, formatRelative, formatAbsolute } from '../format.js'
import {
  artifacts,
  artifactsEnabled,
  artifactsError,
  artifactsLoaded,
  artifactsLoading,
  artifactsMessage,
  reloadArtifacts,
  removeArtifact,
} from '../artifactsStore.js'

const emit = defineEmits(['close'])

/** The row whose delete is armed. One at a time, and only by asking. */
const confirmDelete = ref('')
const busy = ref('')
/** Why the last delete failed, shown in place rather than failing the drawer. */
const failure = ref('')

const rows = computed(() => artifacts.value)

/** Tri-state for AsyncBlock: it owns the loading / empty / ready branch. */
const status = computed(() => {
  if (artifactsLoading.value && !artifactsLoaded.value) return 'loading'
  if (artifactsError.value) return 'error'
  return 'ready'
})

function href(artifact) {
  return artifactUrl(artifact)
}

function armDelete(id) {
  confirmDelete.value = id
}

function cancelDelete() {
  confirmDelete.value = ''
}

async function runDelete(artifact) {
  busy.value = artifact.id
  confirmDelete.value = ''
  failure.value = ''
  try {
    await removeArtifact(artifact.id)
  } catch (err) {
    // The store reloads either way, so the list on screen stays true; the reason
    // is shown in place rather than failing the whole drawer.
    failure.value = (err && err.message) || '删除失败'
  } finally {
    busy.value = ''
  }
}
</script>

<template>
  <aside class="jobs-drawer" aria-label="产物">
    <header class="jobs-drawer-head">
      <div class="jobs-drawer-title">
        <Icon name="package" :size="16" />
        产物
        <span class="muted-note">本会话 {{ rows.length }} 个</span>
        <span v-if="artifactsError" class="chip bad">{{ artifactsError }}</span>
        <span v-if="failure" class="chip bad">{{ failure }}</span>
      </div>
      <div class="row">
        <button type="button" class="btn ghost sm" :disabled="artifactsLoading" @click="reloadArtifacts">
          <Icon name="refresh" :size="15" />
          刷新
        </button>
        <button type="button" class="btn ghost sm" title="关闭" @click="emit('close')">
          <Icon name="x" :size="15" />
        </button>
      </div>
    </header>

    <div class="jobs-drawer-body">
      <!-- Not wired in this deployment: a state to explain, not a failure. -->
      <div v-if="!artifactsEnabled" class="empty">
        <div class="empty-ico" aria-hidden="true">◍</div>
        <div class="empty-text">当前部署未启用产物</div>
        <div class="empty-hint">
          {{ artifactsMessage || '服务端的产物存储没有打开。' }}
        </div>
      </div>

      <AsyncBlock v-else :state="status" :error="artifactsError" :skeleton-rows="3" @retry="reloadArtifacts">
        <div v-if="!rows.length" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">这个对话还没有产物</div>
          <div class="empty-hint">
            智能体做了一份网页、一份报告或一张图并想让保留时，会用
            <code class="md-code">save_artifact</code> 存到这里；文件在服务端，链接可以直接打开。
          </div>
        </div>

        <section v-else class="stack">
          <article v-for="item in rows" :key="item.id" class="card">
            <div class="row">
              <Icon :name="kindIcon(item.kind)" :size="15" />
              <span class="job-name" :title="artifactLabel(item)">{{ artifactLabel(item) }}</span>
              <span class="tag">{{ kindLabel(item.kind) }}</span>
              <span class="spacer" />
              <span class="dimmer nowrap">{{ formatBytes(item.bytes) }}</span>
            </div>

            <div class="dimmer mono cell-note" :title="item.path">
              {{ extensionOf(item.path) || item.mime }}{{ extensionOf(item.path) ? ` · ${item.mime}` : '' }}
            </div>
            <div class="muted-note" :title="formatAbsolute(item.created_at)">
              {{ formatRelative(item.created_at) }}
            </div>

            <div class="row">
              <a
                class="btn sm"
                :href="href(item)"
                target="_blank"
                rel="noopener noreferrer"
                :title="`新标签页打开：${item.path}`"
              >
                <Icon name="external-link" :size="14" />
                打开
              </a>
              <a class="btn ghost sm" :href="href(item)" download :title="`下载 ${artifactLabel(item)}`">
                <Icon name="download" :size="14" />
                下载
              </a>
              <span class="spacer" />
              <template v-if="confirmDelete === item.id">
                <span class="muted-note">同时删除文件，不可撤销</span>
                <button type="button" class="btn danger sm" :disabled="busy === item.id" @click="runDelete(item)">
                  确认删除
                </button>
                <button type="button" class="btn ghost sm" @click="cancelDelete">取消</button>
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
          </article>
        </section>
      </AsyncBlock>
    </div>
  </aside>
</template>
