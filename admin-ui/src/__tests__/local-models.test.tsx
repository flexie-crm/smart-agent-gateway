import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@/test-utils'
import { Models } from '@/pages/Models'

// Adding a model that is already on one of our machines.
//
// The point of the separate entry: a cloud model is an id you type against an
// account you configured, and a local one is already on a machine. Nothing to
// type, only to pick, and nothing is downloaded.

const MACHINES = [
  {
    id: 3,
    name: 'gpu-lab-1',
    reachable: true,
    models: [
      { uid: 'u-1', handle: 'Qwen3-0.6B', kind: 'chat', context_length: 40960, size_bytes: 1e9, here: false },
      { uid: 'u-2', handle: 'Whisper-v3', kind: 'stt', size_bytes: 3e9, here: true },
    ],
  },
]

function serve(machines: unknown = MACHINES) {
  const calls: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      calls.push(`${init?.method ?? 'GET'} ${url}`)
      const body = (v: unknown) => new Response(JSON.stringify(v), { status: 200 })

      if (url.includes('/v1/models/machines')) return body({ machines })
      if (url.includes('/v1/models/local')) return body({ id: 9 })
      if (url.includes('/v1/models')) {
        return body({
          models: [
            {
              id: 1,
              vendor_id: 4,
              machine: 'gpu-lab-1',
              model_key: 'Qwen3-0.6B',
              type: 'chat',
              context_window: 40960,
              description: '',
              input_price_per_1m: 0,
              output_price_per_1m: 0,
              status: 'active',
            },
          ],
          vendors: [{ id: 2, vendor_key: 'anthropic', name: 'Anthropic', status: 'active' }],
        })
      }
      return body({})
    }),
  )
  return calls
}

afterEach(() => vi.unstubAllGlobals())

async function openDialog() {
  render(
    <StrictMode>
      <Models />
    </StrictMode>,
  )
  fireEvent.click(await screen.findByRole('button', { name: /add local model/i }))
  return within(await screen.findByRole('dialog'))
}

describe('adding a local model', () => {
  it('shows a machine model as its source, not a vendor account', async () => {
    // The vendor list beside it is accounts only, so without this a local model
    // would be a row with no source shown at all.
    serve()
    render(
      <StrictMode>
        <Models />
      </StrictMode>,
    )
    expect(await screen.findByText('Qwen3-0.6B')).toBeInTheDocument()
    expect(screen.getByText('gpu-lab-1')).toBeInTheDocument()
    // And it says the data does not leave, which is a property of WHERE it runs.
    expect(screen.getByText('stays local')).toBeInTheDocument()
  })

  it('picks from what is on the machine rather than asking for an id', async () => {
    serve()
    const dialog = await openDialog()

    expect(await dialog.findByText('Qwen3-0.6B')).toBeInTheDocument()
    // Nothing to type: no model id, no context window, no prices.
    expect(dialog.queryByLabelText(/model id/i)).not.toBeInTheDocument()
    expect(dialog.queryByLabelText(/context/i)).not.toBeInTheDocument()
    expect(dialog.queryByLabelText(/price/i)).not.toBeInTheDocument()
  })

  it('does not offer a model this workspace already has', async () => {
    serve()
    const dialog = await openDialog()
    await dialog.findByText('Qwen3-0.6B')
    expect(dialog.queryByText('Whisper-v3')).not.toBeInTheDocument()
  })

  it('writes a route and downloads nothing', async () => {
    const calls = serve()
    const dialog = await openDialog()

    fireEvent.click(await dialog.findByText('Qwen3-0.6B'))
    fireEvent.click(dialog.getByRole('button', { name: 'Add it' }))

    await waitFor(() =>
      expect(calls.some((c) => c === 'POST http://localhost:3000/v1/models/local' || c.endsWith('/v1/models/local'))).toBe(true),
    )
    // The weights are already there. Nothing starts a download.
    expect(calls.some((c) => c.includes('/pulls'))).toBe(false)
  })

  it('says why there is nothing to pick, rather than showing an empty list', async () => {
    serve([{ id: 3, name: 'gpu-lab-1', reachable: true, models: [] }])
    const dialog = await openDialog()
    expect(await dialog.findByText(/Nothing on that machine yet/)).toBeInTheDocument()
  })

  it('says a machine is unreachable rather than offering its models', async () => {
    serve([
      { id: 3, name: 'gpu-lab-1', reachable: false, problem: 'this machine did not answer', models: [] },
    ])
    const dialog = await openDialog()
    expect(await dialog.findByText('this machine did not answer')).toBeInTheDocument()
  })

  it('survives a machine that answers no list at all', async () => {
    // The failure this is here for: the server sent `"models":null` for a
    // machine with nothing on it, and the dialog died on `.filter`. Both
    // fixtures in both suites used `[]`, so both suites passed.
    serve([{ id: 3, name: 'gpu-lab-1', reachable: true, models: null }])
    const dialog = await openDialog()
    expect(await dialog.findByText(/Nothing on that machine yet/)).toBeInTheDocument()
  })

  it('says where machines come from when there are none', async () => {
    serve([])
    const dialog = await openDialog()
    expect(await dialog.findByText(/Machines add themselves/)).toBeInTheDocument()
  })
})

/**
 * And the installation that ships no engine of its own.
 *
 * The Windows personal edition carries none (KB/36), and for a while that hid
 * this button, the Inference screen and its route. It was the wrong conclusion
 * from a true fact: a machine is a server with a graphics card in it, added by
 * exchanging certificates with it (Nodes.tsx), and on a personal installation
 * that is the ONLY way in, because the gateway is on loopback and nothing can
 * call it back. Hiding the screen took away the one kind of inference that
 * works there.
 *
 * So what is left deciding this button is the permission, and nothing else.
 */
describe('an installation that carries no engine', () => {
  it('still offers to add a model from a machine', async () => {
    serve()
    render(
      <StrictMode>
        <Models />
      </StrictMode>,
    )
    // Waited for rather than asserted immediately: the screen renders its
    // actions before its rows arrive, so a bare query would pass on an empty
    // page and prove nothing.
    expect(await screen.findByRole('button', { name: /add cloud model/i })).toBeInTheDocument()
    expect(await screen.findByRole('button', { name: /add local model/i })).toBeInTheDocument()
  })
})
