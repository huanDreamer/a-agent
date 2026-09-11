<script setup>
// Inline markdown renderer: text / `code` / **bold** (bold may nest code).
// Everything is rendered through text interpolation — never v-html — so model
// output cannot inject markup. It references itself for nested tokens.
import { computed } from 'vue'

const props = defineProps({
  tokens: { type: Array, default: () => [] },
})

const items = computed(() => (Array.isArray(props.tokens) ? props.tokens : []))
</script>

<template>
  <template v-for="(token, index) in items" :key="index">
    <code v-if="token.type === 'code'" class="md-code">{{ token.text }}</code>
    <strong v-else-if="token.type === 'strong'">
      <MarkdownInline :tokens="token.tokens" />
    </strong>
    <template v-else>{{ token.text }}</template>
  </template>
</template>
