// A DOMPurify stand-in for the render probe, and only for the probe.
//
// src/markdown.js is the one module allowed to build HTML, and it sanitizes with
// DOMPurify — which needs a real DOM. The probe runs in Node with no DOM, and
// importing any component that renders markdown (对话's header does, through its
// message list) would otherwise fail at import time:
//
//     TypeError: DOMPurify.removeHook is not a function
//
// The probe asserts on numbers and labels in the rendered HTML, never on
// sanitizing, so what this has to provide is the API surface markdown.js calls —
// not the behaviour. It is wired in by ssr-probe/vite.config.js and is NOT part
// of the app bundle: the browser build still uses the real library, which is the
// only thing that decides what reaches v-html.
export default {
  // Pass-through: the probe's assertions are about text the app renders, not
  // about what the sanitizer would have stripped.
  sanitize: (html) => String(html ?? ''),
  addHook: () => {},
  removeHook: () => {},
  setConfig: () => {},
  clearConfig: () => {},
  isSupported: false,
  version: 'probe-stub',
}
