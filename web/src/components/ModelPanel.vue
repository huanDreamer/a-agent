<script setup>
// 模型 — the whole model surface in one tab: what is available (read-only) and
// what is editable (模型管理).
//
// 模型目录 is the exact list 对话's composer offers: both read
// GET /api/chat/models and shape it with the same helpers from llm.js, so a
// name, a group or a "no API key" warning cannot differ between the two panels.
import { computed } from 'vue'
import AsyncBlock from './AsyncBlock.vue'
import Icon from './Icon.vue'
import ModelManager from './ModelManager.vue'
import {
  catalog,
  catalogProviders,
  chat,
  defaultModel,
  loadCatalog,
  maxSteps,
} from '../chatStore.js'
import { capabilityLabel, modelOptionHint } from '../llm.js'
import { formatAbsolute, formatCount, formatRelative } from '../format.js'

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
  <div class="stack">
    <!-- model catalog: the same list, grouping and marks as 对话 -->
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

    <!-- model management: providers, their models, capability bindings -->
    <ModelManager />
  </div>
</template>
