import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, within } from '@/test-utils'
import { Vendors } from '@/pages/Vendors'

// The vendor form offers a kind once (a second DeepSeek account is nonsense),
// except the local kind, which is a different self-hosted server each time. And
// it asks for an endpoint only for a vendor defined by WHERE it runs.

const VENDORS = [
  { id: 1, vendor_key: 'anthropic', name: 'Anthropic', has_credentials: true, status: 'active' },
  { id: 2, vendor_key: 'openai-compatible', name: 'Local Runtime', has_credentials: false, status: 'active' },
]

const KINDS = [
  { key: 'anthropic', name: 'Anthropic', requires_base_url: false },
  { key: 'openai', name: 'OpenAI', requires_base_url: false },
  { key: 'openai-compatible', name: 'Ollama', requires_base_url: true },
  { key: 'azure-openai', name: 'Azure OpenAI', requires_base_url: true },
]

function serve() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.includes('/vendor-kinds')) return new Response(JSON.stringify(KINDS), { status: 200 })
      // The vendors screen is ONE answer: the vendors configured, and the kinds
      // one can be. The catalogue is a constant the screen cannot draw its "add"
      // form without, so it was a second request that could never differ.
      if (url.includes('/vendors')) {
        return new Response(JSON.stringify({ vendors: VENDORS, kinds: KINDS }), { status: 200 })
      }
      return new Response('[]', { status: 200 })
    }),
  )
}

async function openForm() {
  render(
    <StrictMode>
      <Vendors />
    </StrictMode>,
  )
  await screen.findByText('Local Runtime')
  fireEvent.click(screen.getByRole('button', { name: /new vendor/i }))
  return screen.findByRole('dialog')
}

function vendorSelect(dialog: HTMLElement): HTMLSelectElement {
  const select = within(dialog)
    .getAllByRole('combobox')
    .find((box) => [...box.querySelectorAll('option')].some((o) => o.textContent === 'Ollama'))
  if (!select) throw new Error('vendor select not found')
  return select as HTMLSelectElement
}

describe('the vendor list', () => {
  afterEach(() => vi.unstubAllGlobals())

  // A vendor is the name somebody gave it. The kind we dispatch on
  // (`anthropic`, `openai-compatible`) sat under every row: ours, fixed at
  // creation, and for most vendors the same word the name above it already
  // said. It belongs on the form, where it is a choice being made.
  it('shows a vendor by its name, not by the kind it is', async () => {
    serve()
    render(
      <StrictMode>
        <Vendors />
      </StrictMode>,
    )

    expect(await screen.findByText('Anthropic')).toBeInTheDocument()
    expect(screen.getByText('Local Runtime')).toBeInTheDocument()
    expect(screen.queryByText('anthropic')).not.toBeInTheDocument()
    expect(screen.queryByText('openai-compatible')).not.toBeInTheDocument()
  })
})

describe('the vendor form', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('does not offer an already-configured kind, but keeps local repeatable', async () => {
    serve()
    const dialog = await openForm()

    // Anthropic already exists, so it is not offered again.
    expect(within(dialog).queryByRole('option', { name: 'Anthropic' })).not.toBeInTheDocument()
    // OpenAI is not configured, so it is offered.
    expect(within(dialog).getByRole('option', { name: 'OpenAI' })).toBeInTheDocument()
    // Local exists too, but a second self-hosted server is a real thing.
    expect(within(dialog).getByRole('option', { name: 'Ollama' })).toBeInTheDocument()
  })

  it('asks for an endpoint only for a vendor defined by where it runs', async () => {
    serve()
    const dialog = await openForm()

    // The default pick is a fixed-API vendor, which has no endpoint to set.
    expect(within(dialog).queryByText('Endpoint')).not.toBeInTheDocument()

    // Choosing the local kind reveals the endpoint field.
    fireEvent.change(vendorSelect(dialog), { target: { value: 'openai-compatible' } })
    expect(within(dialog).getByText('Endpoint')).toBeInTheDocument()

    // Switching back to a fixed-API vendor hides it again.
    fireEvent.change(vendorSelect(dialog), { target: { value: 'openai' } })
    expect(within(dialog).queryByText('Endpoint')).not.toBeInTheDocument()
  })
})
