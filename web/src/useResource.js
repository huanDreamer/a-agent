// One loading primitive for every data view.
//
// A resource re-fetches when the global refresh token changes (the header's
// 刷新 button), when the time range changes, or when one of the extra `watch`
// sources changes (local filters, a selected row, …), keeps the previous data
// visible while reloading, and exposes a tri-state `status` that maps straight
// onto AsyncBlock's loading / error / ready handling.

import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { state } from './state.js'

export function useResource(loader, options = {}) {
  const { watchRange = true, watch: extraSources = [] } = options

  const data = ref(null)
  const error = ref('')
  const loading = ref(false)
  const loaded = ref(false)

  let seq = 0
  let disposed = false
  let timer = null

  async function load({ quiet = false } = {}) {
    const current = ++seq
    if (!quiet) loading.value = true
    try {
      const result = await loader()
      if (disposed || current !== seq) return
      data.value = result
      error.value = ''
      loaded.value = true
    } catch (err) {
      if (disposed || current !== seq) return
      // 401 already flipped the app back to the login screen; nothing to show.
      if (err && err.status === 401) return
      error.value = (err && err.message) || '加载失败'
      loaded.value = true
    } finally {
      if (!disposed && current === seq) loading.value = false
    }
  }

  /** Debounced reload — range switching can fire several events in a row. */
  function schedule() {
    if (timer) window.clearTimeout(timer)
    timer = window.setTimeout(() => {
      timer = null
      load()
    }, 60)
  }

  const sources = watchRange
    ? [() => state.refreshToken, () => state.range, ...extraSources]
    : [() => state.refreshToken, ...extraSources]

  watch(sources, schedule)

  onMounted(() => load())

  onBeforeUnmount(() => {
    disposed = true
    if (timer) window.clearTimeout(timer)
  })

  const status = computed(() => {
    if (loading.value && !loaded.value) return 'loading'
    if (error.value && !data.value) return 'error'
    return 'ready'
  })

  return { data, error, loading, loaded, status, reload: load, refresh: schedule }
}
