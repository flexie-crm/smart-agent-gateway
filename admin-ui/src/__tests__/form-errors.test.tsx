import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@/test-utils'
import { Groups } from '@/pages/Access'

// A form fails at two levels: a line about the attempt, and a message on the
// input that caused it. These tests drive a real form through both, and pin
// that what the gateway says is what the person reads, not a guess hard-coded
// in a catch block.

function serveGroups(post: (body: string) => Response) {
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (!url.includes('/v1/groups')) throw new Error(`unexpected request: ${url}`)
    if ((init?.method ?? 'GET') === 'GET') return new Response('[]', { status: 200 })
    return post(String(init?.body))
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

function refusal(status: number, code: string, description: string, fields?: Record<string, string>): Response {
  return new Response(
    JSON.stringify({ error: code, error_description: description, fields }),
    { status },
  )
}

async function openForm() {
  render(
    <StrictMode>
      <Groups />
    </StrictMode>,
  )
  fireEvent.click(await screen.findByRole('button', { name: /New group/ }))
  await screen.findByRole('dialog')
}

describe('a form failing at two levels', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('refuses an empty required field locally, on the field, without asking the server', async () => {
    const fetch = serveGroups(() => {
      throw new Error('nothing valid was submitted, so nothing may be sent')
    })

    await openForm()
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('A group needs a name.')).toBeInTheDocument()
    expect(screen.getByText('Check the marked fields.')).toBeInTheDocument()
    // The refusal was local: the only request the server heard is the list.
    expect(fetch.mock.calls.every(([, init]) => (init?.method ?? 'GET') === 'GET')).toBe(true)
  })

  it("shows the gateway's field message on the field, as a sentence", async () => {
    serveGroups(() =>
      refusal(409, 'conflict', 'some fields need a change', {
        name: 'another group already has this name',
      }),
    )

    await openForm()
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'Support' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('Another group already has this name.')).toBeInTheDocument()
    expect(screen.getByText('Check the marked fields.')).toBeInTheDocument()
  })

  it('reports a gateway failure on the form, because no field caused it', async () => {
    serveGroups(() => refusal(500, 'server_error', 'temporary failure'))

    await openForm()
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'Support' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(
      await screen.findByText('Something went wrong on the gateway. Try again.'),
    ).toBeInTheDocument()
  })

  it('says the gateway did not answer when the request itself died', async () => {
    const fetch = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'GET') return new Response('[]', { status: 200 })
      throw new TypeError('network down')
    })
    vi.stubGlobal('fetch', fetch)

    await openForm()
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'Support' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('The gateway did not answer.')).toBeInTheDocument()
  })

  it('clears a local refusal once the field is fixed and the save lands', async () => {
    serveGroups((body) => new Response(body, { status: 201 }))

    await openForm()
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await screen.findByText('A group needs a name.')

    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'Support' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    // The save closed the form, and took the refusal with it.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(screen.queryByText('A group needs a name.')).not.toBeInTheDocument()
  })
})
