<script setup>
// 设置 — everything that is genuinely settings-like, in one page:
//
//   1. 外观           主题（亮色 / 暗色 / 跟随系统），来自 theme.js;
//   2. 服务信息       provider / model / 版本 / 开关状态，只读，来自 GET /api/meta;
//   3. 模型目录       可选 provider / model，只读，来自 GET /api/chat/models;
//   4. 工作区与工具   已注册的工具、单轮步数上限与工作区配置说明，只读;
//   5. 技能           SkillsPanel（原 技能 页面）。
//
// There is deliberately no account, password, login or logout section.
import { computed } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import SkillsPanel from './SkillsPanel.vue'
import ViewHead from './ViewHead.vue'
import { chat, chatModels, defaultModel, loadCatalog, maxSteps } from '../chatStore.js'
import { formatCount } from '../format.js'
import { loadMeta, state } from '../state.js'
import { THEMES, setTheme, theme } from '../theme.js'

const meta = computed(() => state.meta || {})
const tools = computed(() => (chat.catalog && chat.catalog.tools) || [])

/** Any provider / model pair the console could open a session with. */
const catalog = computed(() =>
  chatModels.value.map((entry) => ({
    provider: entry.provider || '—',
    model: entry.model || '—',
    isDefault: Boolean(entry.default),
    hasKey: entry.has_api_key !== false,
  })),
)
</script>

<template>
  <div class="page">
    <ViewHead title="设置" note="外观、服务信息、模型与技能" />

    <div class="page-scroll">
      <div class="stack">
        <!-- 1. appearance -->
        <div class="card">
          <div class="card-head">
            <div class="card-title">
              <span class="dot" />
              外观
              <span class="card-sub">当前：{{ theme.resolved === 'dark' ? '暗色' : '亮色' }}</span>
            </div>
          </div>

          <div class="set-row">
            <div class="set-text">
              <div class="set-name">主题</div>
              <div class="set-desc">选择会立即生效并保存在本机浏览器里。</div>
            </div>
            <div class="seg" role="group" aria-label="主题">
              <button
                v-for="item in THEMES"
                :key="item.key"
                type="button"
                :class="{ active: theme.choice === item.key }"
                :title="`主题：${item.label}`"
                :aria-pressed="theme.choice === item.key"
                @click="setTheme(item.key)"
              >
                <Icon :name="item.icon" :size="14" />
                {{ item.label }}
                <Icon v-if="theme.choice === item.key" name="check" :size="13" />
              </button>
            </div>
          </div>
        </div>

        <!-- 2. server / provider information -->
        <div class="card">
          <div class="card-head">
            <div class="card-title">
              <span class="dot" />
              服务信息
              <span class="card-sub">只读 · 来自 <code class="md-code">GET /api/meta</code></span>
            </div>
            <button
              type="button"
              class="btn ghost sm"
              :disabled="!state.metaError && !state.meta"
              @click="loadMeta()"
            >
              <Icon name="refresh" :size="15" />
              重新读取
            </button>
          </div>

          <div v-if="state.metaError" class="banner error" role="alert">
            <Icon name="circle-alert" :size="16" />
            <span class="banner-text">无法读取服务信息：{{ state.metaError }}</span>
            <button type="button" class="btn sm" @click="loadMeta()">重试</button>
          </div>

          <div v-else-if="!state.meta" class="stack" aria-busy="true" aria-live="polite">
            <span class="skeleton" style="height: 14px; width: 38%" />
            <span class="skeleton" style="height: 13px; width: 62%" />
            <span class="muted-note">正在读取服务信息…</span>
          </div>

          <dl v-else class="kv">
            <div class="kv-row">
              <dt>默认 provider</dt>
              <dd class="mono">{{ meta.provider || '—' }}</dd>
            </div>
            <div class="kv-row">
              <dt>默认 model</dt>
              <dd class="mono">{{ meta.model || '—' }}</dd>
            </div>
            <div class="kv-row">
              <dt>服务版本</dt>
              <dd class="mono">{{ meta.version || 'dev' }}</dd>
            </div>
            <div class="kv-row">
              <dt>Prometheus metrics</dt>
              <dd>{{ meta.metrics_enabled ? '已启用' : '未启用' }}</dd>
            </div>
            <div class="kv-row">
              <dt>飞书接入</dt>
              <dd>{{ meta.feishu_enabled ? '已启用' : '未启用' }}</dd>
            </div>
          </dl>
        </div>

        <!-- 3. model catalog -->
        <div class="card">
          <div class="card-head">
            <div class="card-title">
              <span class="dot" />
              模型目录
              <span class="card-sub">
                只读 · 来自 <code class="md-code">GET /api/chat/models</code>
              </span>
            </div>
          </div>

          <AsyncBlock
            :state="chat.catalogStatus"
            :error="chat.catalogError"
            :skeleton-rows="3"
            @retry="loadCatalog()"
          >
            <div v-if="!catalog.length" class="empty">
              <div class="empty-ico" aria-hidden="true">◍</div>
              <div class="empty-text">暂无数据</div>
              <div class="empty-hint">没有可用的 provider / model，请检查 server 的 llm 配置</div>
            </div>

            <div v-else class="table-wrap">
              <table class="data">
                <thead>
                  <tr>
                    <th>Provider</th>
                    <th>模型</th>
                    <th>状态</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="entry in catalog" :key="`${entry.provider}/${entry.model}`">
                    <td class="mono">{{ entry.provider }}</td>
                    <td class="mono">
                      {{ entry.model }}
                      <span v-if="entry.isDefault" class="tag ok">默认</span>
                    </td>
                    <td>
                      <span v-if="entry.hasKey" class="tag ok">已配置 API Key</span>
                      <span v-else class="tag bad">未配置 API Key</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>

            <p v-if="defaultModel" class="muted-note card-foot">
              新建对话默认使用
              <code class="md-code">{{ defaultModel.provider }} / {{ defaultModel.model }}</code>
              ，单轮最多 {{ formatCount(maxSteps) }} 步。
            </p>
          </AsyncBlock>
        </div>

        <!-- 4. workspace + tools (informational only) -->
        <div class="card">
          <div class="card-head">
            <div class="card-title">
              <span class="dot" />
              工作区与工具
              <span class="card-sub">只读 · 说明性信息，不可在此开关</span>
            </div>
          </div>

          <p class="set-desc">
            工作目录由服务端配置项 <code class="md-code">tools.workspace</code> 决定（文件与命令
            操作都被限制在该目录内），<code class="md-code">tools.read_only</code> /
            <code class="md-code">tools.enable_bash</code> 控制读写与命令执行。接口不返回具体
            路径，因此这里只列出当前服务注册了哪些工具；修改后需要重启服务。
          </p>

          <div class="chips tools-chips">
            <template v-if="tools.length">
              <span v-for="name in tools" :key="name" class="chip mono">{{ name }}</span>
            </template>
            <span v-else class="chip warn">当前没有注册任何工具</span>
          </div>

          <p class="muted-note card-foot">
            共 {{ formatCount(tools.length) }} 个工具 · 单轮最多 {{ formatCount(maxSteps) }} 步 ·
            对话里不展示该列表
          </p>
        </div>

        <!-- 5. skills -->
        <SkillsPanel />
      </div>
    </div>
  </div>
</template>
