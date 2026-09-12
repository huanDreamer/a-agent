<script setup>
// One attachment inside a message bubble.
//
// An image renders as a thumbnail that opens full-size in a lightbox (no new
// dependency: a <Teleport> overlay over a fixed backdrop); audio renders as a
// native <audio controls> player — the browser's own transport is better than
// anything hand-written here — and anything else degrades to a file chip.
//
// A reloaded conversation only knows the asset id, so the mime type is fetched
// once per id (see attachments.js) before the right presentation can be picked.
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import Icon from './Icon.vue'
import { ensure, metaOf } from '../attachments.js'
import { formatBytes } from '../format.js'

const props = defineProps({
  /** Record built by attachments.normalize(): always carries an id. */
  attachment: { type: Object, required: true },
})

const runtime = computed(() => metaOf(props.attachment.id) || props.attachment)
const url = computed(() => runtime.value.url || props.attachment.url || '')
const kind = computed(() => runtime.value.kind || props.attachment.kind || '')
const name = computed(() => runtime.value.name || props.attachment.name || props.attachment.id)
const size = computed(() => formatBytes(runtime.value.bytes ?? props.attachment.bytes))
/** The bytes existed but the browser refuses to draw them. */
const broken = ref(false)
const open = ref(false)

function resolve() {
  if (!kind.value) ensure(props.attachment.id)
}

function onKey(event) {
  if (event.key === 'Escape') open.value = false
}

onMounted(() => {
  resolve()
  window.addEventListener('keydown', onKey)
})

onBeforeUnmount(() => window.removeEventListener('keydown', onKey))

watch(() => props.attachment.id, () => {
  broken.value = false
  open.value = false
  resolve()
})
</script>

<template>
  <div class="attach">
    <!-- image: thumbnail, full-size on click -->
    <template v-if="kind === 'image' && !broken">
      <button
        type="button"
        class="attach-thumb"
        :title="`查看大图：${name}`"
        :aria-label="`查看大图：${name}`"
        @click="open = true"
      >
        <img :src="url" :alt="name" loading="lazy" @error="broken = true" />
        <span class="attach-thumb-ico" aria-hidden="true"><Icon name="eye" :size="15" /></span>
      </button>
    </template>

    <!-- audio: the browser's own player -->
    <div v-else-if="kind === 'audio'" class="attach-audio">
      <div class="attach-audio-head">
        <Icon name="headphones" :size="14" />
        <span class="attach-name">{{ name }}</span>
        <span class="dimmer nowrap">{{ size }}</span>
      </div>
      <audio controls preload="metadata" :src="url" :aria-label="name" />
    </div>

    <!-- anything else (or an image the browser cannot decode) -->
    <a v-else class="attach-file" :href="url" target="_blank" rel="noreferrer">
      <Icon name="paperclip" :size="14" />
      <span class="attach-name">{{ name }}</span>
      <span class="dimmer nowrap">{{ size }}</span>
    </a>

    <Teleport to="body">
      <div
        v-if="open"
        class="lightbox"
        role="dialog"
        aria-modal="true"
        :aria-label="`大图：${name}`"
        @click.self="open = false"
      >
        <img class="lightbox-img" :src="url" :alt="name" />
        <div class="lightbox-bar">
          <span class="attach-name">{{ name }}</span>
          <span class="dimmer nowrap">{{ size }}</span>
          <span class="spacer" />
          <button type="button" class="btn sm" @click="open = false">
            <Icon name="x" :size="14" />
            关闭
          </button>
        </div>
      </div>
    </Teleport>
  </div>
</template>
