// 任务看板的数据模型（纯函数，不依赖 Vue）。
//
// 服务端工具（`tool.Plan` / `tool.Task`，snake_case）和这里是同一份数据的两种
// 写法，本文件只做三件事：把 wire 形状归一成能直接渲染的形状、按状态分组、
// 算出一行摘要。
//
// 它单独成一个模块（而不是写进组件里）有两个原因：一是这些规则值得在没有浏览器
// 的时候被断言（见 ssr-probe/run.mjs 与 scripts/check-*.mjs 的同类做法）；二是
// 看板因此只剩下循环和样式，判断"哪一项算待执行"不会散落在模板条件里。
//
// 每一份计划都是模型写给人看的：它可能不完整（缺字段、状态是它自己编的），所以
// 归一化的原则是**容忍**——能显示的就显示，实在没内容的才丢，而不是因为一处脏
// 数据让整块看板消失。

/** 计划允许的五个状态，与服务端 `tool.Task` 的枚举一致。 */
export const PLAN_STATUSES = ['pending', 'in_progress', 'done', 'failed', 'skipped']

/**
 * 三个分组的标题。
 *
 * 写在这里而不是组件里：分组的含义（failed 为什么在待执行、skipped 算什么）是
 * 模型的一部分，标题只是它的一个说法——换说法不该要求改分组规则。
 */
export const PLAN_GROUP_LABELS = { doing: '执行中', todo: '待执行', done: '已完成' }

/**
 * 取值成一个字符串。
 *
 * 数字也算有值（`title: 30` 是一句合法的任务标题），对象 / 数组 / null 不算：
 * 它们拼进模板只会变成 "[object Object]"。
 */
function text(value) {
  if (typeof value === 'string') return value.trim()
  if (typeof value === 'number' && Number.isFinite(value)) return String(value)
  return ''
}

/** 计数字段：非有限数、负数、字符串里的数字都收敛成一个非负整数。 */
function count(value) {
  const n = Number(value)
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0
}

/**
 * 一个没被占用的补号 id。
 *
 * 服务端会分配 `t1`、`t2`…，但一份本地造的或半截的计划可能只有标题。补号用
 * **它在原数组里的序号**，这样同一份 JSON 每次归一化得到的 id 都一样（否则看板
 * 的 `:key` 每帧都在变，列表会被重建）；万一和别的任务撞了，就往后找一个空的。
 */
function freeId(base, used) {
  if (!used.has(base)) return base
  let n = 2
  while (used.has(`${base}-${n}`)) n += 1
  return `${base}-${n}`
}

/**
 * normalizePlan 把 wire 形状（或任何像计划的东西）读成看板渲染的形状。
 *
 * 返回 `null` 表示"没有计划"，看板据此**整个不渲染**——"没计划"和"计划里没有
 * 任务"对界面是同一件事（没有可显示的内容），服务端也用 `null` 表示空计划。
 */
export function normalizePlan(raw) {
  if (!raw || typeof raw !== 'object') return null

  const tasks = []
  const used = new Set()
  const list = Array.isArray(raw.tasks) ? raw.tasks : []
  list.forEach((item, index) => {
    const source = item && typeof item === 'object' ? item : {}
    // 没有标题的任务没有可渲染的内容，丢掉它而不是画一行空白：看板上"还剩几件
    // 事"这个数字是给人做判断用的，把空行算进去会让它不可信。
    const title = text(source.title)
    if (title === '') return

    let id = text(source.id)
    // 服务端给的 id 原样保留（`plan_update` 靠它定位，改名等于撒谎）；只有真的
    // 没有 id 时才补号。
    if (id === '') id = freeId(`t${index + 1}`, used)
    used.add(id)

    tasks.push({
      id,
      title,
      // 状态只认五个枚举值；别的（拼错的、服务端将来新增的）当待执行——待执行
      // 是最保守的一档：它不会让一项工作从看板上消失。
      status: PLAN_STATUSES.includes(source.status) ? source.status : 'pending',
      note: text(source.note),
    })
  })

  if (tasks.length === 0) return null

  return {
    goal: text(raw.goal),
    revision: count(raw.revision),
    // 服务端每次变更 +1，客户端只读。看板今天不显示这一对（进度与分组已经说清了
    // "到哪了"），但归一化里仍然留着：把它丢掉等于替调用方决定它没用。
    updatedAt: text(raw.updated_at) || text(raw.updatedAt),
    tasks,
  }
}

/**
 * planGroups 把任务分到看板的三个组里。
 *
 * `failed` 和 `skipped` 都留在「待执行」而不是自成一组：对"还剩什么"这个问题，
 * 它们都还是没做完的事。`failed` 排在待执行的最前面，因为它是这一组里唯一需要
 * 人看一眼的（要么重跑，要么换个做法），而 `pending` 只是排队。
 */
export function planGroups(raw) {
  const groups = { doing: [], todo: [], done: [] }
  const plan = normalizePlan(raw)
  if (!plan) return groups

  for (const task of plan.tasks) {
    if (task.status === 'in_progress') groups.doing.push(task)
    else if (task.status === 'done') groups.done.push(task)
    else groups.todo.push(task)
  }
  groups.todo = [
    ...groups.todo.filter((task) => task.status === 'failed'),
    ...groups.todo.filter((task) => task.status !== 'failed'),
  ]
  return groups
}

/**
 * planProgress 是头部那三个数字：已完成数、总数、百分比。
 *
 * 百分比取整（0..100）：它旁边的 `n/m` 才是精确值，百分比只是同一条信息的
 * 另一种读法，"5/6 → 83%" 比 "83.33%" 好读。
 */
export function planProgress(raw) {
  const plan = normalizePlan(raw)
  const total = plan ? plan.tasks.length : 0
  const done = plan ? plan.tasks.filter((task) => task.status === 'done').length : 0
  const percent = total === 0 ? 0 : Math.min(100, Math.max(0, Math.round((done / total) * 100)))
  return { done, total, percent }
}

/**
 * planResumable：计划里还有真正没做完的任务 → 可以「继续执行」。
 *
 * `skipped` 不算没做完：跳过是"这一项不需要做了"的决定，是一个结论而不是遗留
 * 的工作。服务端 tool.Plan.Unfinished() 的算法与这里一致，两边必须同口径——否则
 * 看板会在服务端认为无事可续的时候亮着按钮，点下去只会收到 400。
 *
 * `failed` 算：它恰好是要续跑的理由。
 */
export function planResumable(raw) {
  const plan = normalizePlan(raw)
  return Boolean(plan) && plan.tasks.some((task) => task.status !== 'done' && task.status !== 'skipped')
}

/**
 * planHeadline 是一行摘要，给看板的折叠态和 title 用。
 *
 * 先说总数（`已完成 2/5`），再说"现在在做什么"或"下一步是什么"：折叠之后这一行
 * 就是全部信息，只写 2/5 会让人不知道差在哪一步。没有计划时返回空串，调用方
 * 不必再判空。
 */
export function planHeadline(raw) {
  const plan = normalizePlan(raw)
  if (!plan) return ''

  const { done, total } = planProgress(plan)
  const groups = planGroups(plan)
  const parts = [`已完成 ${done}/${total}`]

  if (groups.doing.length) {
    const running = groups.doing[0].title
    parts.push(`进行中：${running}${groups.doing.length > 1 ? ` 等 ${groups.doing.length} 项` : ''}`)
  } else if (groups.todo.length) {
    const next = groups.todo[0]
    parts.push(`${next.status === 'failed' ? '失败' : '下一步'}：${next.title}`)
  } else {
    parts.push('全部完成')
  }

  return parts.join(' · ')
}
