import { describe, it, expect, vi, beforeEach } from 'vitest'

import {
  askTheApplication,
  paint,
  remember,
  rememberedChoice,
  settledOn,
  followTheMachine,
  whatTheMachineWants,
} from '@/lib/theme'

// What the machine says it wants, controllable.
function machineSays(dark: boolean) {
  const listeners = new Set<() => void>()
  window.matchMedia = ((query: string) => ({
    matches: query.includes('dark') && dark,
    media: query,
    addEventListener: (_: string, fn: () => void) => listeners.add(fn),
    removeEventListener: (_: string, fn: () => void) => listeners.delete(fn),
    dispatchEvent: () => true,
  })) as unknown as typeof window.matchMedia
  return { announce: () => listeners.forEach((fn) => fn()) }
}

describe('choosing light or night', () => {
  beforeEach(() => {
    localStorage.clear()
    document.documentElement.className = ''
    document.documentElement.style.colorScheme = ''
  })

  it('follows the machine until somebody says otherwise', () => {
    machineSays(true)
    expect(rememberedChoice()).toBe('system')
    expect(settledOn('system')).toBe('dark')
    paint('system')
    expect(document.documentElement.classList.contains('dark')).toBe(true)
  })

  it('lets a person overrule the machine in either direction', () => {
    machineSays(true)
    paint('light')
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    machineSays(false)
    paint('dark')
    expect(document.documentElement.classList.contains('dark')).toBe(true)
  })

  // The stored value is the CHOICE, never what it resolved to. Storing "dark"
  // because it happened to be evening would freeze somebody there for good.
  it('remembers the choice, not the result of it', () => {
    machineSays(true)
    remember('system')
    expect(localStorage.getItem('fx_theme')).toBe('system')
    expect(rememberedChoice()).toBe('system')
  })

  it('keeps a deliberate choice across a reload', () => {
    remember('light')
    expect(rememberedChoice()).toBe('light')
  })

  it('tells the rest of the browser too, so scrollbars and form controls follow', () => {
    machineSays(false)
    paint('dark')
    expect(document.documentElement.style.colorScheme).toBe('dark')
    paint('light')
    expect(document.documentElement.style.colorScheme).toBe('light')
  })

  it('changes with the machine while the choice is to follow it', () => {
    const machine = machineSays(false)
    let choice: 'system' | 'light' | 'dark' = 'system'
    const stop = followTheMachine(() => choice)
    paint(choice)
    expect(document.documentElement.classList.contains('dark')).toBe(false)

    // Evening arrives.
    machineSays(true)
    machine.announce()
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    stop()
  })

  it('ignores the machine once somebody has chosen', () => {
    const machine = machineSays(false)
    let choice: 'system' | 'light' | 'dark' = 'light'
    const stop = followTheMachine(() => choice)
    paint(choice)
    machineSays(true)
    machine.announce()
    // Still light: they asked for light.
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    stop()
  })

  it('works where storage is refused, because a webview can refuse it', () => {
    const blown = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const read = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied')
    })
    machineSays(true)
    expect(() => remember('dark')).not.toThrow()
    expect(rememberedChoice()).toBe('system')
    expect(whatTheMachineWants()).toBe('dark')
    blown.mockRestore()
    read.mockRestore()
  })

  // The desktop application paints its own first screen, from its own storage,
  // which is not this page's. It can only know what was chosen if it is told,
  // and this is the moment.
  it('tells the desktop application, so its first screen is the right colour', () => {
    const invoke = vi.fn(() => Promise.resolve())
    ;(window as unknown as { __TAURI__: unknown }).__TAURI__ = { core: { invoke } }
    machineSays(false)
    remember('dark')
    expect(invoke).toHaveBeenCalledWith('remember_appearance', { theme: 'dark' })
    delete (window as unknown as { __TAURI__?: unknown }).__TAURI__
  })

  it('does not need an application to be there', () => {
    delete (window as unknown as { __TAURI__?: unknown }).__TAURI__
    machineSays(false)
    expect(() => remember('light')).not.toThrow()
  })

  // The personal edition's gateway binds to a free port, so the address it
  // serves this page from changes on every launch. A different port is a
  // different origin and a different storage: everything remembered here is gone
  // and the application is the only half that survives it.
  it('takes the application\'s answer over its own empty storage', async () => {
    localStorage.clear()
    machineSays(false)
    const invoke = vi.fn(() => Promise.resolve('dark'))
    ;(window as unknown as { __TAURI__: unknown }).__TAURI__ = { core: { invoke } }

    askTheApplication()
    await new Promise((r) => setTimeout(r, 0))

    expect(invoke).toHaveBeenCalledWith('chosen_appearance')
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    // And kept here too, so this page is right on its own until the port moves.
    expect(localStorage.getItem('fx_theme')).toBe('dark')
    delete (window as unknown as { __TAURI__?: unknown }).__TAURI__
  })

  it('ignores an answer it does not understand', async () => {
    machineSays(false)
    const invoke = vi.fn(() => Promise.resolve('mauve'))
    ;(window as unknown as { __TAURI__: unknown }).__TAURI__ = { core: { invoke } }
    askTheApplication()
    await new Promise((r) => setTimeout(r, 0))
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    delete (window as unknown as { __TAURI__?: unknown }).__TAURI__
  })

  it('does nothing in a browser', () => {
    delete (window as unknown as { __TAURI__?: unknown }).__TAURI__
    expect(() => askTheApplication()).not.toThrow()
  })

  // The desktop application injects this before the document is parsed, which is
  // the only moment early enough to decide what colour to render. It is there
  // for the case where this page remembers nothing through no fault of the
  // person: the personal edition's gateway binds to a free port, so a new launch
  // is a new origin with its own empty store, and having nothing at all to read
  // is what made the chat render light and then turn dark in front of somebody
  // who had chosen night.
  it('falls back to what the application injected when it remembers nothing', () => {
    machineSays(false)
    localStorage.removeItem('fx_theme')
    ;(window as unknown as { __SAG_APPEARANCE__?: string }).__SAG_APPEARANCE__ = 'dark'
    expect(rememberedChoice()).toBe('dark')
    paint(rememberedChoice())
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    delete (window as unknown as { __SAG_APPEARANCE__?: string }).__SAG_APPEARANCE__
  })

  // The other way round, and this is a bug that shipped. What the application
  // injects is decided when the WINDOW is built and never changes after, so
  // somebody who switched to night and then opened the console was handed the
  // value from before they switched, on every navigation, for the rest of the
  // session. Storage is written the moment the choice is made.
  it('prefers what it last remembered over a stale injected value', () => {
    machineSays(false)
    localStorage.setItem('fx_theme', 'dark')
    ;(window as unknown as { __SAG_APPEARANCE__?: string }).__SAG_APPEARANCE__ = 'light'
    expect(rememberedChoice()).toBe('dark')
    paint(rememberedChoice())
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    delete (window as unknown as { __SAG_APPEARANCE__?: string }).__SAG_APPEARANCE__
  })

  it('ignores an injected value it does not understand', () => {
    machineSays(false)
    localStorage.setItem('fx_theme', 'dark')
    ;(window as unknown as { __SAG_APPEARANCE__?: string }).__SAG_APPEARANCE__ = 'chartreuse'
    expect(rememberedChoice()).toBe('dark')
    delete (window as unknown as { __SAG_APPEARANCE__?: string }).__SAG_APPEARANCE__
  })

  // Both records of the same thing, written together. Browser storage is read
  // before anything is drawn; the settings file is what the application reads
  // before it makes a window, and what survives the gateway moving to a new
  // port. Writing one and not the other is how two records drift apart.
  it('writes the choice to the settings file as well as its own storage', async () => {
    machineSays(false)
    const set = vi.fn(() => Promise.resolve())
    const save = vi.fn(() => Promise.resolve())
    vi.doMock('@tauri-apps/plugin-store', () => ({
      load: () => Promise.resolve({ set, save }),
    }))
    const { remember: rememberAgain } = await import('@/lib/theme')

    rememberAgain('dark')
    await new Promise((r) => setTimeout(r, 0))

    expect(localStorage.getItem('fx_theme')).toBe('dark')
    expect(set).toHaveBeenCalledWith('fx_theme', 'dark')
    expect(save).toHaveBeenCalled()
    vi.doUnmock('@tauri-apps/plugin-store')
  })

  it('still works where there is no settings file to write', async () => {
    machineSays(false)
    vi.doMock('@tauri-apps/plugin-store', () => {
      throw new Error('no such plugin')
    })
    const { remember: rememberAgain } = await import('@/lib/theme')
    expect(() => rememberAgain('light')).not.toThrow()
    await new Promise((r) => setTimeout(r, 0))
    expect(localStorage.getItem('fx_theme')).toBe('light')
    vi.doUnmock('@tauri-apps/plugin-store')
  })
})
