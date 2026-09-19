// Render probes for the console's dialogs, sidebar and login gate.
//
// These exist because a bug in this area is invisible everywhere else: a setup
// binding shadowing a prop made the directory picker render on load and ignore
// its `open` prop, which no type check, build or linter can see. Rendering the
// component and asserting on the HTML can.
//
//     cd web && npm run check:ui
import {
  applyApprovalEvent,
  approvalCardFromEvent,
  approvalHelpers,
  approvalPending,
  applyAskEvent,
  askCardAnswer,
  captureApprovalRequest,
  captureAnswerRequest,
  askCardFromEvent,
  askCardFromTool,
  gateNeedsLogin,
  messageSteps,
  nestedOf,
  planHelpers,
  planState,
  renderApprovalCard,
  renderAskUserCard,
  renderBudgetPanel,
  renderChatHeader,
  renderDirPicker,
  renderLogin,
  renderMessageBubble,
  renderModelPanel,
  renderSidebar,
  renderSubagentsDrawer,
  setSubagents,
  subagentChip,
  renderTaskBoard,
  setAuth,
  setChatState,
  stepHelpers,
} from './out/probe.mjs'

let failures = 0
function check(name, ok, detail = '') {
  if (!ok) failures++
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ' — ' + detail : ''}`)
}

// 1. The reported bug: closed must render nothing, whatever else is in scope.
let html = await renderDirPicker({ open: false })
check('closed picker renders no dialog', !html.includes('选择工作区目录'), html.slice(0, 80))

// 2. Open renders it.
html = await renderDirPicker({ open: true })
check('open picker renders the dialog', html.includes('选择工作区目录'))

// 3. The caller's error is the one shown (the prop, not the local browse state).
html = await renderDirPicker({ open: true, error: '工作区名字已被占用: proj' })
check('caller error reaches the dialog', html.includes('工作区名字已被占用'))

// 4. Busy disables the confirm button and is announced.
html = await renderDirPicker({ open: true, busy: true })
check('busy state is visible', html.includes('创建中…'))

// 5. Without a workspace layer the sidebar is a flat list, with no create button.
setChatState({
  workspacesAvailable: false,
  workspaces: [],
  sessions: [],
  sessionsStatus: 'ready',
  creating: false,
})

// 6. The account block is a control, so it must appear exactly where it does
//    something: with require_login off there is no session to end.
setAuth({ probed: true, required: false, signedIn: false, username: '', expired: false })
html = await renderSidebar()
check('no-layer sidebar hides 工作区 tooling', !html.includes('新建工作区'), html.includes('新建工作区') ? 'create button rendered' : '')
check('no-layer sidebar shows the plain list label', html.includes('会话列表'))
check('login-free sidebar offers no 退出', !html.includes('退出'), html.includes('退出') ? 'logout rendered' : '')

// 7. On a login-required deployment the sidebar names the account and offers the
//    way out.
setAuth({ probed: true, required: true, signedIn: true, username: 'root', expired: false })
html = await renderSidebar()
check('login-required sidebar shows the account', html.includes('root'), html.slice(0, 80))
check('login-required sidebar offers 退出', html.includes('退出'))

// 8. The gate itself. These three cases are the whole reason the console asks
//    /api/me before it draws anything: only the middle one is the login form.
setAuth({ probed: true, required: false, signedIn: false, username: '', expired: false })
check('login-free deployment never asks for a password', !gateNeedsLogin())
setAuth({ probed: true, required: true, signedIn: false, username: '', expired: false })
check('login-required deployment asks for one', gateNeedsLogin())
setAuth({ probed: true, required: true, signedIn: true, username: 'admin', expired: false })
check('a held session is not asked again', !gateNeedsLogin())
// Neither screen is right before the probe answers: the shell would fire
// requests a login-required deployment answers with 401.
setAuth({ probed: false, required: false, signedIn: false, username: '', expired: false })
check('nothing is decided before the probe answers', !gateNeedsLogin())

// 9. The form itself: one field (the account is single-user), and the
//    expired-session notice only when the session actually expired — a first
//    visit must not read as a failure.
setAuth({ probed: true, required: true, signedIn: false, username: '', expired: false })
html = await renderLogin()
check('login form renders a password field', html.includes('type="password"'))
check('login form offers 登录', html.includes('登录'))
check('first visit claims no expiry', !html.includes('已失效'))
setAuth({ probed: true, required: true, signedIn: false, username: '', expired: true })
html = await renderLogin()
check('an expired session says so', html.includes('已失效'))


// 10. The ask_user card. It is the one place in the console where a click sends
//     something back mid-turn, so its two halves are both checked: the model that
//     normalises a question (from the live event and from a reloaded tool call),
//     and the HTML the user actually gets.
const questionEvent = {
  type: 'ask_user',
  ask_status: 'pending',
  ask: {
    id: 'q-1',
    header: '数据库选型',
    text: '新服务用哪个数据库？',
    options: [
      { label: 'Postgres', description: '已有实例，迁移成本低' },
      { label: 'SQLite' },
    ],
  },
}
const liveCard = askCardFromEvent(questionEvent)
check('a live question becomes a card', liveCard && liveCard.id === 'q-1' && liveCard.status === 'pending')
check('an omitted allow_custom means free text is allowed', liveCard && liveCard.allowCustom === true)
check('a question is not asked twice by the same event', askCardFromEvent({ type: 'ask_user', ask_id: 'q-1', ask_status: 'answered' }) === null)

html = await renderAskUserCard(liveCard)
check('a pending card shows its question', html.includes('新服务用哪个数据库'))
check('a pending card shows its options', html.includes('Postgres') && html.includes('已有实例'))
check('a pending card offers the submit button', html.includes('提交答案'))
check('a pending card offers a free-text box', html.includes('ask-input'))
check('a pending card waits for an answer before submitting', html.includes('disabled'))

// The label the user picked is marked, and the answer travels as the label
// rather than as an index into the options.
const pickedCard = askCardFromEvent(questionEvent)
pickedCard.selected = ['Postgres']
html = await renderAskUserCard(pickedCard)
check('the picked option is marked', html.includes('picked'))
check('the picked option is announced to assistive tech', html.includes('aria-pressed="true"'))

// Settled: the answer is stated, and the choices are locked.
const settled = applyAskEvent(askCardFromEvent(questionEvent), {
  type: 'ask_user',
  ask_id: 'q-1',
  ask_status: 'answered',
  ask_answer: { status: 'answered', selected: ['SQLite'], text: '先用简单的' },
})
check('an outcome settles the card', settled.status === 'answered')
check('the answer line keeps both halves', askCardAnswer(settled) === 'SQLite（补充：先用简单的）')
html = await renderAskUserCard(settled)
check('a settled card shows the answer', html.includes('SQLite（补充：先用简单的）'))
check('a settled card shows it was submitted', html.includes('已提交'))
check('a settled card no longer offers the submit button', !html.includes('提交答案'))

// A question nobody answered: the card must say so rather than sit there looking
// like it is still waiting.
const timedOut = applyAskEvent(askCardFromEvent(questionEvent), {
  type: 'ask_user',
  ask_id: 'q-1',
  ask_status: 'timeout',
})
html = await renderAskUserCard(timedOut)
check('a timed-out card says the model moved on', html.includes('已超时') && html.includes('模型已按自己的判断继续'))
check('a timed-out card cannot be submitted', !html.includes('提交答案'))

// Reloaded conversation: the question is the persisted tool call's arguments and
// the answer is its result, so the card survives a refresh without a table of its
// own.
const storedCard = askCardFromTool({
  id: 'call-1',
  name: 'ask_user',
  args: JSON.stringify({ question: '改写哪个文件？', header: '改动范围', options: [{ label: 'a.go' }, { label: 'b.go' }] }),
  result: JSON.stringify({ status: 'answered', question: '改写哪个文件？', selected: ['b.go'], answer: 'b.go' }),
})
check('a stored tool call rebuilds its card', storedCard && storedCard.question === '改写哪个文件？')
check('a stored card shows what was answered', storedCard && storedCard.status === 'answered' && storedCard.selected[0] === 'b.go')
html = await renderAskUserCard(storedCard)
check('a reloaded card renders its answer', html.includes('b.go') && html.includes('已提交'))

// A call that produced no result is a turn that died mid-ask: cancelled, not
// silently successful.
const orphan = askCardFromTool({ id: 'call-2', name: 'ask_user', args: JSON.stringify({ question: '选一个？' }), error: 'boom' })
check('an ask that failed is cancelled, not answered', orphan && orphan.status === 'cancelled')
check('a non-question tool is not turned into a card', askCardFromTool({ id: 'c', name: 'bash', args: '{}' }) === null)

// 11. The other half of the contract: what the card actually sends. The path and
//     the body are checked here because the server's own test cannot see them.
const sent = await captureAnswerRequest('sess 1', 'q/2', { selected: ['Postgres'], text: '先用测试库' })
check(
  'the answer goes to the question endpoint',
  sent && sent.url === '/api/chat/sessions/sess%201/questions/q%2F2/answer',
  sent ? sent.url : 'no request',
)
check('the answer is posted', sent && sent.init.method === 'POST')
check(
  'the answer carries the choice and the typed text',
  sent && sent.init.body === JSON.stringify({ selected: ['Postgres'], text: '先用测试库' }),
  sent ? sent.init.body : '',
)

// 12. 设置 → 对话预算: the panel is a render of a server snapshot, and the two
//     facts it must not hide are the source of each value and the warning that
//     in-turn compression is off (the half of "run longer" the panel cannot fix).
html = await renderBudgetPanel({
  max_steps: 60,
  turn_max_tokens: 400000,
  turn_deadline_seconds: 10800,
  default_max_steps: 12,
  default_turn_max_tokens: 0,
  default_turn_deadline_seconds: 0,
  source_max_steps: 'console',
  source_turn_max_tokens: 'config',
  source_turn_deadline_seconds: 'config',
  max_steps_limit: 500,
  has_override: true,
  context_max_tokens: 60000,
  context_requires_restart: false,
})
check('the panel shows the effective step cap', html.includes('value="60"'), html.slice(0, 120))
check('the panel shows the effective token budget', html.includes('value="400000"'))
check('the panel shows the deadline in hours', html.includes('3 小时'))
check('an overridden dimension is labelled', html.includes('已覆盖'))
check('the untouched dimensions read as config', (html.match(/来自配置文件/g) || []).length >= 2)
check('a stored override enables 恢复默认', !html.includes('disabled title="清掉控制台的覆盖"'))
check('compression on is reported, not warned about', html.includes('循环内压缩已开启'))

// With compression off, the panel must warn: raising the cap alone is the
// combination that turns a long task into a context-limit error.
html = await renderBudgetPanel({
  max_steps: 60,
  turn_max_tokens: 0,
  turn_deadline_seconds: 0,
  default_max_steps: 60,
  default_turn_max_tokens: 0,
  default_turn_deadline_seconds: 0,
  source_max_steps: 'config',
  source_turn_max_tokens: 'config',
  source_turn_deadline_seconds: 'config',
  has_override: false,
  context_max_tokens: 0,
  context_requires_restart: true,
})
check('compression off raises the warning', html.includes('context.max_tokens'))
check('the warning names the cost', html.includes('近似平方增长'))
check('unlimited values render as 不限', html.includes('不限'))

// 13. 对话 header: the totals line sits in front of the message count, and a
//     conversation that has run nothing shows one number instead of a row of
//     zeros. Both are asserted because the header is always in view: a wrong
//     number here is seen on every turn.
html = await renderChatHeader({
  session: { id: 's1', title: '统计会话', message_count: 120 },
  stats: {
    messages: 120,
    turns: 7,
    llm_calls: 23,
    llm_duration_ms: 184000,
    tool_calls: 41,
    tool_duration_ms: 5200,
    prompt_tokens: 812345,
    completion_tokens: 41230,
    total_tokens: 853575,
  },
})
check('the header shows the turn count', html.includes('轮 7'), html.slice(0, 120))
check('the header shows model calls with their duration', html.includes('模型 23 次 · 3m04s'))
check('the header shows tool calls with their duration', html.includes('工具 41 次 · 5.2s'))
check('the header shows the token total', html.includes('854K tokens'))
check('the message count is still there', html.includes('共 120 条消息'))
check('the totals carry their explanation', html.includes('不是这段对话的墙钟长度'))

html = await renderChatHeader({
  session: { id: 's2', title: '新对话', message_count: 0 },
  stats: { turns: 0 },
})
check('an unused conversation shows 轮 0', html.includes('轮 0'))
check('an unused conversation shows no token segment', !html.includes('tokens<'), html.slice(0, 200))
check('an unused conversation shows no tool segment', !html.includes('工具 0 次'))

/* ------------------------------------------------------- turn steps -- */

// A stored turn with two steps: one that read a file, one that answered. This is
// the shape the server writes (internal/server/chat.go, persistTurn) and the one
// a reloaded conversation must render.
const twoStepRow = {
  id: 7,
  role: 'assistant',
  content: '文件里写着 hello',
  reasoning: '先看看文件看完了',
  tool_calls: JSON.stringify([
    { id: 'a', name: 'read_file', args: '{"path":"a.txt"}', result: 'hello', duration_ms: 12, step: 1 },
  ]),
  steps: JSON.stringify([
    {
      index: 1,
      reasoning: '先看看文件',
      text: '我先读一下这个文件。',
      tools: [
        { id: 'a', name: 'read_file', args: '{"path":"a.txt"}', result: 'hello', duration_ms: 12, step: 1 },
      ],
    },
    { index: 2, reasoning: '看完了', text: '文件里写着 hello' },
  ]),
}

const builtSteps = messageSteps(twoStepRow)
check('a stored turn keeps its steps', builtSteps.length === 2, JSON.stringify(builtSteps.length))
check('each step keeps its own thinking', builtSteps[0].reasoning === '先看看文件' && builtSteps[1].reasoning === '看完了')
check('each step keeps the tools it ran', builtSteps[0].tools.length === 1 && builtSteps[0].tools[0].name === 'read_file')
check('a step that answered has no tools', builtSteps[1].tools.length === 0)
check(
  'a stored turn arrives folded, so a reload shows the answer first',
  builtSteps[0].open === false && builtSteps[1].open === false,
)

// The bubble itself. A finished turn folds the whole process — steps and
// thinking both — behind one line that says how much work it was, and the answer
// stands below it alone. That is what a reply should look like once it has
// landed; a running turn keeps the same block open (below).
let bubble = await renderMessageBubble({ ...twoStepRow, streaming: false })
const answerIdx = bubble.indexOf('文件里写着 hello</div>')
check('a finished turn folds its process', bubble.includes('steps-head') && !bubble.includes('steps-body'), bubble.slice(0, 200))
check(
  'the folded line counts the tool calls and the messages',
  bubble.includes('1 次工具调用') && bubble.includes('2 条消息'),
  bubble.slice(0, 260),
)
check('the folded line is above the answer', bubble.indexOf('执行过程') < answerIdx && answerIdx > 0)
check('the folded line says how long the tools took', bubble.includes('12ms'))
check('a folded process renders no step headers', !bubble.includes('#1') && !bubble.includes('#2'))
check('a folded process renders no thinking', !bubble.includes('reason-body'))
check('a folded process renders no tool cards', !bubble.includes('tool-card'))
check('the step keeps its thinking for when it is opened', builtSteps[0].reasoning === '先看看文件')

// A turn that is still streaming keeps the process open, so the work — thinking
// and tool cards included — is visible while it happens (one the reader attached
// to halfway through included). The fold header is there too: it is the control,
// not the folded state.
bubble = await renderMessageBubble({ ...twoStepRow, content: '' }, { live: true, pending: true })
check('a streaming turn opens its process', bubble.includes('steps-body') && bubble.includes('#1') && bubble.includes('#2'))
check('a streaming turn opens its steps', bubble.includes('tool-card'))
check('an open process shows the thinking', bubble.includes('reason-body'))
check('an open step shows what it said before acting', bubble.includes('我先读一下这个文件。'))
check('the last step of a live turn reports progress', bubble.includes('推理中…'))
check(
  'the fold line is the same one whether open or folded',
  bubble.includes('1 次工具调用') && bubble.includes('2 条消息'),
)

// A message written before steps were stored: it carries one blob of reasoning
// and a flat tool list, and nothing says which thought asked for which call. It
// is shown as the single block of process it actually is — and still folded, so
// the answer stays on screen.
bubble = await renderMessageBubble({ ...twoStepRow, steps: '', content: '' }, { live: true, pending: true })
check(
  'a turn with no steps stored folds up as one block of process',
  bubble.includes('#1') && bubble.includes('read_file'),
)
check(
  'a legacy turn still reports its counts on the fold line',
  bubble.includes('1 次工具调用') && bubble.includes('1 条消息'),
)
check('the legacy reasoning reaches the step', bubble.includes('先看看文件看完了'))

bubble = await renderMessageBubble({ ...twoStepRow, steps: '', reasoning: '', content: '' }, { live: true, pending: true })
check(
  'a legacy turn rebuilds one step from its flat tool list',
  bubble.includes('#1') && bubble.includes('read_file'),
  bubble.slice(0, 200),
)

// A question is a card, not a tool row: the raw ask_user call must not appear as
// a step of its own next to the answer. The step here has nothing but the call —
// the shape a turn that only asks takes — so it must be dropped entirely.
const askStepRow = {
  id: 8,
  role: 'assistant',
  content: '按你选的做了',
  steps: JSON.stringify([
    {
      index: 1,
      tools: [
        {
          id: 'q1',
          name: 'ask_user',
          args: JSON.stringify({ question: '用哪个方案？', options: [{ label: 'A' }] }),
          result: JSON.stringify({ status: 'answered', selected: ['A'] }),
          step: 1,
        },
      ],
    },
    { index: 2, text: '按你选的做了' },
  ]),
}
// 这一行是进行中的一轮：过程块打开着，所以步骤本身能被断言（收起的形态在上面的
// twoStepRow 那几条里已经覆盖）。
bubble = await renderMessageBubble(askStepRow, { live: true })
check('an ask_user call is not rendered as a tool row', !bubble.includes('ask_user'))
check('the question still renders as the card', bubble.includes('用哪个方案？'))
check(
  'a question-only step is dropped from the process',
  !bubble.includes('#1') && bubble.includes('#2'),
)
check('the card sits between the process and the answer', bubble.indexOf('ask-card') < bubble.indexOf('按你选的做了'))
// ask_user 不算"工具调用"：它渲染成上面的卡片，不是过程里的工具行，计数口径必须
// 与渲染口径一致，否则那一行数字和它下面的内容对不上。
check(
  'an ask_user call is not counted as a tool call',
  bubble.includes('1 条消息') && !bubble.includes('次工具调用'),
  bubble.slice(0, 240),
)

// A turn that just answered has no process to fold, and must not grow an empty one.
bubble = await renderMessageBubble({ id: 9, role: 'assistant', content: '直接回答' })
check('a direct answer renders no step block', !bubble.includes('#1') && bubble.includes('直接回答'))

/* The helpers the fold rules are built from. */
const {
  foldSteps,
  normalizeSteps,
  processShouldBeOpen,
  stepFailed,
  stepSummary,
  stepToolMs,
  visibleSteps,
  isAskTool,
} = stepHelpers

// 过程块的折叠规则：跑着时开着，结束就收起，读者点过之后由读者说了算（浏览器里
// 那一下点击在这里只能用规则本身来断言——SSR 渲染不了交互）。
check('a running turn keeps the process open', processShouldBeOpen({ streaming: true }) === true)
check('a finished turn folds the process', processShouldBeOpen({ streaming: false }) === false)
check('the default is folded', processShouldBeOpen() === false)
check(
  'a reader who opened it keeps it open',
  processShouldBeOpen({ streaming: false, pinned: true }) === true,
)
check(
  'a reader who folded a running turn keeps it shut',
  processShouldBeOpen({ streaming: true, pinned: false }) === false,
)
check(
  'a reader who opened it while it ran keeps it open when the answer lands',
  processShouldBeOpen({ streaming: true, pinned: true }) === true &&
    processShouldBeOpen({ streaming: false, pinned: true }) === true,
)

const live = normalizeSteps(JSON.stringify([{ index: 1, reasoning: 'r' }]))
check('a stored step is readable by the renderer', live.length === 1 && live[0].index === 1)

const folded = [{ open: true, touched: false }, { open: true, touched: true }]
foldSteps(folded, { answered: true, streaming: false })
check('the fold closes an untouched step', folded[0].open === false)
check('the fold leaves a step the reader opened alone', folded[1].open === true)

const unfolded = [{ open: false, touched: false }]
foldSteps(unfolded, { answered: false, streaming: true })
check('a turn with no answer yet opens its steps', unfolded[0].open === true)

const arrived = [{ open: false, touched: false }]
foldSteps(arrived, { answered: true, streaming: false })
check('a step folded by the server stays folded once the answer is there', arrived[0].open === false)

check(
  'a folded step summarizes what it ran',
  stepSummary({ tools: [{ name: 'read_file' }, { name: 'bash' }] }) === 'read_file, bash',
)
check(
  'a long tool list is summarized rather than listed',
  stepSummary({ tools: [{ name: 'a' }, { name: 'b' }, { name: 'c' }, { name: 'd' }] }) === 'a, b, c 等 4 个工具',
)
check('a thinking step says so', stepSummary({ reasoning: 'x' }) === '思考')
check('the newest step of a live turn reports progress', stepSummary({}, { streaming: true, last: true }) === '推理中…')
check('a failed tool marks its step', stepFailed({ tools: [{ status: 'failed' }] }) === true)
check('a clean step is not marked', stepFailed({ tools: [{ status: 'ok' }] }) === false)
check('tool durations are summed', stepToolMs({ tools: [{ durationMs: 5 }, { durationMs: 7 }] }) === 12)
check('a step with no durations reports none', stepToolMs({ tools: [{ durationMs: null }] }) === null)
check(
  'the ask tool is filtered out of the rendered steps',
  visibleSteps([{ index: 1, tools: [{ name: 'ask_user' }] }], isAskTool).length === 0,
)
check(
  'a step with reasoning survives even with no tools',
  visibleSteps([{ index: 1, reasoning: 'x' }]).length === 1,
)

/* --------------------------------------------------------- 任务看板 -- */

// 14. 看板：模型执行长任务时的计划，贴在输入框正上方。这里断言的正是它最容易
//     悄悄错掉的两件事——"没有计划时不能画一个空壳"，以及"分组规则是不是模型
//     真正想要的那一种"（failed 留在待执行，而不是消失在一个没人看的组里）。
const { normalizePlan, planGroups, planProgress, planResumable, planHeadline } = planHelpers

// 没有计划 = 整个组件不渲染。空计划（tasks: []）与缺字段都归一成同一个 null。
html = await renderTaskBoard({ plan: null })
check('no plan renders nothing at all', !html.includes('plan-') && !html.includes('执行中'), html.slice(0, 80))
check('an empty task list is the same as no plan', normalizePlan({ goal: 'x', tasks: [] }) === null)
check('a missing tasks field is the same as no plan', normalizePlan({ goal: 'x' }) === null)
check('garbage is the same as no plan', normalizePlan('nope') === null && normalizePlan(null) === null)

const planFixture = {
  goal: '把 chat 的失败续跑做出来',
  revision: 4,
  updated_at: '2026-09-17T10:00:00Z',
  tasks: [
    { id: 't1', title: '读 runner.go 的循环', status: 'done', note: '已经确认 step 边界' },
    { id: 't2', title: '加步骤级重试', status: 'in_progress', note: '' },
    { id: 't3', title: '跑 go test ./...', status: 'pending' },
    { id: 't4', title: '接上 /resume 端点', status: 'failed', note: '409: 已有一轮在跑' },
    { id: 't5', title: '写迁移脚本', status: 'skipped' },
  ],
}

const builtPlan = normalizePlan(planFixture)
check(
  'the wire plan is readable',
  builtPlan && builtPlan.tasks.length === 5 && builtPlan.revision === 4,
  builtPlan ? JSON.stringify(builtPlan.tasks.length) : 'null',
)
check('the goal survives', builtPlan && builtPlan.goal === '把 chat 的失败续跑做出来')
check('the server timestamps are read', builtPlan && builtPlan.updatedAt === '2026-09-17T10:00:00Z')

html = await renderTaskBoard({ plan: planFixture })
check('the board shows its goal', html.includes('把 chat 的失败续跑做出来'), html.slice(0, 160))
check(
  'the board shows all three groups',
  html.includes('执行中') && html.includes('待执行') && html.includes('已完成'),
)
check('the board shows the progress numbers', html.includes('1/5'), html.slice(0, 200))
check('the board shows the task titles', html.includes('加步骤级重试') && html.includes('跑 go test ./...'))

// failed 留在「待执行」组里（标红并写出原因），skipped 划掉；它们都不自成一组。
// 位置用分组标题的标记来锚定：头部的 title 里也有"已完成"三个字，拿它当界标会
// 把组头自己也算进去。
const todoStart = html.indexOf('<span>待执行</span>')
const doneStart = html.indexOf('<span>已完成</span>')
const failedAt = html.indexOf('接上 /resume 端点')
check(
  'a failed task stays in 待执行',
  todoStart >= 0 && doneStart > todoStart && failedAt > todoStart && failedAt < doneStart,
  `todo=${todoStart} failed=${failedAt} done=${doneStart}`,
)
check('a failed task states its reason', html.includes('409: 已有一轮在跑'))
// Vue 在 SSR 里把动态 class 拼在静态 class 前面，所以这里按"类名里两者都有"来断。
check('a failed task is marked as failed', html.includes('class="failed plan-item"'))
check('a failed task says so in words too', html.includes('>失败</span>'))
check('a skipped task is struck through', html.includes('class="skipped plan-item"'))
check('a task note is rendered', html.includes('已经确认 step 边界'))

// 折叠：只剩头部一行，摘要必须在其中，分组标题一个都不该出现。
html = await renderTaskBoard({ plan: planFixture, collapsed: true })
check('collapsed keeps the goal and the numbers', html.includes('把 chat 的失败续跑做出来') && html.includes('1/5'))
check('collapsed shows the headline', html.includes(planHeadline(planFixture)))
check(
  'collapsed hides every group',
  !html.includes('<span>执行中</span>') && !html.includes('<span>待执行</span>') && !html.includes('plan-list'),
  html.slice(0, 200),
)
check('the probe can read the board state back', planState().collapsed === true && planState().tasks === 5)

// 「继续执行」只在有未完成项、并且没有轮次在跑时出现。
html = await renderTaskBoard({ plan: planFixture, streaming: false })
check('a resumable plan offers 继续执行', html.includes('继续执行'))
check('the button explains what it does', html.includes('不会从零重来'))
html = await renderTaskBoard({ plan: planFixture, streaming: true })
check('a running turn dashes the resume button', !html.includes('继续执行'), html.slice(0, 200))

const allDone = { goal: '收尾', tasks: [{ id: 't1', title: '全部做完', status: 'done' }] }
html = await renderTaskBoard({ plan: allDone })
check('a finished plan has nothing to resume', !html.includes('继续执行'))

// 归一化：脏数据不能把看板搞崩。未知 status 当待执行、缺 id 用序号补、
// 没标题的整条丢掉（一行没有内容的行会让"还剩几件"这个数字不可信）。
const dirty = normalizePlan({
  goal: '脏数据',
  tasks: [
    { id: 't1', title: 'A', status: 'weird' },
    { title: 'B' },
    { id: 't3', title: '   ' },
    { id: 't3', title: 'C', status: 'done' },
  ],
})
check('an unknown status becomes pending', dirty && dirty.tasks[0].status === 'pending')
check('a missing id is numbered from the list', dirty && dirty.tasks[1].id === 't2')
check('a task with no title is dropped', dirty && dirty.tasks.length === 3 && !dirty.tasks.some((t) => t.title === ''))
check('a title is trimmed', dirty && dirty.tasks[2].title === 'C')
check('a missing note is an empty string', dirty && dirty.tasks[1].note === '')

// 分组与进度：failed 排在待执行最前，done 单独一组。
const grouped = planGroups(planFixture)
check('in_progress is the doing group', grouped.doing.length === 1 && grouped.doing[0].id === 't2')
check('done is its own group', grouped.done.length === 1 && grouped.done[0].id === 't1')
check(
  'failed leads the todo group',
  grouped.todo.length === 3 && grouped.todo[0].id === 't4',
  grouped.todo.map((t) => t.id).join(','),
)
check(
  'the todo group keeps the other tasks in order',
  grouped.todo[1].id === 't3' && grouped.todo[2].id === 't5',
)
const progress = planProgress(planFixture)
check('progress counts the finished tasks', progress.done === 1 && progress.total === 5)
check('progress is a whole percentage', progress.percent === 20, String(progress.percent))
check('an empty plan is 0/0 = 0%', planProgress(null).percent === 0 && planProgress(null).total === 0)
check('all done is 100%', planProgress(allDone).percent === 100)
check('a plan with work left is resumable', planResumable(planFixture) === true)
check('a finished plan is not resumable', planResumable(allDone) === false)
check('no plan is not resumable', planResumable(null) === false)
// 只剩 skipped 的计划没有接续点：跳过是结论，不是遗留的工作。这一条必须与服务端
// tool.Plan.Unfinished() 同口径，否则按钮点下去只会收到 400。
const nothingLeft = {
  goal: '只剩结论了',
  tasks: [
    { id: 't1', title: '做完的', status: 'done' },
    { id: 't2', title: '不做了的', status: 'skipped' },
  ],
}
check('a plan of done and skipped work is not resumable', planResumable(nothingLeft) === false)
check(
  'a plan whose only work failed is resumable',
  planResumable({ goal: 'g', tasks: [{ id: 't1', title: '炸了', status: 'failed' }] }) === true,
)
check(
  'the headline names what is running',
  planHeadline(planFixture) === '已完成 1/5 · 进行中：加步骤级重试',
  planHeadline(planFixture),
)
check(
  'the headline falls back to the next task when nothing is running',
  planHeadline({ tasks: [{ id: 't1', title: 'A', status: 'done' }, { id: 't2', title: 'B', status: 'pending' }] }) ===
    '已完成 1/2 · 下一步：B',
)
check('no plan has no headline', planHeadline(null) === '')

/* --------------------------------------------- 步骤级重试与继续执行 -- */

// 15. store 里的两段"看不见的"逻辑：`step_retry` 的 reducer，和「继续执行」被拒绝
//     时的回滚。它们都不产出 HTML，只在事件到达的那一刻起作用——错的话要么界面上
//     留着半截失败的文字，要么留下一条其实没发出去的气泡，两者都很难在事后看出来。
//
// 这里直接跑 store（不是渲染探针）：SSE 由 fetch 的桩喂进去，服务端写什么帧就用
// 什么帧。放在最后，因为它会替换 globalThis.fetch 与 window。
globalThis.window = globalThis
const store = await import('../src/chatStore.js')

const encoder = new TextEncoder()
function sseResponse(events) {
  const body = events.map((event) => `data: ${JSON.stringify(event)}\n\n`).join('')
  return {
    ok: true,
    status: 200,
    headers: { get: () => 'text/event-stream' },
    body: new ReadableStream({
      start(controller) {
        controller.enqueue(encoder.encode(body))
        controller.close()
      },
    }),
  }
}

// 第 2 步吐到一半断了，服务端重发这一步：第一次写的东西必须从界面上退回去。
globalThis.fetch = async () =>
  sseResponse([
    { type: 'step_start', step: 1 },
    { type: 'reasoning_delta', step: 1, text: '先看文件' },
    { type: 'step_end', step: 1 },
    { type: 'step_start', step: 2 },
    { type: 'reasoning_delta', step: 2, text: '被丢弃的思考' },
    { type: 'text_delta', step: 2, text: '半截回答' },
    { type: 'step_retry', step: 2, attempt: 2, max_attempts: 3, delay_ms: 1600, error: 'unexpected EOF' },
    { type: 'text_delta', step: 2, text: '重试后的回答' },
    { type: 'done', text: '重试后的回答' },
    { type: 'stream_end' },
  ])

store.chat.activeId = 'retry-1'
store.chat.session = { id: 'retry-1', streaming: true }
store.chat.items = []
await store.attachTurn('retry-1', { show: true })
const retried = store.chat.items.find((item) => item.role === 'assistant')
check('the retried step keeps only the second attempt', retried.steps[1].text === '重试后的回答', JSON.stringify(retried.steps[1].text))
check('the retried step drops the discarded thinking', retried.steps[1].reasoning === '')
check('the answer is the retried one', retried.text === '重试后的回答')
check('an earlier step keeps its own thinking', retried.steps[0].reasoning === '先看文件')
check('the turn keeps the thinking that was not retried', retried.reasoning === '先看文件', JSON.stringify(retried.reasoning))
check(
  'the retry says which attempt it is and why',
  retried.notices.length === 1 &&
    retried.notices[0].kind === 'retry' &&
    retried.notices[0].text === '第 2/3 次尝试，1.6s 后重试（unexpected EOF）',
  retried.notices.length ? retried.notices[0].text : 'no notice',
)

// 继续执行：被拒绝时那条本地气泡必须消失（界面上不能留下没发出去的消息）。
async function resumeAgainst(status, body) {
  const sent = []
  globalThis.fetch = async (url, init) => {
    sent.push({ url: String(url), init: init || {} })
    return { ok: status >= 200 && status < 300, status, text: async () => JSON.stringify(body || {}) }
  }
  store.chat.activeId = 'retry-1'
  store.chat.streaming = false
  store.chat.items = []
  store.chat.actionError = ''
  await store.resumeTurn()
  return sent
}

let resumeSent = await resumeAgainst(409, { error: 'busy' })
check(
  'resume posts to the conversation resume route',
  resumeSent[0] && resumeSent[0].url === '/api/chat/sessions/retry-1/resume' && resumeSent[0].init.method === 'POST',
  resumeSent[0] ? resumeSent[0].url : 'no request',
)
check('a 409 says the conversation is busy', store.chat.actionError === '这个对话已有一轮正在生成，先停止它或等它结束', store.chat.actionError)
check('a 409 takes the optimistic bubble back', store.chat.items.length === 0)

resumeSent = await resumeAgainst(400, { error: '没有可接续的内容' })
check('a 400 shows the server reason', store.chat.actionError === '没有可接续的内容', store.chat.actionError)
check('a 400 takes the optimistic bubble back', store.chat.items.length === 0)

globalThis.fetch = async (url) => {
  const path = String(url)
  if (path.includes('/resume')) return { ok: true, status: 202, text: async () => JSON.stringify({ turn: {} }) }
  if (path.includes('/sessions?')) return { ok: true, status: 200, text: async () => JSON.stringify({ sessions: [] }) }
  return { ok: true, status: 200, text: async () => JSON.stringify({ session: { id: 'retry-1' }, messages: [] }) }
}
store.chat.items = []
store.chat.actionError = 'stale'
await store.resumeTurn()
check(
  'a successful resume shows 继续执行 as the user said it',
  store.chat.items.some((item) => item.role === 'user' && item.text === '继续执行'),
)
check('a successful resume clears the old error', store.chat.actionError === '')

// 新消息＝新任务：上一轮已经收尾的计划要在点发送的瞬间从看板上消失，而还有
// 未完成项的计划必须留着（它是「继续执行」的接续点）。
async function sendAgainst(plan) {
  globalThis.fetch = async (url) => {
    const path = String(url)
    if (path.includes('/messages')) return { ok: true, status: 202, text: async () => JSON.stringify({ turn: {} }) }
    if (path.includes('/sessions?')) return { ok: true, status: 200, text: async () => JSON.stringify({ sessions: [] }) }
    return { ok: true, status: 200, text: async () => JSON.stringify({ streaming: false }) }
  }
  store.chat.activeId = 'plan-1'
  store.chat.streaming = false
  store.chat.items = []
  store.chat.plan = plan
  await store.sendMessage('下一件事')
  return store.chat.plan
}

const afterFinished = await sendAgainst({
  goal: '上一件事',
  tasks: [
    { id: 't1', title: '做完了', status: 'done' },
    { id: 't2', title: '不做了', status: 'skipped' },
  ],
})
check('a new message clears a finished plan', afterFinished === null, JSON.stringify(afterFinished))

const afterOpen = await sendAgainst({
  goal: '还没做完的',
  tasks: [
    { id: 't1', title: '做完了', status: 'done' },
    { id: 't2', title: '还没做', status: 'pending' },
  ],
})
check(
  'a new message keeps an unfinished plan',
  afterOpen && afterOpen.tasks.length === 2,
  JSON.stringify(afterOpen),
)

// 服务端广播的空计划（新请求开始时清掉上一件事的计划）也必须让看板消失。
const cleared = normalizePlan({ revision: 3, tasks: [] })
check('an empty plan event means no board', cleared === null)

/* ---------------------------------------------------------- approval gate -- */

// The card that asks before a write or a command runs. What it has to get right is
// the thing the feature exists for: showing enough to judge, and never offering a
// button for a request the server has already settled.

const approvalEvent = {
  type: 'approval',
  approval: {
    id: 'ap-1',
    tool: 'bash',
    capability: 'exec',
    summary: '执行: git push origin main',
    preview: [
      { kind: 'meta', text: '完整命令：git push origin main' },
      { kind: 'meta', text: '工作目录：/srv/app' },
    ],
    default: 'deny',
    timeout_ms: 300000,
  },
}

const pendingApproval = approvalCardFromEvent(approvalEvent)
check('an announcement becomes a card', Boolean(pendingApproval) && pendingApproval.id === 'ap-1')
check('the card keeps the summary', pendingApproval.summary.includes('git push origin main'))
check('the card keeps the preview kinds', pendingApproval.preview.length === 2 && pendingApproval.preview[0].kind === 'meta')
check('an outcome event is not an announcement', approvalCardFromEvent({ type: 'approval', approval_id: 'ap-1' }) === null)
check('a request with no id is dropped', approvalCardFromEvent({ approval: { summary: 'x' } }) === null)

html = await renderApprovalCard(pendingApproval)
check('a pending card shows the summary', html.includes('git push origin main'))
check('a pending card shows the full command', html.includes('完整命令'))
check('a pending card says what it is about to do', html.includes('需要确认：执行命令'))
check('a pending card offers three decisions', html.includes('允许一次') && html.includes('本轮都允许') && html.includes('拒绝'))
check('a pending card shows the wait it is under', html.includes('后按拒绝处理'))
check('a pending card explains the two allows', html.includes('本轮都允许在这一次回答结束前不再询问'))

// Allowing: the card loses its buttons and shows what was decided.
const allowed = approvalCardFromEvent(approvalEvent)
applyApprovalEvent(allowed, { approval_id: 'ap-1', approval_decision: 'allow_once', approval_source: 'human' })
html = await renderApprovalCard(allowed)
// The outcome line says "允许一次" too, so the assertion has to be about the
// action row rather than about that word appearing somewhere in the markup.
check(
  'a settled card offers no buttons',
  !html.includes('approval-actions'),
  html.includes('approval-actions') ? 'the action row is still rendered' : '',
)
check('a settled card shows the outcome', html.includes('approval-outcome') && html.includes('允许一次'))
check('a settled card is not open', approvalHelpers.isApprovalOpen(allowed) === false)

// Refusing with a reason: the reason is what the model is given instead.
const refused = approvalCardFromEvent(approvalEvent)
applyApprovalEvent(refused, { approval_id: 'ap-1', approval_decision: 'deny', approval_reason: '不要动主分支', approval_source: 'human' })
html = await renderApprovalCard(refused)
check('a refusal shows the reason', html.includes('不要动主分支'))
check('a refusal offers no buttons', !html.includes('approval-actions'))

// A timeout is a refusal, and the card has to say which of the two it was: "a
// person said no" and "nobody was there" are different facts.
const expiredApproval = approvalCardFromEvent(approvalEvent)
applyApprovalEvent(expiredApproval, { approval_id: 'ap-1', approval_decision: 'deny', approval_source: 'timeout' })
check('a timeout reads as a refusal', approvalHelpers.approvalOutcomeLine(expiredApproval).includes('拒绝'))
check('a timeout says nobody answered', approvalHelpers.approvalOutcomeLine(expiredApproval).includes('没有人回应'))

// The pending list is what the console shows above the composer.
const turn = { approvals: [approvalCardFromEvent(approvalEvent), allowed, refused] }
check('only open requests are shown above the composer', approvalPending(turn).length === 1)

// The countdown never goes negative: a negative number would suggest the request
// is still alive after the server has refused it.
const counting = approvalCardFromEvent(approvalEvent)
check('the countdown starts at the server wait', approvalHelpers.approvalSecondsLeft(counting, counting.announcedAt) === 300)
check('the countdown counts down', approvalHelpers.approvalSecondsLeft(counting, counting.announcedAt + 60000) === 240)
check('the countdown stops at zero', approvalHelpers.approvalSecondsLeft(counting, counting.announcedAt + 999999) === 0)
applyApprovalEvent(counting, { approval_decision: 'allow_once' })
check('a settled card has no countdown', approvalHelpers.approvalSecondsLeft(counting, counting.announcedAt) === null)

// The decision travels on a URL the server defines; a typo would look exactly
// like a refused request.
const captured = await captureApprovalRequest('sess-1', 'ap-1', { decision: 'allow_once', reason: '' })
check(
  'the decision posts to the approval route',
  captured && captured.url === '/api/chat/sessions/sess-1/approvals/ap-1',
  captured ? captured.url : 'no request captured',
)
check(
  'the decision travels in the body',
  captured && captured.init.body === JSON.stringify({ decision: 'allow_once', reason: '' }),
  captured ? captured.init.body : '',
)



/* ------------------------------------------------ nested subagent work -- */

// What a spawned agent's actions look like on the card of the call that spawned it.
// The property that matters most is the negative one: a subagent's intermediate work
// must not look like the turn's own steps, or a reader counts work the model
// delegated as work it did.

const nestedRaw = [
  { kind: 'reasoning', text: '先看看目录结构。' },
  { kind: 'text', text: '结论是：有 39 个文件。' },
  { kind: 'tool', name: 'grep', id: 'call-2', result: '24 matches\nsecond line' },
  { kind: 'tool', name: 'read_file', id: 'call-3', error: 'no such file' },
  { kind: 'nonsense', text: 'dropped' },
  { text: 'no kind, dropped' },
]

const nested = nestedOf(nestedRaw)
check('nested entries are normalised', nested.length === 4, JSON.stringify(nested))
check('an unknown kind is dropped', !nested.some((n) => n.kind === 'nonsense'))
check('a tool entry keeps what it returned', nested[2].result.includes('24 matches'))
check('a failed nested call keeps its error', nested[3].error === 'no such file')
check('no nested entries is an empty list, not undefined', nestedOf(undefined).length === 0)

// A stored turn carries them, so a reloaded conversation shows the same thing.
//
// The assertion is on the built step data rather than the HTML because a settled
// turn's process is folded, and a probe that read the markup would be testing the
// fold rather than the nesting.
const spawnMessage = {
  id: 'm2', role: 'assistant', content: '它说：结论是 39 个文件。', session_id: 's1', created_at: '',
  tool_calls: JSON.stringify([
    {
      id: 'call-1', name: 'spawn_agent', args: '{"prompt":"看看 internal/tool"}',
      result: '子 agent 的结论。', nested: nestedRaw.slice(0, 3),
    },
  ]),
}
const spawnSteps = messageSteps(spawnMessage)
const spawnCall = spawnSteps.length ? spawnSteps[0].tools[0] : null
check('a stored spawn call rebuilds its nested actions', Boolean(spawnCall) && spawnCall.nested.length === 3,
  spawnCall ? JSON.stringify(spawnCall.nested) : 'no call')
check('a nested tool line names the tool', spawnCall && spawnCall.nested[2].name === 'grep')
check('a nested reasoning line is kept as thinking', spawnCall && spawnCall.nested[0].kind === 'reasoning')
check('a delivered report is still the call result', spawnCall && spawnCall.result.includes('子 agent 的结论。'))
// The nested calls are not the turn's own steps: a reader must not count work the
// model delegated as work it did.
const spawnStepCalls = spawnSteps.length ? spawnSteps[0].tools.length : 0
check('the turn has exactly one tool call', spawnStepCalls === 1, String(spawnStepCalls))



/* ------------------------------------------------ delegated subagents -- */

// The drawer that answers "what did this conversation delegate, and what is it
// doing". Two things it must get right, and both are about honesty rather than
// layout: a run that is still working is shown as working, and a run whose step
// count is unavailable says so instead of showing a zero that reads as "it did
// nothing".

html = await renderSubagentsDrawer()
check('an empty drawer explains what subagents are for', html.includes('还没有委派过子 agent'))

// The store is the single source: the probe drives it directly, the way a load
// would, and the drawer renders from the same place the chip does.
setSubagents([
  {
    id: 'sa-2', name: 'auth-survey', prompt: '调研鉴权怎么做', status: 'running',
    parent_tool_call_id: 'call-1', started_at: new Date().toISOString(), duration_ms: 4200,
    steps: -1, tokens: 0,
  },
  {
    id: 'sa-1', name: 'db-survey', prompt: '调研数据库层', status: 'ok',
    parent_tool_call_id: 'call-0', started_at: new Date().toISOString(),
    ended_at: new Date().toISOString(), duration_ms: 8100, steps: 6, tokens: 2400,
  },
  {
    id: 'sa-0', name: 'net-survey', prompt: '调研网络层', status: 'failed',
    error: '模型调用超时', duration_ms: 30000, steps: -1, tokens: 100,
  },
], { running: 1, maxConcurrent: 2, runningAll: 3 })

html = await renderSubagentsDrawer()
check('a running subagent is listed as running', html.includes('运行中') && html.includes('auth-survey'))
check('the running run shows its task', html.includes('调研鉴权怎么做'))
check('a finished run is listed separately', html.includes('已结束') && html.includes('db-survey'))
check('a finished run shows its steps and tokens', html.includes('6 步') && html.includes('2,400'))
check(
  'an uncountable step count says so rather than showing zero',
  !html.includes('0 步') && html.includes('步数不可得'),
  html.includes('0 步') ? 'the drawer showed "0 步", which reads as "it did nothing"' : '',
)
// The rule is precise, not "never show a zero": a run that really did no steps and
// said so is honest, and hiding it would be the opposite mistake.
setSubagents([{ id: 'sa-z', name: 'z', prompt: 'q', status: 'ok', steps: 0, duration_ms: 5 }])
html = await renderSubagentsDrawer()
check('a known zero step count is still shown', html.includes('0 步'), html.slice(0, 120))
setSubagents([
  {
    id: 'sa-2', name: 'auth-survey', prompt: '调研鉴权怎么做', status: 'running',
    parent_tool_call_id: 'call-1', started_at: new Date().toISOString(), duration_ms: 4200,
    steps: -1, tokens: 0,
  },
  {
    id: 'sa-1', name: 'db-survey', prompt: '调研数据库层', status: 'ok',
    parent_tool_call_id: 'call-0', started_at: new Date().toISOString(),
    ended_at: new Date().toISOString(), duration_ms: 8100, steps: 6, tokens: 2400,
  },
  {
    id: 'sa-0', name: 'net-survey', prompt: '调研网络层', status: 'failed',
    error: '模型调用超时', duration_ms: 30000, steps: -1, tokens: 100,
  },
], { running: 1, maxConcurrent: 2, runningAll: 3 })
html = await renderSubagentsDrawer()
check('a failed run says why', html.includes('模型调用超时'))
check('the cap is stated when runs are queued', html.includes('排队') && html.includes('2'))
check('the drawer offers no stop button it cannot honour', !html.includes('停止'))

// The chip's own facts come from the same store.
check('the store reports liveness', subagentChip().running === 1)
check('the chip leads with liveness', subagentChip().label.includes('1 个子 agent 运行中'))
check('the chip explains the cap', subagentChip().hint.includes('上限 2'))

// Nothing running: the label switches to the record count rather than showing the
// stale liveness figure.
setSubagents([{ id: 'sa-9', name: 'x', prompt: 'y', status: 'ok', steps: 1, duration_ms: 10 }],
  { running: 0, maxConcurrent: 2, runningAll: 0 })
check('an idle chip counts records', subagentChip().label.includes('1 个子 agent 记录'))

/* -------------------------------------------- 设置 → 模型 (window, capabilities) -- */

// The window size and the capability set have to be visible where the models are
// listed, with the provenance of each — the failure this probe exists for is a
// column that was added to the wrong panel.
{
  const html = await renderModelPanel({
    catalog: {
      models: [
        {
          provider: 'commandcode', provider_name: 'commandcode',
          model: 'MiniMaxAI/MiniMax-M3', display_name: 'MiniMax M3',
          capabilities: ['chat', 'vision', 'tools'], capabilities_source: 'asked',
          context_window: 1000000, context_window_source: 'api',
          chat_capable: true, default: true, has_api_key: true,
        },
        {
          provider: 'deepseek', provider_name: 'DeepSeek',
          model: 'deepseek-v4-pro', display_name: 'DeepSeek V4 Pro',
          capabilities: ['chat'], capabilities_source: 'inferred',
          context_window: 128000, context_window_source: 'asked',
          chat_capable: true, has_api_key: true,
        },
        {
          provider: 'deepseek', provider_name: 'DeepSeek',
          model: 'unknown-model', display_name: 'Unknown',
          capabilities: [], chat_capable: true, has_api_key: false,
        },
      ],
      providers: [
        { id: 'commandcode', name: 'commandcode', model_count: 1, has_key: true },
        { id: 'deepseek', name: 'DeepSeek', model_count: 2, has_key: true },
      ],
      groups: [
        {
          provider: 'commandcode', providerName: 'commandcode', hasKey: true,
          models: [{
            provider: 'commandcode', providerName: 'commandcode',
            model: 'MiniMaxAI/MiniMax-M3', displayName: 'MiniMax M3',
            capabilities: ['chat', 'vision', 'tools'], capabilitiesSource: 'asked',
            contextWindow: 1000000, contextWindowSource: 'api',
            chatCapable: true, isDefault: true, hasKey: true,
          }],
        },
        {
          provider: 'deepseek', providerName: 'DeepSeek', hasKey: true,
          models: [
            {
              provider: 'deepseek', providerName: 'DeepSeek',
              model: 'deepseek-v4-pro', displayName: 'DeepSeek V4 Pro',
              capabilities: ['chat'], capabilitiesSource: 'inferred',
              contextWindow: 128000, contextWindowSource: 'asked',
              chatCapable: true, hasKey: true,
            },
            {
              provider: 'deepseek', providerName: 'DeepSeek',
              model: 'unknown-model', displayName: 'Unknown',
              capabilities: [], chatCapable: true, hasKey: false,
            },
          ],
        },
      ],
    },
  })

  check('设置 → 模型 has a window column', html.includes('窗口大小'), '')
  check('the window column is a table heading', /<th[^>]*>\s*窗口大小/.test(html), '')
  check('a published window is shown', html.includes('1,000,000'), '')
  check('its provenance is shown next to it', html.includes('接口'), '')
  check('a self-reported window says so', html.includes('自报'), '')
  check('a model nobody described reads as unknown', html.includes('未知'), '')
  check('capabilities are still listed', html.includes('工具调用'), '')
  check('the capabilities come with their source', html.includes('模型自报'), '')
  check('the footer says where to change a window', html.includes('模型管理'), '')
}

console.log(failures === 0 ? '\nALL PROBES PASSED' : `\n${failures} PROBE(S) FAILED`)
process.exit(failures === 0 ? 0 : 1)

