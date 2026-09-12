// Model vocabulary and catalog shaping shared by every model surface.
//
// This module is the one place that knows:
//
//   * the capability keys the API speaks (chat / vision / image_gen /
//     audio_transcribe / audio_speech / embedding) and their Chinese labels;
//   * the shape a capability binding has in the UI;
//   * how GET /api/chat/models is read — how its entries are grouped, named and
//     marked.
//
// The third point is why 对话's composer and 设置's model panels import from
// here rather than shaping the response themselves: both surfaces must group by
// provider, label a model and flag a missing API key *identically*, and two
// implementations of that would drift. The chat selector and 设置 read the same
// endpoint, and this module is what makes them show the same thing.
//
// The capability list of a downloaded model is a *guess inferred from the model
// name* by the server, so the panels say so out loud — treating it as fact is
// how a vision request ends up on a text-only model.

/** Every capability the contract defines, in the order the UI shows them. */
export const CAPABILITIES = [
  { key: 'chat', label: '对话' },
  { key: 'vision', label: '图像理解' },
  { key: 'image_gen', label: '图像生成' },
  { key: 'audio_transcribe', label: '语音转写' },
  { key: 'audio_speech', label: '语音合成' },
  { key: 'embedding', label: '向量嵌入' },
]

/**
 * The capabilities that can be bound to a model, and the tools they switch on.
 * `chat` and `embedding` are deliberately absent: they are properties of a
 * model, not of a tool that has to be routed somewhere.
 */
export const BINDINGS = [
  { key: 'vision', label: '图像理解', hint: '让模型能看图；未绑定将导致图片理解工具不可用' },
  { key: 'image_gen', label: '图像生成', hint: '让模型能画图；未绑定将导致图片生成工具不可用' },
  { key: 'audio_transcribe', label: '语音转写', hint: '把语音转成文字；未绑定将导致转写工具不可用' },
  { key: 'audio_speech', label: '语音合成', hint: '把文字读成语音；未绑定将导致朗读工具不可用' },
]

const LABEL_KEYS = CAPABILITIES.map((item) => item.key)

/** Keep the list to known keys, de-duplicated, in CAPABILITIES order. */
export function normalizeCapabilities(raw) {
  const list = Array.isArray(raw) ? raw.map((item) => String(item || '')) : []
  const known = LABEL_KEYS.filter((key) => list.includes(key))
  const extra = list.filter((key) => key !== '' && !LABEL_KEYS.includes(key))
  return [...known, ...new Set(extra)]
}

/**
 * Provider `id` is a slug: it is used as the key of the config map, in URLs and
 * in the model rows, so it must not contain anything that needs escaping.
 */
const ID_PATTERN = /^[a-z0-9][a-z0-9._-]*$/

export function checkProviderId(value) {
  const id = String(value || '').trim()
  if (!id) return '请填写 provider id'
  if (!ID_PATTERN.test(id)) return 'id 只能包含小写字母、数字、点、下划线和连字符，且以字母或数字开头'
  if (id.length > 64) return 'id 不能超过 64 个字符'
  return ''
}

/** base_url must be an absolute http(s) URL — the server appends /models to it. */
export function checkBaseUrl(value) {
  const url = String(value || '').trim()
  if (!url) return '请填写 base_url'
  let parsed = null
  try {
    parsed = new URL(url)
  } catch (err) {
    return 'base_url 必须是完整的绝对地址，例如 https://api.example.com/v1'
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    return 'base_url 只支持 http 或 https'
  }
  return ''
}

/* ----------------------------------------------------------- the catalog -- */

/**
 * One normalised entry of GET /api/chat/models `models`.
 *
 * A model is always determined by (provider, model): the provider is the store's
 * primary key and the model is the id sent upstream. An entry missing either is
 * dropped rather than rendered as a blank row.
 */
export function catalogEntry(raw) {
  if (!raw || typeof raw !== 'object') return null
  const provider = String(raw.provider || '')
  const model = String(raw.model || '')
  if (!provider || !model) return null
  return {
    provider,
    providerName: String(raw.provider_name || provider),
    model,
    displayName: String(raw.display_name || model),
    capabilities: normalizeCapabilities(raw.capabilities),
    isDefault: Boolean(raw.default),
    // A server that does not report the flag is assumed to have a key, so an
    // older response cannot make every model look unconfigured.
    hasKey: raw.has_api_key !== false,
    // Likewise for the chat mark: the endpoint only sends models it means to
    // offer, and chat_capable=false is a deliberate "kept, but flagged" state.
    chatCapable: raw.chat_capable !== false,
  }
}

/** One normalised entry of the catalog's `providers` array. */
export function providerChoice(raw) {
  if (!raw || typeof raw !== 'object') return null
  const id = String(raw.id || '')
  if (!id) return null
  return {
    id,
    name: String(raw.name || id),
    source: String(raw.source || ''),
    enabled: raw.enabled !== false,
    hasKey: raw.has_api_key !== false,
    modelCount: numberOr(raw.model_count, 0),
    enabledModelCount: numberOr(raw.enabled_model_count, 0),
    chatModelCount: numberOr(raw.chat_model_count, 0),
    // RFC3339 from the server, or '' when the list was never fetched.
    lastFetchedAt: raw.last_fetched_at || '',
    stale: Boolean(raw.stale),
    lastError: String(raw.last_error || ''),
  }
}

function numberOr(value, fallback) {
  const n = typeof value === 'number' ? value : Number(value)
  return Number.isFinite(n) ? n : fallback
}

/**
 * The whole catalog, normalised: the models in the order the surfaces render
 * them, the same set grouped by provider, and the provider summaries.
 *
 * Grouping and ordering are recomputed here rather than trusted to the response
 * order, so a change on the server cannot make the composer list providers in a
 * different sequence than 设置 does.
 */
export function catalogView(catalog) {
  const raw = catalog && typeof catalog === 'object' ? catalog : {}
  const models = (Array.isArray(raw.models) ? raw.models : []).map(catalogEntry).filter(Boolean)
  const providers = (Array.isArray(raw.providers) ? raw.providers : [])
    .map(providerChoice)
    .filter(Boolean)
  return { models: sortedModels(models), groups: groupByProvider(models), providers }
}

/** Sort by provider display name, then by model id: the order both surfaces use. */
export function sortedModels(models) {
  return [...models].sort((a, b) => {
    if (a.providerName !== b.providerName) return a.providerName < b.providerName ? -1 : 1
    if (a.provider !== b.provider) return a.provider < b.provider ? -1 : 1
    if (a.model !== b.model) return a.model < b.model ? -1 : 1
    return 0
  })
}

/** Group normalised models by provider, for a `<optgroup>` or a section list. */
export function groupByProvider(models) {
  const groups = new Map()
  for (const entry of sortedModels(models)) {
    if (!groups.has(entry.provider)) {
      groups.set(entry.provider, {
        provider: entry.provider,
        providerName: entry.providerName,
        hasKey: entry.hasKey,
        models: [],
      })
    }
    groups.get(entry.provider).models.push(entry)
  }
  return [...groups.values()].sort((a, b) => {
    if (a.providerName !== b.providerName) return a.providerName < b.providerName ? -1 : 1
    return a.provider < b.provider ? -1 : 1
  })
}

/**
 * What every surface prints for one model.
 *
 * The provider is NOT part of it: the model is always shown under its provider
 * (an `<optgroup>` label, a table's provider column), and repeating it on every
 * option is noise. The markers, on the other hand, must travel with the name —
 * they are the only warning a user gets before choosing something that cannot
 * work.
 */
export function modelOptionLabel(entry) {
  if (!entry) return ''
  let label = entry.displayName
  if (entry.isDefault) label += '（默认）'
  if (!entry.hasKey) label += '（未配置 API Key）'
  if (!entry.chatCapable) label += '（非对话模型）'
  return label
}

/** The tooltip for one model: its id when that differs from the display name. */
export function modelOptionHint(entry) {
  if (!entry) return ''
  const parts = []
  if (entry.displayName !== entry.model) parts.push(entry.model)
  if (entry.capabilities.length) {
    parts.push(
      `能力：${entry.capabilities.map((key) => capabilityLabel(key)).join('、')}（由模型名推断，请自行确认）`,
    )
  }
  if (!entry.hasKey) parts.push('该 provider 未配置 API Key，调用会失败')
  if (!entry.chatCapable) parts.push('该模型未声明「对话」能力，按名字推断可能有误，可在模型管理里修正')
  return parts.join(' · ')
}

/** The Chinese label of a capability key, falling back to the raw key. */
export function capabilityLabel(key) {
  const found = CAPABILITIES.find((item) => item.key === key)
  return found ? found.label : String(key || '')
}

/** The provider summaries, keyed by id, for the panels that show per-provider stats. */
export function providerStatsById(catalog) {
  const map = new Map()
  for (const provider of catalogView(catalog).providers) map.set(provider.id, provider)
  return map
}

/**
 * The providers whose cached model list is worth refetching: enabled, with a key
 * that could authenticate, and stale (never fetched, or older than the TTL).
 *
 * The UI uses this to refresh on opening 设置 and to say so when it does not.
 */
export function staleProviders(catalog) {
  return catalogView(catalog).providers.filter(
    (provider) => provider.enabled && provider.hasKey && provider.stale,
  )
}
