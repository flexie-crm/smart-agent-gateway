import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@/test-utils'
import { Groups } from '@/pages/Access'
import { ApiError, Offline, Unreadable } from '@/lib/api'
import { describeError } from '@/lib/form'

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

  // A fault of OUR OWN is the fourth thing, and the one that used to be
  // reported as the gateway not answering: a claim about a server that is
  // working, made by the only code that knew better.
  it('does not blame the gateway for a fault in the console', async () => {
    const logged = vi.spyOn(console, 'error').mockImplementation(() => {})
    const boom = new TypeError("Cannot read properties of undefined (reading 'filter')")

    expect(describeError(boom)).toBe(
      'Something went wrong in the console. Reload the page and try again.',
    )
    // The control: this is exactly what a dead network produces, and the two
    // must not read the same.
    expect(describeError(new Offline())).toBe('The gateway did not answer.')
    expect(describeError(boom)).not.toBe(describeError(new Offline()))

    // And the real error is not swallowed: it is the only copy of what
    // happened, and a message on a form cannot carry a stack.
    expect(logged).toHaveBeenCalledWith('the console failed while handling an answer', boom)
    logged.mockRestore()
  })

  it('tells an unreadable answer apart from no answer, and a refusal from both', () => {
    expect(describeError(new Unreadable())).toBe("The gateway's answer could not be read.")
    expect(describeError(new ApiError(400, 'invalid_request', 'a group needs a name', {}))).toBe(
      'A group needs a name.',
    )
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
