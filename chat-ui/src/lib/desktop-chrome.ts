/**
 * Makes a webview behave like an application window.
 *
 * Two things give it away as a web page. Right-clicking anywhere offers the
 * BROWSER's menu, which on macOS means "Search with Google", "Translate",
 * "Speech" and "Services" over the inside of an application that does none of
 * those. And chrome that is not content, a button's label, a heading, drags into
 * a blue selection the moment the pointer moves.
 *
 * So the browser's menu is replaced everywhere by the application's own, built
 * by the shell (`desktop/shared/src/menu.rs`) from what the page says is under
 * the pointer: Copy over a selection, Cut, Paste and Select all in a field, and
 * Reload always. Selection is taken away from labels only. Content stays
 * selectable, because people copy answers, ids and keys, and an application that
 * will not let them is worse than one that looks like a web page.
 *
 * It used to SUPPRESS the browser's menu and put nothing in its place, keeping
 * it only inside a text field and over a selection. That left most of the window
 * with no menu at all, and nowhere to reload a page that had gone wrong short of
 * quitting; and where it did keep the browser's, it kept "Search with Google"
 * with it. One menu, ours, everywhere is both simpler and the thing an
 * application does.
 *
 * Desktop builds only. In a browser this would be hijacking a menu that belongs
 * to the browser.
 */
export function makeItFeelNative(): void {
  // Once. It attaches a listener to the document, and a second call would leave
  // two, so one right-click would ask the shell for two menus. It is called once
  // at startup today, which is why nothing had gone wrong yet: a guard is what
  // keeps that from being a fact about the caller rather than about this.
  if (document.documentElement.getAttribute('data-desktop') === 'true') return
  document.documentElement.setAttribute('data-desktop', 'true')

  document.addEventListener('contextmenu', (event) => {
    const target = event.target as HTMLElement | null
    event.preventDefault()
    ourMenu(target)
  })
}

/**
 * Ask the shell for the application's own menu.
 *
 * What is offered is decided THERE, from what is said here, because only the
 * page knows whether the click landed in a field and whether anything is
 * selected. The items are then the platform's own: a real Copy that acts on the
 * webview's selection with the platform label and accelerator, rather than one
 * reimplemented against a selection this code would have to read and write back
 * itself.
 *
 * No shell means no menu and nothing done, which is right: without one there is
 * nobody to build it, and the click has already been taken from the browser.
 */
function ourMenu(target: HTMLElement | null): void {
  const invoke = (window as unknown as { __TAURI__?: { core?: { invoke?: Invoke } } }).__TAURI__
    ?.core?.invoke
  if (!invoke) return
  const selection = window.getSelection()
  void invoke('show_context_menu', {
    at: {
      editable: Boolean(target?.closest('input, textarea, [contenteditable="true"]')),
      selection: Boolean(selection && !selection.isCollapsed),
    },
  })
}

type Invoke = (command: string, args?: Record<string, unknown>) => Promise<unknown>
