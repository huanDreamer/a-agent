// Guard against two silent Vue bugs in <script setup> that no type check, build
// or linter can see, because both are invisible until the component renders.
//
//   node scripts/check-component-bindings.mjs
//
// 1. A setup binding that shares a name with a prop wins in the template. So
//    `v-if="open"` where `open` is also a function declared in the script
//    compiles to the *function* — always truthy — and the component renders when
//    it should not and ignores the prop forever. (Not hypothetical: the
//    directory picker shipped with exactly this bug, which is what this half of
//    the check was written for.)
//
// 2. A template that references a name the script never bound. It compiles to
//    `_ctx.<name>`, builds cleanly, and throws only when the branch that uses it
//    renders — `TypeError: _ctx.shortId is not a function`. The dynamic
//    `_ctx[...]` fallback is Vue's intended escape hatch for global properties,
//    but nothing in this app registers one, so every such reference is a typo or
//    a forgotten import. (Not hypothetical either: 产物中心 referenced `shortId`
//    without importing it, so the panel rendered its counts and then threw
//    before the first row — the list came out empty with only a number showing.)
//
// Both halves compile the real SFC with the same binding metadata the build
// uses, so the check sees what the bundler sees rather than guessing at the
// source text.
import fs from 'node:fs'
import path from 'node:path'
import { parse, compileScript, compileTemplate } from 'vue/compiler-sfc'

const dir = 'src/components'
let found = 0

// Vue's own instance properties ($slots, $attrs, $emit, …) resolve through the
// render context and are legitimately absent from the setup bindings. Anything
// else reaching `_ctx.` is unbound.
const isBuiltin = (name) => name.startsWith('$')

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

  // The unbound-identifier rule. Compile the template exactly as the build does
  // — with the script's binding metadata — and look at what the compiler could
  // not resolve to a binding, which it emits as `_ctx.<name>`.
  if (!descriptor.template) continue
  let compiled
  try {
    const compiledScript = compileScript(descriptor, { id: 'bindings-check', inlineTemplate: false })
    compiled = compileTemplate({
      source: descriptor.template.content,
      filename: file,
      id: 'bindings-check',
      compilerOptions: { bindingMetadata: compiledScript.bindings, prefixIdentifiers: true },
    })
  } catch (err) {
    found++
    console.log(`COMPILE ${file}: ${err.message}`)
    continue
  }

  const unresolved = new Set(
    [...compiled.code.matchAll(/_ctx\.([A-Za-z_$][\w$]*)/g)].map((m) => m[1]).filter((n) => !isBuiltin(n)),
  )
  for (const name of [...unresolved].sort()) {
    found++
    console.log(
      `UNBOUND ${file}: the template uses "${name}", which the script never binds ` +
        `(compiles to _ctx.${name} and throws when that branch renders)`,
    )
  }
}

console.log(found === 0 ? 'no prop/setup-binding collisions, no unbound template identifiers' : `${found} problem(s)`)
process.exit(found === 0 ? 0 : 1)
