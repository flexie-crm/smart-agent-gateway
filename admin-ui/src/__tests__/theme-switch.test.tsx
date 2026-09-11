import { StrictMode } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@/test-utils'
import { ThemeSwitch } from '@/components/ThemeSwitch'

// Appearance and leaving sit in one frame in the corner of the sidebar. They
// were two controls of the same size, one framed and one not, which reads as
// one thing somebody had not finished aligning.
//
// Leaving is NOT a fourth position of the appearance setting, and the markup has
// to say so: the three are a radiogroup because only one of them can hold, and
// signing out is a button beside them.
describe('the appearance switch', () => {
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
