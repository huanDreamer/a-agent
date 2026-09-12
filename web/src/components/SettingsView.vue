<script setup>
// 设置 — everything that is genuinely settings-like, in one page:
//
//   1. 外观           主题（亮色 / 暗色 / 跟随系统），来自 theme.js;
//   2. 服务信息       provider / model / 版本 / 开关状态，只读，来自 GET /api/meta;
//   3. 模型目录       当前可调用的 provider / model，只读，来自 GET /api/chat/models;
//   4. 模型管理       可写的 provider / 模型 / 能力绑定（ModelManager）;
//   5. 工作区与工具   已注册的工具、单轮步数上限与工作区配置说明，只读;
//   6. 技能           SkillsPanel（原 技能 页面）。
//
// 模型目录 is the read-only half of the model surface and the exact list 对话's
// composer offers: both read GET /api/chat/models and shape it with the same
// helpers from llm.js, so a name, a group or a "no API key" warning cannot differ
// between the two panels.
//
// There is deliberately no account, password, login or logout section.
import { computed } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import ModelManager from './ModelManager.vue'
import SkillsPanel from './SkillsPanel.vue'
import ViewHead from './ViewHead.vue'
import {
  catalog,
  catalogProviders,
  chat,
  defaultModel,
  loadCatalog,
  maxSteps,
} from '../chatStore.js'
import { capabilityLabel, modelOptionHint, modelOptionLabel } from '../llm.js'
import { formatAbsolute, formatCount, formatRelative } from '../format.js'
import { loadMeta, state } from '../state.js'
import { THEMES, setTheme, theme } from '../theme.js'

const meta = computed(() => state.meta || {})
const tools = computed(() => (chat.catalog && chat.catalog.tools) || [])

/** The catalog's models, groups and provider summaries, as the composer sees them. */
const models = computed(() => catalog.value.models)
const groups = computed(() => catalog.value.groups)
const providers = computed(() => catalogProviders.value)

/**
 * The catalog per provider, for the 目录 table: one section per provider, with
 * the same name and the same model labels 对话 shows.
 */
const catalogByProvider = computed(() =>
  groups.value.map((group) => ({
    provider: group.provider,
    providerName: group.providerName,
    hasKey: group.hasKey,
    stats: providers.value.find((p) => p.id === group.provider) || null,
    models: group.models,
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

        <!-- 3. model catalog: the same list, grouping and marks as 对话 -->
        <div class="card">
          <div class="card-head">
            <div class="card-title">
              <span class="dot" />
              模型目录
              <span class="card-sub">
                只读 · 来自 <code class="md-code">GET /api/chat/models</code>
              </span>
            </div>
            <button type="button" class="btn ghost sm" @click="loadCatalog({ quiet: true })">
              <Icon name="refresh" :size="15" />
              重新读取
            </button>
          </div>

          <AsyncBlock
            :state="chat.catalogStatus"
            :error="chat.catalogError"
            :skeleton-rows="3"
            @retry="loadCatalog()"
          >
            <div v-if="!models.length" class="empty">
              <div class="empty-ico" aria-hidden="true">◍</div>
              <div class="empty-text">暂无可用模型</div>
              <div class="empty-hint">
                没有启用的 provider 或模型。在下面的「模型管理」里添加 provider、配置 API Key
                并刷新模型列表，这里（以及对话的模型选择器）会同步出现。
              </div>
            </div>

            <div v-else class="table-wrap">
              <table class="data">
                <thead>
                  <tr>
                    <th>Provider</th>
                    <th>模型</th>
                    <th>能力</th>
                    <th>状态</th>
                  </tr>
                </thead>
                <tbody>
                  <template v-for="group in catalogByProvider" :key="group.provider">
                    <tr v-for="entry in group.models" :key="`${entry.provider}/${entry.model}`">
                      <td class="mono" :title="`provider id：${group.provider}`">
                        {{ group.providerName }}
                      </td>
                      <td class="mono" :title="modelOptionHint(entry)">
                        {{ entry.displayName }}
                        <span v-if="entry.displayName !== entry.model" class="tag mono">
                          {{ entry.model }}
                        </span>
                        <span v-if="entry.isDefault" class="tag ok">默认</span>
                      </td>
                      <td>
                        <span v-if="entry.capabilities.length" class="row caps">
                          <span v-for="cap in entry.capabilities" :key="cap" class="tag">
                            {{ capabilityLabel(cap) }}
                          </span>
                        </span>
                        <span v-else class="dimmer">未声明</span>
                      </td>
                      <td>
                        <span v-if="entry.hasKey" class="tag ok">已配置 API Key</span>
                        <span v-else class="tag bad">未配置 API Key</span>
                        <span v-if="!entry.chatCapable" class="tag warn">非对话模型</span>
                      </td>
                    </tr>
                  </template>
                </tbody>
              </table>
            </div>

            <p class="muted-note card-foot">
              {{ formatCount(models.length) }} 个模型来自启用的 provider；这张表就是对话里
              模型选择器的内容。
              <template v-if="defaultModel">
                新建对话默认使用
                <code class="md-code">{{ defaultModel.providerName }} / {{ defaultModel.displayName }}</code>
                。
              </template>
              <template v-else>
                当前没有可对话的模型，新建对话需要先在模型管理里声明至少一个「对话」模型。
              </template>
              单轮最多 {{ formatCount(maxSteps) }} 步。
            </p>

            <!-- freshness of each provider's cached list: the fact the automatic
                 refresh is based on, shown rather than hidden -->
            <div v-if="providers.length" class="stack catalog-providers">
              <div v-for="provider in providers" :key="provider.id" class="catalog-provider">
                <span class="mono">{{ provider.name }}</span>
                <span class="tag" :title="`provider id：${provider.id}`">
                  {{ formatCount(provider.modelCount) }} 个模型 · 其中
                  {{ formatCount(provider.chatModelCount) }} 个可对话
                </span>
                <span
                  v-if="provider.lastFetchedAt"
                  class="tag"
                  :title="`最近一次抓取：${formatAbsolute(provider.lastFetchedAt)}`"
                >
                  已刷新 {{ formatRelative(provider.lastFetchedAt) }}
                </span>
                <span v-else class="tag">尚未抓取模型列表</span>
                <span v-if="provider.stale" class="tag warn">列表可能过期</span>
                <span v-if="!provider.enabled" class="tag">已停用</span>
                <span v-if="!provider.hasKey" class="tag bad">未配置密钥</span>
                <span v-if="provider.lastError" class="cell-err">
                  最近一次抓取失败：{{ provider.lastError }}
                </span>
              </div>
            </div>
          </AsyncBlock>
        </div>

        <!-- 4. model management: providers, their models, capability bindings -->
        <ModelManager />

        <!-- 5. workspace + tools (informational only) -->
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

        <!-- 6. skills -->
        <SkillsPanel />
      </div>
    </div>
  </div>
</template>
