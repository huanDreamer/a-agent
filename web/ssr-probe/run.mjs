// Render probes for the console's dialogs, sidebar and login gate.
//
// These exist because a bug in this area is invisible everywhere else: a setup
// binding shadowing a prop made the directory picker render on load and ignore
// its `open` prop, which no type check, build or linter can see. Rendering the
// component and asserting on the HTML can.
//
//     cd web && npm run check:ui
import {
  applyAskEvent,
  askCardAnswer,
  captureAnswerRequest,
  askCardFromEvent,
  askCardFromTool,
  gateNeedsLogin,
  renderAskUserCard,
  renderDirPicker,
  renderLogin,
  renderSidebar,
  setAuth,
  setChatState,
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

console.log(failures === 0 ? '\nALL PROBES PASSED' : `\n${failures} PROBE(S) FAILED`)
process.exit(failures === 0 ? 0 : 1)
