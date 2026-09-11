import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// The Go server embeds `internal/server/webui/dist` (//go:embed dist) and may
// serve it under an arbitrary path prefix, so every emitted URL must be
// relative: `base: './'` + `assetsDir: 'assets'` keeps us at `./assets/...`.
export default defineConfig({
  plugins: [vue()],
  base: './',
  build: {
    outDir: '../internal/server/webui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    // Single-page admin tool: one chunk keeps the embed small and simple.
    chunkSizeWarningLimit: 1024,
  },
  server: {
    // Dev only: the Go server owns /api. Not used by the embedded bundle.
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
})
