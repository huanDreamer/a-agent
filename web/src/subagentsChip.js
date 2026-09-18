// What the header chip says.
//
// It lives outside the component so the wording is testable without rendering a
// whole view: the chip's job is to answer "is anything running, and what did it
// delegate" in a few words, and getting that wording wrong is how a header starts
// lying — a stale count under a new conversation's title, or a liveness figure that
// outlives the work.
import { subagentsRunning, subagentsTotal, subagentsMaxConcurrent } from './subagentsStore.js'

/** The chip's label: liveness first, then the record count. */
export function subagentChipLabel(running, total) {
  const live = typeof running === 'number' ? running : subagentsRunning.value
  const all = typeof total === 'number' ? total : subagentsTotal.value
  if (live > 0) return `${live} 个子 agent 运行中`
  return `${all} 个子 agent 记录`
}

/** The chip's tooltip: the facts behind the label, including the cap. */
export function subagentHintText() {
  const parts = [`本会话：运行中 ${subagentsRunning.value}，记录 ${subagentsTotal.value}`]
  if (subagentsMaxConcurrent.value > 0) {
    parts.push(`全进程上限 ${subagentsMaxConcurrent.value}`)
  }
  parts.push('点击查看委派过哪些、各自状态')
  return parts.join(' · ')
}
