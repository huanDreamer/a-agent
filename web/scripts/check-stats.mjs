// The conversation header's statistics line, checked without a browser.
//
// 设置 → 对话预算 put the budget under test; this is the same idea for the header
// the user reads every turn. What can go wrong is not the rendering but the
// reading of the payload: a missing field rendered as `NaN 次`, a zero rendered
// as if it were a measurement, a duration shown as if it were the wall-clock
// length of the conversation, or a segment dropped silently.
//
//     cd web && node scripts/check-stats.mjs
import { normalizeStats, statsSegments, statsTitle } from '../src/sessionStats.js'

let failures = 0
const check = (name, cond, detail = '') => {
  if (!cond) {
    failures++
    console.error(`FAIL ${name}${detail ? ' — ' + detail : ''}`)
  }
}

const full = {
  messages: 120,
  turns: 7,
  llm_calls: 23,
  llm_duration_ms: 184000,
  tool_calls: 41,
  tool_duration_ms: 5200,
  prompt_tokens: 812345,
  completion_tokens: 41230,
  total_tokens: 853575,
}

// --- reading the payload ------------------------------------------------

const s = normalizeStats(full)
check('every field is read', s.turns === 7 && s.llmCalls === 23 && s.toolCalls === 41 && s.totalTokens === 853575)
check('the token split is kept', s.promptTokens === 812345 && s.completionTokens === 41230)

const junk = normalizeStats({ turns: 'lots', llm_calls: -3, total_tokens: null })
check('a non-numeric field becomes 0, not NaN', junk.turns === 0 && junk.llmCalls === 0 && junk.totalTokens === 0)
check('an absent payload is all zeros', normalizeStats(undefined).turns === 0 && normalizeStats(null).llmCalls === 0)

// --- the segments -------------------------------------------------------

// The header passes the session row's own count in, because that is the number
// the sidebar lists and the two must never disagree.
const segments = statsSegments(full, { messageCount: 120 })
check('the line has five segments for a used conversation', segments.length === 5, String(segments.length))
check('turns come first', segments[0].text === '轮 7', segments[0].text)
check(
  'the message count sits beside the turn count',
  segments[1].text === '共 120 条消息',
  segments[1].text,
)
check(
  'the model segment carries calls and duration',
  segments[2].text === '模型 23 次 · 3m04s',
  segments[2].text,
)
check(
  'the tool segment carries calls and duration',
  segments[3].text === '工具 41 次 · 5.2s',
  segments[3].text,
)
// formatCompact keeps three significant digits below 100 and rounds above it,
// so 853575 renders as 854K rather than 853.6K.
check('the token segment is the compact total', segments[4].text === '854K tokens', segments[4].text)
check('every segment explains itself', segments.every((seg) => typeof seg.title === 'string' && seg.title.length > 8))

// A conversation that has not run anything yet shows the two numbers that mean
// something at zero, and no rows of zeros behind them.
const empty = statsSegments({}, { messageCount: 0 })
check(
  'an empty conversation shows the turn and message counts',
  empty.length === 2 && empty[0].text === '轮 0' && empty[1].text === '共 0 条消息',
  empty.map((seg) => seg.text).join(' · '),
)
check('an empty conversation has no token segment', !empty.some((seg) => seg.key === 'tokens'))

// A caller with no session to count (the pure module on its own) does not invent
// a message count — the segment belongs to the header, which knows it.
const noCount = statsSegments(full)
check('no count passed in means no message segment', !noCount.some((seg) => seg.key === 'messages'))

// A conversation whose provider reports no usage: the token segment must be
// absent rather than claiming 0 tokens were spent.
const noUsage = statsSegments({ turns: 2, llm_calls: 3, tool_calls: 0 }, { messageCount: 7 })
check(
  'no reported usage means no token segment',
  noUsage.length === 3 && !noUsage.some((seg) => seg.key === 'tokens'),
)
check('no tool calls means no tool segment', !noUsage.some((seg) => seg.key === 'tools'))

// --- the tooltip --------------------------------------------------------

const title = statsTitle(full, { messageCount: 120 })
check('the tooltip carries every segment', title.split('\n').length === segments.length + 1)
check('the tooltip spells out the token split', title.includes('812,345') && title.includes('41,230'))
check(
  'the tooltip says the durations are not wall-clock',
  title.includes('不是这段对话的墙钟长度'),
)

if (failures) {
  console.error(`\n${failures} stats check(s) failed`)
  process.exit(1)
}
console.log('stats checks passed')
