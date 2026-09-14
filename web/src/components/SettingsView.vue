<script setup>
// 设置 — every knob the console can turn, as six sub-tabs inside one page.
//
// 工作区 is deliberately NOT here: a workspace is a directory and a folder in the
// sidebar, so it is created and managed where conversations live, not in a
// settings page that would have to invent switches for it.
//
// 后台进程 is deliberately not here either, for the same reason: a background
// process belongs to the conversation that started it, so its count hangs on
// that conversation's header and its list opens beside it. A setting is
// something the operator turns; what the agent currently has running is state,
// and state belongs where it happened.
//
// The shape (and the reason for it) is the same as 统计监控: a page head with a
// sub-tab strip, then one scrolling panel. It replaced a single long scroll of
// cards, which had grown to the point where "外观" and "技能" were nine screens
// apart.
//
//   * 外观        theme, from theme.js — this browser only;
//   * 模型        the read-only catalog (what 对话's picker offers) + 模型管理;
//   * MCP         the MCP servers, their live state, and the AI assistant that
//                 drafts a definition from a description;
//   * OpenViking  the context database: connection, memory counters, workspace
//                 sync and manual document save;
//   * 技能        the skill files, their toggles, and the AI assistant that
//                 writes a SKILL.md;
//   * 服务与工具  server facts and the tool set — read-only.
//
// 模型's catalog is the read-only half of the model surface: it reads the same
// GET /api/chat/models as 对话's composer and shapes it with the same helpers
// from llm.js, so a name, a group or a "no API key" warning cannot differ
// between the two.
//
// There is deliberately no account, password, login or logout section *here*:
// the console's own session is the sidebar's business, not a setting panel —
// see the account block in AppSidebar.vue, which exists only where
// admin.require_login is on.
import { computed } from 'vue'
import AppearancePanel from './AppearancePanel.vue'
import McpPanel from './McpPanel.vue'
import ModelPanel from './ModelPanel.vue'
import OpenVikingPanel from './OpenVikingPanel.vue'
import ServicePanel from './ServicePanel.vue'
import SkillPanel from './SkillPanel.vue'
import ViewHead from './ViewHead.vue'
import { SETTINGS_TABS, setSettings, state } from '../state.js'

const PANELS = {
  appearance: AppearancePanel,
  models: ModelPanel,
  mcp: McpPanel,
  openviking: OpenVikingPanel,
  skills: SkillPanel,
  service: ServicePanel,
}

/** Sub-title per sub-tab: what the panel governs, and what it costs to change. */
const NOTES = {
  appearance: '主题与显示偏好 · 立即生效，只保存在本机浏览器',
  models: '模型目录与模型管理 · 对话的模型选择器读的就是这里',
  mcp: 'MCP 服务器 · 保存后立即连接，模型下一轮对话即可调用',
  openviking: 'OpenViking 上下文库 · 长期记忆与文档，设置来自服务端配置',
  skills: '技能（Skill）· 启用的技能会出现在对话的系统提示里，由模型按需加载',
  service: '服务信息与工具 · 只读，来自服务端配置',
}

const active = computed(() => PANELS[state.settings] || AppearancePanel)
const note = computed(() => NOTES[state.settings] || '')
</script>

<template>
  <div class="page">
    <ViewHead title="设置" :note="note" />

    <div class="subtabs" role="tablist" aria-label="设置分区">
      <button
        v-for="tab in SETTINGS_TABS"
        :key="tab.key"
        type="button"
        role="tab"
        class="subtab"
        :class="{ active: state.settings === tab.key }"
        :aria-selected="state.settings === tab.key"
        @click="setSettings(tab.key)"
      >
        {{ tab.label }}
      </button>
    </div>

    <div class="page-scroll">
      <component :is="active" :key="state.settings" />
    </div>
  </div>
</template>
