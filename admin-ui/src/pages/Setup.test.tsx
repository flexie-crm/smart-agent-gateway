import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import { Setup } from './Setup'
import type { SetupState } from '@/lib/resources'
import { api } from '@/lib/resources'
import { NotifyProvider } from '@/lib/notify'

/**
 * The first screen anybody sees, and the one that has broken most.
 *
 * Every case here is one that actually happened on a real machine and was found
 * by a person clicking rather than by a test: a button that turned forever, a
 * model created with nothing pointing at it, a step that could not finish and
 * did not say so. They are tests now because the cost of finding them that way
 * is measured in rebuilds.
 */

const nothingSetUp: SetupState = {
  ready: false,
  has_name: true,
  has_vendor: false,
  has_model: false,
}
const vendorAdded: SetupState = { ...nothingSetUp, has_vendor: true }

function renderSetup(state: SetupState, onDone = vi.fn()) {
  render(
    <NotifyProvider>
      <Setup state={state} onDone={onDone} />
    </NotifyProvider>,
  )
  return onDone
}

beforeEach(() => {
  vi.restoreAllMocks()
})

describe('the provider step', () => {
  it('offers the kinds the gateway supports and adds the one chosen', async () => {
    vi.spyOn(api.vendors, 'list').mockResolvedValue({
      vendors: [],
      kinds: [
        { key: 'anthropic', name: 'Anthropic' },
        { key: 'openai', name: 'OpenAI' },
      ],
    } as never)
    const create = vi.spyOn(api.vendors, 'create').mockResolvedValue({ id: 1 } as never)
    const onDone = renderSetup(nothingSetUp)

    await screen.findByRole('option', { name: 'Anthropic' })
    await userEvent.type(screen.getByPlaceholderText('API key'), 'sk-test-key')
    await userEvent.click(screen.getByRole('button', { name: 'Add provider' }))

    await waitFor(() => expect(create).toHaveBeenCalledOnce())
    expect(create.mock.calls[0][0]).toMatchObject({
      vendor_key: 'anthropic',
      credentials: 'sk-test-key',
    })
    expect(onDone).toHaveBeenCalled()
  })
})

describe('the model step', () => {
  const oneVendor = { vendors: [{ id: 7, name: 'Anthropic' }], models: [] }

  it('lists what the provider publishes rather than asking anybody to type it', async () => {
    vi.spyOn(api.models, 'list').mockResolvedValue(oneVendor as never)
    vi.spyOn(api.agents, 'screen').mockResolvedValue({ gateway: { id: 3 }, agents: [] } as never)
    vi.spyOn(api.vendors, 'catalog').mockResolvedValue({
      listed: true,
      models: [{ id: 'claude-opus-4-8' }, { id: 'claude-sonnet-5' }],
    } as never)

    renderSetup(vendorAdded)
    expect(await screen.findByRole('option', { name: 'claude-opus-4-8' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'claude-sonnet-5' })).toBeInTheDocument()
    // A provider that publishes a list must never leave a text box beside it.
    expect(screen.queryByPlaceholderText(/Model name/)).not.toBeInTheDocument()
  })

  it('falls back to typing when the provider publishes nothing', async () => {
    vi.spyOn(api.models, 'list').mockResolvedValue(oneVendor as never)
    vi.spyOn(api.agents, 'screen').mockResolvedValue({ gateway: { id: 3 }, agents: [] } as never)
    // A provider that cannot list, or a key that cannot ask.
    vi.spyOn(api.vendors, 'catalog').mockRejectedValue(new Error('no catalogue'))

    renderSetup(vendorAdded)
    // An empty menu with no way past it would be a dead end.
    expect(await screen.findByPlaceholderText(/Model name/)).toBeInTheDocument()
  })

  it('points the Gateway at the model it just created', async () => {
    vi.spyOn(api.models, 'list').mockResolvedValue(oneVendor as never)
    vi.spyOn(api.agents, 'screen').mockResolvedValue({ gateway: { id: 3 }, agents: [] } as never)
    vi.spyOn(api.vendors, 'catalog').mockResolvedValue({
      listed: true,
      models: [{ id: 'claude-opus-4-8' }],
    } as never)
    const create = vi.spyOn(api.models, 'create').mockResolvedValue({ id: 42 } as never)
    vi.spyOn(api.agents, 'form').mockResolvedValue({
      agent: { id: 3, key: 'default', name: 'Gateway', status: 'active' },
      models: [],
      tools: [],
      brains: [],
    } as never)
    const update = vi.spyOn(api.agents, 'update').mockResolvedValue({} as never)
    const onDone = renderSetup(vendorAdded)

    await screen.findByRole('option', { name: 'claude-opus-4-8' })
    await userEvent.click(screen.getByRole('button', { name: 'Add model' }))

    await waitFor(() => expect(create).toHaveBeenCalledOnce())
    // Creating it is only half the step. Without this the model exists, nothing
    // uses it, and the screen never advances.
    //
    // The WHOLE agent, because this endpoint replaces rather than patches. An
    // earlier version of this test asserted `{ model_id: 42 }` alone and passed
    // while the real call returned 400 "a name is required": mocking an
    // endpoint tests the caller against an assumption about it, and the
    // assumption was wrong.
    await waitFor(() =>
      expect(update).toHaveBeenCalledWith(3, expect.objectContaining({ name: 'Gateway', model_id: 42 })),
    )
    expect(onDone).toHaveBeenCalled()
  })

  it('says so instead of turning forever when there is no Gateway', async () => {
    vi.spyOn(api.models, 'list').mockResolvedValue(oneVendor as never)
    // Exactly what a fresh installation used to look like: no Gateway at all.
    vi.spyOn(api.agents, 'screen').mockResolvedValue({ gateway: null, agents: [] } as never)
    vi.spyOn(api.vendors, 'catalog').mockResolvedValue({
      listed: true,
      models: [{ id: 'claude-opus-4-8' }],
    } as never)
    vi.spyOn(api.models, 'create').mockResolvedValue({ id: 42 } as never)
    const update = vi.spyOn(api.agents, 'update')

    renderSetup(vendorAdded)
    await screen.findByRole('option', { name: 'claude-opus-4-8' })
    const button = screen.getByRole('button', { name: 'Add model' })
    await userEvent.click(button)

    expect(update).not.toHaveBeenCalled()
    // The button MUST come back. It stayed disabled and spinning, which is the
    // failure that reads as "the application is broken" rather than "that did
    // not work".
    await waitFor(() => expect(button).not.toBeDisabled())
  })

  it('comes back when the model cannot be created', async () => {
    vi.spyOn(api.models, 'list').mockResolvedValue(oneVendor as never)
    vi.spyOn(api.agents, 'screen').mockResolvedValue({ gateway: { id: 3 }, agents: [] } as never)
    vi.spyOn(api.vendors, 'catalog').mockResolvedValue({
      listed: true,
      models: [{ id: 'claude-opus-4-8' }],
    } as never)
    vi.spyOn(api.models, 'create').mockRejectedValue(new Error('that key was refused'))

    renderSetup(vendorAdded)
    await screen.findByRole('option', { name: 'claude-opus-4-8' })
    const button = screen.getByRole('button', { name: 'Add model' })
    await userEvent.click(button)

    await waitFor(() => expect(button).not.toBeDisabled())
  })
})
