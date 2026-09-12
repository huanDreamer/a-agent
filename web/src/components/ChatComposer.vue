<script setup>
// Message composer — one integrated control, the way a modern chat client does
// it: a single large rounded container holding the pending attachment chips, the
// textarea, the model selector at its bottom-left and the send (or 停止) button
// at its bottom-right. The container lights up with a focus ring while the
// textarea has focus, and again — more strongly — while a file is dragged over
// it.
//
// Files arrive three ways and all three end in the same place: the hidden file
// input (📎), a `paste` event carrying `clipboardData.files` (the screenshot
// route, and the one that matters most day to day), and drag & drop. Only
// `image/*` and `audio/*` are accepted; anything else is refused with a notice
// rather than silently dropped.
//
// Enter sends, Shift+Enter inserts a newline; Enter is ignored while an IME
// composition is active, otherwise confirming a Chinese candidate would send
// the message. A message may carry attachments only, with no text at all.
import { computed, nextTick, ref, watch } from 'vue'
import Icon from './Icon.vue'
import { assetUrl, uploadAttachment } from '../api.js'
import { remember } from '../attachments.js'
import { changeModel, chat, modelOptionGroups, selectedModelKey } from '../chatStore.js'
import { formatBytes } from '../format.js'

const props = defineProps({
  /** No conversation selected: the composer is inert and explains why. */
  disabled: { type: Boolean, default: false },
  streaming: { type: Boolean, default: false },
  /** Text of a failed send, restored so the user does not retype it. */
  pending: { type: String, default: '' },
  /** Attachments of that failed send, restored with the text. */
  pendingAttachments: { type: Array, default: () => [] },
})

const emit = defineEmits(['send', 'stop'])

/** Resting height ≈ 4 lines; grows with the content, then scrolls. */
const MIN_HEIGHT = 96
const MAX_HEIGHT = 260
/** How long a rejection notice stays up. */
const NOTICE_MS = 3200

const value = ref('')
const composing = ref(false)
const area = ref(null)
const picker = ref(null)
/** [{ key, name, size, kind, mime, url, id, status, progress, error }] */
const pendingFiles = ref([])
const uploading = computed(() => pendingFiles.value.some((f) => f.status === 'uploading'))
const readyFiles = computed(() => pendingFiles.value.filter((f) => f.status === 'ready'))
const dragging = ref(false)
const notice = ref('')

const canSend = computed(
  () =>
    !props.disabled &&
    !props.streaming &&
    !uploading.value &&
    (value.value.trim() !== '' || readyFiles.value.length > 0),
)
const groups = computed(() => modelOptionGroups.value)
const modelKey = computed(() => selectedModelKey.value)
const pickerDisabled = computed(() => props.disabled || props.streaming)
const attachDisabled = computed(() => props.disabled || props.streaming || !chat.activeId)
const hint = computed(() => {
  if (props.disabled) return '请先新建或选择一个对话'
  if (props.streaming) return '正在生成，可点击「停止」中断本轮'
  if (uploading.value) return '正在上传附件…'
  return 'Enter 发送 · Shift+Enter 换行 · 可粘贴或拖入图片 / 音频'
})

let noticeTimer = null
let keySeq = 0

function setNotice(text) {
  notice.value = text
  if (noticeTimer) window.clearTimeout(noticeTimer)
  noticeTimer = window.setTimeout(() => {
    noticeTimer = null
    notice.value = ''
  }, NOTICE_MS)
}

function autosize() {
  const el = area.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = `${Math.min(MAX_HEIGHT, Math.max(MIN_HEIGHT, el.scrollHeight))}px`
}

function reset() {
  value.value = ''
  pendingFiles.value = []
  nextTick(autosize)
}

function submit() {
  if (!canSend.value) return
  const text = value.value.trim()
  const attachments = readyFiles.value.map((file) => file.record)
  emit('send', text, attachments)
  reset()
}

function onKeydown(event) {
  if (event.key !== 'Enter' || event.shiftKey) return
  if (composing.value || event.isComposing) return
  event.preventDefault()
  submit()
}

function onModelChange(event) {
  changeModel(event.target.value)
}

function focus() {
  if (area.value) area.value.focus()
}

/* ------------------------------------------------------------- attachments -- */

/** Accepted mime families. Everything else is refused with a notice. */
function accepts(file) {
  const type = String((file && file.type) || '').toLowerCase()
  return type.startsWith('image/') || type.startsWith('audio/')
}

/** A visible name for the rejection notice: the mime, or the extension. */
function describe(file) {
  const type = String((file && file.type) || '').toLowerCase()
  if (type) return type
  const name = String((file && file.name) || '')
  const dot = name.lastIndexOf('.')
  return dot > 0 ? `.${name.slice(dot + 1)}` : '未知类型'
}

/** A chip for an attachment the server already stored (uploaded, or restored
    from a failed send — the bytes are on disk, so it is ready immediately). */
function readyChip(attachment) {
  remember(attachment)
  keySeq += 1
  return {
    key: `att-${keySeq}`,
    name: attachment.name || attachment.id,
    size: typeof attachment.bytes === 'number' ? attachment.bytes : null,
    kind: attachment.kind || '',
    mime: attachment.mime || '',
    url: attachment.url ? assetUrl(attachment) : '',
    id: attachment.id,
    record: attachment,
    status: 'ready',
    progress: 100,
    error: '',
  }
}

/**
 * Upload one accepted file.
 *
 * The chip appears immediately in the 上传中 state, so a slow upload is visible;
 * a failure replaces it with the server's reason and removes the chip from the
 * sendable set (nothing stuck, nothing left selected).
 */
async function upload(file) {
  const sessionId = chat.activeId
  if (!sessionId) {
    setNotice('请先新建或选择一个对话，再添加附件')
    return
  }
  keySeq += 1
  const chip = {
    key: `att-${keySeq}`,
    name: file.name || '附件',
    size: typeof file.size === 'number' ? file.size : null,
    kind: String(file.type || '').startsWith('audio/') ? 'audio' : 'image',
    mime: file.type || '',
    url: '',
    id: '',
    record: null,
    status: 'uploading',
    progress: 0,
    error: '',
  }
  pendingFiles.value = [...pendingFiles.value, chip]

  try {
    const attachment = await uploadAttachment(sessionId, file, {
      onProgress: (ratio) => {
        chip.progress = Math.round(Math.min(1, Math.max(0, ratio)) * 100)
        pendingFiles.value = [...pendingFiles.value]
      },
    })
    chip.id = attachment.id
    chip.url = attachment.url ? assetUrl(attachment) : ''
    chip.name = attachment.name || chip.name
    chip.size = typeof attachment.bytes === 'number' ? attachment.bytes : chip.size
    chip.kind = attachment.kind || chip.kind
    chip.mime = attachment.mime || chip.mime
    chip.record = attachment
    chip.status = 'ready'
    chip.progress = 100
    remember(attachment)
    // The list itself is replaced so the shallow-ref array re-renders.
    pendingFiles.value = [...pendingFiles.value]
  } catch (err) {
    if (err && err.status === 401) {
      remove(chip)
      return
    }
    chip.status = 'failed'
    chip.error = (err && err.message) || '上传失败，请重试'
    pendingFiles.value = [...pendingFiles.value]
    setNotice(chip.error)
  }
}

/** Hand a FileList / array of File to the uploader, reporting refusals. */
function acceptFiles(files) {
  const list = [...(files || [])]
  if (!list.length) return
  const refused = []
  for (const file of list) {
    if (accepts(file)) upload(file)
    else refused.push(describe(file))
  }
  if (refused.length) {
    setNotice(`仅支持图片或音频，已忽略：${refused.slice(0, 3).join('、')}${refused.length > 3 ? ' 等' : ''}`)
  }
}

/** Removing is local only: the file stays on the server (there is no delete route). */
function remove(chip) {
  pendingFiles.value = pendingFiles.value.filter((file) => file !== chip)
}

function pickFiles() {
  if (attachDisabled.value) return
  if (picker.value) picker.value.click()
}

function onPicked(event) {
  acceptFiles(event.target.files)
  // Allow picking the same file again after removing its chip.
  event.target.value = ''
}

function onPaste(event) {
  if (attachDisabled.value) return
  const data = event.clipboardData
  if (!data) return
  const files = data.files && data.files.length ? [...data.files] : []
  if (!files.length) {
    // Some browsers expose a pasted screenshot only through `items`.
    for (const item of [...(data.items || [])]) {
      if (item.kind === 'file') {
        const file = item.getAsFile()
        if (file) files.push(file)
      }
    }
  }
  if (files.length) {
    event.preventDefault()
    acceptFiles(files)
  }
}

/* Drag & drop. `dragDepth` counts enter/leave pairs: without it, moving over a
   child element fires `dragleave` and the drop target flickers off. */
let dragDepth = 0

function onDragEnter(event) {
  if (attachDisabled.value) return
  event.preventDefault()
  dragDepth += 1
  dragging.value = true
}

function onDragOver(event) {
  // The default is always prevented — even while the composer is inert — so a
  // file dropped on it can never navigate the tab to that file.
  event.preventDefault()
  if (attachDisabled.value) return
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy'
}

function onDragLeave(event) {
  event.preventDefault()
  dragDepth = Math.max(0, dragDepth - 1)
  if (dragDepth === 0) dragging.value = false
}

function onDrop(event) {
  event.preventDefault()
  dragDepth = 0
  dragging.value = false
  if (attachDisabled.value) return
  const data = event.dataTransfer
  acceptFiles(data && data.files)
}

watch(
  [() => props.pending, () => props.pendingAttachments],
  ([text, attachments]) => {
    if (!text && !attachments.length) return
    value.value = text || ''
    // A failed send gives its attachments back, already uploaded: they become
    // pending chips again instead of being uploaded a second time.
    pendingFiles.value = attachments.map(readyChip)
    nextTick(() => {
      autosize()
      focus()
    })
  },
)

defineExpose({ focus })
</script>

<template>
  <form class="composer" @submit.prevent="submit">
    <div
      class="composer-box"
      :class="{ dropping: dragging }"
      @dragenter="onDragEnter"
      @dragover="onDragOver"
      @dragleave="onDragLeave"
      @drop="onDrop"
    >
      <!-- pending attachments: thumbnail for images, name + size for audio -->
      <div v-if="pendingFiles.length" class="composer-files">
        <div
          v-for="file in pendingFiles"
          :key="file.key"
          class="composer-file"
          :class="{ failed: file.status === 'failed' }"
        >
          <img
            v-if="file.kind === 'image' && file.url && file.status === 'ready'"
            class="composer-file-thumb"
            :src="file.url"
            :alt="file.name"
          />
          <span v-else class="composer-file-ico" aria-hidden="true">
            <Icon :name="file.kind === 'audio' ? 'headphones' : 'paperclip'" :size="14" />
          </span>

          <span class="composer-file-text">
            <span class="composer-file-name">{{ file.name }}</span>
            <span class="composer-file-meta">
              <template v-if="file.status === 'uploading'">上传中 {{ file.progress }}%</template>
              <template v-else-if="file.status === 'failed'">{{ file.error }}</template>
              <template v-else>{{ formatBytes(file.size) }}</template>
            </span>
          </span>

          <span v-if="file.status === 'uploading'" class="composer-file-bar" aria-hidden="true">
            <span class="composer-file-bar-fill" :style="{ width: `${file.progress}%` }" />
          </span>

          <button
            type="button"
            class="composer-file-x"
            :title="`移除 ${file.name}`"
            :aria-label="`移除 ${file.name}`"
            @click="remove(file)"
          >
            <Icon name="x" :size="13" />
          </button>
        </div>
      </div>

      <span v-if="notice" class="composer-notice" role="status">
        <Icon name="circle-alert" :size="14" />
        {{ notice }}
      </span>

      <textarea
        ref="area"
        v-model="value"
        class="composer-input"
        rows="1"
        :placeholder="disabled ? '请先新建或选择一个对话…' : '输入消息…（可粘贴或拖入图片 / 音频）'"
        :disabled="disabled"
        aria-label="消息内容"
        @input="autosize"
        @keydown="onKeydown"
        @paste="onPaste"
        @compositionstart="composing = true"
        @compositionend="composing = false"
      />

      <div class="composer-bar">
        <input
          ref="picker"
          class="composer-picker"
          type="file"
          accept="image/*,audio/*"
          multiple
          aria-label="选择图片或音频附件"
          @change="onPicked"
        />
        <button
          type="button"
          class="composer-attach"
          :disabled="attachDisabled"
          :title="attachDisabled ? '当前不可添加附件' : '添加图片 / 音频（也可粘贴或拖入）'"
          aria-label="添加附件"
          @click="pickFiles"
        >
          <Icon name="paperclip" :size="16" />
        </button>

        <label class="composer-model" :title="pickerDisabled ? '当前不可切换模型' : '选择模型'">
          <Icon name="cpu" :size="14" />
          <select
            class="composer-select"
            :value="modelKey"
            aria-label="选择模型"
            :disabled="pickerDisabled"
            @change="onModelChange"
          >
            <!-- Grouped and labelled by the shared catalog helper, so this list
                 is the same data, in the same order, as 设置 → 模型管理. -->
            <optgroup v-for="group in groups" :key="group.provider" :label="group.label">
              <option
                v-for="option in group.options"
                :key="option.key"
                :value="option.key"
                :title="option.hint"
                :disabled="option.disabled"
              >
                {{ option.label }}
              </option>
            </optgroup>
          </select>
          <Icon name="chevron-down" :size="13" />
        </label>

        <span class="composer-hint">{{ hint }}</span>

        <button v-if="streaming" type="button" class="composer-stop" title="停止生成" @click="emit('stop')">
          <Icon name="square" :size="14" />
          停止
        </button>
        <button
          v-else
          type="submit"
          class="composer-send"
          :disabled="!canSend"
          :title="canSend ? '发送（Enter）' : '输入内容或添加附件后可发送'"
          aria-label="发送"
        >
          <Icon name="arrow-up" :size="18" />
        </button>
      </div>

      <!-- A visible drop target: the whole container, not a strip. -->
      <div v-if="dragging" class="composer-drop" aria-hidden="true">
        <Icon name="upload" :size="18" />
        松开即可添加图片 / 音频
      </div>
    </div>
  </form>
</template>
