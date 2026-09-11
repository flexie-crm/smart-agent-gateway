import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@/test-utils'
import { MCPServers, connectionOutcome } from '@/pages/MCPServers'
import type { SocketMessage } from '@/lib/reconnecting-socket'

// The console's socket, standing in for the real one. There is no server here
// to push from, and what is under test is the screen's reaction rather than the
// connection: `push` is the gateway saying something.
const handlers = new Set<(message: SocketMessage) => void>()
vi.mock('@/lib/ws', () => ({
  listen: (handler: (message: SocketMessage) => void) => {
    handlers.add(handler)
    return () => handlers.delete(handler)
  },
}))
function push(message: SocketMessage) {
  for (const handler of [...handlers]) handler(message)
}

// The MCP servers screen manages the connections whose tools the agents end
// up carrying. What matters here: the sync says what it changed, and the
// OAuth connect sends the person to the address the gateway minted.

const SERVERS = [
  {
    id: 1,
    name: 'CRM',
    url: 'https://crm.example.test/mcp',
    auth_type: 'api_key',
    has_api_key: true,
    connected: false,
    status: 'active',
    tool_prefix: 'crm',
    created_at: '2026-07-01T09:00:00Z',
    last_synced_at: '2026-07-13T10:00:00Z',
  },
  {
    id: 2,
    name: 'Docs',
    url: 'https://docs.example.test/mcp',
    auth_type: 'oauth',
    has_api_key: false,
    connected: false,
    status: 'active',
    tool_prefix: 'docs',
    created_at: '2026-07-02T09:00:00Z',
    last_synced_at: null,
    last_error: 'the service did not answer',
  },
]

function serve(onPost: (url: string) => Response) {
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if ((init?.method ?? 'GET') === 'GET') return new Response(JSON.stringify(SERVERS), { status: 200 })
    return onPost(url)
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

describe('the mcp servers screen', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows each connection by name, not by the prefix its tools are stored under', async () => {
    serve(() => new Response('{}', { status: 200 }))
    render(
      <StrictMode>
        <MCPServers />
      </StrictMode>,
    )

    expect(await screen.findByText('CRM')).toBeInTheDocument()
    // The prefix is ours. It named nothing a person chose, it was shown in a
    // form (`crm.*`) the tools have not used since they were renamed for the
    // model, and the catalogue now groups by the connection's own name.
    expect(screen.queryByText('crm.*')).not.toBeInTheDocument()
    expect(screen.queryByText(/crm_/)).not.toBeInTheDocument()
    expect(screen.getByText(new Date('2026-07-01T09:00:00Z').toLocaleDateString())).toBeInTheDocument()
    expect(screen.getByText('key stored')).toBeInTheDocument()
    // The failed connection wears its failure; the reason rides the hover.
    expect(screen.getByText('failed')).toHaveAttribute('title', 'the service did not answer')
  })

  it('says what a sync changed, in words', async () => {
    serve((url) => {
      if (!url.includes('/sync')) throw new Error(`unexpected post: ${url}`)
      return new Response(
        JSON.stringify({ offered: 3, added: 2, changed: 1, missing: 1, skipped: 0 }),
        { status: 200 },
      )
    })
    render(
      <StrictMode>
        <MCPServers />
      </StrictMode>,
    )
    await screen.findByText('CRM')
    fireEvent.click(screen.getByLabelText('Sync CRM'))

    // The outcome lands in a toast: the connection's name and, in words, what
    // the sync changed.
    await screen.findByText('2 new, 1 changed, 1 gone.')
  })

  // Connecting, as it actually happens.
  //
  // The address cannot be opened from the click that asked for it: minting it
  // means discovery and, often, registering ourselves with the remote, which is
  // most of a second. A window opened after that wait is one no webview will
  // allow, and the desktop application does not even refuse it out loud: it
  // ignores it, so the button sits there having quietly registered a client on
  // a remote server. So the address is shown, and the person opens it.
  it('shows the person where they are being sent, and opens it from their own click', async () => {
    serve((url) => {
      if (!url.includes('/connect')) throw new Error(`unexpected post: ${url}`)
      return new Response(JSON.stringify({ authorize_url: 'https://as.example.test/authorize?x=1' }), {
        status: 200,
      })
    })
    const open = vi.fn()
    vi.stubGlobal('open', open)

    render(
      <StrictMode>
        <MCPServers />
      </StrictMode>,
    )
    await screen.findByText('Docs')
    fireEvent.click(screen.getByRole('button', { name: /Connect/ }))

    // The whole address, readable: a browser that will not open it can still be
    // given it by hand.
    expect(await screen.findByText('https://as.example.test/authorize?x=1')).toBeInTheDocument()
    // Nothing has been opened yet. This is the point: the opening waits for a
    // click of its own.
    expect(open).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(open).toHaveBeenCalledWith('https://as.example.test/authorize?x=1', '_blank', 'noopener')
  })

  // The consent ends somewhere this screen cannot see: another tab, another
  // browser, and on the desktop another application entirely. So the gateway
  // reports it, and the screen has to act on being told.
  it('hears the outcome from the gateway and refreshes itself', async () => {
    const fetch = serve(
      () =>
        new Response(JSON.stringify({ authorize_url: 'https://as.example.test/authorize' }), {
          status: 200,
        }),
    )
    const gets = () =>
      fetch.mock.calls.filter(([, init]) => (init?.method ?? 'GET') === 'GET').length
    vi.stubGlobal('open', vi.fn())

    render(
      <StrictMode>
        <MCPServers />
      </StrictMode>,
    )
    await screen.findByText('Docs')
    fireEvent.click(screen.getByRole('button', { name: /Connect/ }))
    await screen.findByText('https://as.example.test/authorize')

    const before = gets()
    push({
      type: 'notification',
      payload: {
        type: 'mcp_connection',
        payload: { id: 2, name: 'Docs', connected: true, detail: 'Its tools are now available to grant.' },
      },
    })

    // Said out loud, the dialog put away, and the list re-read: the person is
    // not left to work out whether it took.
    expect(await screen.findByText('Its tools are now available to grant.')).toBeInTheDocument()
    await waitFor(() =>
      expect(screen.queryByText('https://as.example.test/authorize')).not.toBeInTheDocument(),
    )
    await waitFor(() => expect(gets()).toBeGreaterThan(before))
  })

  it('says so when the remote refused, in the words the remote used', async () => {
    serve(
      () =>
        new Response(JSON.stringify({ authorize_url: 'https://as.example.test/authorize' }), {
          status: 200,
        }),
    )
    vi.stubGlobal('open', vi.fn())
    render(
      <StrictMode>
        <MCPServers />
      </StrictMode>,
    )
    await screen.findByText('Docs')
    fireEvent.click(screen.getByRole('button', { name: /Connect/ }))
    await screen.findByText('https://as.example.test/authorize')

    push({
      type: 'notification',
      payload: {
        type: 'mcp_connection',
        payload: { id: 2, name: 'Docs', connected: false, detail: 'the user is not eligible for MCP access' },
      },
    })
    expect(await screen.findByText('the user is not eligible for MCP access')).toBeInTheDocument()
  })
})

describe('reading a completion off the socket', () => {
  it('takes the one it is for', () => {
    const outcome = connectionOutcome({
      type: 'notification',
      payload: { type: 'mcp_connection', payload: { id: 4, connected: true, detail: 'done' } },
    })
    expect(outcome).toEqual({ id: 4, connected: true, detail: 'done' })
  })

  // The socket carries everything the server pushes. A screen that acted on the
  // wrong message would announce a connection every time a delegation finished.
  it('ignores every other message on the same wire', () => {
    for (const message of [
      { type: 'topic', topic: 'dashboard', payload: { connected: 2 } },
      { type: 'notification', payload: { type: 'delegation', payload: { id: 9 } } },
      { type: 'notification' },
      { type: 'notification', payload: { type: 'mcp_connection' } },
      { type: 'ack' },
    ]) {
      expect(connectionOutcome(message), JSON.stringify(message)).toBeNull()
    }
  })
})
