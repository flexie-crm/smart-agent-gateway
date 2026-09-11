import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@/test-utils'
import { Roles, Users } from '@/pages/Access'

// The access screens speak human. The roles form shows the catalog's words,
// never a machine key; the user form says which groups a person is in and
// saves the memberships in one wholesale write.

vi.mock('@/lib/auth', () => ({
  useAuth: () => ({
    workspaces: [{ id: 1, name: 'Acme' }],
  }),
}))

const CATALOG = [
  { key: '*', area: 'Everything', label: 'Everything (superuser)' },
  { key: 'models:view', area: 'Models', label: 'View' },
  { key: 'models:create', area: 'Models', label: 'Create' },
  { key: 'brains:delete', area: 'Brains', label: 'Delete' },
]

describe('the roles form', () => {
  afterEach(() => vi.unstubAllGlobals())

  function serve() {
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      if (url.includes('/v1/permissions')) return new Response(JSON.stringify(CATALOG), { status: 200 })
      // The roles screen is ONE answer: the roles, and the permissions a role
      // can hold. The catalogue is a constant this screen cannot draw a form
      // without, so it was a second request that could never differ.
      if (url.includes('/v1/roles') && method === 'GET') {
        return new Response(JSON.stringify({ roles: [], permissions: CATALOG }), { status: 200 })
      }
      if (url.includes('/v1/roles') && method === 'POST') {
        return new Response(JSON.stringify({ id: 9, name: 'Ops', permissions: [] }), { status: 201 })
      }
      throw new Error(`unexpected request: ${method} ${url}`)
    })
    vi.stubGlobal('fetch', fetch)
    return fetch
  }

  it('shows the words a person reads, grouped by area, never the machine key', async () => {
    serve()
    render(
      <StrictMode>
        <Roles />
      </StrictMode>,
    )
    fireEvent.click(await screen.findByRole('button', { name: /New role/ }))
    await screen.findByRole('dialog')

    // Areas are headings; entries are the catalog's labels.
    expect(await screen.findByText('Models')).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: 'View' })).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: 'Everything (superuser)' })).toBeInTheDocument()
    // The key is for the API, not for people.
    expect(screen.queryByText('models:view')).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox', { name: '*' })).not.toBeInTheDocument()
  })

  it('saves the keys behind the words it showed', async () => {
    const fetch = serve()
    render(
      <StrictMode>
        <Roles />
      </StrictMode>,
    )
    fireEvent.click(await screen.findByRole('button', { name: /New role/ }))
    await screen.findByRole('dialog')
    await screen.findByRole('checkbox', { name: 'View' })

    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'Ops' } })
    fireEvent.click(screen.getByRole('checkbox', { name: 'Create' }))
    fireEvent.click(screen.getByRole('checkbox', { name: 'Delete' }))
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    const post = fetch.mock.calls.find(([, init]) => init?.method === 'POST')
    expect(post).toBeDefined()
    expect(JSON.parse(String(post?.[1]?.body))).toEqual({
      name: 'Ops',
      permissions: ['models:create', 'brains:delete'],
    })
  })
})

describe('the user form and its groups', () => {
  afterEach(() => vi.unstubAllGlobals())

  const BOB = {
    id: 7,
    email: 'bob@acme.test',
    name: 'Bob',
    status: 'active',
    workspaces: [1],
  }
  const GROUPS = [
    { id: 5, name: 'Sales' },
    { id: 6, name: 'Support' },
  ]

  function serve() {
    const writes: { url: string; body: string }[] = []
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      if (method === 'GET') {
        // The dialog's own answer: this workspace's groups, each already saying
        // whether Bob is in it. Not the group catalogue plus his memberships
        // everywhere, for this screen to intersect.
        if (url.includes('/v1/users/7/form')) {
          return new Response(
            JSON.stringify({
              groups: [
                { ...GROUPS[0], member: true },
                { ...GROUPS[1], member: false },
              ],
            }),
            { status: 200 },
          )
        }
        if (url.includes('/v1/users')) return new Response(JSON.stringify([BOB]), { status: 200 })
      }
      if (method === 'PUT') {
        writes.push({ url, body: String(init?.body) })
        if (url.endsWith('/groups')) return new Response(null, { status: 204 })
        return new Response(JSON.stringify(BOB), { status: 200 })
      }
      throw new Error(`unexpected request: ${method} ${url}`)
    })
    vi.stubGlobal('fetch', fetch)
    return writes
  }

  async function openBob() {
    render(
      <StrictMode>
        <Users />
      </StrictMode>,
    )
    await screen.findByText('Bob')
    fireEvent.click(screen.getByLabelText('Edit'))
    await screen.findByRole('dialog')
    // The dialog opens at once and the memberships land in it; Sales must come
    // up already checked, never as an empty box that fills in later.
    await waitFor(() =>
      expect(screen.getByRole('checkbox', { name: 'Sales' })).toHaveAttribute(
        'data-state',
        'checked',
      ),
    )
  }

  it('shows which groups the person is in, and saves the changed set wholesale', async () => {
    const writes = serve()
    await openBob()

    expect(screen.getByRole('checkbox', { name: 'Support' })).toHaveAttribute(
      'data-state',
      'unchecked',
    )
    fireEvent.click(screen.getByRole('checkbox', { name: 'Support' }))
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    const membership = writes.find((w) => w.url.endsWith('/v1/users/7/groups'))
    expect(membership).toBeDefined()
    expect(JSON.parse(membership!.body)).toEqual({ groups: [5, 6] })
  })

  it('does not rewrite memberships that were not touched', async () => {
    const writes = serve()
    await openBob()

    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    // The user was saved; the groups were left alone.
    expect(writes.some((w) => w.url.endsWith('/v1/users/7'))).toBe(true)
    expect(writes.some((w) => w.url.endsWith('/groups'))).toBe(false)
  })

  it('asks one question when the dialog opens, and none before', async () => {
    serve()
    render(
      <StrictMode>
        <Users />
      </StrictMode>,
    )
    await screen.findByText('Bob')

    const fetch = globalThis.fetch as unknown as { mock: { calls: [RequestInfo | URL][] } }
    const before = fetch.mock.calls.map(([input]) => String(input))
    // The user TABLE is one request. The workspace's groups used to be fetched
    // here too, on every view, for a dialog nobody had opened.
    expect(before.filter((url) => url.includes('/v1/groups'))).toEqual([])

    fireEvent.click(screen.getByLabelText('Edit'))
    await waitFor(() =>
      expect(screen.getByRole('checkbox', { name: 'Sales' })).toHaveAttribute(
        'data-state',
        'checked',
      ),
    )
    const opened = fetch.mock.calls
      .map(([input]) => String(input))
      .filter((url) => !before.includes(url))
    // And ONE when it opens, not a catalogue plus a membership list to join.
    expect(opened).toEqual(['/v1/users/7/form'])
  })
})
