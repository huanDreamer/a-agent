// The model window and capability rules, checked without a browser.
//
// What can go wrong on 设置 → 模型 is not the rendering but the reading and the
// writing: a window typed as "128K" reaching the server as 0 (which clears it), a
// window typed as "1,000,000" reaching it as 1000000, a source label that says
// "按模型名推断" for a number the provider published. That last one is not
// decoration — an operator debugging "why does it compress so often" reads it —
// and none of it is visible in a screenshot.
//
//     cd web && node scripts/check-models.mjs
import {
  catalogEntry,
  capabilitySourceLabel,
  formatWindow,
  parseWindowInput,
  windowSourceLabel,
  windowSourceShort,
} from '../src/llm.js'

let failures = 0
const check = (name, cond, detail = '') => {
  if (!cond) {
    failures++
    console.error(`FAIL ${name}${detail ? ' — ' + detail : ''}`)
  }
}

// --- typing a window ------------------------------------------------------

check('plain digits', parseWindowInput('128000') === 128000)
check('thousands separators', parseWindowInput('1,000,000') === 1000000)
check('spaces and underscores', parseWindowInput(' 128 000 ') === 128000)
check('a K suffix means thousands', parseWindowInput('128K') === 128000)
check('a lower-case k too', parseWindowInput('128k') === 128000)
check('an M suffix means millions', parseWindowInput('1M') === 1000000)
check('a decimal suffix', parseWindowInput('1.5M') === 1500000)
check('a trailing "tokens" is tolerated', parseWindowInput('131072 tokens') === 131072)
// Clearing is a real action: an empty box or anything unreadable must clear the
// value rather than store a number nobody meant.
check('an empty box clears', parseWindowInput('') === 0)
check('whitespace clears', parseWindowInput('   ') === 0)
check('nonsense clears', parseWindowInput('未知') === 0)
check('a negative clears rather than going negative', parseWindowInput('-5') === 0)
check('zero clears', parseWindowInput('0') === 0)
check('null clears', parseWindowInput(null) === 0 && parseWindowInput(undefined) === 0)
check('a number input value passes through', parseWindowInput(200000) === 200000)

// --- how a window is shown ------------------------------------------------

// A window is read the way people say it, not as a digit count: 100k, 128k, 1M.
check('a million is 1M', formatWindow(1000000) === '1M')
check('a power-of-two million is 1M too', formatWindow(1048576) === '1M')
check('200000 is 200k', formatWindow(200000) === '200k')
check('a decimal 128k is 128k', formatWindow(128000) === '128k')
check('a vendor 128k is 128k too', formatWindow(131072) === '128k')
check('128000 is not 125k', formatWindow(128000) !== '125k')
check('a vendor 256k is 256k', formatWindow(262144) === '256k')
check('32768 is 32k', formatWindow(32768) === '32k')
check('100000 is 100k', formatWindow(100000) === '100k')
check('a billion is 1G', formatWindow(1000000000) === '1G')
// An odd value is shown as approximate rather than silently rounded in storage.
check('an odd value is marked approximate', formatWindow(83558) === '≈84k')
check('an empty window formats as nothing', formatWindow(0) === '' && formatWindow(null) === '')

// The round trip that matters: the table shows 128k for 131072, and saving the
// row without touching the field must keep 131072 rather than store 128000.
check('the display is lossy, so the value must not be re-parsed', parseWindowInput(formatWindow(131072)) === 128000)
check('a hand-typed 128k does mean 128000', parseWindowInput('128k') === 128000)

// --- where a value came from ---------------------------------------------

// The short form is what a table cell shows; the long one is the tooltip. Both
// have to exist for all four sources, and neither may leak the raw server value
// ("api") into the Chinese console.
check('api reads as the interface', windowSourceShort('api') === '接口')
check('asked reads as the model itself', windowSourceShort('asked') === '自报')
check('user reads as hand-written', windowSourceShort('user') === '手填')
check('an unknown source shows nothing', windowSourceShort('something') === '')
check('the long form names the interface', windowSourceLabel('api').includes('接口'))
check('the long form names the model', windowSourceLabel('asked').includes('模型'))
check('the long form names the operator', windowSourceLabel('user').includes('手动'))
check('no raw value leaks into a label', !Object.values(['api', 'asked', 'user']).some((v) => windowSourceShort(v) === v))
check('capabilities say where they came from', capabilitySourceLabel('asked') === '模型自报')
check('capabilities from a listing', capabilitySourceLabel('api') === '来自 provider 接口')
check('capabilities inferred from a name', capabilitySourceLabel('inferred') === '按模型名推断')
check('capabilities set by hand', capabilitySourceLabel('user') === '手动设置')
check('an unknown capability source is blank', capabilitySourceLabel('') === '')

// --- the catalog entry the tables render ----------------------------------

const entry = catalogEntry({
  provider: 'p',
  provider_name: 'P',
  model: 'm',
  display_name: 'M',
  capabilities: ['chat', 'tools'],
  capabilities_source: 'asked',
  context_window: 200000,
  context_window_source: 'api',
  chat_capable: true,
})
check('the window survives the mapping', entry.contextWindow === 200000)
check('its source survives too', entry.contextWindowSource === 'api')
check('capabilities survive', entry.capabilities.join(',') === 'chat,tools')
check('their source survives', entry.capabilitiesSource === 'asked')
// A model nobody has described is not "0 tokens": it is unknown, and the tables
// have to be able to tell the difference.
const undescribed = catalogEntry({ provider: 'p', model: 'm2', capabilities: ['chat'] })
check('no window is 0, not undefined', undescribed.contextWindow === 0)
check('no window has no source', undescribed.contextWindowSource === '')
check('a string window from a sloppy payload still parses', catalogEntry({ provider: 'p', model: 'm3', context_window: '131072' }).contextWindow === 131072)

// --- what the row sends back ----------------------------------------------

// A window the operator did not touch must not be rewritten by a save that was
// about something else (a renamed model, a toggled capability): the row keeps
// whatever it displayed, and the server decides what may overwrite what.
const untouched = catalogEntry({ provider: 'p', model: 'm', context_window: 1000000, context_window_source: 'api' })
check('an untouched window is sent as it stands', parseWindowInput(untouched.contextWindow) === 1000000)
const cleared = catalogEntry({ provider: 'p', model: 'm', context_window: 0 })
check('an empty window is sent as a clear', parseWindowInput(cleared.contextWindow) === 0)

if (failures > 0) {
  console.error(`\n${failures} model-panel check(s) failed`)
  process.exit(1)
}
console.log('model panel checks: ok')
