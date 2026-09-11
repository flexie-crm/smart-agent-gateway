import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@/test-utils'
import { MCPServer } from '@/pages/MCPServer'
import { OAuthClients } from '@/pages/OAuthClients'

// Our MCP server's two screens: the exposure config (unconfigured means the
// whole catalog, and saving takes control) and the OAuth clients with their
// shown-once service token.

const TOOLS = [
  { id: 1, name: 'current_time', kind: 'builtin', friendly_name: 'Current time', description: '', risk: 'read_only', requires_approval: false, approval_locked: false, status: 'active', grants: [] },
  { id: 2, name: 'set_model_status', kind: 'builtin', friendly_name: 'Model switch', description: '', risk: 'admin_action', requires_approval: true, approval_locked: true, status: 'active', grants: [] },
]

describe('the MCP Server screen', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the whole catalog checked when unconfigured, and saving takes control', async () => {
    const writes: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        const method = init?.method ?? 'GET'
        if (method === 'PUT') {
          writes.push(String(init?.body))
          return new Response(
            JSON.stringify({ configured: true, tool_config: {}, brain_config: [] }),
            { status: 200 },
          )
        }
        // The screen is ONE answer, and it says whether each box is ticked
        // rather than handing over the catalogue and the config for the client
        // to join. Unconfigured means everything is exposed, so the server
        // resolves that here and the checkboxes arrive already true.
        if (url.includes('/v1/mcp-server')) {
          return new Response(
            JSON.stringify({
              configured: false,
              tools: TOOLS.map((t) => ({ ...t, exposed: true })),
              brains: [],
            }),
            { status: 200 },
          )
        }
        throw new Error(`unexpected request: ${method} ${url}`)
      }),
    )

    render(
      <StrictMode>
        <MCPServer />
      </StrictMode>,
    )

    // Unconfigured: it says so, and everything is checked.
    expect(await screen.findByText(/Nothing is configured yet/)).toBeInTheDocument()
    await waitFor(() =>
      expect(screen.getByRole('checkbox', { name: /Current time/ })).toHaveAttribute(
        'data-state',
        'checked',
      ),
    )

    // Uncheck the dangerous one and save: the write says exactly that.
    fireEvent.click(screen.getByRole('checkbox', { name: /Model switch/ }))
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(writes).toHaveLength(1))
    const body = JSON.parse(writes[0]) as {
      tool_config: Record<string, { enabled: boolean }>
    }
    expect(body.tool_config.current_time.enabled).toBe(true)
    expect(body.tool_config.set_model_status.enabled).toBe(false)
  })
})

describe('the OAuth clients screen', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('mints a service token and shows it exactly once', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        if ((init?.method ?? 'GET') === 'GET') return new Response('[]', { status: 200 })
        return new Response(
          JSON.stringify({
            id: 9,
            client_id: 'sag_ci_x',
            name: 'Bot',
            client_type: 'service',
            redirect_uris: [],
            grant_types: [],
            scopes: ['mcp'],
            is_dcr: false,
            status: 'active',
            service_token: 'sag_at_shown-exactly-once',
          }),
          { status: 201 },
        )
      }),
    )

    render(
      <StrictMode>
        <OAuthClients />
      </StrictMode>,
    )
    fireEvent.click(await screen.findByRole('button', { name: /New OAuth Client/ }))
    await screen.findByRole('dialog')

    fireEvent.change(screen.getByPlaceholderText('e.g. Acme Integration'), {
      target: { value: 'Bot' },
    })
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'service' } })
    // Service means no redirect addresses to fill.
    expect(screen.queryByText('Redirect URIs')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    // The token appears once, with the warning that it never will again.
    expect(await screen.findByDisplayValue('sag_at_shown-exactly-once')).toBeInTheDocument()
    expect(screen.getByText(/can never be shown again/)).toBeInTheDocument()
  })
})
