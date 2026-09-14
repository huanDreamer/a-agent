<script setup>
// The directory picker: choose an existing directory on the machine the server
// runs on, in the column view macOS uses.
//
// It exists because a browser cannot hand the server a path: `<input
// type="file">` yields file contents and an opaque handle, never "/Users/me/code".
// So the console browses the server's filesystem through `GET /api/fs/dirs`,
// which lists directories only and never writes anything.
//
// The interaction is Finder's column view, because it is the one people already
// know for "walk down to a folder and see what is inside":
//
//   * every column lists the subdirectories of one directory;
//   * clicking a folder highlights it and shows its contents in the column to its
//     right — so the folder you are about to choose is always the one whose
//     contents you can see;
//   * clicking a folder that is already highlighted (one that has a column of its
//     own to the right) collapses the columns beyond it;
//   * the chosen directory is the deepest one, i.e. the one whose contents fill
//     the rightmost column.
//
// The whole chain is loaded at once — a column without its listing would render
// as an empty column, which in a Miller column layout reads as "this folder has
// nothing in it".
import { computed, nextTick, ref, watch } from 'vue'
import Icon from './Icon.vue'
import { api } from '../api.js'

const props = defineProps({
  /** Whether the dialog is open. */
  open: { type: Boolean, default: false },
  /** Starting directory; empty asks the server for the home directory. */
  start: { type: String, default: '' },
  /**
   * A failure from the caller's last attempt (a duplicate label, a directory that
   * vanished between browsing and confirming).
   *
   * It is rendered here, inside the dialog, rather than in the app's banner: the
   * overlay would hide that banner, and the fix belongs next to the field that
   * caused it.
   */
  error: { type: String, default: '' },
  /** True while the caller is creating the workspace. */
  busy: { type: Boolean, default: false },
})
const emit = defineEmits(['close', 'pick'])

/**
 * maxColumns bounds the chain.
 *
 * Opening at ~/code/my-project would otherwise render one column per path
 * segment, and a deep home directory would fill the dialog with ancestors nobody
 * needs to see. Six keeps the ones that identify where you are.
 */
const maxColumns = 6

/**
 * A column: one directory and the subdirectories it holds.
 *
 * The listing is part of the column rather than a separate map so a column can
 * never be rendered without knowing whether it is empty, still loading, or
 * unreadable — all three have to look different.
 *
 * `key` exists for the in-flight check below. Identity comparison across Vue's
 * reactive boundary is a trap: `columns.value` hands out proxies, so a column
 * object that was created here and one read back from the array are different
 * values even though they are the same column. A primitive key compares
 * correctly from either side.
 */
let columnSeq = 0
function newColumn(path) {
  return { key: ++columnSeq, path, entries: [], loading: true, error: '', truncated: false }
}

/** The column as the reactive array holds it, so writes to it are tracked. */
function trackColumn(column) {
  return columns.value.find((c) => c.key === column.key)
}

/** The chain of directories, leftmost ancestor first. */
const columns = ref([])
/** The home directory, for the 主目录 button. */
const home = ref(props.start || '')
/** Name typed for the workspace; prefilled from the selected directory's name. */
const name = ref('')
/** A path to jump to, for the 路径 field. */
const pathDraft = ref('')
const pathInput = ref(null)
const colsEl = ref(null)

/** The selected directory: the deepest one, whose contents fill the last column. */
const selected = computed(() => (columns.value.length ? columns.value[columns.value.length - 1].path : ''))
/** True while any column is still loading. */
const loading = computed(() => columns.value.some((c) => c.loading))
/** The filesystem root cannot be chosen as a workspace (see internal/workspaces). */
const atRoot = computed(() => selected.value === '/')
const selectedName = computed(() => baseName(selected.value) || '/')
/** Why a browse failed before any column existed (a start directory that cannot
 *  be read at all, as opposed to one column among several failing). */
const browseError = ref('')
/** True when nothing is loaded and the first attempt failed outright. */
const emptyError = computed(() => (columns.value.length === 0 ? browseError.value : ''))
/** The column holding the selection, whose contents fill the rightmost column. */
const selectedColumn = computed(() => (columns.value.length ? columns.value[columns.value.length - 1] : null))
/** Whether the selection is usable: loaded, readable, and not the filesystem root. */
const canConfirm = computed(
  () => Boolean(selected.value) && !atRoot.value && !selectedColumn.value?.loading && !selectedColumn.value?.error,
)

/** baseName is the last path segment, used to prefill the workspace name. */
function baseName(p) {
  if (!p) return ''
  const parts = p.split('/').filter(Boolean)
  return parts.length ? parts[parts.length - 1] : ''
}

/** The chain of ancestors plus the directory itself, capped to the last N. */
function chainFor(path) {
  const parts = path.split('/').filter(Boolean)
  const out = ['/']
  let acc = ''
  for (const part of parts) {
    acc += '/' + part
    out.push(acc)
  }
  return out.slice(-maxColumns)
}

/** Load one column's listing in place. */
async function loadColumn(column) {
  column.loading = true
  column.error = ''
  try {
    const res = await api.browseDirs(column.path)
    // The chain may have moved on while this was in flight (the user folded the
    // columns, or picked another folder); a column that is no longer in the chain
    // must not write into it.
    if (!trackColumn(column)) return
    if (res && res.ok === false) {
      column.error = res.error || '无法读取该目录'
      column.entries = []
      return
    }
    column.entries = res.entries || []
    column.truncated = Boolean(res.truncated)
    // The server's answer is authoritative (symlinks resolved): keep the column's
    // path in the form it reports so the highlight and the selection agree.
    if (res.path) column.path = res.path
    if (res.home) home.value = res.home
  } catch (err) {
    column.error = (err && err.message) || '无法读取该目录'
    column.entries = []
  } finally {
    column.loading = false
  }
}

/** Replace the whole chain and load every column. */
async function setChain(paths) {
  browseError.value = ''
  columns.value = paths.map(newColumn)
  await Promise.all(columns.value.map(loadColumn))
  await scrollToEnd()
  // Mirror the selection into the path field — unless someone is typing in it.
  if (document.activeElement !== pathInput.value) {
    pathDraft.value = selected.value
  }
  if (!nameTouched.value) name.value = selectedName.value
}

/** Browse a directory, showing the chain that leads to it. */
async function browse(path) {
  const target = (path || props.start || '').trim()
  browseError.value = ''
  if (!target) {
    // Ask the server where home is; it answers with the listing we need anyway.
    try {
      const res = await api.browseDirs('')
      if (res && res.ok === false) {
        browseError.value = res.error || '无法读取起始目录'
        return
      }
      home.value = res.home || home.value
      await setChain(chainFor(res.path))
      return
    } catch (err) {
      browseError.value = (err && err.message) || '无法读取起始目录'
      return
    }
  }
  await setChain(chainFor(target))
}

/** Select a folder in column `i`: the chain becomes everything up to it plus it. */
async function select(i, entry) {
  // Clicking a folder that is already in the chain (it has a column of its own to
  // the right) keeps it selected and drops everything after it — Finder's
  // behaviour, and the reason clicking a folder you are already in does not move
  // you back to its parent. The slice therefore includes that column (i + 2),
  // while the branch below replaces it (i + 1).
  if (entry.path === columns.value[i + 1]?.path) {
    columns.value = columns.value.slice(0, i + 2)
    await mirrorSelection()
    await scrollToEnd()
    return
  }
  columns.value = [...columns.value.slice(0, i + 1), newColumn(entry.path)]
  // Load the column *as the reactive array holds it*: mutating the object that
  // was pushed (rather than the proxy the array hands back) would update the
  // data without telling Vue, and the column would stay empty on screen.
  await loadColumn(columns.value[columns.value.length - 1])
  await scrollToEnd()
  await mirrorSelection()
}

/** Keep the path field and the prefill in step with the selection. */
async function mirrorSelection() {
  if (document.activeElement !== pathInput.value) pathDraft.value = selected.value
  if (!nameTouched.value) name.value = selectedName.value
}

/** Whether the name field still holds a derived value the user has not edited. */
const nameTouched = ref(false)

/** Scroll the column strip to its end, so the new column is visible. */
async function scrollToEnd() {
  await nextTick()
  const el = colsEl.value
  if (el) el.scrollLeft = el.scrollWidth
}

/** 上一级: drop the deepest selection, or step above the leftmost column. */
async function goUp() {
  if (columns.value.length > 1) {
    columns.value = columns.value.slice(0, -1)
    await mirrorSelection()
    await scrollToEnd()
    return
  }
  const current = columns.value[0]?.path
  if (!current || current === '/') return
  const parent = current.split('/').slice(0, -1).join('/') || '/'
  await setChain(chainFor(parent))
}

/** 主目录: the whole chain down to home, so the columns show where it sits. */
async function goHome() {
  if (!home.value) return
  await setChain(chainFor(home.value))
}

/** Jump to the path typed in the field. */
async function goToTypedPath() {
  const typed = pathDraft.value.trim()
  if (!typed || typed === selected.value) return
  await setChain(chainFor(typed))
}

/** Confirm: the deepest directory in the chain is the one being chosen. */
function confirm() {
  if (!canConfirm.value) return
  emit('pick', { root: selected.value, name: name.value.trim() })
}

/** Whether the folder in column `i` is the selected one. */
function isSelected(i, entry) {
  return columns.value[i + 1]?.path === entry.path
}

/**
 * Whether the dialog is showing.
 *
 * It is an alias rather than `v-if="open"` on purpose: a setup binding named
 * `open` takes precedence over the prop in the template, and the probe scripts
 * guard against that, but reading the prop through a computed removes the trap
 * rather than relying on the guard.
 */
const isOpen = computed(() => props.open)

watch(
  isOpen,
  (showing) => {
    if (!showing) return
    // A fresh chain every time: the dialog may have been left anywhere.
    columns.value = []
    name.value = ''
    nameTouched.value = false
    pathDraft.value = ''
    browseError.value = ''
    browse(props.start || '')
  },
  { immediate: true },
)
</script>

<template>
  <Teleport to="body">
    <div v-if="isOpen" class="modal-backdrop" @click.self="emit('close')">
      <div class="modal dir-picker" role="dialog" aria-label="选择目录" aria-modal="true">
        <div class="modal-head">
          <span class="modal-title">选择工作区目录</span>
          <button type="button" class="btn ghost sm icon-btn" aria-label="关闭" @click="emit('close')">
            <Icon name="x" :size="15" />
          </button>
        </div>

        <div class="picker-toolbar">
          <button
            type="button"
            class="btn ghost sm"
            :disabled="loading || (columns.length <= 1 && selected === '/')"
            title="上一级"
            @click="goUp"
          >
            <Icon name="arrow-left" :size="14" />
            上一级
          </button>
          <button
            type="button"
            class="btn ghost sm"
            :disabled="!home || loading"
            title="回到主目录"
            @click="goHome"
          >
            <Icon name="folder" :size="14" />
            主目录
          </button>
          <input
            ref="pathInput"
            v-model="pathDraft"
            class="input mono picker-path"
            type="text"
            aria-label="直接输入目录路径"
            placeholder="/绝对/路径"
            :disabled="loading"
            @keyup.enter="goToTypedPath"
          />
          <button
            type="button"
            class="btn ghost sm"
            :disabled="loading || !pathDraft.trim()"
            title="跳转到输入的路径"
            @click="goToTypedPath"
          >
            前往
          </button>
        </div>

        <div v-if="error" class="banner error" role="alert">
          <span aria-hidden="true">⚠</span>
          <span class="banner-text">{{ error }}</span>
        </div>
        <div v-else-if="emptyError" class="banner error" role="alert">
          <span aria-hidden="true">⚠</span>
          <span class="banner-text">{{ emptyError }}</span>
        </div>

        <!-- The columns: click a folder, its contents appear to the right. -->
        <div ref="colsEl" class="cols" role="listbox" aria-label="目录">
          <div v-for="(col, i) in columns" :key="col.path" class="col" :data-path="col.path">
            <div v-if="col.loading" class="col-note">读取中…</div>
            <div v-else-if="col.error" class="col-note error">{{ col.error }}</div>
            <div v-else-if="!col.entries.length" class="col-note">没有子目录</div>
            <template v-else>
              <button
                v-for="entry in col.entries"
                :key="entry.path"
                type="button"
                class="col-row"
                role="option"
                :aria-selected="isSelected(i, entry)"
                :class="{ active: isSelected(i, entry) }"
                :title="entry.path"
                @click="select(i, entry)"
              >
                <Icon name="folder" :size="15" />
                <span class="col-name">{{ entry.name }}</span>
                <span v-if="entry.link" class="chip">链接</span>
              </button>
              <p v-if="col.truncated" class="col-note">目录过多，只显示了一部分。</p>
            </template>
          </div>
          <div v-if="!columns.length && !emptyError" class="col-note col-note-start">读取中…</div>
        </div>

        <div class="picker-foot">
          <label class="field">
            <span class="field-label">工作区名字</span>
            <input
              v-model="name"
              class="input"
              placeholder="默认用目录名"
              :disabled="busy"
              @input="nameTouched = true"
              @keyup.enter="confirm"
            />
          </label>
          <p class="muted-note" :data-selected="selected">
            选中：<code class="md-code">{{ selected || '（未选择）' }}</code>
            <span v-if="atRoot" class="dimmer"> · 不能选文件系统根目录</span>
            <span v-else-if="selectedColumn && selectedColumn.error" class="dimmer"> · 该目录无法读取</span>
          </p>
          <div class="row row-gap">
            <button type="button" class="btn primary sm" :disabled="!canConfirm || busy" @click="confirm">
              <Icon name="check" :size="15" />
              {{ busy ? '创建中…' : '使用这个目录' }}
            </button>
            <button type="button" class="btn ghost sm" :disabled="busy" @click="emit('close')">取消</button>
            <span class="muted-note">点击文件夹查看它的内容，最右侧一栏即当前选中的目录。</span>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>
