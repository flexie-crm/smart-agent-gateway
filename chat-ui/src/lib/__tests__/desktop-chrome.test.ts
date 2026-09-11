import { beforeEach, describe, expect, it, vi } from 'vitest'

import { makeItFeelNative } from '../desktop-chrome'

// The desktop window replaces the browser's context menu with the
// application's own, everywhere. Inside an application "Search with Google" and
// "Speech" are offers it cannot keep, and the previous arrangement kept the
// browser's menu in the two places copying happens, which meant keeping those
// offers with it, and left the rest of the window with no menu at all: nothing
// to copy an answer with, and nowhere to reload a page that had gone wrong
// short of quitting.
//
// So what these assert is the new contract in two halves: the browser's menu is
// always taken, and the shell is always asked for ours, told what is under the
// pointer so it can offer what applies.

/** A selection, or the absence of one. */
function selecting(collapsed: boolean) {
  return { isCollapsed: collapsed } as unknown as Selection
}

/** Right-clicks, and answers whether the browser's menu was allowed through. */
function rightClickOn(target: Element): boolean {
  const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true })
  target.dispatchEvent(event)
  return !event.defaultPrevented
}

/** What the shell was asked for, if anything. */
type Asked = { command: string; args?: Record<string, unknown> }

function withShell(): Asked[] {
  const asked: Asked[] = []
  ;(window as unknown as { __TAURI__: unknown }).__TAURI__ = {
    core: {
      invoke: (command: string, args?: Record<string, unknown>) => {
        asked.push({ command, args })
        return Promise.resolve()
      },
    },
  }
  return asked
}

describe('the desktop window brings its own menu', () => {
  let asked: Asked[]

  beforeEach(() => {
    document.body.innerHTML = `
      <button id="chrome">Save</button>
      <input id="field" />
      <div id="answer"><p id="one">first line</p></div>`
    vi.spyOn(window, 'getSelection').mockReturnValue(selecting(true))
    asked = withShell()
    // Called every time and guarded, so there is exactly ONE listener across the
    // whole suite. jsdom keeps one document, and without the guard each test
    // added another listener and one right-click asked for two menus, which is
    // the fault the guard exists to prevent rather than a quirk of testing.
    makeItFeelNative()
  })

  it("never leaves the browser's menu, wherever the click lands", () => {
    // Including over a text field and over a selection, which is where it used
    // to be kept: the browser's menu there brought "Search with Google" with it.
    for (const id of ['chrome', 'field', 'one']) {
      expect(rightClickOn(document.getElementById(id)!), id).toBe(false)
    }
  })

  it('asks the shell for one instead', () => {
    rightClickOn(document.getElementById('chrome')!)
    expect(asked.map((a) => a.command)).toEqual(['show_context_menu'])
  })

  it('says a field is a field, so paste and select all are offered', () => {
    rightClickOn(document.getElementById('field')!)
    expect(asked[0]?.args?.at).toEqual({ editable: true, selection: false })
  })

  it('says there is a selection, so copy is offered', () => {
    vi.spyOn(window, 'getSelection').mockReturnValue(selecting(false))
    rightClickOn(document.getElementById('one')!)
    expect(asked[0]?.args?.at).toEqual({ editable: false, selection: true })
  })

  it('says neither over chrome, where only reload applies', () => {
    rightClickOn(document.getElementById('chrome')!)
    expect(asked[0]?.args?.at).toEqual({ editable: false, selection: false })
  })

  it('does not fall over when there is no shell to ask', () => {
    // The same code runs in a browser during development. There is nobody to
    // build a menu, and asking must not throw into the click handler.
    delete (window as unknown as { __TAURI__?: unknown }).__TAURI__
    expect(() => rightClickOn(document.getElementById('chrome')!)).not.toThrow()
  })
})
