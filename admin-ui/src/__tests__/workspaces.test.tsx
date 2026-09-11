import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@/test-utils'
import { Workspaces } from '@/pages/Workspaces'

// The workspaces screen manages the partitions of the tenant. The delete path
// matters most: the server refuses to delete the ground the caller stands on,
// and that refusal must reach the person's eyes, not vanish in a rejected
// promise.

const LIST = [
  { id: 1, slug: 'acme', name: 'Acme', description: 'The sales team.', status: 'active' },
  { id: 2, slug: 'globex', name: 'Globex', description: '', status: 'suspended' },
]

describe('the workspaces screen', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('lists the partitions with their description and status', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify(LIST), { status: 200 })),
    )
    render(
      <StrictMode>
        <Workspaces />
      </StrictMode>,
    )

    expect(await screen.findByText('Acme')).toBeInTheDocument()
    // The description is what a person reads; the slug is never shown.
    expect(screen.getByText('The sales team.')).toBeInTheDocument()
    expect(screen.queryByText('acme')).not.toBeInTheDocument()
    expect(screen.getByText('suspended')).toBeInTheDocument()
  })

  it("speaks the server's refusal when a delete is denied", async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        if ((init?.method ?? 'GET') === 'GET') {
          return new Response(JSON.stringify(LIST), { status: 200 })
        }
        return new Response(
          JSON.stringify({
            error: 'conflict',
            error_description: 'you cannot delete the workspace you are working in',
          }),
          { status: 409 },
        )
      }),
    )
    render(
      <StrictMode>
        <Workspaces />
      </StrictMode>,
    )
    await screen.findByText('Acme')

    fireEvent.click(screen.getAllByLabelText('Delete')[0])

    // The delete asks first, in the console's own dialog; confirming it sends
    // the request.
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete' }))

    // The server's refusal comes back as an error toast, in its own words.
    await waitFor(() =>
      expect(
        screen.getByText('You cannot delete the workspace you are working in.'),
      ).toBeInTheDocument(),
    )
  })
})
