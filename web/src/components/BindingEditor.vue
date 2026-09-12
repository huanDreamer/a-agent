<script setup>
// 能力绑定 — which model serves each media capability.
//
// This is the switch that makes the media tools exist: the tool that reads an
// image, generates one, transcribes audio or speaks text has no model of its
// own, it calls whatever is bound here. An unbound capability is therefore not
// a cosmetic gap — the corresponding tool is simply unavailable — and the panel
// says so in the row rather than leaving an empty select.
//
// Each row offers only the models that *declare* that capability, grouped by
// provider, plus a 清除 option that unbinds (PUT with a blank model).
import { computed, reactive } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import { api } from '../api.js'
import { BINDINGS } from '../llm.js'
import { useResource } from '../useResource.js'

const props = defineProps({
  /** Provider rows, for the optgroup labels. */
  providers: { type: Array, default: () => [] },
  /** Every model of every provider, with its declared capabilities. */
  models: { type: Array, default: () => [] },
})

const { data, status, error, reload } = useResource(() => api.llmBindings(), { watchRange: false })

const bindings = computed(() => {
  const list = data.value && data.value.bindings
  return Array.isArray(list) ? list : []
})

const bound = computed(() => {
  const map = new Map()
  for (const item of bindings.value) {
    if (item && item.capability && item.model_id) map.set(item.capability, item)
  }
  return map
})

const providerNames = computed(() => {
  const map = new Map()
  for (const provider of props.providers) {
    if (provider && provider.id) map.set(provider.id, provider.name || provider.id)
  }
  return map
})

/**
 * Candidate models for one capability, grouped by provider.
 *
 * A disabled model cannot serve a tool, so it is left out; a binding that points
 * at one of those still shows up as its own entry (see below) instead of the
 * select silently falling back to 未绑定.
 */
const optionSets = computed(() => {
  const out = {}
  for (const item of BINDINGS) {
    const groups = new Map()
    for (const model of props.models) {
      if (!model || model.enabled === false) continue
      const caps = Array.isArray(model.capabilities) ? model.capabilities : []
      if (!caps.includes(item.key)) continue
      const pid = model.provider_id || ''
      if (!groups.has(pid)) groups.set(pid, [])
      groups.get(pid).push(model)
    }
    const flat = []
    const grouped = []
    for (const [pid, list] of groups) {
      const options = list.map((model) => {
        const option = {
          key: `c${flat.length}`,
          provider_id: pid,
          model_id: model.model_id,
          label: model.display_name ? `${model.display_name}（${model.model_id}）` : model.model_id,
        }
        flat.push(option)
        return option
      })
      grouped.push({ provider: pid, label: providerNames.value.get(pid) || pid || '—', options })
    }

    const current = bound.value.get(item.key)
    if (current && !flat.some((o) => o.provider_id === current.provider_id && o.model_id === current.model_id)) {
      const label = `${current.provider_name || providerNames.value.get(current.provider_id) || current.provider_id || '—'} / ${current.display_name || current.model_id}`
      flat.unshift({
        key: 'current',
        provider_id: current.provider_id,
        model_id: current.model_id,
        label: `${label}（未声明该能力）`,
        stale: true,
      })
      grouped.unshift({
        provider: '__current__',
        label: '当前绑定',
        options: [flat[0]],
      })
    }
    out[item.key] = { flat, grouped }
  }
  return out
})

function optionsFor(capability) {
  const set = optionSets.value[capability]
  return set ? set.flat : []
}

function groupsFor(capability) {
  const set = optionSets.value[capability]
  return set ? set.grouped : []
}

/** Select value: the key of the bound option, '' when nothing is bound. */
function selectedKey(capability) {
  const current = bound.value.get(capability)
  if (!current) return ''
  const found = optionsFor(capability).find(
    (o) => o.provider_id === current.provider_id && o.model_id === current.model_id,
  )
  return found ? found.key : ''
}

function isBound(capability) {
  return bound.value.has(capability)
}

/** The bound pair as text, for the row summary. */
function boundLabel(capability) {
  const current = bound.value.get(capability)
  if (!current) return ''
  const provider = current.provider_name || providerNames.value.get(current.provider_id) || current.provider_id
  return `${provider || '—'} / ${current.display_name || current.model_id}`
}

const pending = reactive({})
const rowError = reactive({})

async function change(capability, key) {
  if (pending[capability]) return
  const option = optionsFor(capability).find((o) => o.key === key)
  pending[capability] = true
  rowError[capability] = ''
  try {
    // A blank model is the documented way to clear the binding.
    await api.saveLlmBinding({
      capability,
      provider_id: option ? option.provider_id : '',
      model_id: option ? option.model_id : '',
    })
    await reload()
  } catch (err) {
    if (err && err.status === 401) return
    rowError[capability] = (err && err.message) || '保存绑定失败，请重试'
    // Re-read so the select cannot show a binding that was never stored.
    await reload()
  } finally {
    delete pending[capability]
  }
}

const unboundCount = computed(() => BINDINGS.filter((item) => !isBound(item.key)).length)
</script>

<template>
  <div class="card">
    <div class="card-head">
      <div class="card-title">
        <span class="dot" />
        能力绑定
        <span class="card-sub">
          图像 / 语音能力各需要一个模型 ·
          <code class="md-code">/api/llm/bindings</code>
        </span>
      </div>
      <button type="button" class="btn ghost sm" :disabled="status === 'loading'" @click="reload()">
        <Icon name="refresh" :size="15" />
        刷新
      </button>
    </div>

    <AsyncBlock :state="status" :error="error" :skeleton-rows="4" @retry="reload">
      <div v-if="unboundCount" class="banner info" role="status">
        <Icon name="circle-alert" :size="16" />
        <span class="banner-text">
          有 {{ unboundCount }} 项能力未绑定，对应工具/功能在当前部署中不可用。可选项来自已声明该能力的模型。
        </span>
      </div>

      <div class="bind-list">
        <div v-for="item in BINDINGS" :key="item.key" class="bind-row">
          <div class="bind-text">
            <div class="set-name">{{ item.label }}</div>
            <div class="set-desc">{{ item.hint }}</div>
          </div>

          <div class="bind-pick">
            <select
              class="input bind-select"
              :value="selectedKey(item.key)"
              :aria-label="`${item.label} 使用的模型`"
              :disabled="Boolean(pending[item.key])"
              @change="change(item.key, $event.target.value)"
            >
              <option value="">未绑定（清除）</option>
              <optgroup v-for="group in groupsFor(item.key)" :key="group.provider" :label="group.label">
                <option v-for="option in group.options" :key="option.key" :value="option.key">
                  {{ option.label }}
                </option>
              </optgroup>
            </select>

            <span v-if="pending[item.key]" class="dimmer nowrap">保存中…</span>
            <span v-else-if="isBound(item.key)" class="tag ok bind-now" :title="boundLabel(item.key)">
              {{ boundLabel(item.key) }}
            </span>
            <span v-else class="tag bad">未绑定</span>
          </div>

          <div v-if="rowError[item.key]" class="cell-err bind-error">{{ rowError[item.key] }}</div>
        </div>
      </div>

      <p class="muted-note card-foot">
        选择器里只列出「声明了该能力」且未被停用的模型；如果某个模型的能力不准确，请到上面的模型列表里改。
        清空选择即解绑。
      </p>
    </AsyncBlock>
  </div>
</template>
