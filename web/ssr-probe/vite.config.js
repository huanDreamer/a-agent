// Build config for the render probe: it exists so the probe can import .vue
// files outside the app bundle. The output goes into ssr-probe/out (gitignored)
// and never touches the embedded dist that the Go server serves.
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  root: '.',
  plugins: [vue()],
  resolve: {
    // The order of these aliases matters: the long one first, so the component
    // path is not swallowed by the module path it lives next to.
    alias: [
      {
        // A component the bubble renders must be renderable in Node, and the
        // probe renders the message bubble through ChatView's own import chain,
        // so this alias has to catch the relative specifier itself.
        find: './MarkdownText.vue',
        replacement: new URL('./markdowntext-stub.js', import.meta.url).pathname,
      },
      {
        // The absolute form, for the same file when imported by path.
        find: /components\/MarkdownText\.vue$/,
        replacement: new URL('./markdowntext-stub.js', import.meta.url).pathname,
      },
      {
        // DOMPurify needs a real DOM, which the probe does not have. Any
        // component the probe renders that also renders markdown (对话's header
        // does, via its message list) would fail at import without this. The
        // alias is scoped to the probe's own build; see
        // ssr-probe/dompurify-stub.js.
        find: 'dompurify',
        replacement: new URL('./dompurify-stub.js', import.meta.url).pathname,
      },
    ],
  },
  // Paths are resolved from the config file's own directory, so the probe runs
  // the same way from web/ or from the repository root.
  build: {
    ssr: new URL('./entry.js', import.meta.url).pathname,
    outDir: new URL('./out', import.meta.url).pathname,
    emptyOutDir: true,
    rollupOptions: { output: { entryFileNames: 'probe.mjs' } },
  },
})
