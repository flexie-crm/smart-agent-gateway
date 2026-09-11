import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@/test-utils'
import { DataTable } from '@/components/DataTable'
import { ApiError } from '@/lib/api'

/**
 * Deleting says three things, and it cannot say fewer.
 *
 * A row disappearing is not a confirmation. It looks exactly like a row
 * disappearing for some other reason, and on a screen where a delete can be
 * REFUSED (a vendor still holding models, a group somebody stands in), silence
 * is the ambiguous answer: the person is left to work out from an unchanged
 * table whether anything happened.
 *
 * So the question, the work and the sentence afterwards are one prop. They used
 * to be separate, `confirm` optional and nothing saying it had happened at all,
 * which is how nine screens came to delete things in silence. Together, there
 * is no way to add a delete to a table without saying what is being risked and
 * what was done.
 */

interface Thing {
  id: number
  name: string
}

const THINGS: Thing[] = [{ id: 1, name: 'Anthropic' }]

function show(run: (item: Thing) => void | Promise<void>) {
  render(
    <StrictMode>
      <DataTable
        items={THINGS}
        empty="nothing"
        columns={[{ header: 'Name', cell: (t: Thing) => t.name }]}
        remove={{
          run,
          confirm: (t) => `Delete "${t.name}"? Its models go with it.`,
          done: (t) => `${t.name} was deleted.`,
        }}
      />
    </StrictMode>,
  )
}

async function pressDelete() {
  fireEvent.click(screen.getByLabelText('Delete'))
  // The question is asked first, and names what will be lost.
  expect(await screen.findByText(/Delete "Anthropic"\? Its models go with it\./)).toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
}

describe('deleting from a table', () => {
  afterEach(() => vi.restoreAllMocks())

  it('says what was deleted, rather than leaving a row to vanish', async () => {
    show(vi.fn())
    await pressDelete()

    expect(await screen.findByText('Anthropic was deleted.')).toBeInTheDocument()
  })

  it('says nothing of the sort when the gateway refuses', async () => {
    show(() => {
      // As the gateway refuses it: the envelope, with its sentence about the
      // whole request. That sentence is what the person has to read.
      throw new ApiError(409, 'conflict', 'it is still in use by two agents', {})
    })
    await pressDelete()

    // The refusal is what is heard, and the success sentence is NOT: a screen
    // that congratulates you on a delete the server rejected is worse than one
    // that says nothing.
    expect(await screen.findByText(/still in use by two agents/)).toBeInTheDocument()
    expect(screen.queryByText('Anthropic was deleted.')).not.toBeInTheDocument()
  })

  it('does nothing at all when the question is answered no', async () => {
    const run = vi.fn()
    show(run)

    fireEvent.click(screen.getByLabelText('Delete'))
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }))

    await waitFor(() => expect(screen.queryByText(/Its models go with it/)).not.toBeInTheDocument())
    expect(run).not.toHaveBeenCalled()
    expect(screen.queryByText('Anthropic was deleted.')).not.toBeInTheDocument()
  })
})
