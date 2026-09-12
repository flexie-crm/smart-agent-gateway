import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { Posture } from '@/lib/api'

/**
 * A grant names a GROUP of people, and a personal installation has one person
 * in it and no Groups screen to make another.
 *
 * So "Who may use it" is not merely redundant there: the column answers
 * "everyone" on every row for ever, and the field in the edit form renders
 * "No group exists yet.", which reads as something waiting to be set up on an
 * installation where it never will be. Both send somebody looking for a screen
 * that the personal edition deliberately does not have.
 *
 * The posture is read at module import time, so each case imports the page
 * fresh, the same way the navigation test does.
 */
async function toolsPageFor(personal: boolean) {
  vi.resetModules()
  vi.doMock('@/lib/api', async () => {
    const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
    const base: Posture = personal ? actual.PERSONAL_POSTURE : actual.SERVER_POSTURE
    return { ...actual, POSTURE: base }
  })
  // test-utils has to be imported from the SAME fresh graph as the page, or
  // its providers are a different module instance from the ones the page's
  // hooks look for and every render fails on "must be used inside a Provider".
  const { Tools } = await import('@/pages/Tools')
  const { render, screen, waitFor } = await import('@/test-utils')
  return { Tools, render, screen, waitFor }
}

function serve() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      const body = (v: unknown) => new Response(JSON.stringify(v), { status: 200 })
      if (url.includes('/v1/tools/templates')) return body([])
      if (url.includes('/v1/tools')) {
        return body([
          {
            id: 1,
            name: 'terminal',
            friendly_name: 'Terminal',
            kind: 'builtin',
            source: 'builtin',
            risk: 'internal_write',
            status: 'active',
            description: 'Runs a command.',
            grants: [],
          },
        ])
      }
      return body([])
    }),
  )
}

afterEach(() => vi.unstubAllGlobals())

describe('the tools screen and who may use a tool', () => {
  it('offers the grant column on a deployment, where groups exist', async () => {
    serve()
    const { Tools, render, screen, waitFor } = await toolsPageFor(false)
    render(
      <StrictMode>
        <Tools />
      </StrictMode>,
    )
    await waitFor(() => expect(screen.getByText('Terminal')).toBeInTheDocument())
    expect(screen.getByText('Who may use it')).toBeInTheDocument()
  })

  it('does not offer it on a personal installation, where there is nobody to grant to', async () => {
    serve()
    const { Tools, render, screen, waitFor } = await toolsPageFor(true)
    render(
      <StrictMode>
        <Tools />
      </StrictMode>,
    )
    // Asserted after the table has actually rendered, so this is the column
    // being absent rather than the page not having arrived yet, which would
    // pass on any broken build.
    await waitFor(() => expect(screen.getByText('Terminal')).toBeInTheDocument())
    expect(screen.queryByText('Who may use it')).not.toBeInTheDocument()
  })
})
