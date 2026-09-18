<script setup>
// 对话预算 — the per-turn budget of the web console, editable.
//
// This is the one panel in 设置 that changes how a *turn* runs rather than what
// the console shows, and it exists because the alternative was editing
// config.yaml and restarting the process: a turn that stopped on its step cap
// used to be fixable only from a shell. The value it writes is stored in the
// database (app_settings) and read per turn, so a change applies to the next
// message.
//
// What the panel deliberately does NOT let anyone change: context.max_tokens, the
// in-turn window the compressor was built with at startup. It is reported
// instead, with a warning when it is 0 — because raising the step cap without
// compression is exactly the combination that turns a long task into a
// context-limit error partway through, and a panel that hid that would be
// selling the half of the fix that does not work.
//
// The three inputs show the *effective* value, which is what a turn would use
// right now. Each row says where its value came from (config.yaml or a console
// override), and 恢复默认 is one button that hands all three back to the file.
import { computed, onMounted, reactive, ref, watch } from 'vue'
import Icon from './Icon.vue'
import AsyncBlock from './AsyncBlock.vue'
import { budget, loadBudget, resetBudget, saveBudget } from '../chatStore.js'
import {
  BUDGET_LIMITS,
  budgetForm,
  defaultForm,
  formatBudget,
  formatSeconds,
  parseBudgetForm,
  sourceLabel,
} from '../budget.js'
import { formatCount } from '../format.js'

const form = reactive({ max_steps: '', turn_max_tokens: '', turn_deadline_seconds: '' })
const errors = reactive({})
const notice = ref('')
const actionError = ref('')

const snapshot = computed(() => budget.snapshot || {})
const defaults = computed(() => defaultForm(budget.snapshot))
const summary = computed(() => formatBudget(budget.snapshot))

/** The rows, so the template is one loop instead of three copies. */
const rows = computed(() => [
  {
    key: 'max_steps',
    label: '步数上限',
    hint: '一轮里模型最多发起多少次工具调用。撞上它只是「这一轮到此为止」，工作区改动已经落盘。',
    unit: '步',
    source: snapshot.value.source_max_steps,
  },
  {
    key: 'turn_max_tokens',
    label: 'token 预算',
    hint: '一轮累计 token 上限，按 provider 上报的用量计算。0 表示不限。',
    unit: 'tokens',
    source: snapshot.value.source_turn_max_tokens,
  },
  {
    key: 'turn_deadline_seconds',
    label: '时间上限',
    hint: '一轮的墙钟上限，用来兜住「步数不多、token 不多，但很慢」的那种卡住。0 表示不限。',
    unit: '秒',
    source: snapshot.value.source_turn_deadline_seconds,
  },
])

/** 只填这一步：把某一项改成「新对话常用」的一档。 */
const presets = {
  max_steps: [30, 60, 120],
}

function clearMessages() {
  notice.value = ''
  actionError.value = ''
  for (const key of Object.keys(errors)) delete errors[key]
}

/** fill resets the inputs to the effective values, discarding edits. */
function fill() {
  const next = budgetForm(budget.snapshot)
  for (const key of Object.keys(next)) form[key] = next[key]
}

// The inputs follow every new snapshot — the one already in the store at mount,
// and every one a save or a refresh produces. It is a watch rather than a call
// from onMounted so a panel opened after the store loaded (the usual case: 对话
// loads the catalog first) shows the values immediately in the same setup pass,
// instead of rendering empty inputs until the lifecycle hook runs.
//
// A store object never mutates in place — loadBudget and saveBudget both replace
// it — so this cannot fire while the operator is typing.
watch(() => budget.snapshot, fill, { immediate: true })

async function refresh() {
  await loadBudget()
}

async function submit() {
  clearMessages()
  const parsed = parseBudgetForm(form)
  if (!parsed.ok) {
    Object.assign(errors, parsed.errors)
    return
  }
  try {
    const saved = await saveBudget(parsed.body)
    fill()
    notice.value = `已保存并立即生效：${formatBudget(saved)}`
  } catch (err) {
    actionError.value = err && err.message ? err.message : '保存失败'
  }
}

async function restoreDefaults() {
  clearMessages()
  try {
    const saved = await resetBudget()
    fill()
    notice.value = `已恢复配置文件里的默认值：${formatBudget(saved)}`
  } catch (err) {
    actionError.value = err && err.message ? err.message : '恢复默认失败'
  }
}

onMounted(refresh)
</script>

<template>
  <div class="stack">
    <div class="card">
      <div class="card-head">
        <div class="card-title">
          <span class="dot" />
          每轮预算
          <span class="card-sub">
            改完立即生效 · 存在数据库里，覆盖 <code class="md-code">config.yaml</code> 的默认值
          </span>
        </div>
        <button
          type="button"
          class="btn ghost sm"
          :disabled="budget.status === 'loading'"
          @click="refresh()"
        >
          <Icon name="refresh" :size="15" />
          重新读取
        </button>
      </div>

      <AsyncBlock
        :state="budget.status"
        :error="budget.error"
        :skeleton-rows="3"
        @retry="refresh()"
      >
        <p class="muted-note">
          当前生效：<strong>{{ summary }}</strong>
        </p>

        <div class="form-grid">
          <label v-for="row in rows" :key="row.key" class="field">
            <span class="field-label">
              {{ row.label }}
              <span class="tag" :class="{ warn: row.source === 'console' }">
                {{ sourceLabel(row.source) }}
              </span>
            </span>
            <input
              v-model="form[row.key]"
              class="input mono"
              type="text"
              inputmode="numeric"
              autocomplete="off"
              :placeholder="`默认 ${defaults[row.key] || '不限'}`"
              :aria-label="row.label"
              :aria-invalid="Boolean(errors[row.key])"
            />
            <span v-if="errors[row.key]" class="field-error">{{ errors[row.key] }}</span>
            <span v-else class="field-hint">
              {{ row.hint }}
              <template v-if="row.key === 'turn_deadline_seconds'">
                当前 {{ formatSeconds(form[row.key]) }}。
              </template>
              <template v-else-if="row.key === 'max_steps'">
                可填 {{ formatCount(BUDGET_LIMITS[row.key].min) }} –
                {{ formatCount(BUDGET_LIMITS[row.key].max) }}。
              </template>
            </span>

            <span v-if="presets[row.key]" class="seg" role="group" :aria-label="`${row.label} 常用档`">
              <button
                v-for="preset in presets[row.key]"
                :key="preset"
                type="button"
                @click="form[row.key] = String(preset)"
              >
                {{ preset }}
              </button>
            </span>
          </label>
        </div>

        <div v-if="notice" class="banner notice" role="status">
          <Icon name="check" :size="16" />
          <span class="banner-text">{{ notice }}</span>
        </div>
        <div v-if="actionError" class="banner error" role="alert">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">{{ actionError }}</span>
        </div>
        <div v-if="budget.error && budget.snapshot" class="banner error" role="alert">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">{{ budget.error }}</span>
        </div>

        <div class="row">
          <button type="button" class="btn" :disabled="budget.saving" @click="submit()">
            {{ budget.saving ? '保存中…' : '保存' }}
          </button>
          <button type="button" class="btn ghost" :disabled="budget.saving" @click="fill()">
            撤销修改
          </button>
          <button
            type="button"
            class="btn ghost"
            :disabled="budget.saving || !snapshot.has_override"
            title="清掉控制台的覆盖，三个维度都回到 config.yaml 的值"
            @click="restoreDefaults()"
          >
            恢复默认
          </button>
          <span class="spacer" />
          <span v-if="snapshot.has_override" class="dimmer nowrap">有覆盖写入了数据库</span>
        </div>

        <!-- the half of "run longer" this panel cannot change: the in-turn
             window the compressor was built with at startup -->
        <div v-if="snapshot.context_requires_restart" class="banner warn" role="note">
          <Icon name="circle-alert" :size="16" />
          <span class="banner-text">
            这一轮没有开启循环内上下文压缩（<code class="md-code">context.max_tokens = 0</code>）：
            步数调大后每一步都会重发整段历史，token 成本近似平方增长，最后会撞模型上下文上限。
            在 <code class="md-code">config.yaml</code> 里设
            <code class="md-code">context.max_tokens</code>（例如模型窗口的 60%~70%）并重启，是让它跑完的另一半。
          </span>
        </div>
        <p v-else class="muted-note card-foot">
          循环内压缩已开启（<code class="md-code">context.max_tokens =
          {{ formatCount(snapshot.context_max_tokens) }}</code>），长任务的窗口会被折叠成摘要，
          所以上面的步数上限可以放心调大。
        </p>
      </AsyncBlock>
    </div>
  </div>
</template>
