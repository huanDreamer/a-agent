<script setup>
// 模型管理 — the writable half of 设置's model surface.
//
//   * the provider list (GET /api/llm/providers) with its add / edit form,
//     测试连接 and 刷新模型;
//   * the model list of the selected provider, whose capabilities are editable
//     (PUT /api/llm/models) — the capability chips a download produces are an
//     inference from the model name, so the UI says so and lets the user fix it;
//   * 能力绑定, the compact capability → provider/model editor that decides
//     whether the media tools exist at all;
//   * 刷新全部, which asks the server to refetch every enabled provider's model
//     list in one request.
//
// Read this together with 对话: both surfaces read GET /api/chat/models, so the
// model count, the last-refreshed time and the stale marker shown here are the
// same facts the composer's picker is built from — never a second derivation.
// Anything this panel displays about models therefore comes from that endpoint
// (via the chat store), while the provider *rows* still come from /api/llm/*
// because they carry what only an editor needs (base_url, source, key hint).
//
// Two rules this panel never breaks:
//
//   1. An API key is never displayed. The API does not return one; the form's
//      key field is write-only (empty, with 留空则保持不变 as its placeholder) and
//      a blank value is omitted from the request body rather than sent as "".
//   2. A provider with source=config is re-synced from the config file at
//      startup, so it is not editable here — the row says why instead of
//      offering actions that would be silently undone.
import { computed, onMounted, reactive, ref, watch } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import BindingEditor from './BindingEditor.vue'
import Icon from './Icon.vue'
import { api } from '../api.js'
import { CAPABILITIES, checkBaseUrl, checkProviderId, normalizeCapabilities } from '../llm.js'
import {
  catalogProviderStats,
  claimAutoRefresh,
  loadCatalog,
  refreshAllModels,
  staleCatalogProviders,
} from '../chatStore.js'
import { formatAbsolute, formatCount, formatRelative } from '../format.js'
import { useResource } from '../useResource.js'

/* ------------------------------------------------------------- resources -- */

const providersResource = useResource(() => api.llmProviders(), { watchRange: false })
const modelsResource = useResource(() => api.llmModels(), { watchRange: false })

const {
  data: providerData,
  status: providerStatus,
  error: providerError,
  loading: providerLoading,
  reload: reloadProviders,
} = providersResource
const {
  data: modelData,
  status: modelStatus,
  error: modelErrorAll,
  reload: reloadModels,
} = modelsResource

const providers = computed(() => {
  const list = providerData.value && providerData.value.providers
  return Array.isArray(list) ? list : []
})

/**
 * Every model of every provider, in one list.
 *
 * The 能力绑定 editor needs models from more than one provider at a time, and
 * the model table needs the subset of the selected one; fetching the whole list
 * once keeps the two views from ever disagreeing about a capability.
 */
const allModels = computed(() => {
  const list = modelData.value && modelData.value.models
  return Array.isArray(list) ? list : []
})

const selectedId = ref('')
const selected = computed(() => providers.value.find((p) => p.id === selectedId.value) || null)
const providerModels = computed(() => allModels.value.filter((m) => m.provider_id === selectedId.value))

/** Keep a valid selection as the list changes (first load, delete, rename). */
watch(providers, (list) => {
  if (list.some((p) => p && p.id === selectedId.value)) return
  const first = list[0]
  selectedId.value = first && first.id ? first.id : ''
})

/**
 * The catalog's per-provider facts: how many models the provider has, when its
 * list was last fetched, and whether it is stale. They come from
 * GET /api/chat/models, the same snapshot 对话's picker uses.
 */
function providerStats(id) {
  return catalogProviderStats.value.get(id) || null
}

/** A provider's display name, for a refresh report that only carries ids. */
function providerName(id) {
  const found = providers.value.find((p) => p && p.id === id)
  return (found && (found.name || found.id)) || id
}


/* ------------------------------------------------------------ row + form -- */

/** Per-provider action state: 'test' | 'refresh' | 'delete' | 'key' | 'toggle'. */
const busy = reactive({})
const rowError = reactive({})
const testResult = reactive({})
/** Provider ids awaiting a second click (destructive actions are two-step). */
const confirmDelete = ref('')
const confirmKey = ref('')
/** Transient confirmation line, mirroring the notice chip used elsewhere. */
const flash = ref('')
let flashTimer = null

const form = reactive({
  open: false,
  mode: 'add',
  id: '',
  name: '',
  base_url: '',
  api_key: '',
  api_key_env: '',
  kind: 'openai',
  enabled: true,
})
const formErrors = reactive({ id: '', base_url: '' })
const formError = ref('')
const saving = ref(false)

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

function isConfig(provider) {
  return Boolean(provider) && provider.source === 'config'
}

function isEnabled(provider) {
  return !provider || provider.enabled !== false
}

function sourceLabel(provider) {
  return isConfig(provider) ? '配置' : '自定义'
}

function openAdd() {
  form.mode = 'add'
  form.id = ''
  form.name = ''
  form.base_url = ''
  form.api_key = ''
  form.api_key_env = ''
  form.kind = 'openai'
  form.enabled = true
  formErrors.id = ''
  formErrors.base_url = ''
  formError.value = ''
  form.open = true
}

function openEdit(provider) {
  if (!provider || isConfig(provider)) return
  form.mode = 'edit'
  form.id = provider.id || ''
  form.name = provider.name || ''
  form.base_url = provider.base_url || ''
  form.api_key = '' // write-only: never prefilled, never read back
  form.api_key_env = provider.api_key_env || ''
  form.kind = provider.kind || 'openai'
  form.enabled = isEnabled(provider)
  formErrors.id = ''
  formErrors.base_url = ''
  formError.value = ''
  form.open = true
}

function closeForm() {
  form.open = false
  form.api_key = ''
  formError.value = ''
}

async function saveForm() {
  if (saving.value) return
  formErrors.id = form.mode === 'add' ? checkProviderId(form.id) : ''
  formErrors.base_url = checkBaseUrl(form.base_url)
  if (formErrors.id || formErrors.base_url) return

  const id = form.id.trim()
  const body = {
    id,
    name: form.name.trim() || id,
    base_url: form.base_url.trim(),
    api_key_env: form.api_key_env.trim(),
    kind: form.kind.trim(),
    enabled: form.enabled,
  }
  // Blank means "keep the stored key": the field is left out of the body, so an
  // edit can never wipe a key the user did not retype.
  if (form.api_key.trim() !== '') body.api_key = form.api_key.trim()

  saving.value = true
  formError.value = ''
  try {
    await api.saveLlmProvider(body)
    form.open = false
    form.api_key = ''
    selectedId.value = id
    setFlash(form.mode === 'add' ? `provider ${id} 已创建` : `provider ${id} 已保存`)
    await resync()
  } catch (err) {
    if (err && err.status === 401) return
    formError.value = errorText(err, '保存失败，请重试')
  } finally {
    saving.value = false
  }
}

/* --------------------------------------------------------- provider ops -- */

/**
 * The enabled toggle.
 *
 * `enabled` is not a route of its own; the provider upsert is the only body that
 * carries it. The request repeats the row's current fields and never an API key
 * — the server keeps the stored key when the field is absent. A provider whose
 * source is `config` may still refuse it, and then the reason is shown in the
 * row and the switch flips back.
 */
async function toggleEnabled(provider) {
  const id = provider.id
  if (busy[id]) return
  const next = !isEnabled(provider)
  const previous = isEnabled(provider)
  provider.enabled = next
  busy[id] = 'toggle'
  rowError[id] = ''

  try {
    await api.saveLlmProvider({
      id,
      name: provider.name || id,
      base_url: provider.base_url || '',
      api_key_env: provider.api_key_env || '',
      kind: provider.kind || 'openai',
      enabled: next,
    })
    // Enabling a provider changes what the catalog offers, so the composer's
    // selector must be reloaded too.
    await resync()
  } catch (err) {
    if (err && err.status === 401) return
    provider.enabled = previous
    rowError[id] = errorText(err, '切换启用状态失败')
  } finally {
    delete busy[id]
  }
}

async function testProvider(provider) {
  const id = provider.id
  if (busy[id]) return
  busy[id] = 'test'
  rowError[id] = ''
  testResult[id] = null
  try {
    const res = (await api.testLlmProvider(id)) || {}
    const ok = res.ok !== false
    testResult[id] = {
      ok,
      count: typeof res.models_count === 'number' ? res.models_count : null,
      error: res.error || '',
    }
    if (!ok && res.error) rowError[id] = res.error
  } catch (err) {
    if (err && err.status === 401) return
    testResult[id] = { ok: false, count: null, error: errorText(err, '测试失败') }
  } finally {
    delete busy[id]
  }
}

async function refreshModels(provider) {
  const id = provider.id
  if (busy[id]) return
  busy[id] = 'refresh'
  rowError[id] = ''
  try {
    const res = (await api.refreshLlmProviderModels(id)) || {}
    const count = Array.isArray(res.models) ? res.models.length : 0
    setFlash(`${provider.name || id}：已刷新 ${formatCount(count)} 个模型`)
    await resync()
  } catch (err) {
    if (err && err.status === 401) return
    rowError[id] = errorText(err, '刷新模型失败')
  } finally {
    delete busy[id]
  }
}

async function removeProvider(provider) {
  const id = provider.id
  if (busy[id]) return
  confirmDelete.value = ''
  busy[id] = 'delete'
  rowError[id] = ''
  try {
    await api.deleteLlmProvider(id)
    setFlash(`provider ${id} 已删除`)
    await resync()
  } catch (err) {
    if (err && err.status === 401) return
    rowError[id] = errorText(err, '删除失败，请重试')
  } finally {
    delete busy[id]
  }
}

/** PUT /key with an empty string is the documented way to clear a stored key. */
async function clearKey(provider) {
  const id = provider.id
  if (busy[id]) return
  confirmKey.value = ''
  busy[id] = 'key'
  rowError[id] = ''
  try {
    await api.setLlmProviderKey(id, '')
    setFlash(`provider ${id} 的密钥已清除`)
    // has_api_key is part of the catalog, so the picker's warning follows.
    await resync()
  } catch (err) {
    if (err && err.status === 401) return
    rowError[id] = errorText(err, '清除密钥失败')
  } finally {
    delete busy[id]
  }
}

/* ------------------------------------------------------------ model ops -- */

const modelBusy = reactive({})
const modelError = reactive({})
const addDraft = reactive({ model_id: '', display_name: '' })
const addError = ref('')

function modelKey(model) {
  return `${model.provider_id}/${model.model_id}`
}

function hasCapability(model, key) {
  return Array.isArray(model.capabilities) && model.capabilities.includes(key)
}

/**
 * Persist one model row. Called by the display-name field (on change), by the
 * capability chips (on click) and by the enabled switch.
 *
 * The whole record is sent every time: the endpoint replaces the row, so a
 * partial body would be a silent way to drop capabilities.
 */
async function saveModel(model, { quiet = false } = {}) {
  const key = modelKey(model)
  if (modelBusy[key]) return
  modelBusy[key] = 'save'
  modelError[key] = ''
  try {
    await api.saveLlmModel({
      provider_id: model.provider_id,
      model_id: model.model_id,
      display_name: model.display_name || '',
      capabilities: normalizeCapabilities(model.capabilities),
      enabled: model.enabled !== false,
    })
    if (!quiet) setFlash(`${model.model_id} 已保存`)
    // A capability change decides whether the chat offers this model, so the
    // catalog is reloaded even on the quiet path.
    await loadCatalog({ quiet: true })
  } catch (err) {
    if (err && err.status === 401) return
    modelError[key] = errorText(err, '保存失败，请重试')
    // The server is authoritative: reload so the row cannot show a state that
    // was never stored.
    await resync()
  } finally {
    delete modelBusy[key]
  }
}

function toggleCapability(model, cap) {
  const list = normalizeCapabilities(model.capabilities)
  model.capabilities = list.includes(cap.key) ? list.filter((k) => k !== cap.key) : [...list, cap.key]
  saveModel(model, { quiet: true })
}

function toggleModelEnabled(model) {
  model.enabled = model.enabled === false
  saveModel(model, { quiet: true })
}

async function removeModel(model) {
  const key = modelKey(model)
  if (modelBusy[key]) return
  modelBusy[key] = 'delete'
  modelError[key] = ''
  try {
    await api.deleteLlmModel(model.provider_id, model.model_id)
    await resync()
  } catch (err) {
    if (err && err.status === 401) return
    modelError[key] = errorText(err, '删除失败，请重试')
  } finally {
    delete modelBusy[key]
  }
}

async function addModel() {
  const modelId = addDraft.model_id.trim()
  if (!selectedId.value) {
    addError.value = '请先选择一个 provider'
    return
  }
  if (modelId === '') {
    addError.value = '请填写模型 ID'
    return
  }
  if (providerModels.value.some((m) => m.model_id === modelId)) {
    addError.value = '该 provider 下已经有同名模型'
    return
  }
  addError.value = ''
  modelBusy[`${selectedId.value}/${modelId}`] = 'save'
  try {
    // A hand-added model starts as a chat model; the chips next to it are the
    // place to declare anything else.
    await api.saveLlmModel({
      provider_id: selectedId.value,
      model_id: modelId,
      display_name: addDraft.display_name.trim(),
      capabilities: ['chat'],
      enabled: true,
    })
    addDraft.model_id = ''
    addDraft.display_name = ''
    setFlash(`${modelId} 已添加`)
    await resync()
  } catch (err) {
    if (err && err.status === 401) return
    addError.value = errorText(err, '添加失败，请重试')
  } finally {
    delete modelBusy[`${selectedId.value}/${modelId}`]
  }
}

function reloadAll() {
  reloadProviders()
  reloadModels()
  loadCatalog({ quiet: true })
}

/**
 * Reload every model surface after a write.
 *
 * The catalog is included on purpose: it is what 对话's picker and the counts on
 * these rows both read, so a provider or model change that skipped it would
 * leave the composer offering something the server no longer serves.
 */
async function resync() {
  await Promise.all([
    reloadProviders(),
    reloadModels(),
    // `quiet` keeps a background resync from flashing the loading state.
    loadCatalog({ quiet: true }),
  ])
}

/* --------------------------------------------------------- refresh (all) -- */

/**
 * Per-provider outcome of the last 刷新全部, and of the automatic pass:
 * `{provider_id, ok, models_count, error}` straight from the server.
 *
 * A failed provider is a result to show, never an exception: a pass that
 * refreshed seven of eight providers succeeded, and the eighth's reason is the
 * useful part.
 */
const refresh = reactive({
  running: false,
  results: [],
  error: '',
  /** True when the pass was started by opening 设置 rather than by a click. */
  automatic: false,
})

const refreshOk = computed(() => refresh.results.filter((r) => r && r.ok).length)
const refreshFailed = computed(() => refresh.results.filter((r) => r && !r.ok).length)

function describeRefresh(result) {
  const name = providerName(result.provider_id)
  if (result && result.ok) return `${name}：已刷新 ${formatCount(result.models_count)} 个模型`
  return `${name}：刷新失败 — ${(result && result.error) || '请检查 base_url 与密钥'}`
}

/**
 * Refresh every enabled provider that has an API key, then reload the catalog.
 *
 * One pass at a time: the button is disabled while a pass runs, so an automatic
 * pass can never pile a second one on top.
 */
async function runRefreshAll({ automatic = false } = {}) {
  if (refresh.running) return
  refresh.running = true
  refresh.error = ''
  refresh.automatic = automatic
  try {
    const results = await refreshAllModels()
    refresh.results = results
    setFlash(
      results.length === 0
        ? '没有可刷新的 provider（需已启用且已配置 API Key）'
        : `已刷新 ${refreshOk.value} 个 provider${refreshFailed.value ? `，${refreshFailed.value} 个失败` : ''}`,
    )
    // refreshAllModels already reloaded the catalog; the panels reload here.
    await Promise.all([reloadProviders(), reloadModels()])
  } catch (err) {
    if (err && err.status === 401) return
    refresh.error = errorText(err, '刷新全部失败，请重试')
    refresh.results = []
  } finally {
    refresh.running = false
  }
}

function dismissRefresh() {
  refresh.results = []
  refresh.error = ''
  refresh.automatic = false
}

/**
 * An automatic refresh for every provider that turns out to be stale, once per
 * provider per page load.
 *
 * Opening 设置 is the moment a user wants current model lists, and the server's
 * own pass only runs at startup — so a console left open for days would keep
 * showing yesterday's list. The claim in the chat store is what keeps it
 * bounded: a provider already attempted is never attempted again (so re-entering
 * 设置, or a provider that keeps failing, cannot start a loop), and a pass never
 * starts while another one is running. The 刷新全部 button is always there for a
 * deliberate refresh.
 */
function maybeAutoRefresh() {
  if (refresh.running) return
  if (!claimAutoRefresh().length) return
  runRefreshAll({ automatic: true })
}

onMounted(() => {
  // On a cold start the catalog may still be in flight; the watcher below picks
  // it up the moment it arrives.
  maybeAutoRefresh()
})

watch(() => staleCatalogProviders.value.length, maybeAutoRefresh)

function selectProvider(provider) {
  selectedId.value = provider.id
  confirmDelete.value = ''
  confirmKey.value = ''
}

/** Inferred capabilities are a guess; the note under the table says so. */
const inferredCount = computed(
  () => providerModels.value.filter((m) => m.source === 'fetched').length,
)
</script>

<template>
  <div class="card">
    <div class="card-head">
      <div class="card-title">
        <span class="dot" />
        模型管理
        <span class="card-sub">
          provider、模型能力与绑定 · 写操作来自
          <code class="md-code">/api/llm/*</code>
        </span>
        <span v-if="flash" class="chip ok">{{ flash }}</span>
      </div>
      <div class="row">
        <button
          type="button"
          class="btn sm"
          :disabled="refresh.running"
          title="重新拉取所有已启用且已配置密钥的 provider 的模型列表"
          @click="runRefreshAll()"
        >
          <Icon name="refresh" :size="15" />
          {{ refresh.running ? '刷新中…' : '刷新全部' }}
        </button>
        <button
          type="button"
          class="btn ghost sm"
          :disabled="providerLoading"
          @click="reloadAll"
        >
          <Icon name="refresh" :size="15" />
          重新读取
        </button>
        <button type="button" class="btn primary sm" @click="openAdd">
          <Icon name="plus" :size="15" />
          添加 provider
        </button>
      </div>
    </div>

    <!-- the per-provider report of the last 刷新全部 (manual or automatic) -->
    <div v-if="refresh.error" class="banner error" role="alert">
      <Icon name="circle-alert" :size="16" />
      <span class="banner-text">{{ refresh.error }}</span>
      <button type="button" class="btn sm" @click="runRefreshAll()">重试</button>
      <button type="button" class="btn sm ghost" @click="dismissRefresh">关闭</button>
    </div>

    <div v-else-if="refresh.results.length" class="refresh-report" role="status">
      <div class="refresh-head">
        <Icon name="circle-check" :size="14" />
        <span>
          {{ refresh.automatic ? '已自动刷新过期的模型列表' : '刷新全部完成' }} ·
          成功 {{ formatCount(refreshOk) }} 个<template v-if="refreshFailed">，失败
            {{ formatCount(refreshFailed) }} 个</template>
        </span>
        <span class="spacer" />
        <button type="button" class="btn sm ghost" @click="dismissRefresh">关闭</button>
      </div>
      <ul class="refresh-list">
        <li
          v-for="result in refresh.results"
          :key="result.provider_id"
          class="refresh-item"
          :class="result.ok ? 'ok' : 'bad'"
        >
          <Icon :name="result.ok ? 'circle-check' : 'circle-alert'" :size="13" />
          {{ describeRefresh(result) }}
        </li>
      </ul>
    </div>

    <AsyncBlock
      :state="providerStatus"
      :error="providerError"
      :skeleton-rows="3"
      @retry="reloadAll"
    >
      <div v-if="!providers.length" class="empty">
        <div class="empty-ico" aria-hidden="true">◍</div>
        <div class="empty-text">还没有任何 provider</div>
        <div class="empty-hint">
          配置文件里的 llm.providers 会在这里显示为「配置」来源；也可以直接添加一个自定义
          provider。
        </div>
      </div>

      <div v-else class="prov-list">
        <div
          v-for="provider in providers"
          :key="provider.id"
          class="prov-row"
          :class="{ selected: provider.id === selectedId, off: !isEnabled(provider) }"
        >
          <button
            type="button"
            class="prov-pick"
            :title="`查看 ${provider.name || provider.id} 的模型`"
            @click="selectProvider(provider)"
          >
            <span class="prov-name">
              {{ provider.name || provider.id }}
              <!-- The id is the config key and appears in URLs, so it stays
                   visible whenever it is not simply the name again. -->
              <span v-if="provider.name && provider.name !== provider.id" class="tag mono">
                {{ provider.id }}
              </span>
              <span class="tag" :class="isConfig(provider) ? 'blue' : 'purple'">
                {{ sourceLabel(provider) }}
              </span>
              <span v-if="!isEnabled(provider)" class="tag">已停用</span>
            </span>
            <span class="prov-url mono">{{ provider.base_url || '—' }}</span>
          </button>

          <div class="prov-status">
            <span v-if="provider.has_api_key" class="tag ok" :title="`密钥 …${provider.api_key_hint || ''}`">
              已配置{{ provider.api_key_hint ? ` …${provider.api_key_hint}` : '' }}
            </span>
            <span v-else class="tag bad" title="没有可用的 API Key，调用会失败">未配置密钥</span>
            <span v-if="provider.api_key_env" class="tag mono">{{ provider.api_key_env }}</span>

            <!-- Model count and freshness come from GET /api/chat/models, the
                 same snapshot 对话's picker uses, so this row and the composer
                 cannot disagree about what a provider offers. -->
            <template v-if="providerStats(provider.id)">
              <span class="tag" :title="`其中 ${formatCount(providerStats(provider.id).chatModelCount)} 个可用于对话`">
                {{ formatCount(providerStats(provider.id).modelCount) }} 个模型
              </span>
              <span
                v-if="providerStats(provider.id).lastFetchedAt"
                class="tag"
                :title="`最近一次抓取：${formatAbsolute(providerStats(provider.id).lastFetchedAt)}`"
              >
                已刷新 {{ formatRelative(providerStats(provider.id).lastFetchedAt) }}
              </span>
              <span v-else class="tag">尚未抓取模型列表</span>
              <span
                v-if="providerStats(provider.id).stale"
                class="tag warn"
                title="模型列表已过期，点「刷新模型」或「刷新全部」重新抓取"
              >
                列表可能过期
              </span>
            </template>
          </div>

          <div class="prov-actions">
            <button
              type="button"
              class="switch"
              :class="{ on: isEnabled(provider) }"
              role="switch"
              :aria-checked="isEnabled(provider)"
              :aria-label="`${isEnabled(provider) ? '停用' : '启用'} ${provider.name || provider.id}`"
              :disabled="Boolean(busy[provider.id])"
              @click="toggleEnabled(provider)"
            >
              <span class="knob" />
            </button>

            <button
              type="button"
              class="btn sm"
              :disabled="Boolean(busy[provider.id])"
              title="请求该 provider 的模型列表，验证地址与密钥"
              @click="testProvider(provider)"
            >
              <Icon :name="busy[provider.id] === 'test' ? 'refresh' : 'plug'" :size="14" />
              测试连接
            </button>
            <button
              type="button"
              class="btn sm"
              :disabled="Boolean(busy[provider.id])"
              title="拉取 GET {base_url}/models 并推断能力"
              @click="refreshModels(provider)"
            >
              <Icon name="refresh" :size="14" />
              刷新模型
            </button>

            <template v-if="!isConfig(provider)">
              <button type="button" class="btn sm" @click="openEdit(provider)">
                <Icon name="pencil" :size="14" />
                编辑
              </button>
              <template v-if="confirmKey === provider.id">
                <span class="dimmer nowrap">清除已存密钥？</span>
                <button type="button" class="btn sm danger-text" @click="clearKey(provider)">清除</button>
                <button type="button" class="btn sm ghost" @click="confirmKey = ''">取消</button>
              </template>
              <button
                v-else-if="provider.has_api_key"
                type="button"
                class="btn sm ghost"
                title="删除数据库中保存的密钥（api_key_env 指向的环境变量不受影响）"
                @click="confirmKey = provider.id"
              >
                清除密钥
              </button>

              <template v-if="confirmDelete === provider.id">
                <span class="dimmer nowrap">确认删除？</span>
                <button type="button" class="btn sm danger" @click="removeProvider(provider)">删除</button>
                <button type="button" class="btn sm ghost" @click="confirmDelete = ''">取消</button>
              </template>
              <button
                v-else
                type="button"
                class="btn sm ghost danger-text"
                @click="confirmDelete = provider.id"
              >
                <Icon name="trash" :size="14" />
                删除
              </button>
            </template>
          </div>

          <div v-if="isConfig(provider)" class="prov-note cell-note">
            <Icon name="circle-alert" :size="13" />
            来自配置文件（<code class="md-code">llm.providers.{{ provider.id }}</code>），字段会在服务启动时重新同步，
            因此不能在这里编辑或删除；请改配置文件后重启服务。
          </div>

          <div v-if="provider.last_error" class="prov-error cell-err">
            最近一次调用失败：{{ provider.last_error }}
          </div>

          <div
            v-if="testResult[provider.id]"
            class="prov-test"
            :class="testResult[provider.id].ok ? 'ok' : 'bad'"
            role="status"
          >
            <Icon :name="testResult[provider.id].ok ? 'circle-check' : 'circle-alert'" :size="14" />
            <template v-if="testResult[provider.id].ok">
              连接成功<template v-if="testResult[provider.id].count !== null">
                ，共 {{ formatCount(testResult[provider.id].count) }} 个模型</template>
            </template>
            <template v-else>
              连接失败：{{ testResult[provider.id].error || '请检查 base_url 与密钥' }}
            </template>
          </div>

          <div v-if="rowError[provider.id]" class="cell-err prov-error">{{ rowError[provider.id] }}</div>
            </div>
      </div>
    </AsyncBlock>

    <!-- add / edit form -->
    <form v-if="form.open" class="prov-form" @submit.prevent="saveForm">
      <div class="prov-form-head">
        {{ form.mode === 'add' ? '添加 provider' : `编辑 ${form.id}` }}
        <span class="card-sub">
          {{
            form.mode === 'add'
              ? 'id 会被用作配置键，创建后不可修改'
              : '留空的字段保持不变'
          }}
        </span>
        <span class="spacer" />
        <span v-if="saving" class="dimmer nowrap">保存中…</span>
        <button type="button" class="btn sm ghost" @click="closeForm">取消</button>
      </div>

      <div class="form-grid">
        <label class="field">
          <span class="field-label">id</span>
          <input
            v-model="form.id"
            class="input mono"
            type="text"
            placeholder="my-provider"
            :disabled="form.mode === 'edit'"
            autocomplete="off"
            aria-label="provider id"
          />
          <span v-if="formErrors.id" class="field-error">{{ formErrors.id }}</span>
        </label>

        <label class="field">
          <span class="field-label">名称</span>
          <input
            v-model="form.name"
            class="input"
            type="text"
            placeholder="显示名称，留空则用 id"
            autocomplete="off"
            aria-label="provider 名称"
          />
        </label>

        <label class="field field-wide">
          <span class="field-label">base_url</span>
          <input
            v-model="form.base_url"
            class="input mono"
            type="text"
            placeholder="https://api.example.com/v1"
            autocomplete="off"
            aria-label="provider base_url"
          />
          <span v-if="formErrors.base_url" class="field-error">{{ formErrors.base_url }}</span>
          <span v-else class="field-hint">服务端会请求 <code class="md-code">{base_url}/models</code></span>
        </label>

        <label class="field">
          <span class="field-label">api_key</span>
          <input
            v-model="form.api_key"
            class="input mono"
            type="password"
            :placeholder="form.mode === 'edit' ? '留空则保持不变' : '可留空，改用下面的环境变量'"
            autocomplete="new-password"
            aria-label="api_key"
          />
          <span class="field-hint">只写不读：服务端不会返回密钥，这里也不会显示已保存的值</span>
        </label>

        <label class="field">
          <span class="field-label">api_key_env</span>
          <input
            v-model="form.api_key_env"
            class="input mono"
            type="text"
            placeholder="DEEPSEEK_API_KEY"
            autocomplete="off"
            aria-label="api_key_env"
          />
          <span class="field-hint">推荐：引用环境变量，密钥就不会落进数据库</span>
        </label>

        <label class="field">
          <span class="field-label">kind</span>
          <input
            v-model="form.kind"
            class="input mono"
            type="text"
            placeholder="openai"
            autocomplete="off"
            aria-label="provider kind"
          />
          <span class="field-hint">协议类型，默认 openai</span>
        </label>

        <label class="field field-inline">
          <span class="field-label">启用</span>
          <span class="row">
            <button
              type="button"
              class="switch"
              :class="{ on: form.enabled }"
              role="switch"
              :aria-checked="form.enabled"
              aria-label="启用该 provider"
              @click="form.enabled = !form.enabled"
            >
              <span class="knob" />
            </button>
            <span class="muted-note">{{ form.enabled ? '已启用' : '已停用' }}</span>
          </span>
        </label>
      </div>

      <div v-if="formError" class="banner error" role="alert">
        <Icon name="circle-alert" :size="16" />
        <span class="banner-text">{{ formError }}</span>
      </div>

      <div class="row prov-form-foot">
        <button type="submit" class="btn primary" :disabled="saving">
          {{ form.mode === 'add' ? '创建' : '保存' }}
        </button>
        <span class="muted-note">
          保存后可以点「测试连接」确认地址与密钥是否可用。
        </span>
      </div>
    </form>

    <!-- models of the selected provider -->
    <section v-if="selected" class="models">
      <div class="card-head models-head">
        <div class="card-title">
          <span class="dot" />
          {{ selected.name || selected.id }} 的模型
          <span class="card-sub">
            共 {{ formatCount(providerModels.length) }} 个 · 能力由模型名推断，请自行确认
          </span>
        </div>
        <button
          type="button"
          class="btn ghost sm"
          :disabled="Boolean(busy[selected.id])"
          @click="refreshModels(selected)"
        >
          <Icon name="refresh" :size="15" />
          刷新模型
        </button>
      </div>

      <AsyncBlock
        :state="modelStatus"
        :error="modelErrorAll"
        :skeleton-rows="3"
        @retry="reloadModels"
      >
        <div v-if="!providerModels.length" class="empty">
          <div class="empty-ico" aria-hidden="true">◍</div>
          <div class="empty-text">该 provider 下还没有模型</div>
          <div class="empty-hint">
            点「刷新模型」从 <code class="md-code">{base_url}/models</code> 拉取，或在下面手动添加一个。
          </div>
        </div>

        <div v-else class="model-list">
          <div v-for="model in providerModels" :key="model.model_id" class="model-row">
            <span class="model-id mono" :title="model.model_id">{{ model.model_id }}</span>

            <input
              v-model="model.display_name"
              class="input model-name"
              type="text"
              :placeholder="model.model_id"
              :aria-label="`${model.model_id} 的显示名`"
              @change="saveModel(model)"
              @keyup.enter="$event.target.blur()"
            />

            <span class="model-caps">
              <button
                v-for="cap in CAPABILITIES"
                :key="cap.key"
                type="button"
                class="cap-chip"
                :class="{ on: hasCapability(model, cap.key) }"
                :aria-pressed="hasCapability(model, cap.key)"
                :title="`${hasCapability(model, cap.key) ? '取消' : '声明'}能力：${cap.label}`"
                @click="toggleCapability(model, cap)"
              >
                {{ cap.label }}
              </button>
            </span>

            <span class="model-flags">
              <span class="tag" :class="model.source === 'fetched' ? 'blue' : 'purple'">
                {{ model.source === 'fetched' ? '抓取' : '自定义' }}
              </span>
              <button
                type="button"
                class="switch"
                :class="{ on: model.enabled !== false }"
                role="switch"
                :aria-checked="model.enabled !== false"
                :aria-label="`${model.enabled !== false ? '停用' : '启用'}模型 ${model.model_id}`"
                :disabled="Boolean(modelBusy[modelKey(model)])"
                @click="toggleModelEnabled(model)"
              >
                <span class="knob" />
              </button>
              <button
                type="button"
                class="btn sm ghost danger-text icon-btn"
                :title="`删除模型 ${model.model_id}`"
                :aria-label="`删除模型 ${model.model_id}`"
                :disabled="Boolean(modelBusy[modelKey(model)])"
                @click="removeModel(model)"
              >
                <Icon name="trash" :size="14" />
              </button>
              <span v-if="modelBusy[modelKey(model)] === 'save'" class="dimmer nowrap">保存中…</span>
            </span>

            <div v-if="modelError[modelKey(model)]" class="cell-err model-error">
              {{ modelError[modelKey(model)] }}
            </div>
          </div>
        </div>
      </AsyncBlock>

      <div class="model-add">
        <input
          v-model="addDraft.model_id"
          class="input mono model-add-id"
          type="text"
          placeholder="模型 ID，例如 gpt-4o-mini"
          autocomplete="off"
          aria-label="新增模型 ID"
        />
        <input
          v-model="addDraft.display_name"
          class="input model-add-name"
          type="text"
          placeholder="显示名（可留空）"
          autocomplete="off"
          aria-label="新增模型显示名"
          @keyup.enter="addModel"
        />
        <button type="button" class="btn sm" @click="addModel">
          <Icon name="plus" :size="14" />
          添加模型
        </button>
      </div>
      <p v-if="addError" class="cell-err">{{ addError }}</p>
      <p class="muted-note card-foot">
        手动添加的模型默认只勾选「对话」，其余能力请点上面的能力标签自行声明。
        <template v-if="inferredCount">
          当前列表里有 {{ formatCount(inferredCount) }} 个模型的能力是抓取时按名字推断的。
        </template>
      </p>
    </section>
  </div>

  <BindingEditor :providers="providers" :models="allModels" />
</template>
