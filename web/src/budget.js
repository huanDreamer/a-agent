// The per-turn budget, as a form (设置 → 对话预算).
//
// This module owns everything that can be got wrong about it, so the panel is a
// render of `parseBudgetField`'s answer rather than a place where three numbers
// are read and hoped for:
//
//   * the limits are the server's, repeated here so a typo is caught before the
//     round trip (the server refuses the same values with a 400, and that refusal
//     is still shown when it happens);
//   * "" means "leave this field alone" for a save and "restore the configured
//     default" for 恢复默认 — those are different intents and the API expresses
//     them with pointer fields and reset_all, so the panel must not conflate them;
//   * 0 is a real value for the two dimensions where the server reads it as
//     unlimited. It is never treated as "empty".
//
// It is a separate module from chatStore.js on purpose: the store talks to the
// API and holds the state, this decides what a field's text means. The second is
// the part worth testing on its own.

/** The server's ceilings, mirroring internal/server/budget.go. */
export const BUDGET_LIMITS = {
  max_steps: { min: 1, max: 500, label: '步数上限' },
  turn_max_tokens: { min: 0, max: 20000000, label: 'token 预算' },
  turn_deadline_seconds: { min: 0, max: 86400, label: '时间上限（秒）' },
}

/**
 * parseBudgetField turns one field's text into a decision.
 *
 * Returns `{ ok: true, value }` with `value === null` meaning "empty", or
 * `{ ok: false, error }` with a message the panel can show next to the input.
 */
export function parseBudgetField(key, raw) {
  const limit = BUDGET_LIMITS[key]
  if (!limit) return { ok: false, error: `未知字段 ${key}` }

  const text = String(raw ?? '').trim()
  if (text === '') return { ok: true, value: null }

  // Digits and an optional leading minus only: Number('') is 0 and Number('1e3')
  // is 1000, and neither is what someone typing into a step cap meant.
  if (!/^-?\d+$/.test(text)) {
    return { ok: false, error: `${limit.label}必须是整数` }
  }
  const value = Number(text)
  if (!Number.isFinite(value)) {
    return { ok: false, error: `${limit.label}必须是整数` }
  }
  if (value < limit.min) {
    return {
      ok: false,
      error: limit.min === 0
        ? `${limit.label}不能小于 0（0 表示不限）`
        : `${limit.label}至少为 ${limit.min}`,
    }
  }
  if (value > limit.max) {
    return { ok: false, error: `${limit.label}不能超过 ${limit.max}` }
  }
  return { ok: true, value }
}

/**
 * parseBudgetForm validates every field at once.
 *
 * Returns `{ ok: true, body }` where body is the PUT payload — only the fields
 * that were filled in — or `{ ok: false, errors }` keyed by field. Sending only
 * the filled fields is deliberate: the server resolves each dimension
 * independently, so writing one must not pin the other two.
 */
export function parseBudgetForm(form) {
  const errors = {}
  const body = {}
  for (const key of Object.keys(BUDGET_LIMITS)) {
    const parsed = parseBudgetField(key, form[key])
    if (!parsed.ok) {
      errors[key] = parsed.error
      continue
    }
    if (parsed.value !== null) body[key] = parsed.value
  }
  if (Object.keys(errors).length) return { ok: false, errors }
  if (!Object.keys(body).length) {
    return { ok: false, errors: { max_steps: '至少填一项，或点「恢复默认」' } }
  }
  return { ok: true, body }
}

/**
 * budgetForm fills the form from a GET /api/chat/budget snapshot: the effective
 * value in each input, so what is shown is what a turn would use right now.
 */
export function budgetForm(snapshot) {
  const s = snapshot || {}
  const num = (v) => (Number.isFinite(Number(v)) ? String(Number(v)) : '')
  return {
    max_steps: num(s.max_steps),
    turn_max_tokens: num(s.turn_max_tokens),
    turn_deadline_seconds: num(s.turn_deadline_seconds),
  }
}

/** defaultForm is what 恢复默认 would restore, for the placeholders. */
export function defaultForm(snapshot) {
  const s = snapshot || {}
  const num = (v) => (Number.isFinite(Number(v)) ? String(Number(v)) : '')
  return {
    max_steps: num(s.default_max_steps),
    turn_max_tokens: num(s.default_turn_max_tokens),
    turn_deadline_seconds: num(s.default_turn_deadline_seconds),
  }
}

/**
 * formatSeconds renders the wall-clock dimension the way the panel labels it:
 * 10800 is "3 小时", not ninety lines of arithmetic for the reader to do.
 * 0 is "不限".
 */
export function formatSeconds(seconds) {
  const n = Number(seconds)
  if (!Number.isFinite(n) || n <= 0) return '不限'
  if (n % 3600 === 0) return `${n / 3600} 小时`
  if (n % 60 === 0) return `${n / 60} 分钟`
  return `${n} 秒`
}

/** formatBudget is the one-line summary of an effective budget. */
export function formatBudget(snapshot) {
  const s = snapshot || {}
  const steps = Number(s.max_steps) > 0 ? `${Number(s.max_steps)} 步` : '不限步数'
  const tokens = Number(s.turn_max_tokens) > 0 ? `${compact(Number(s.turn_max_tokens))} tokens` : '不限 token'
  return `单轮 ${steps} · ${tokens} · ${formatSeconds(s.turn_deadline_seconds)}`
}

/** sourceLabel says where one dimension's value came from. */
export function sourceLabel(source) {
  return source === 'console' ? '已覆盖' : '来自配置文件'
}

function compact(n) {
  if (n >= 1_000_000) return `${Math.round(n / 100_000) / 10}M`
  if (n >= 1_000) return `${Math.round(n / 100) / 10}K`
  return String(n)
}
