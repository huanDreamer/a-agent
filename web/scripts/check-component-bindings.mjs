// Guard against a silent Vue bug: in <script setup>, a setup binding that shares a
// name with a prop wins in the template. So `v-if="open"` where `open` is also a
// function declared in the script compiles to the *function* — always truthy —
// and the component renders when it should not and ignores the prop forever.
//
// It is invisible to the type system, to the build and to any test that does not
// render the component, so it is checked here instead:
//
//     node scripts/check-component-bindings.mjs
//
// (This is not hypothetical: the directory picker shipped with exactly this bug,
// which is what the check was written for.)
// Finds setup bindings whose name collides with a prop name: in <script setup>
// the binding wins in the template, so `v-if="open"` silently becomes a function
// reference (always truthy) instead of reading the prop.
import fs from 'node:fs'
import path from 'node:path'
import { parse, compileScript } from 'vue/compiler-sfc'

const dir = 'src/components'
let found = 0
for (const f of fs.readdirSync(dir).filter((f) => f.endsWith('.vue'))) {
  const file = path.join(dir, f)
  const src = fs.readFileSync(file, 'utf8')
  const { descriptor } = parse(src, { filename: file })
  if (!descriptor.scriptSetup) continue
  const script = descriptor.scriptSetup.content

  // prop names, from defineProps
  const props = new Set()
  const block = script.match(/defineProps\(\s*\{([\s\S]*?)\n\}\)/)
  if (block) {
    for (const m of block[1].matchAll(/^\s*([A-Za-z_$][\w$]*)\s*:/gm)) props.add(m[1])
    for (const m of block[1].matchAll(/^\s*'([^']+)'\s*:/gm)) props.add(m[1])
  }
  const arr = script.match(/defineProps\(\[([^\]]*)\]\)/)
  if (arr) for (const m of arr[1].matchAll(/['"]([^'"]+)['"]/g)) props.add(m[1])
  if (!props.size) continue

  const bindings = new Set()
  for (const m of script.matchAll(/^\s*(?:const|let|var)\s+([A-Za-z_$][\w$]*)/gm)) bindings.add(m[1])
  for (const m of script.matchAll(/^\s*(?:async\s+)?function\s+([A-Za-z_$][\w$]*)/gm)) bindings.add(m[1])

  for (const name of bindings) {
    if (props.has(name)) {
      found++
      console.log(`COLLISION ${file}: prop "${name}" is shadowed by a setup binding`)
    }
  }

  // The other half of the same trap: a template that calls a prop as if it were
  // a function. It happens when a local function is renamed and a handler is
  // missed — `@click="open(...)"` starts resolving to the Boolean prop, which
  // builds fine and throws the moment someone clicks it.
  const template = descriptor.template?.content || ''
  for (const name of props) {
    if (bindings.has(name)) continue
    const called = new RegExp(`["'\\s(]${name}\\s*\\(`).test(template)
    if (called) {
      found++
      console.log(`PROP-CALL ${file}: the template calls prop "${name}" as a function`)
    }
  }
}
console.log(found === 0 ? 'no prop/setup-binding collisions' : `${found} collision(s)`)
