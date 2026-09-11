<script setup>
// Block-level markdown renderer for assistant answers: fenced code blocks and
// paragraphs. Line breaks are preserved by CSS (`white-space: pre-wrap`), so
// paragraphs need no <br>. See markdown.js for the supported subset.
import { computed } from 'vue'
import MarkdownInline from './MarkdownInline.vue'
import { parseBlocks } from '../markdown.js'

const props = defineProps({
  text: { type: String, default: '' },
})

const blocks = computed(() => parseBlocks(props.text))
</script>

<template>
  <div v-if="blocks.length" class="md">
    <template v-for="(block, index) in blocks" :key="index">
      <pre v-if="block.type === 'code'" class="md-pre" :title="block.lang || ''">{{ block.code }}</pre>
      <p v-else class="md-p">
        <MarkdownInline :tokens="block.tokens" />
      </p>
    </template>
  </div>
</template>
