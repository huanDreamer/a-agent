<script setup>
// The ClaudeCode corner ribbon — the whole console's one word about 兼容模式.
//
// 兼容模式 overrides the model *every* conversation picked, so it is a fact about
// the app rather than about the conversation on screen, and it belongs in the
// shell (App.vue renders it above both columns) instead of in the chat header or
// the sidebar list. What it replaced: a two-line purple badge that took the
// sidebar's first slot — competing with the button that is actually clicked there
// — and rendered while the mode was *off* too, so the default state was permanent
// furniture.
//
// What is left is a diagonal sash floating over the window's top-left corner: the
// name of the mode and nothing else, drawn only while the mode is on, and **not a
// control**. The switch lives in 设置 → ClaudeCode; this says which mode is in
// effect and stops there. It takes no click, no focus and — this is the part that
// matters — no space: nothing on the page moves because it is there, so it floats
// over the corner of 新建对话 rather than pushing it aside. Because of that it is
// `pointer-events: none` (see .cc-ribbon): a marker that answers no clicks must not
// swallow the ones aimed at what it covers, which is also why it carries no hover
// text — the model the mode actually runs is in 设置 → ClaudeCode, next to the
// switch.
//
// Geometry is the whole trick (see .cc-ribbon): the band is one element, rotated
// -45° about a point on the corner diagonal, with both ends hanging off the top
// and left edges of the window. The viewport clips them, which is what makes it
// read as a sash laid across the corner rather than as a floating label — and what
// means there is no wrapper element, no `overflow: hidden`, and no layout rule
// anywhere that knows the ribbon exists.
import { computed } from 'vue'
import { claudeCompat, claudeCode } from '../state.js'

/**
 * What a screen reader hears in place of the band's two words: the mode, the model
 * it actually runs, and where the switch is. The model is the part that has no
 * other place on screen — every conversation's own picker still shows the model it
 * chose, which is no longer what runs — and with the ribbon unable to receive a
 * hover it is the only surface that can carry it.
 */
const label = computed(() => {
  const model = (claudeCode.snapshot && claudeCode.snapshot.model) || {}
  const name = model.effective_model || model.model || '模型未解析'
  return (
    `ClaudeCode 兼容模式已开启：所有对话都跑 ${name}，覆盖每个对话自己选的模型` +
    '（在 设置 → ClaudeCode 里切换，关掉即恢复，下一个对话生效）'
  )
})
</script>

<template>
  <div v-if="claudeCompat" class="cc-ribbon" role="note" :aria-label="label">
    ClaudeCode
  </div>
</template>
