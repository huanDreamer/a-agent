// The 产物 rules, checked without a browser.
//
// What can go wrong with an artifact list is not the rendering but the reading:
// a count that belongs to the previous conversation, a title that is empty where
// the model gave none, a kind rendered as a raw value, an orphan row (from a
// one-shot run) shown as a blank cell. None of that is visible in a rendered
// page, so the rules are functions and are asserted here:
//
//     cd web && node scripts/check-artifacts.mjs
//
// (Run from web/, like check-stats.mjs.)
import { artifactLabel, artifactChipLabel, artifactHintText, extensionOf, kindIcon, kindLabel } from '../src/artifactsChip.js'
import { filterArtifacts, orphanCount, sessionLabel, totalBytes } from '../src/artifactsView.js'

let failures = 0
const check = (name, cond, detail = '') => {
  if (!cond) {
    failures++
    console.error(`FAIL ${name}${detail ? ' — ' + detail : ''}`)
  }
}

// --- one artifact's label ------------------------------------------------

check('a title is the label', artifactLabel({ title: '巡检报告', path: 'a/b.html' }) === '巡检报告')
check('a blank title falls back to the file name', artifactLabel({ title: '   ', path: 'pages/巡检报告.html' }) === '巡检报告.html')
check('no title at all falls back to the file name', artifactLabel({ path: 'a/b/c.png' }) === 'c.png')
check('a path with no slash is its own name', artifactLabel({ path: 'index.html' }) === 'index.html')
check('a missing artifact labels as empty', artifactLabel(null) === '' && artifactLabel(undefined) === '')

// The kinds are a closed set with a label and an icon each: a raw value leaking
// into the tag is how a Chinese console starts showing "document" in English.
check('html is 网页 with the monitor glyph', kindLabel('html') === '网页' && kindIcon('html') === 'monitor')
check('document is 文档 with the file glyph', kindLabel('document') === '文档' && kindIcon('document') === 'file-text')
check('image is 图片 with the picture glyph', kindLabel('image') === '图片' && kindIcon('image') === 'image')
check('an unknown kind is 其他, not the raw value', kindLabel('video') === '其他' && kindLabel('') === '其他')
check('an unknown kind still gets an icon', kindIcon('video') === 'package')

// --- the folder button ---------------------------------------------------

check('the button counts', artifactChipLabel(3, true) === '3 个产物')
check('a disabled deployment says so rather than counting', artifactChipLabel(3, false) === '产物未启用')
check('a live deployment with none says zero', artifactChipLabel(0, true) === '0 个产物')

// --- a name's extension --------------------------------------------------

check('the extension is taken from the last dot', extensionOf('a/b/report.html') === '.html')
check('a dot in a folder name is not the extension', extensionOf('v1.2/report') === '')
check('a trailing dot is not an extension', extensionOf('a/b.') === '')
check('no extension is empty', extensionOf('a/b') === '' && extensionOf('') === '')

// --- the listing ---------------------------------------------------------

const rows = [
  { id: 'a1', kind: 'html', session_id: 's1', session_title: '巡检', bytes: 2048, path: 's1/a.html' },
  { id: 'a2', kind: 'image', session_id: 's2', session_title: '', bytes: 1024, path: 's2/b.png' },
  { id: 'a3', kind: 'document', session_id: '', bytes: 512, path: 'orphan/c.md' },
]

check('no filter keeps every row', filterArtifacts(rows, {}).length === 3)
check('a kind filter keeps only that kind', filterArtifacts(rows, { kind: 'image' }).map((r) => r.id).join() === 'a2')
check('a session filter matches the title', filterArtifacts(rows, { session: '巡检' }).map((r) => r.id).join() === 'a1')
check('a session filter matches the id', filterArtifacts(rows, { session: 'S2' }).map((r) => r.id).join() === 'a2', 'case must not matter')
check('an unknown filter matches nothing', filterArtifacts(rows, { session: 'nope' }).length === 0)
check('whitespace is not a filter', filterArtifacts(rows, { session: '   ' }).length === 3)
check('the two filters compose', filterArtifacts(rows, { kind: 'html', session: '巡检' }).length === 1)
check('a non-list is an empty list', filterArtifacts(undefined, {}).length === 0 && filterArtifacts(null, {}).length === 0)
check('a null row is dropped, not rendered', filterArtifacts([null, rows[0]], {}).length === 1)

// --- the session cell ----------------------------------------------------

check('a resolved title is the cell', sessionLabel({ session_id: 's1', session_title: '巡检' }) === '巡检')
check('an unresolved title falls back to the id', sessionLabel({ session_id: '0123456789abcdef' }).startsWith('01234567'))
check(
  'an artifact with no session says so rather than rendering blank',
  sessionLabel({ session_id: '', session_title: '' }) === '—' && sessionLabel(null) === '—',
)

// --- the header facts ----------------------------------------------------

check('the size total adds the rows on screen', totalBytes(rows) === 3584)
check('an empty list totals zero', totalBytes([]) === 0)
check('a missing size counts as zero', totalBytes([{ bytes: 10 }, {}, { bytes: 5 }]) === 15)
check('orphans are counted, not hidden', orphanCount(rows) === 1)
check('a list with no orphans says zero', orphanCount([rows[0]]) === 0)
check('the hint names both surfaces', artifactHintText().includes('产物中心'))

console.log(failures === 0 ? 'ALL ARTIFACT CHECKS PASSED' : `${failures} failure(s)`)
process.exit(failures === 0 ? 0 : 1)
