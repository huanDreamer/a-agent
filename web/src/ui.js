// Shell UI state that is not tied to any one view.
//
// The only thing in here today is the mobile drawer: below 900px the sidebar
// leaves the layout and becomes an overlay, opened by the drawer button that the
// main area's context header renders. Views therefore never have to pass that
// state around — they import it.

import { reactive } from 'vue'

export const ui = reactive({
  /** True while the off-canvas sidebar is open (only meaningful at ≤900px). */
  drawerOpen: false,
})

/** Open/close the sidebar drawer. */
export function setDrawer(open) {
  ui.drawerOpen = Boolean(open)
}

/**
 * Close the drawer after a navigation action. Kept separate from setDrawer so
 * the call sites read as "we are leaving this screen".
 */
export function closeDrawer() {
  ui.drawerOpen = false
}
