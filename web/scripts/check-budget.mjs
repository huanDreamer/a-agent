// The panel's input rules, checked without a browser.
//
// 设置 → 对话预算 turns three text inputs into a PUT body, and every mistake it can
// make is a number a turn then runs with: 0 read as "empty" instead of
// "unlimited", a typo'd 600 passed through as a six-hundred-step turn, or a save
// that silently pins the two dimensions the operator never touched. None of that
// is visible in a rendered page, so it is asserted here:
//
//     node scripts/check-budget.mjs
//
// (Run from web/, like check-component-bindings.mjs.)
import {
  BUDGET_LIMITS,
  budgetForm,
  defaultForm,
  formatBudget,
  formatSeconds,
  parseBudgetField,
  parseBudgetForm,
} from '../src/budget.js'

let failures = 0
const check = (name, cond) => {
  if (!cond) {
    failures++
    console.error(`FAIL ${name}`)
  }
}

// --- one field ---------------------------------------------------------

check('empty means leave alone', parseBudgetField('max_steps', '').value === null)
check('whitespace is empty', parseBudgetField('max_steps', '   ').value === null)
check('a plain integer passes', parseBudgetField('max_steps', '60').value === 60)
check('zero is a real value for tokens', parseBudgetField('turn_max_tokens', '0').value === 0)
check('zero steps is refused', parseBudgetField('max_steps', '0').ok === false)
check('a negative is refused', parseBudgetField('max_steps', '-3').ok === false)
check('above the ceiling is refused', parseBudgetField('max_steps', '501').ok === false)
check('the ceiling itself passes', parseBudgetField('max_steps', '500').value === 500)
check('exponent notation is refused', parseBudgetField('max_steps', '1e3').ok === false)
check('a decimal is refused', parseBudgetField('max_steps', '60.5').ok === false)
check('letters are refused', parseBudgetField('max_steps', 'sixty').ok === false)
check('an unknown field is refused', parseBudgetField('nope', '1').ok === false)
check(
  'the deadline ceiling is a day',
  parseBudgetField('turn_deadline_seconds', '86401').ok === false &&
    parseBudgetField('turn_deadline_seconds', '86400').value === 86400,
)

// --- the whole form ----------------------------------------------------

const partial = parseBudgetForm({ max_steps: '60', turn_max_tokens: '', turn_deadline_seconds: '' })
check('only the filled field is sent', partial.ok === true && partial.body.max_steps === 60)
check(
  'an untouched field is not pinned',
  partial.ok === true && !('turn_max_tokens' in partial.body) && !('turn_deadline_seconds' in partial.body),
)

const explicitZero = parseBudgetForm({ max_steps: '', turn_max_tokens: '0', turn_deadline_seconds: '' })
check('explicit 0 is sent as 0', explicitZero.ok === true && explicitZero.body.turn_max_tokens === 0)

const empty = parseBudgetForm({ max_steps: '', turn_max_tokens: '', turn_deadline_seconds: '' })
check('an empty form is refused with a message', empty.ok === false && Boolean(empty.errors.max_steps))

const bad = parseBudgetForm({ max_steps: '900', turn_max_tokens: '-1', turn_deadline_seconds: '60' })
check(
  'errors are reported per field',
  bad.ok === false && Boolean(bad.errors.max_steps) && Boolean(bad.errors.turn_max_tokens) &&
    !bad.errors.turn_deadline_seconds,
)

// --- rendering from a snapshot ----------------------------------------

const snapshot = {
  max_steps: 60,
  turn_max_tokens: 400000,
  turn_deadline_seconds: 10800,
  default_max_steps: 12,
  default_turn_max_tokens: 0,
  default_turn_deadline_seconds: 0,
  context_max_tokens: 60000,
}
check(
  'the form shows the effective values',
  budgetForm(snapshot).max_steps === '60' &&
    budgetForm(snapshot).turn_max_tokens === '400000' &&
    budgetForm(snapshot).turn_deadline_seconds === '10800',
)
check(
  'the placeholders name the configured defaults',
  defaultForm(snapshot).max_steps === '12' && defaultForm(snapshot).turn_max_tokens === '0',
)
check('0 seconds reads as unlimited', formatSeconds(0) === '不限')
check('10800 seconds reads as hours', formatSeconds(10800) === '3 小时')
check('90 seconds reads as seconds', formatSeconds(90) === '90 秒')
check(
  'the summary is one line about all three',
  formatBudget(snapshot) === '单轮 60 步 · 400K tokens · 3 小时',
)

// The limits the panel repeats must be the ones the server enforces; if this
// drifts, the panel starts accepting input the server answers with a 400.
check('every dimension has limits', Object.keys(BUDGET_LIMITS).length === 3)

if (failures) {
  console.error(`\n${failures} budget check(s) failed`)
  process.exit(1)
}
console.log('budget checks passed')
