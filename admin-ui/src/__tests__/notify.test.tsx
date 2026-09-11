import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { NotifyProvider, useNotify } from '@/lib/notify'

// The console's own voice: toasts for outcomes, a confirm dialog for a question
// that must be answered before acting. What matters here is that a toast says
// its words and can be dismissed, an error announces itself, and the confirm
// hands back a plain yes or no.

function Harness() {
  const notify = useNotify()
  const [answer, setAnswer] = useState<string>('')
  return (
    <div>
      <button onClick={() => notify.success('Saved the vendor.', 'Done')}>raise success</button>
      <button onClick={() => notify.error('The gateway did not answer.')}>raise error</button>
      <button
        onClick={async () => {
          const ok = await notify.confirm({ title: 'Delete "Acme"?', confirmLabel: 'Delete', destructive: true })
          setAnswer(ok ? 'confirmed' : 'cancelled')
        }}
      >
        ask
      </button>
      <span>answer: {answer}</span>
    </div>
  )
}

function mount() {
  return render(
    <NotifyProvider>
      <Harness />
    </NotifyProvider>,
  )
}

describe('the console notifications', () => {
  it('shows a success toast with its title and message', async () => {
    mount()
    fireEvent.click(screen.getByText('raise success'))
    const toast = await screen.findByRole('status')
    expect(within(toast).getByText('Done')).toBeInTheDocument()
    expect(within(toast).getByText('Saved the vendor.')).toBeInTheDocument()
  })

  it('announces an error toast, and dismisses when asked', async () => {
    mount()
    fireEvent.click(screen.getByText('raise error'))
    const toast = await screen.findByRole('alert')
    expect(within(toast).getByText('The gateway did not answer.')).toBeInTheDocument()

    fireEvent.click(within(toast).getByLabelText('Dismiss'))
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument())
  })

  it('resolves the confirm as yes when the action is taken', async () => {
    mount()
    fireEvent.click(screen.getByText('ask'))
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete' }))
    await screen.findByText('answer: confirmed')
  })

  it('resolves the confirm as no when it is cancelled', async () => {
    mount()
    fireEvent.click(screen.getByText('ask'))
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await screen.findByText('answer: cancelled')
  })
})
