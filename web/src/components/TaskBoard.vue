<script setup>
// 任务看板 — 模型执行长任务时的计划，实时贴在输入框正上方。
//
// 它读的是模块单例（chat.plan / chat.planCollapsed / chat.streaming），不是 prop：
// 计划与流式状态本来就活在 chatStore 里，看板只是它们的一种渲染（同 BudgetPanel）。
//
// 位置是它的一半意义。计划属于"这一轮还没做完的事"，不属于消息历史——放进消息流
// 会被新消息一路推走，而它要一直看得见，直到最后一项变成 done。放在输入框上方，
// 也就是"还能对它做点什么"的地方：看板唯一的操作（继续执行）就在同一层。
//
// 分组、进度、摘要全是 plan.js 的纯函数（那些规则值得在无浏览器时被断言），这里
// 只剩下循环与样式。
import { computed } from 'vue'
import Icon from './Icon.vue'
import { chat, togglePlanCollapsed } from '../chatStore.js'
import {
  PLAN_GROUP_LABELS,
  planGroups,
  planHeadline,
  planProgress,
  planResumable,
} from '../plan.js'

/** 「继续执行」由父组件接住：它走的是 chatStore.resumeTurn，与发消息同一条路。 */
const emit = defineEmits(['resume'])

/**
 * 三个分组，按这个顺序渲染。
 *
 * 空组不画标题：一个"待执行 0"的小标题占掉的是看板最宝贵的高度，说的却是"这里
 * 没有东西"——数量只在真有内容的组上出现才有信息量。
 */
const GROUPS = [
  { key: 'doing', label: PLAN_GROUP_LABELS.doing },
  { key: 'todo', label: PLAN_GROUP_LABELS.todo },
  { key: 'done', label: PLAN_GROUP_LABELS.done },
]

/** v-if 的判据就是这个：没有计划，整个组件渲染成空。 */
const plan = computed(() => chat.plan)
const progress = computed(() => planProgress(chat.plan))
const groups = computed(() => planGroups(chat.plan))
const visibleGroups = computed(() => GROUPS.filter((group) => groups.value[group.key].length))
const headline = computed(() => planHeadline(chat.plan))

// 计划里没写完的事属于**这一轮之外**的下一步，所以有一轮在跑时不给这个按钮：
// 它现在按下去也只会被服务端拒绝（409），而按钮挨着"生成中"的输入框，看起来
// 像是能插队。
const canResume = computed(() => planResumable(chat.plan) && !chat.streaming)

// 目标可能为空（模型只给了任务清单），那就用一个中性的名字，而不是留一段空白
// 让进度看起来孤零零的。
const goal = computed(() => plan.value.goal || '任务计划')

/** 状态 → 色点。颜色是 token，这里只挑一个类名。 */
function dotClass(status) {
  if (status === 'done') return 'done'
  if (status === 'in_progress') return 'doing'
  if (status === 'failed') return 'failed'
  return 'pending' // pending / skipped：还没做完，但也没有在动
}
</script>

<template>
  <div v-if="plan" class="plan-dock">
    <div class="card plan">
      <div class="plan-head">
        <Icon name="list-checks" :size="14" />
        <span class="plan-goal" :title="goal">{{ goal }}</span>
        <span class="plan-count" :title="`已完成 ${progress.done}/${progress.total} 项`">
          {{ progress.done }}/{{ progress.total }}
        </span>
        <!-- 细进度条：数字之外的一个视觉锚点，扫一眼就知道还剩多少。 -->
        <span class="plan-bar" aria-hidden="true">
          <span class="plan-bar-fill" :style="{ width: `${progress.percent}%` }" />
        </span>

        <!-- 折叠态就只剩这一行，所以摘要放在这里；展开时每一组自己说话，不必
             再重复一遍。整行的 title 也是它。 -->
        <span v-if="chat.planCollapsed" class="plan-headline">{{ headline }}</span>

        <span class="spacer" />

        <button
          v-if="canResume"
          type="button"
          class="btn sm"
          title="会带着上一轮的计划和已完成步骤接着跑，不会从零重来"
          @click="emit('resume')"
        >
          <Icon name="refresh" :size="14" />
          继续执行
        </button>

        <button
          type="button"
          class="btn ghost sm icon-btn"
          :title="chat.planCollapsed ? '展开任务清单' : '折叠任务清单'"
          :aria-expanded="!chat.planCollapsed"
          :aria-label="chat.planCollapsed ? '展开任务清单' : '折叠任务清单'"
          @click="togglePlanCollapsed()"
        >
          <Icon :name="chat.planCollapsed ? 'chevron-right' : 'chevron-down'" :size="15" />
        </button>
      </div>

      <div v-if="!chat.planCollapsed" class="plan-groups">
        <div v-for="group in visibleGroups" :key="group.key" class="plan-group">
          <div class="plan-group-head">
            <span>{{ group.label }}</span>
            <span class="plan-group-count">{{ groups[group.key].length }}</span>
          </div>
          <ul class="plan-list">
            <li
              v-for="task in groups[group.key]"
              :key="task.id"
              class="plan-item"
              :class="task.status"
            >
              <span class="plan-dot" :class="dotClass(task.status)" aria-hidden="true" />
              <span class="plan-title" :title="task.title">{{ task.title }}</span>
              <span v-if="task.status === 'failed'" class="tag bad">失败</span>
              <span v-if="task.note" class="plan-note" :title="task.note">{{ task.note }}</span>
            </li>
          </ul>
        </div>
      </div>
    </div>
  </div>
</template>
