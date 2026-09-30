import { StrictMode } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@/test-utils'
import { ThemeSwitch } from '@/components/ThemeSwitch'

// Appearance and leaving sit in one frame in the corner of the sidebar. They
// were two controls of the same size, one framed and one not, which reads as
// one thing somebody had not finished aligning.
//
// Leaving is NOT a fourth position of the appearance setting, and the markup has
// to say so: the three are a radiogroup because only one of them can hold, and
// signing out is a button beside them.
// What the machine says it wants, controllable. The same shape as the one in
// lib/__tests__/theme.test.ts, because it fakes the same browser API.
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

describe('the appearance switch', () => {
  beforeEach(() => {
    localStorage.clear()
    document.documentElement.className = ''
    document.documentElement.style.colorScheme = ''
  })

  it('offers the three positions and no way out, when there is nothing to leave', () => {
    // The personal edition. Nobody signed in, so nobody to sign out.
    render(
      <StrictMode>
        <ThemeSwitch />
      </StrictMode>,
    )
    expect(screen.getAllByRole('radio')).toHaveLength(3)
    expect(screen.queryByRole('button', { name: /sign out/i })).not.toBeInTheDocument()
    expect(screen.getByRole('radiogroup')).toBeInTheDocument()
  })

  it('adds leaving to the same frame when there is a session', () => {
    const leave = vi.fn()
    render(
      <StrictMode>
        <ThemeSwitch onSignOut={leave} />
      </StrictMode>,
    )
    expect(screen.getAllByRole('radio')).toHaveLength(3)

    const out = screen.getByRole('button', { name: /sign out/i })
    fireEvent.click(out)
    expect(leave).toHaveBeenCalledTimes(1)
  })

  // A long name pushed the switch out of the sidebar entirely: the moon and the
  // way out were drawn past its edge, and the name did not truncate although it
  // carries `truncate`.
  //
  // Neither is a fault of the name. A flex item defaults to `min-width: auto`,
  // so the row holding the name and the switch refused to shrink below the two
  // of them together, and `truncate` on a descendant can never fire while
  // nothing above it is constrained. The row must be allowed to give way, and
  // the switch must not be what gives: the name is what shortens.
  it('keeps its width whatever the name beside it is', () => {
    render(
      <StrictMode>
        <ThemeSwitch onSignOut={() => {}} />
      </StrictMode>,
    )
    expect(screen.getByRole('group').className).toContain('shrink-0')
  })

  // What the switch is WIRED to, which none of the tests above could see.
  //
  // The component listens for the machine changing its mind, and that listener
  // has to read the choice as it is NOW. Held wrong it reads the choice the
  // component was mounted with, and the screen then goes dark in the evening
  // for somebody who explicitly asked for light. The old code kept the current
  // choice in a ref it wrote during render, which is a side effect in the
  // middle of rendering; this asserts the behaviour that arrangement existed
  // for, so replacing it cannot quietly lose it.
  it('stops following the machine once a position is chosen', () => {
    const machine = machineSays(false)
    render(
      <StrictMode>
        <ThemeSwitch />
      </StrictMode>,
    )
    // Following the computer, which is light at the moment.
    expect(document.documentElement.classList.contains('dark')).toBe(false)

    // Evening arrives, and the computer goes dark. So does the page.
    machineSays(true)
    machine.announce()
    expect(document.documentElement.classList.contains('dark')).toBe(true)

    // Now somebody asks for light, explicitly.
    fireEvent.click(screen.getByRole('radio', { name: /light/i }))
    expect(document.documentElement.classList.contains('dark')).toBe(false)

    // The computer changes its mind again. Their choice stands.
    machineSays(false)
    machine.announce()
    machineSays(true)
    machine.announce()
    expect(document.documentElement.classList.contains('dark')).toBe(false)
  })

  it('does not call leaving a radio position', () => {
    // A radiogroup means "one of these holds". Signing out holds nothing and is
    // not an appearance, so the group becomes a plain one and the three keep
    // their own meaning rather than gaining a fourth member that never checks.
    render(
      <StrictMode>
        <ThemeSwitch onSignOut={() => {}} />
      </StrictMode>,
    )
    expect(screen.queryByRole('radiogroup')).not.toBeInTheDocument()
    expect(screen.getByRole('group')).toBeInTheDocument()
    expect(screen.getAllByRole('radio')).toHaveLength(3)
  })
})
