<script setup>
// 子 agent — what this conversation delegated, as a drawer beside it.
//
// Modelled on the jobs drawer, for the reason the chip is modelled on the jobs
// chip: the header answers "what is this conversation running" the same way for
// both. What differs is the nature of the thing:
//
//   1. a subagent is a nested *turn*, not a process. It cannot be stopped from
//      here (stopping the parent turn is what stops it — the nested run shares the
//      turn's context), so this drawer offers no 停止 button. Offering one that
//      could not work would be worse than offering none.
//   2. the list keeps finished runs, so the drawer is also the answer to "what did
//      it delegate while I was reading" — with each run's steps, tokens and how it
//      ended. A failed run says why; that is the case a reader most needs.
//   3. `steps: -1` means the runner could not count them (Eino's ReAct loop returns
//      only the final message). It is shown as "—", never as "0", because "0 steps"
//      for a minute of work reads as "it did nothing" and is simply false.
//
// The list itself lives in subagentsStore: the header chip and this drawer read the
// same poller, so the count on the chip and the rows here cannot disagree.
import { computed } from 'vue'
import Icon from './Icon.vue'
import { formatCount, formatDuration, formatRelative } from '../format.js'
import {
  subagents as runs,
  subagentsError,
  subagentsMaxConcurrent,
  subagentsRunning,
  subagentsRunningAll,
} from '../subagentsStore.js'

const emit = defineEmits(['close'])

/** Oldest first would bury the live ones; the store already returns newest first. */
const ordered = computed(() => runs.value)

const live = computed(() => ordered.value.filter((r) => r.status === 'running'))
const done = computed(() => ordered.value.filter((r) => r.status !== 'running'))

/** The wait is process-wide, so say so when more is queued than may run. */
const queued = computed(() => {
  const over = subagentsRunningAll.value - (subagentsMaxConcurrent.value || 0)
  return over > 0 ? over : 0
})

function statusLabel(run) {
  if (run.status === 'running') return '运行中'
  if (run.status === 'failed') return '未完成'
  return '已完成'
}

function statusTone(run) {
  if (run.status === 'running') return 'blue'
  if (run.status === 'failed') return 'warn'
  return 'ok'
}

/** The steps column: a dash when the runner cannot count, never a zero. */
function stepsLabel(run) {
  if (run.status === 'running') return '—'
  if (typeof run.steps !== 'number' || run.steps < 0) return '步数不可得'
  return `${run.steps} 步`
}

function tokensLabel(run) {
  return run.tokens > 0 ? `${formatCount(run.tokens)} tokens` : ''
}
</script>

<template>
  <aside class="overlay-drawer jobs-drawer" role="dialog" aria-label="子 agent">
    <header class="drawer-head">
      <Icon name="git-branch" :size="14" />
      <span class="drawer-title">子 agent</span>
      <span class="spacer" />
      <span v-if="subagentsRunning > 0" class="tag blue">
        运行中 {{ subagentsRunning }}<template v-if="subagentsMaxConcurrent > 0">
          / 上限 {{ subagentsMaxConcurrent }}</template>
      </span>
      <button type="button" class="btn sm ghost" aria-label="关闭" @click="emit('close')">
        <Icon name="x" :size="14" />
      </button>
    </header>

    <div class="drawer-body">
      <p v-if="runs.length === 0" class="muted-note">
        这个对话还没有委派过子 agent。模型在需要读很多文件才能回答一个问题时会派一个，
        只把结论带回来。
      </p>

      <p v-if="queued > 0" class="muted-note">
        还有 {{ queued }} 个在排队：全进程同时最多跑 {{ subagentsMaxConcurrent }} 个，
        其余的等前面跑完再开始。
      </p>

      <div v-if="subagentsError" class="cell-err">{{ subagentsError }}</div>

      <section v-if="live.length" class="stack">
        <h4 class="section-label">正在跑</h4>
        <article v-for="run in live" :key="run.id" class="card job-row">
          <div class="job-head">
            <span class="spin" aria-hidden="true" />
            <span class="job-name" :title="run.prompt">{{ run.name }}</span>
            <span class="tag" :class="statusTone(run)">{{ statusLabel(run) }}</span>
            <span class="dimmer nowrap">{{ formatDuration(run.duration_ms) }}</span>
          </div>
          <div v-if="run.prompt" class="job-command mono" :title="run.prompt">{{ run.prompt }}</div>
        </article>
      </section>

      <section v-if="done.length" class="stack">
        <h4 class="section-label">已结束</h4>
        <article v-for="run in done" :key="run.id" class="card job-row">
          <div class="job-head">
            <span class="job-name" :title="run.prompt">{{ run.name }}</span>
            <span class="tag" :class="statusTone(run)">{{ statusLabel(run) }}</span>
            <span class="dimmer nowrap">{{ stepsLabel(run) }}</span>
            <span v-if="tokensLabel(run)" class="dimmer nowrap">{{ tokensLabel(run) }}</span>
            <span class="dimmer nowrap">{{ formatDuration(run.duration_ms) }}</span>
          </div>
          <div v-if="run.prompt" class="job-command mono" :title="run.prompt">{{ run.prompt }}</div>
          <!-- A run that did not finish says why: it is the one case a reader
               cannot reconstruct from the conversation, because a failed subagent
               reports its failure to the model, not to the person. -->
          <div v-if="run.error" class="cell-err">{{ run.error }}</div>
          <div v-else-if="run.stop_reason" class="muted-note">
            因预算提前结束（{{ run.stop_reason }}）
          </div>
          <div v-if="run.ended_at" class="muted-note">结束于 {{ formatRelative(run.ended_at) }}</div>
        </article>
      </section>
    </div>
  </aside>
</template>
