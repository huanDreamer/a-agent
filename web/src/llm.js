// Model-management vocabulary shared by the 模型管理 and 能力绑定 panels.
//
// Two things live here and nowhere else:
//
//   * the capability keys the API speaks (chat / vision / image_gen /
//     audio_transcribe / audio_speech / embedding) and their Chinese labels;
//   * the shape a capability binding has in the UI.
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
