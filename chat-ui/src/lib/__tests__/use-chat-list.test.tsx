import type { ReactNode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, renderHook, screen, waitFor } from '@testing-library/react'
import { NotifyProvider } from '../notify'
import { useChatList } from '../use-chat-list'

/**
 * A write the sidebar makes is heard, whichever way it goes.
 *
 * Renaming, pinning and deleting a conversation all ran through one helper that
 * caught its own failure, wrote it to the console and refreshed the list
 * anyway. So a delete the server refused looked exactly like a delete that
 * worked, up until the row was still there with nothing to say why.
 */

const CHATS = [
  { id: 'c1', title: 'Invoices', last_message_at: null, is_pinned: false, message_count: 2 },
]

function serve(write: (url: string) => Response, canDelete = true) {
  const calls: string[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if ((init?.method ?? 'GET') === 'GET') {
        return new Response(JSON.stringify({ chats: CHATS, can_delete: canDelete }), { status: 200 })
      }
      calls.push(url)
      return write(url)
    }),
  )
  return calls
}

function wrapper({ children }: { children: ReactNode }) {
  return <NotifyProvider>{children}</NotifyProvider>
}

function list() {
  return renderHook(() => useChatList('/v1/chat/chats'), { wrapper })
}

describe('what the sidebar says about its own writes', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('says a conversation was deleted, because a row going is not an answer', async () => {
    serve(() => new Response('{}', { status: 200 }))
    const { result } = list()

    await act(async () => {
      await result.current.deleteChat('c1')
    })

    expect(await screen.findByText('The conversation was deleted.')).toBeTruthy()
  })

  it('says so when the delete is refused, instead of writing it to a console', async () => {
    serve(() => new Response('{"error":"conflict"}', { status: 409 }))
    const { result } = list()

    await act(async () => {
      await result.current.deleteChat('c1')
    })

    expect(await screen.findByText('The conversation could not be deleted.')).toBeTruthy()
    // And it does NOT claim success as well: the two are exclusive.
    expect(screen.queryByText('The conversation was deleted.')).toBeNull()
  })

  it('reports a rename that did not land, and stays quiet about one that did', async () => {
    // Renaming succeeds visibly: the title on the row IS the confirmation, so a
    // toast repeating what somebody just watched happen would be noise. The
    // failure has nothing else to say it.
    serve(() => new Response('{}', { status: 200 }))
    const { result, unmount } = list()
    await act(async () => {
      await result.current.renameChat('c1', 'Paid invoices')
    })
    expect(screen.queryByRole('status')).toBeNull()
    unmount()

    vi.unstubAllGlobals()
    serve(() => new Response('{}', { status: 500 }))
    const second = list()
    await act(async () => {
      await second.result.current.renameChat('c1', 'Paid invoices')
    })
    expect(await screen.findByText('The conversation could not be renamed.')).toBeTruthy()
  })

  it('names the direction when pinning fails, since one button does both', async () => {
    serve(() => new Response('{}', { status: 500 }))
    const { result } = list()

    await act(async () => {
      await result.current.setPinned('c1', false)
    })

    expect(await screen.findByText('The conversation could not be unpinned.')).toBeTruthy()
  })
})

// The chat is mounted by a shell that may not have wrapped it, and a tree that
// crashes because nobody was listening for a toast is worse than a toast nobody
// hears.
describe('a chat mounted without the provider', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('still deletes, and says nothing rather than throwing', async () => {
    const calls = serve(() => new Response('{}', { status: 200 }))
    const { result } = renderHook(() => useChatList('/v1/chat/chats'))

    await act(async () => {
      await result.current.deleteChat('c1')
    })

    expect(calls).toEqual(['/v1/chat/chats/delete/c1'])
  })
})

// A toast leaves on its own, and waits while somebody is reading it.
describe('a toast', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('goes away by itself', async () => {
    serve(() => new Response('{}', { status: 200 }))
    const { result } = list()
    await act(async () => {
      await result.current.deleteChat('c1')
    })
    await screen.findByText('The conversation was deleted.')

    await waitFor(
      () => expect(screen.queryByText('The conversation was deleted.')).toBeNull(),
      { timeout: 6000 },
    )
  }, 10000)

  it('is dismissed by hand as well', async () => {
    serve(() => new Response('{}', { status: 500 }))
    const { result } = list()
    await act(async () => {
      await result.current.deleteChat('c1')
    })
    const shown = await screen.findByText('The conversation could not be deleted.')
    expect(shown).toBeTruthy()

    await act(async () => {
      screen.getByLabelText('Dismiss').click()
    })
    expect(screen.queryByText('The conversation could not be deleted.')).toBeNull()
  })
})

// The provider mounts once, above everything, so a screen below never renders a
// viewport of its own.
describe('the provider', () => {
  afterEach(cleanup)

  it('renders what it wraps', () => {
    render(
      <NotifyProvider>
        <p>the chat</p>
      </NotifyProvider>,
    )
    expect(screen.getByText('the chat')).toBeTruthy()
  })
})

/**
 * Whether deleting is offered at all is the server's answer, not a guess.
 *
 * `chats:delete` is a permission a role grants, and the route enforces it. The
 * sidebar reads the same answer so it never draws a bin that is going to come
 * back 403: an action that will be refused should not be offered, and offering
 * it for one paint and withdrawing it is the same mistake with worse timing.
 */
describe('whether the sidebar may offer deleting', () => {
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  it('starts closed, because a permission is not assumed while it is unknown', () => {
    serve(() => new Response('{}', { status: 200 }))
    const { result } = list()
    expect(result.current.canDelete).toBe(false)
  })

  it('opens when the list says the permission is held', async () => {
    serve(() => new Response('{}', { status: 200 }), true)
    const { result } = list()
    await waitFor(() => expect(result.current.canDelete).toBe(true))
  })

  it('stays closed when it is not, with the conversations still listed', async () => {
    // Not being allowed to delete is not being locked out: the conversations
    // are still theirs to read, rename and pin.
    serve(() => new Response('{}', { status: 200 }), false)
    const { result } = list()
    await waitFor(() => expect(result.current.chats).toHaveLength(1))
    expect(result.current.canDelete).toBe(false)
  })

  it('closes again the moment the answer changes, without a reload', async () => {
    // The permission is re-read live on every list, so a role edited while
    // somebody is sitting here takes effect on their next refresh.
    let allowed = true
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        new Response(JSON.stringify({ chats: CHATS, can_delete: allowed }), { status: 200 }),
      ),
    )
    const { result } = list()
    await waitFor(() => expect(result.current.canDelete).toBe(true))

    allowed = false
    await act(async () => {
      await result.current.refresh()
    })
    expect(result.current.canDelete).toBe(false)
  })
})
