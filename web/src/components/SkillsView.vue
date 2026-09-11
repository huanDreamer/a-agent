<script setup>
// 技能 — list of skills with an on/off switch that calls
// POST /api/skills/{name} with {"enabled":bool}.
import { computed, ref, watch } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import { api } from '../api.js'
import { formatCount } from '../format.js'
import { useResource } from '../useResource.js'

const { data, status, error, reload } = useResource(() => api.skills())

/** Local working copy: the toggle flips immediately, then is confirmed/reverted. */
const skills = ref([])
const pending = ref({})
const rowError = ref({})

watch(data, (value) => {
  skills.value = ((value && value.skills) || []).map((skill) => ({
    name: skill.name || '（未命名）',
    description: skill.description || '',
    enabled: Boolean(skill.enabled),
  }))
  rowError.value = {}
})

const enabledCount = computed(() => skills.value.filter((s) => s.enabled).length)

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
      [skill.name]: (err && err.message) || '切换失败，请重试',
    }
  } finally {
    const copy = { ...pending.value }
    delete copy[skill.name]
    pending.value = copy
  }
}
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div class="page-title">技能</div>
      <div class="row">
        <span class="muted-note">
          已启用 {{ formatCount(enabledCount) }} / {{ formatCount(skills.length) }}
        </span>
        <button type="button" class="btn sm" :disabled="status === 'loading'" @click="reload()">
          刷新
        </button>
      </div>
    </div>

    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          技能开关
          <span class="card-sub">切换会立即生效并写入配置</span>
        </div>
      </div>

      <AsyncBlock :state="status" :error="error" :skeleton-rows="5" @retry="reload">
        <div v-if="!skills.length" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">暂无数据</div>
          <div class="empty-hint">没有发现任何已注册的技能</div>
        </div>

        <div v-else class="skill-list">
          <div v-for="skill in skills" :key="skill.name" class="skill-row">
            <div class="skill-main">
              <div class="skill-name">
                <span class="mono">{{ skill.name }}</span>
                <span class="tag" :class="skill.enabled ? 'ok' : ''">
                  {{ skill.enabled ? '已启用' : '已停用' }}
                </span>
                <span v-if="pending[skill.name]" class="dimmer">保存中…</span>
              </div>
              <div v-if="skill.description" class="skill-desc">{{ skill.description }}</div>
              <div v-if="rowError[skill.name]" class="cell-err" style="margin-top: 5px">
                {{ rowError[skill.name] }}
              </div>
            </div>

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
      </AsyncBlock>
    </div>
  </div>
</template>
