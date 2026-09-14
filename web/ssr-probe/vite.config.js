// Build config for the render probe: it exists so the probe can import .vue
// files outside the app bundle. The output goes into ssr-probe/out (gitignored)
// and never touches the embedded dist that the Go server serves.
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  root: '.',
  plugins: [vue()],
  // Paths are resolved from the config file's own directory, so the probe runs
  // the same way from web/ or from the repository root.
  build: {
    ssr: new URL('./entry.js', import.meta.url).pathname,
    outDir: new URL('./out', import.meta.url).pathname,
    emptyOutDir: true,
    rollupOptions: { output: { entryFileNames: 'probe.mjs' } },
  },
})
