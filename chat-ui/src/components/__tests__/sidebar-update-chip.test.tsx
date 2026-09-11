import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'

// The chip must reach BOTH editions, and the test environment is already the
// one that was broken: PERSONAL is `import.meta.env.VITE_SAG_PERSONAL === '1'`
// (lib/api.ts:87), which is false here exactly as it is in the web build a
// server hands to the enterprise application.
//
// It was rendered inside `if (PERSONAL)`, so it was compiled out of that build
// and an enterprise installation could never be told a new version had been
// installed. Nothing failed; the chip was simply not in the bundle.

type Handler = (event: { payload?: { version?: string } }) => void

function shell() {
  const live = new Set<string>()
  let last: Handler | undefined
  ;(window as unknown as { __TAURI__: unknown }).__TAURI__ = {
    event: {
      listen: (name: string, handler: Handler) => {
        live.add(name)
        last = handler
        return Promise.resolve(() => live.delete(name))
      },
    },
  }
  return {
    heard: (name: string) => live.has(name),
    fire: (version: string) => {
      if (!last) throw new Error('nothing was listening')
      last({ payload: { version } })
    },
  }
}

// The sidebar pulls in the whole chat list; none of that is the subject here.
vi.mock('@lib/api', async () => {
  const actual = await vi.importActual<Record<string, unknown>>('@lib/api')
  return { ...actual, PERSONAL: false }
})

const list = {
  chats: [],
  loading: false,
  error: null,
  query: '',
  setQuery: () => {},
  reload: () => {},
  hasMore: false,
  loadMore: () => {},
  rename: async () => {},
  togglePin: async () => {},
} as unknown as Parameters<typeof import('../ChatSidebar').ChatSidebar>[0]['list']

describe('the sidebar, on the edition whose pages come from a server', () => {
  beforeEach(() => window.localStorage.clear())
  afterEach(cleanup)

  it('still shows the update chip, because a desktop shell may be behind it', async () => {
    const { ChatSidebar } = await import('../ChatSidebar')
    const tauri = shell()
    render(
      <ChatSidebar
        open
        docked
        onClose={() => {}}
        list={list}
        currentChatId={null}
        onSelect={() => {}}
        onNewChat={() => {}}
        onDeleteChat={() => {}}
        account={{ name: 'Someone' }}
      />,
    )
    await waitFor(() => expect(tauri.heard('sag://update-ready')).toBe(true))
    tauri.fire('9.9.9')
    expect(await screen.findByText(/Version 9\.9\.9 is installed/)).toBeTruthy()
  })
})
