import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import * as rtl from '@/test-utils'
import { fireEvent, waitFor, within } from '@/test-utils'
import { Route, Routes, useLocation } from 'react-router-dom'
import { Nodes } from '@/pages/Nodes'

// The machines screen shows what a MACHINE said, never what a row holds. So
// what these assert is that nothing is invented on the way: a machine that is
// off is still a row saying so, a model nothing routes to is marked, the
// difference between what was asked for and what happened is visible, and a
// download cannot be started from a search result without the machine first
// saying it will work.

const MACHINE = {
  id: 7,
  node_id: 'nd_abc',
  name: 'gpu-1',
  base_url: 'https://gpu-1:8081/v1',
  version: '0.1.0',
  reachable: true,
  info: {
    name: 'gpu-1',
    can_infer: true,
    version: '0.1.0',
    uptime_seconds: 7200,
    models: 1,
    resident: 1,
    downloads_active: 0,
    machine: {
      memory_total: 68_000_000_000,
      memory_free: 40_000_000_000,
      disk_total: 500_000_000_000,
      disk_free: 280_000_000_000,
      processors: 32,
    },
  },
}

const MODEL = {
  uid: 'u-1',
  repo: 'Qwen/Qwen3-0.6B',
  revision: 'c1899de289a04d12100db370d81485cdf75e47ca',
  name: 'Qwen3-0.6B',
  handle: 'Qwen3-0.6B',
  kind: 'chat',
  facts: { context_length: 40960, license: 'apache-2.0', files: 7, size_bytes: 1_519_182_365 },
  settings: {},
  resident: true,
  residency: 'resident',
  added_at: '2026-08-04T22:37:31Z',
  workspaces: [{ workspace_id: 1, name: 'Acme', model_id: 12 }],
}

const WORKSPACES = [
  { id: 1, name: 'Acme' },
  { id: 2, name: 'Beta' },
]

type Overrides = {
  nodes?: unknown
  detail?: unknown
  search?: unknown
  describe?: unknown
}

/** The machine screen answers the machine AND the workspaces in one. */
function screen(detail: unknown) {
  return { machine: detail, workspaces: WORKSPACES }
}

function serve(overrides: Overrides = {}) {
  const calls: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      calls.push(`${init?.method ?? 'GET'} ${url}`)
      const body = (value: unknown) => new Response(JSON.stringify(value), { status: 200 })

      if (url.includes('/library/search')) return body(overrides.search ?? { models: [] })
      if (url.includes('/library/describe')) return body(overrides.describe ?? {})
      if (url.includes('/join-token')) return body({ token: 'abc123_TESTTOKEN' })
      if (url.includes('/nodes/certificate'))
        return body({
          certificate: '-----BEGIN CERTIFICATE-----\nGATEWAYCERT\n-----END CERTIFICATE-----',
          key: 'TESTKEY123',
        })
      if (url.includes('/nodes/by-certificate'))
        return body({ id: 9, node_id: 'nd_test', name: 'gpu-1', base_url: 'https://203.0.113.7:19443' })
      if (/\/v1\/nodes\/\d+/.test(url)) {
        return body(screen(overrides.detail ?? { ...MACHINE, models: [MODEL], pulls: [] }))
      }
            // TWO by default, because a list of one is skipped: the machine IS the
      // screen. A fixture of one made every list test assert against a detail
      // page, which is the screen doing the right thing and the test asking the
      // wrong question.
      if (url.includes('/v1/nodes'))
        return body(overrides.nodes ?? { nodes: [MACHINE, { ...MACHINE, id: 2, name: 'gpu-2' }] })
      return body({})
    }),
  )
  return calls
}

afterEach(() => vi.unstubAllGlobals())

/** The router's own path. MemoryRouter never touches window.location, so this
 *  is the only place the current path can be read from. */
function Path() {
  return <span data-testid="path">{useLocation().pathname}</span>
}

function show() {
  rtl.render(
    <StrictMode>
      <Path />
      <Routes>
        <Route path="/" element={<Nodes />} />
        <Route path="/machines" element={<Nodes />} />
        <Route path="/machines/:machineID" element={<Nodes />} />
      </Routes>
    </StrictMode>,
  )
}


async function openMachine() {
  show()
  fireEvent.click(await rtl.screen.findByText('gpu-1'))
  return await rtl.screen.findByText('Qwen3-0.6B')
}

describe('machines', () => {
  it('lists a machine with what it said about itself', async () => {
    serve()
    show()
    expect(await rtl.screen.findByText('gpu-1')).toBeInTheDocument()
    // Two machines are listed now, so this says what it means: at least one row
    // reports itself ready, not that exactly one string exists on the page.
    expect(rtl.screen.getAllByText('ready').length).toBeGreaterThan(0)
    // One on disk and one in memory are different numbers and both are shown.
    expect(rtl.screen.getAllByText(/1 on disk/).length).toBeGreaterThan(0)
    expect(rtl.screen.getAllByText(/280\.0 GB free of 500\.0 GB/).length).toBeGreaterThan(0)
  })

  it('keeps a machine that is off on the list, saying why', async () => {
    // A row missing from the list would leave somebody wondering whether they
    // imagined configuring it.
    serve({
      nodes: {
        nodes: [
          { ...MACHINE, id: 9, name: 'gpu-on' },
          {
            id: 7,
            node_id: 'nd_off',
            name: 'gpu-off',
            base_url: 'https://gpu-off:8081/v1',
            version: '0.1.0',
            reachable: false,
            problem: 'this machine did not answer',
          },
        ],
      },
    })
    show()
    expect(await rtl.screen.findByText('gpu-off')).toBeInTheDocument()
    expect(rtl.screen.getByText('unreachable')).toBeInTheDocument()
    expect(rtl.screen.getByText('this machine did not answer')).toBeInTheDocument()
  })

  it('says nothing about a certificate that is renewing itself', async () => {
    // A machine renews by checking back in, daily, so a healthy one never gets
    // near its expiry. Putting a countdown on every row would be noise nobody
    // can act on, and would train people to ignore the one row that matters.
    const healthy = new Date(Date.now() + 89 * 86_400_000).toISOString()
    serve({ nodes: { nodes: [{ ...MACHINE, cert_expires_at: healthy }, { ...MACHINE, id: 2, name: 'gpu-2' }] } })
    show()
    expect(await rtl.screen.findByText('gpu-1')).toBeInTheDocument()
    expect(rtl.screen.queryByText(/certificate/)).not.toBeInTheDocument()
  })

  it('says so when a machine has stopped renewing its certificate', async () => {
    // Which is the real signal: it is answering, so it is up, but it has not
    // checked back in, and on the day it runs out it goes silent with nothing to
    // explain it. Said while there is still time to look.
    const soon = new Date(Date.now() + 3 * 86_400_000).toISOString()
    serve({ nodes: { nodes: [{ ...MACHINE, cert_expires_at: soon }, { ...MACHINE, id: 2, name: 'gpu-2' }] } })
    show()
    expect(await rtl.screen.findByText('gpu-1')).toBeInTheDocument()
    expect(rtl.screen.getByText('its certificate expires in 3 days')).toBeInTheDocument()
  })

  it('marks a model no workspace may use yet', async () => {
    // The weights sit on one disk; what varies is who may route to them, and
    // there is nowhere else this is visible.
    serve({ detail: { ...MACHINE, models: [{ ...MODEL, workspaces: [] }], pulls: [] } })
    await openMachine()
    expect(rtl.screen.getByText('no workspace yet')).toBeInTheDocument()
  })

  it('survives a model that answers no workspace list at all', async () => {
    // Same failure as the dialog's: `"workspaces":null` rather than `[]`.
    serve({
      detail: { ...MACHINE, models: [{ ...MODEL, workspaces: null }], pulls: [] },
    })
    await openMachine()
    expect(rtl.screen.getByText('no workspace yet')).toBeInTheDocument()
  })

  it('names the workspaces that may use a model', async () => {
    serve({
      detail: {
        ...MACHINE,
        models: [
          {
            ...MODEL,
            workspaces: [
              { workspace_id: 1, name: 'Acme', model_id: 12 },
              { workspace_id: 2, name: 'Beta', model_id: 13 },
            ],
          },
        ],
        pulls: [],
      },
    })
    await openMachine()
    expect(rtl.screen.getByText('Acme, Beta')).toBeInTheDocument()
  })

  it('sends the whole set of workspaces, and downloads nothing', async () => {
    // The property the platform-scoped shape exists for: a workspace added here
    // gets a route to the copy already on the machine.
    const calls = serve()
    await openMachine()

    fireEvent.click(rtl.screen.getByText('Acme'))
    const dialog = within(await rtl.screen.findByRole('dialog'))
    fireEvent.click(dialog.getByLabelText('Beta'))
    fireEvent.click(dialog.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(calls.some((c) => c.startsWith('PUT') && c.includes('/workspaces'))).toBe(true),
    )
    expect(calls.some((c) => c.startsWith('POST') && c.includes('/pulls'))).toBe(false)
  })

  it('keeps the machine in the url, so a refresh stays on it', async () => {
    // Which machine you are looking at is navigation, not component state: a
    // reload used to drop you back on the list, and the address was not worth
    // sending to anybody.
    serve()
    show()
    fireEvent.click(await rtl.screen.findByText('gpu-1'))
    await rtl.screen.findByText('Qwen3-0.6B')
    expect(rtl.screen.getByTestId('path')).toHaveTextContent('/machines/7')

    // And "All machines" goes back to the list rather than leaving a stale path.
    fireEvent.click(rtl.screen.getByRole('button', { name: /All machines/ }))
    await rtl.screen.findByText('Room')
    expect(rtl.screen.getByTestId('path')).toHaveTextContent('/machines')
  })

  // Which way in is possible is decided by the origin this console is served
  // from, so a test about joining has to say it is on a reachable one. jsdom
  // defaults to localhost, which is exactly the case where joining CANNOT work.
  const onAReachableServer = () => {
    Object.defineProperty(window, 'location', {
      value: new URL('https://sag.example.com/machines'),
      writable: true,
      configurable: true,
    })
  }

  // A console on loopback cannot be registered with, so the whole join flow is
  // impossible there and offering it is offering something that cannot work.
  // This is the personal edition's only way in, and was missing entirely: its
  // dialog printed `curl http://127.0.0.1:55271/...` for a machine somewhere
  // else to run.
  it('swaps certificates when this gateway cannot be called back', async () => {
    const calls = serve()
    show()
    fireEvent.click(await rtl.screen.findByRole('button', { name: /Add a machine/ }))

    const dialog = within(await rtl.screen.findByRole('dialog'))
    // No token is minted: nothing is going to present one.
    expect(calls.filter((c) => c.includes('/nodes/join-token'))).toHaveLength(0)

    // OURS goes out, inside the command, so the machine will accept only us.
    // Base64, not PEM: one word with no newlines and no shell quoting, which is
    // what makes it pasteable and what makes a truncated paste visible.
    const encoded = btoa('-----BEGIN CERTIFICATE-----\nGATEWAYCERT\n-----END CERTIFICATE-----')
    expect(await dialog.findByText(new RegExp(encoded))).toBeInTheDocument()
    expect(dialog.getByText(/--gateway-cert/)).toBeInTheDocument()
    // The key goes out WITH the certificate. Minting one at each end instead
    // meant the machine refused every call after a good TLS handshake.
    expect(dialog.getByText(/--node-key TESTKEY123/)).toBeInTheDocument()
    // Where models land is in the command, not left to a flag somebody has to
    // notice: the default is on the root disk and a model is tens of gigabytes.
    expect(dialog.getByText(/--data \/var\/lib\/sag-inference/)).toBeInTheDocument()
    fireEvent.change(dialog.getByPlaceholderText('/var/lib/sag-inference'), {
      target: { value: '/mnt/big' },
    })
    expect(dialog.getByText(/--data \/mnt\/big/)).toBeInTheDocument()
    expect(dialog.getByText(/sag-repo\.flexie\.io/)).toBeInTheDocument()

    // THEIRS comes back, with where to reach them.
    fireEvent.change(dialog.getByPlaceholderText('gpu-1'), { target: { value: 'gpu-1' } })
    fireEvent.change(dialog.getByPlaceholderText('https://203.0.113.7:19443'), {
      target: { value: 'https://203.0.113.7:19443' },
    })
    fireEvent.change(dialog.getByPlaceholderText('-----BEGIN CERTIFICATE-----'), {
      target: { value: '-----BEGIN CERTIFICATE-----\nNODECERT\n-----END CERTIFICATE-----' },
    })
    fireEvent.click(dialog.getByRole('button', { name: /Add the machine/ }))

    await rtl.waitFor(() =>
      expect(calls.some((c) => c.includes('/nodes/by-certificate'))).toBe(true),
    )
  })

  it('adds a machine by showing one command to paste, not a form', async () => {
    onAReachableServer()
    // Nobody types an address and nobody chooses a key: the machine mints its
    // own and registers itself. What is pasted INSTALLS the node, rather than
    // assuming somebody has already put a binary on the box.
    serve()
    show()
    fireEvent.click(await rtl.screen.findByRole('button', { name: /Add a machine/ }))

    const dialog = within(await rtl.screen.findByRole('dialog'))
    expect(await dialog.findByText(/abc123_TESTTOKEN/)).toBeInTheDocument()
    expect(dialog.getByText(/install\.sh/)).toBeInTheDocument()
    // From the published download host, NOT this console's origin. It used to be
    // the origin, which is unreachable the moment the console is on loopback.
    expect(dialog.getByText(/sag-repo\.flexie\.io/)).toBeInTheDocument()
    // No address field: it is read from where the machine calls in.
    expect(dialog.queryByPlaceholderText(/http/)).not.toBeInTheDocument()
  })

  // The button asked a question nobody adding a machine came to answer. A fresh
  // token arrives with the dialog instead, in ONE request: there is nothing to
  // read back, because a token is spent by the machine that uses it.
  it('mints the token on opening, in one request, with no button for it', async () => {
    onAReachableServer()
    const calls = serve()
    show()
    fireEvent.click(await rtl.screen.findByRole('button', { name: /Add a machine/ }))

    const dialog = within(await rtl.screen.findByRole('dialog'))
    await dialog.findByText(/abc123_TESTTOKEN/)
    expect(dialog.queryByRole('button', { name: /Rotate/i })).not.toBeInTheDocument()

    const tokenCalls = calls.filter((c) => c.includes('/nodes/join-token'))
    expect(
      tokenCalls,
      `opening the dialog should mint once and read nothing back; it made: ${calls.join(', ')}`,
    ).toEqual(['POST /v1/nodes/join-token'])
  })

  // The disclosure was one click between somebody and three flags they might
  // need; the flags are short and there are three of them.
  it('shows the other flags outright, in an aligned pair of columns', async () => {
    onAReachableServer()
    serve()
    show()
    fireEvent.click(await rtl.screen.findByRole('button', { name: /Add a machine/ }))

    const dialog = within(await rtl.screen.findByRole('dialog'))
    await dialog.findByText(/abc123_TESTTOKEN/)
    expect(dialog.queryByText(/Other cases/i)).not.toBeInTheDocument()
    expect(dialog.getByText('--accel cpu')).toBeInTheDocument()
    expect(dialog.getByText('--data /big/disk')).toBeInTheDocument()
    expect(dialog.getByText('--port <number>')).toBeInTheDocument()
    // A number in the flag column reads as the default and was read as one. The
    // default is stated above instead, emphasised, and with the reason somebody
    // needs it: a firewall they have to go and open, on another screen.
    expect(dialog.getByText('19443')).toBeInTheDocument()
    expect(dialog.getByText(/Open it on the machine's firewall/)).toBeInTheDocument()

    // Every one of them optional, and said so. What is pasted onto a machine is
    // --url and --token; --advertise is listed last because it is worked out on
    // its own now, where it used to be mandatory while being described as a flag
    // for machines behind NAT.
    expect(dialog.getByText(/Optional\. Add to the end of the command/)).toBeInTheDocument()
    expect(dialog.getByText(/^--advertise/)).toBeInTheDocument()
  })

  it('shows a model that was asked for and did not load', async () => {
    // The decision and the reality disagree when a load fails. Showing only one
    // of them hides the fault.
    serve({
      detail: {
        ...MACHINE,
        models: [{ ...MODEL, resident: true, residency: 'released' }],
        pulls: [],
      },
    })
    await openMachine()
    expect(rtl.screen.getByText('wanted, not loaded')).toBeInTheDocument()
  })

  it('says a model is loading rather than pretending it is in', async () => {
    // Loading is minutes. A control that flipped straight to "in memory" would
    // be lying for all of them.
    serve({
      detail: { ...MACHINE, models: [{ ...MODEL, residency: 'waking' }], pulls: [] },
    })
    await openMachine()
    expect(rtl.screen.getByText('loading…')).toBeInTheDocument()
    expect(rtl.screen.queryByRole('button', { name: 'Release' })).not.toBeInTheDocument()
  })

  it('shows how far a download has got', async () => {
    serve({
      detail: {
        ...MACHINE,
        models: [],
        pulls: [
          {
            id: 'p-1',
            repo: 'Qwen/Qwen3-4B',
            revision: 'abc',
            state: 'downloading',
            bytes_done: 500_000_000,
            bytes_total: 4_000_000_000,
            files_done: 3,
            files_total: 7,
            started_at: '2026-08-04T22:31:20Z',
            updated_at: '2026-08-04T22:33:20Z',
          },
        ],
      },
    })
    show()
    fireEvent.click(await rtl.screen.findByText('gpu-1'))
    expect(await rtl.screen.findByText('Qwen/Qwen3-4B')).toBeInTheDocument()
    expect(rtl.screen.getByText(/500\.0 MB of 4\.0 GB · 3\/7 files/)).toBeInTheDocument()
    expect(rtl.screen.getByRole('button', { name: 'Stop' })).toBeInTheDocument()
  })

  it('offers to resume or delete a download that was stopped', async () => {
    // Stopping keeps the bytes, so there is something to carry on with. Before
    // this there was no way to do either: the entry just sat there.
    const calls = serve({
      detail: {
        ...MACHINE,
        models: [],
        pulls: [
          {
            id: 'p-1',
            repo: 'Qwen/Qwen3-4B',
            revision: 'abc',
            state: 'cancelled',
            bytes_done: 500_000_000,
            bytes_total: 4_000_000_000,
            files_done: 3,
            files_total: 7,
            started_at: '2026-08-05T00:00:00Z',
            updated_at: '2026-08-05T00:01:00Z',
          },
        ],
      },
    })
    show()
    fireEvent.click(await rtl.screen.findByText('gpu-1'))
    await rtl.screen.findByText('Qwen/Qwen3-4B')

    // Not offered a Stop, because it is not going.
    expect(rtl.screen.queryByRole('button', { name: 'Stop' })).not.toBeInTheDocument()

    fireEvent.click(rtl.screen.getByRole('button', { name: /Resume/ }))
    await waitFor(() =>
      expect(calls.some((c) => c.startsWith('POST') && c.endsWith('/pulls/p-1/resume'))).toBe(true),
    )

    fireEvent.click(rtl.screen.getByRole('button', { name: /Delete/ }))
    await waitFor(() =>
      expect(calls.some((c) => c.startsWith('DELETE') && c.endsWith('/pulls/p-1'))).toBe(true),
    )
  })

  // This used to assert that a finished download offered neither Resume nor
  // Delete, while still being listed. Its own comment gave the better answer and
  // the assertions did not follow it: there is nothing to resume and nothing to
  // throw away BECAUSE IT BECAME A MODEL. So that is what is checked now, and
  // the buttons cannot come back by accident because the row is not there at all.
  it('moves a finished download into the model table rather than listing it twice', async () => {
    serve({
      detail: {
        ...MACHINE,
        models: [MODEL],
        pulls: [
          {
            id: 'p-1',
            repo: 'Qwen/Qwen3-0.6B',
            revision: 'abc',
            state: 'ready',
            bytes_done: 10,
            bytes_total: 10,
            files_done: 1,
            files_total: 1,
            started_at: '2026-08-05T00:00:00Z',
            updated_at: '2026-08-05T00:01:00Z',
          },
        ],
      },
    })
    show()
    fireEvent.click(await rtl.screen.findByText('gpu-1'))

    // Where the work ended up: the model table, once.
    expect(await rtl.screen.findByText('Qwen3-0.6B')).toBeInTheDocument()
    // And not a second time as a download that has nothing left to do.
    expect(rtl.screen.queryByText('ready')).toBeNull()
    expect(rtl.screen.queryByRole('button', { name: /Resume/ })).not.toBeInTheDocument()
  })

  // The header must not move once the machine answers.
  //
  // It is a fixed-height bar with its contents centred, so a description that
  // arrives late takes the title block from one line to two and the title jumps
  // upward as it re-centres. This screen is the only one that waits on a request
  // for its description, which is why it was the only one that shifted.
  it('has a description from the first render, so the header does not shift', async () => {
    serve({ nodes: { nodes: [MACHINE] } })
    show()

    // The heading is the same word from the first frame to the last. It used to
    // be the machine's name, which meant the header said "Inference" while the
    // list was asking and then "This computer" when the answer came back: two
    // words in the same place, half a second apart.
    const heading = () => rtl.screen.getByRole('heading', { level: 1 }).textContent
    expect(heading()).toBe('Inference')
    await rtl.screen.findByText('gpu-1')
    expect(heading()).toBe('Inference')
  })

  it('offers somewhere to put a library token, and never shows one back', async () => {
    // The gap this closes: the refusal for a gated model told people to "add an
    // access token under Model library", and no such screen existed. A message
    // that points at nothing is worse than one that says nothing.
    // On the MACHINE screen, which is where somebody meets a gated model: they
    // search from here and are told it is refused. It used to be on the list,
    // which a personal installation never sees, since a list of one machine is
    // skipped, so the edition most likely to want a Meta model had nowhere to
    // put a token.
    serve({ nodes: { nodes: [MACHINE] } })
    show()

    expect(await rtl.screen.findByText('Model library')).toBeInTheDocument()
    fireEvent.click(rtl.screen.getByRole('button', { name: /Add a token/ }))

    // Both steps, with their addresses. Somebody who has not done this cannot be
    // expected to know either.
    const accept = await rtl.screen.findByRole('link', { name: /huggingface\.co$/ })
    expect(accept).toHaveAttribute('href', 'https://huggingface.co')
    expect(rtl.screen.getByRole('link', { name: /settings\/tokens/ })).toHaveAttribute(
      'href',
      'https://huggingface.co/settings/tokens',
    )
    // And it must say plainly that nobody can accept the licence for them.
    expect(rtl.screen.getByText(/Nobody can accept it for you/)).toBeInTheDocument()

    // The field is a password field: a credential typed in the clear is one a
    // screen share or a screenshot carries away.
    const field = rtl.screen.getByPlaceholderText('Access token')
    expect(field).toHaveAttribute('type', 'password')
  })

  it('will not start a download the machine says it cannot take', async () => {
    // Approval must equal success. The confirmation is drawn from what the
    // machine said about the exact commit, and a refusal disables the button
    // rather than letting somebody press it and be told afterwards.
    const calls = serve({
      search: { models: [{ repo: 'Qwen/Qwen3-32B', name: 'Qwen3-32B', kind: 'chat', downloads: 10, likes: 1 }] },
      describe: {
        repo: 'Qwen/Qwen3-32B',
        name: 'Qwen3-32B',
        revision: 'deadbeefcafe',
        kind: 'chat',
        gated: false,
        files: [{ path: 'model.safetensors', size: 65_000_000_000 }],
        size_bytes: 65_000_000_000,
        choices: [],
        storage: { ok: false, reason: 'needs 65.0 GB and this node has 10.0 GB free' },
        memory: { ok: false, reason: 'needs about 65.0 GB and this node has 68.0 GB in total' },
        access: { ok: true },
        runtime: { ok: true },
      },
    })
    await openMachine()

    fireEvent.click(rtl.screen.getByRole('button', { name: /Add a model/ }))
    fireEvent.change(await rtl.screen.findByPlaceholderText('Qwen3-0.6B'), {
      target: { value: 'Qwen3-32B' },
    })
    fireEvent.click(rtl.screen.getByRole('button', { name: 'Search' }))
    fireEvent.click(await rtl.screen.findByText('Qwen3-32B'))

    // The machine's own words, not ours.
    expect(await rtl.screen.findByText(/needs 65\.0 GB and this node has 10\.0 GB free/)).toBeInTheDocument()
    expect(rtl.screen.getByRole('button', { name: 'Download it' })).toBeDisabled()
    expect(calls.some((c) => c.startsWith('POST') && c.includes('/pulls'))).toBe(false)
  })

  it('will not download a model nothing on the machine could ever load', async () => {
    // The one refusal that is not about resources and cannot be resolved by
    // freeing any. A speech recognition model passed the disk, memory and
    // credential checks, downloaded 1.3 GB, and then failed at load with
    // nothing on the screen to explain it.
    //
    // Two things are asserted, and the second is the one that was missing: the
    // button is disabled, AND the sentence says what the model is and what this
    // machine does instead, so somebody knows what to look for next.
    const calls = serve({
      search: {
        models: [
          {
            repo: 'facebook/wav2vec2-base-960h',
            name: 'wav2vec2-base-960h',
            kind: 'stt',
            downloads: 99,
            likes: 9,
          },
        ],
      },
      describe: {
        repo: 'facebook/wav2vec2-base-960h',
        name: 'wav2vec2-base-960h',
        revision: '22aad52d435eb6d',
        kind: 'stt',
        gated: false,
        architecture: 'Wav2Vec2ForCTC',
        files: [{ path: 'model.safetensors', size: 1_300_000_000 }],
        size_bytes: 1_300_000_000,
        choices: [],
        // Every resource check passes. That was the trap: nothing said no.
        storage: { ok: true },
        memory: { ok: true },
        access: { ok: true },
        runtime: {
          ok: false,
          reason:
            'This machine cannot run this model. This one listens to recordings and writes down ' +
            'the words, which is not something it can do. This machine runs models that write ' +
            'text, models that read images and write about them, and models that turn text into ' +
            'numbers for search.',
        },
      },
    })
    await openMachine()

    fireEvent.click(rtl.screen.getByRole('button', { name: /Add a model/ }))
    fireEvent.change(await rtl.screen.findByPlaceholderText('Qwen3-0.6B'), {
      target: { value: 'wav2vec2' },
    })
    fireEvent.click(rtl.screen.getByRole('button', { name: 'Search' }))
    fireEvent.click(await rtl.screen.findByText('wav2vec2-base-960h'))

    expect(await rtl.screen.findByText(/listens to recordings/)).toBeInTheDocument()
    expect(rtl.screen.getByText(/models that write text/)).toBeInTheDocument()
    // What it read, for somebody who wants to check the machine's working.
    expect(rtl.screen.getByText('Wav2Vec2ForCTC')).toBeInTheDocument()

    expect(rtl.screen.getByRole('button', { name: 'Download it' })).toBeDisabled()
    expect(calls.some((c) => c.startsWith('POST') && c.includes('/pulls'))).toBe(false)
  })

  it('starts a download the machine said it can take', async () => {
    const calls = serve({
      search: { models: [{ repo: 'Qwen/Qwen3-0.6B', name: 'Qwen3-0.6B', kind: 'chat', downloads: 10, likes: 1 }] },
      describe: {
        repo: 'Qwen/Qwen3-0.6B',
        name: 'Qwen3-0.6B',
        revision: 'c1899de289a04',
        kind: 'chat',
        gated: false,
        license: 'apache-2.0',
        files: [{ path: 'model.safetensors', size: 1_500_000_000 }],
        size_bytes: 1_519_182_365,
        choices: [],
        storage: { ok: true },
        memory: { ok: true },
        access: { ok: true },
        runtime: { ok: true },
      },
    })
    await openMachine()

    fireEvent.click(rtl.screen.getByRole('button', { name: /Add a model/ }))
    fireEvent.change(await rtl.screen.findByPlaceholderText('Qwen3-0.6B'), {
      target: { value: 'Qwen3' },
    })
    fireEvent.click(rtl.screen.getByRole('button', { name: 'Search' }))

    // Scoped to the dialog: the model already on the machine has the same name,
    // and clicking the row behind would prove nothing.
    const dialog = within(rtl.screen.getByRole('dialog'))
    fireEvent.click(await dialog.findByText('Qwen/Qwen3-0.6B'))

    // The exact commit, so what is agreed to is one set of bytes rather than
    // whatever "latest" means by the time it arrives.
    expect(await rtl.screen.findByText('c1899de289a0')).toBeInTheDocument()
    fireEvent.click(rtl.screen.getByRole('button', { name: 'Download it' }))

    await waitFor(() =>
      expect(calls.some((c) => c.startsWith('POST') && c.includes('/pulls'))).toBe(true),
    )
  })

  it('asks the machine for the settings form rather than knowing the fields', async () => {
    const calls = serve()
    await openMachine()

    const row = rtl.screen.getByText('Qwen3-0.6B').closest('tr')
    expect(row).not.toBeNull()
    fireEvent.click(within(row!).getByRole('button', { name: 'Edit' }))

    await waitFor(() =>
      expect(calls.some((c) => c.includes('/models/u-1/form'))).toBe(true),
    )
  })

  // A machine that is down is the ONE case where the answer carries no model
  // list, and it is the one screen somebody opens to find out why. It threw on
  // `models.map` and React unmounted the tree, so the report was "blank page"
  // rather than "it says nothing".
  it('opens the detail screen for a machine that is down and says so', async () => {
    serve({
      detail: {
        ...MACHINE,
        reachable: false,
        problem: 'This machine did not answer.',
        info: null,
        // Null, not empty: we could not ask it what it holds. An empty list
        // would be a claim that it holds nothing.
        models: null,
        pulls: null,
      },
    })
    // Not openMachine(): that helper waits for a model to appear, and a machine
    // that cannot be asked has none to show. Waiting on the failure itself is
    // the point of the test.
    show()
    fireEvent.click(await rtl.screen.findByText('gpu-1'))

    expect(await rtl.screen.findByText('This machine did not answer.')).toBeInTheDocument()
    // The page is THERE, not a white screen: its heading and its way back both
    // rendered beside the explanation.
    expect(rtl.screen.getByText('gpu-1')).toBeInTheDocument()
    expect(rtl.screen.getByRole('button', { name: /All machines/ })).toBeInTheDocument()

    // And the model table SETTLES. This assertion is the point of the test and
    // was missing: the answer carries models null, the table read null as
    // "still loading", and drew a spinner that never stopped underneath the
    // sentence saying the machine did not answer. Asserting only that the
    // sentence was present passed against exactly that.
    expect(
      await rtl.screen.findByText('This machine could not be asked what it holds.'),
    ).toBeInTheDocument()
    expect(document.querySelector('.animate-spin')).toBeNull()

    // And nothing can be added to a machine that cannot be asked what it has:
    // the button opened a search dialog that then failed against that machine.
    expect(rtl.screen.getByRole('button', { name: /Add a model/ })).toBeDisabled()
  })

  // A machine can answer the first question and fail the next: /node replies,
  // /node/models does not. The answer then carries reachable true WITH a
  // problem, which the screen printed only in its unreachable branch, so the
  // machine read as ready and its reason went on the floor. That is an ordinary
  // state while an installation is still coming up.
  it('shows the reason when a machine answers but cannot be asked what it holds', async () => {
    serve({
      detail: {
        ...MACHINE,
        reachable: true,
        problem: "read the machine's models: connection refused",
        models: null,
        pulls: null,
      },
    })
    show()
    fireEvent.click(await rtl.screen.findByText('gpu-1'))

    expect(
      await rtl.screen.findByText("read the machine's models: connection refused"),
    ).toBeInTheDocument()
    expect(rtl.screen.getByRole('button', { name: /Add a model/ })).toBeDisabled()
    expect(document.querySelector('.animate-spin')).toBeNull()
  })

  // A load that fails has no reply to travel back on: it is started by a
  // request and finishes long after it. The badge alone is the same red chip
  // whether the machine ran out of memory or the engine cannot load that
  // architecture AT ALL, and those are the same picture with completely
  // different answers. This is the case that cost somebody an evening: a
  // wav2vec2 speech model on an engine that only loads causal language models,
  // refused precisely, in a log file nobody reads.
  it('says why a load failed, beside saying that it did', async () => {
    serve({
      detail: {
        ...MACHINE,
        models: [
          {
            ...MODEL,
            resident: true,
            residency: 'released',
            load_error: 'the engine refused: Unsupported model class `Wav2Vec2ForCTC`',
          },
        ],
        pulls: [],
      },
    })
    show()
    fireEvent.click(await rtl.screen.findByText('gpu-1'))

    expect(await rtl.screen.findByText('wanted, not loaded')).toBeInTheDocument()
    expect(rtl.screen.getByText(/Unsupported model class/)).toBeInTheDocument()
  })

  // And the badge stays alone when the machine said nothing, rather than
  // rendering an empty space where a sentence would be.
  it('shows the badge without a reason when there is none', async () => {
    serve({
      detail: {
        ...MACHINE,
        models: [{ ...MODEL, resident: true, residency: 'released' }],
        pulls: [],
      },
    })
    show()
    fireEvent.click(await rtl.screen.findByText('gpu-1'))
    expect(await rtl.screen.findByText('wanted, not loaded')).toBeInTheDocument()
  })

  // The node keeps a pull for an hour after it ends, so a finished download
  // outlives the model it produced: delete the model and the row sat there
  // saying "ready" about something that no longer existed, with no button on it
  // to dismiss it. A finished download has already become a row in the model
  // table; saying it twice is noise, and saying it after a delete is wrong.
  it('drops a download that finished, and keeps the ones still to deal with', async () => {
    serve({
      detail: {
        ...MACHINE,
        models: [],
        pulls: [
          {
            id: 'p-done',
            repo: 'Alimzhan/wav2vec2-large-xls-r-300m-albanian-colab',
            state: 'ready',
            bytes_done: 100,
            bytes_total: 100,
            files_done: 1,
            files_total: 1,
          },
          {
            id: 'p-going',
            repo: 'Qwen/Qwen3-8B',
            state: 'downloading',
            bytes_done: 50,
            bytes_total: 100,
            files_done: 1,
            files_total: 2,
          },
          {
            id: 'p-broken',
            repo: 'some/failed',
            state: 'failed',
            error: 'the transfer stopped',
            bytes_done: 10,
            bytes_total: 100,
            files_done: 0,
            files_total: 2,
          },
        ],
      },
    })
    show()
    fireEvent.click(await rtl.screen.findByText('gpu-1'))

    // Still arriving, so it is shown and can be stopped.
    expect(await rtl.screen.findByText('Qwen/Qwen3-8B')).toBeInTheDocument()
    // Failed, so it is shown: it has a Resume and a Delete and somebody has to
    // decide.
    expect(rtl.screen.getByText('the transfer stopped')).toBeInTheDocument()
    // Finished. It became a model row; this list is not where it lives.
    expect(
      rtl.screen.queryByText('Alimzhan/wav2vec2-large-xls-r-300m-albanian-colab'),
    ).toBeNull()
  })

  // A personal installation goes straight to its one machine, and the case that
  // matters is the one where it has none. It held a single piece of state for
  // "we have not asked yet" and "there was nothing", rendered nothing for both,
  // and so showed a blank page for ever with no error on it and nothing in any
  // log. On Windows that is the ORDINARY state: the engine is deliberately not
  // in the installer and arrives afterwards.
  it('says so when this computer has no machine, rather than showing nothing', async () => {
    serve({ nodes: { nodes: [] } })
    show()

    expect(await rtl.screen.findByRole('heading', { name: 'Inference' })).toBeInTheDocument()
    // Empty is its own answer and not a blank page: one piece of state standing
    // for both "have not asked" and "there is nothing" is how a build with no
    // engine showed nothing at all, for ever, with no error anywhere.
    expect(await rtl.screen.findByText(/Nothing to run models with yet/)).toBeInTheDocument()
    // And there is a way out of it, which is the point of the list always being
    // the screen: a machine can be added, including a remote one with a card.
    expect(rtl.screen.getByRole('button', { name: /Add a machine/ })).toBeInTheDocument()
  })

  // Decided by what is there, not by which edition this is: one machine is one
  // machine whether the installer was personal or a deployment happens to have
  // exactly one box.
  it('goes straight to the machine when there is only one', async () => {
    serve({ nodes: { nodes: [MACHINE] } })
    show()

    expect(await rtl.screen.findByText('gpu-1')).toBeInTheDocument()
    // No way back to a list of one.
    expect(rtl.screen.queryByRole('button', { name: /All machines/ })).toBeNull()
  })
})
