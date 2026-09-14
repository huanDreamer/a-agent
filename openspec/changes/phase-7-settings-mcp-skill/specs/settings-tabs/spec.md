# Capability: settings-tabs

## Purpose

把 设置 分成清晰的分类：一页多 tab，每个分类一个 tab，形状与 统计监控 一致。

## Scope

- `web/src/state.js` — `SETTINGS_TABS` 与 `state.settings`。
- `web/src/components/SettingsView.vue` — 外壳。
- `web/src/components/{Appearance,Model,Mcp,Skill,Service}Panel.vue` — 五个分区。

## Requirements (MUST)

1. **分区。** 设置 MUST 是五个 sub-tab：外观、模型、MCP、技能、服务与工具。
2. **形状一致。** MUST 复用 统计监控 的骨架：`ViewHead`（标题 + 当前分区的副标题）
   + `.subtabs` + `.page-scroll` + 分区组件；切分区 MUST 换掉整个面板。
3. **状态在 store 里。** 当前分区 MUST 存在 `state.settings`（与 `state.monitor`
   对称），因此离开设置再回来 MUST 停在同一个分区。
4. **布局。** 文档本身 MUST NOT 滚动，只有内层面板滚动；380px 下 tab 条 MUST 保持
   一行、可横向滚动、不把页面撑出横向溢出；新面板（MCP / 技能）在亮色与暗色下
   文字对比度 MUST ≥ 4。
5. **分类归属。** 外观 MUST 只放本机浏览器的偏好（主题）；模型 MUST 同时给出只读的
   模型目录与可写的模型管理；MCP MUST 给出服务器列表 + 配置助手；技能 MUST 给出
   技能列表 + 编辑器 + 写作助手；服务与工具 MUST 只放只读信息（服务信息、工具列表）。
6. **工具列表说真话。** 服务与工具 里的工具列表 MUST 是模型实际可调用的集合
   （内置 + MCP + `skill`），并 MUST 在打开该分区时重新读取，使 MCP 分区的改动
   立刻可见。
7. **AI 入口的措辞。** 两个配置助手 MUST 明确说明「只生成草稿、保存由你确认」，
   MUST 给出所用模型，MUST 可展开查看模型原始回复。
