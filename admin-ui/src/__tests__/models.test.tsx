import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@/test-utils'
import { Models } from '@/pages/Models'

// A model id is typed by a person, and a person mistypes. The vendor is the only
// authority on what it offers, so the form asks it and offers the answer. These
// tests pin the two halves of that: the picker when the vendor answers, and the
// text box when it will not, because an empty menu would be a lie about a vendor
// that never said it had nothing.

const VENDORS = [
  { id: 1, vendor_key: 'anthropic', name: 'Anthropic', status: 'active', has_credentials: true },
  { id: 2, vendor_key: 'azure-openai', name: 'Azure', status: 'active', has_credentials: true },
]

const CATALOG: Record<number, { listed: boolean; models: { id: string; name?: string }[] }> = {
  // Anthropic says what it offers, and names it.
  1: {
    listed: true,
    models: [
      { id: 'claude-opus-4-8', name: 'Claude Opus 4.8' },
      { id: 'claude-sonnet-5', name: 'Claude Sonnet 5' },
    ],
  },
  // Azure names deployments somebody created, not models it publishes, so it
  // cannot be asked. That is not the same as offering nothing.
  2: { listed: false, models: [] },
}

// One of each: a model from a vendor account, and one running on a machine of
// ours. The second points at a machine, and a machine pointer is deliberately
// kept OUT of the vendor list, so anything that renders it as a vendor renders
// it as the wrong one.
const MODELS = [
  {
    id: 10,
    vendor_id: 1,
    model_key: 'claude-opus-4-8',
    kind: 'chat',
    status: 'active',
    context_window: 200000,
    notes: '',
  },
  {
    id: 11,
    vendor_id: 99,
    machine: 'RunPod',
    model_key: 'Qwen3.8-27B',
    kind: 'chat',
    status: 'active',
    context_window: 262144,
    notes: '',
  },
]

function serve() {
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    const catalog = /\/v1\/vendors\/(\d+)\/catalog$/.exec(url)
    if (catalog) {
      return new Response(JSON.stringify(CATALOG[Number(catalog[1])]), { status: 200 })
    }
    if (url.includes('/v1/vendors')) {
      return new Response(JSON.stringify(VENDORS), { status: 200 })
    }
    // The models screen is ONE answer: the models and the vendors they belong
    // to. The page used to fetch the two separately, which is the request per
    // component this pattern replaces.
    if (url.includes('/v1/models')) {
      return new Response(JSON.stringify({ models: MODELS, vendors: VENDORS }), { status: 200 })
    }
    return new Response(JSON.stringify([]), { status: 200 })
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

describe('the model form', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('offers what the vendor says it offers, instead of asking someone to type it', async () => {
    serve()

    render(
      <StrictMode>
        <Models />
      </StrictMode>,
    )

    fireEvent.click(await screen.findByRole('button', { name: /add cloud model/i }))

    // The vendor was asked, and its answer became the menu. Each option READS as
    // the friendly name; the value it carries is the id the gateway will send.
    const opus = await screen.findByRole('option', { name: 'Claude Opus 4.8' })
    expect(opus).toHaveAttribute('value', 'claude-opus-4-8')
    expect(screen.getByRole('option', { name: 'Claude Sonnet 5' })).toHaveAttribute(
      'value',
      'claude-sonnet-5',
    )
    expect(screen.getByText('Offered by this vendor right now.')).toBeInTheDocument()

    // Nothing to type: the box that invited a typo is gone.
    expect(screen.queryByPlaceholderText('claude-sonnet-5')).not.toBeInTheDocument()
  })

  it('falls back to a text box for a vendor that will not say what it offers', async () => {
    serve()

    render(
      <StrictMode>
        <Models />
      </StrictMode>,
    )

    fireEvent.click(await screen.findByRole('button', { name: /add cloud model/i }))
    await screen.findByRole('option', { name: 'Claude Opus 4.8' })

    // Switch to the vendor that publishes no catalog. The Vendor select is the
    // one holding the vendor ids.
    const vendorPicker = screen
      .getAllByRole('combobox')
      .find((box) => [...box.querySelectorAll('option')].some((o) => o.textContent === 'Azure'))
    expect(vendorPicker).toBeDefined()
    fireEvent.change(vendorPicker!, { target: { value: '2' } })

    // The field becomes a text box: the person is trusted, because nothing is in
    // a position to contradict them. An empty menu would strand them instead,
    // and would say the vendor has no models, which it never said.
    await waitFor(() =>
      expect(screen.getByPlaceholderText('claude-sonnet-5')).toBeInTheDocument(),
    )
    expect(
      screen.queryByRole('option', { name: 'Claude Opus 4.8' }),
    ).not.toBeInTheDocument()
    expect(
      screen.getByText('This vendor does not publish a list, so the name is not checked here.'),
    ).toBeInTheDocument()
  })

  it('keeps a free note on a model, with no capability toggles', async () => {
    serve()

    render(
      <StrictMode>
        <Models />
      </StrictMode>,
    )

    fireEvent.click(await screen.findByRole('button', { name: /add cloud model/i }))
    await screen.findByRole('dialog')

    // Whether a model can call tools or reason is the agent's setting now, so the
    // model form no longer toggles capabilities.
    expect(screen.queryByText('Call tools')).not.toBeInTheDocument()
    expect(screen.queryByText('Reason before answering')).not.toBeInTheDocument()
    expect(screen.queryByText('The data stays local')).not.toBeInTheDocument()

    // It keeps a note instead.
    expect(screen.getByText('Notes')).toBeInTheDocument()
  })

  // A model on one of our machines has no vendor account behind it: it points at
  // the machine, and machine pointers are kept out of the vendor list on
  // purpose. The select could therefore not find its own value and fell back to
  // rendering the first option, so a Qwen model on RunPod was shown, in a
  // disabled field, as DeepSeek.
  it('does not offer a vendor for a model that runs on one of our machines', async () => {
    serve()

    render(
      <StrictMode>
        <Models />
      </StrictMode>,
    )

    // A row is opened by its Edit button, not by its text.
    const openRowFor = async (modelKey: string) => {
      const cell = await screen.findByText(modelKey)
      const row = cell.closest('tr')!
      fireEvent.click(within(row).getByRole('button', { name: /edit/i }))
      return within(await screen.findByRole('dialog'))
    }

    // The cloud one still has a vendor.
    const cloud = await openRowFor('claude-opus-4-8')
    expect(cloud.getByText('Vendor')).toBeInTheDocument()
    fireEvent.click(cloud.getByRole('button', { name: /cancel/i }))

    // The local one does not: it points at a MACHINE, and machine pointers are
    // kept out of the vendor list, so the select could not find its own value
    // and fell back to rendering the first option.
    const local = await openRowFor('Qwen3.8-27B')
    expect(local.queryByText('Vendor')).not.toBeInTheDocument()
    expect(local.queryByText('DeepSeek')).not.toBeInTheDocument()
  })
})
