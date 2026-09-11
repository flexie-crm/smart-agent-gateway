import { StrictMode } from 'react'
import { beforeEach, describe, expect, it } from 'vitest'
import { render, screen, waitFor } from '@/test-utils'
import { UpdateReady } from '@/components/UpdateReady'

// A chip that stops appearing fails silently: nothing errors, nobody is told a
// version is waiting, and the installation quietly stays a release behind while
// the update mechanism underneath it works perfectly. So the two things that
// could break it are asserted: that it listens for the name the shell actually
// sends, and that it shows nothing until it hears it.

type Handler = (event: { payload?: { version?: string } }) => void

/** A shell that records what was listened for, and can send an event. */
function shell() {
  const listeners = new Map<string, Handler>()
  ;(window as unknown as { __TAURI__: unknown }).__TAURI__ = {
    event: {
      listen: (name: string, handler: Handler) => {
        listeners.set(name, handler)
        return Promise.resolve(() => listeners.delete(name))
      },
    },
  }
  return listeners
}

describe('the update chip', () => {
  beforeEach(() => window.localStorage.clear())

  it('says nothing until there is something installed', () => {
    shell()
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    expect(screen.queryByText(/installed/i)).not.toBeInTheDocument()
  })

  it('names the version and says what to do about it', async () => {
    const listeners = shell()
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )

    // The event name is a contract with desktop/shared/src/update.rs. Mishearing
    // it is a banner that never appears and an error nobody sees, which is why
    // it is asserted rather than assumed.
    await waitFor(() => expect(listeners.has('sag://update-ready')).toBe(true))
    listeners.get('sag://update-ready')!({ payload: { version: '0.1.1' } })

    expect(await screen.findByText(/Version 0\.1\.1 is installed/)).toBeInTheDocument()
    expect(screen.getByText(/Reopen SAG to use it/)).toBeInTheDocument()
  })

  it('still says something useful when the version is not named', async () => {
    const listeners = shell()
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    await waitFor(() => expect(listeners.has('sag://update-ready')).toBe(true))
    listeners.get('sag://update-ready')!({ payload: {} })

    expect(await screen.findByText(/A new version is installed/)).toBeInTheDocument()
  })

  it('shows nothing in a browser, where there is no shell to hear', () => {
    delete (window as unknown as { __TAURI__?: unknown }).__TAURI__
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    expect(screen.queryByText(/installed/i)).not.toBeInTheDocument()
  })

  it('is still there after a reload, because the update still is', async () => {
    // The update stays pending until somebody reopens, so a refresh that
    // forgets it is a refresh that tells them nothing is waiting. The first
    // version of this chip lived in component state and did exactly that.
    const listeners = shell()
    const { unmount } = render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    await waitFor(() => expect(listeners.has('sag://update-ready')).toBe(true))
    listeners.get('sag://update-ready')!({ payload: { version: '0.1.4' } })
    expect(await screen.findByText(/Version 0\.1\.4 is installed/)).toBeInTheDocument()

    unmount()
    shell()
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    expect(await screen.findByText(/Version 0\.1\.4 is installed/)).toBeInTheDocument()
  })

  it('says nothing on a fresh launch, which is a new origin with an empty store', () => {
    // Not a detail of the test: the gateway takes a free port every launch
    // (gateway.rs:513), so a reopened application cannot see what the previous
    // one wrote. That is what clears the chip, rather than any code here.
    window.localStorage.clear()
    shell()
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    expect(screen.queryByText(/installed/i)).not.toBeInTheDocument()
  })
})
