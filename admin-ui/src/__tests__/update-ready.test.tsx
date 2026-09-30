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
function shell(running = '0.1.0') {
  const listeners = new Map<string, Handler>()
  ;(window as unknown as { __TAURI__: unknown }).__TAURI__ = {
    app: { getVersion: () => Promise.resolve(running) },
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

  it('shows nothing in a browser even when its storage holds a version', () => {
    // The test above cannot tell whether the shell is what is being checked,
    // because there is nothing in storage for it to find either way. This one
    // puts a version there first, so the only thing that can keep the chip away
    // is asking whether there is an application at all.
    //
    // It matters because the console is ONE build served in two places: inside
    // the application, where an update really can be waiting, and in a browser
    // against a server, where nothing installs anything and a chip telling
    // somebody to reopen would be advice about a thing that does not exist.
    window.localStorage.setItem('sag.update-ready', '0.9.9')
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

  it('forgets the saved version once it is the one running', async () => {
    // 0.1.19 on 25 September: the old process saved the notice, the application
    // was reopened on the same port and so the same storage, and the new one
    // read it back and said to reopen into the version it already was. A test
    // with an empty store stood in for "a fresh launch" and could not fail.
    window.localStorage.setItem('sag.update-ready', '0.1.19')
    shell('0.1.19')
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    await waitFor(() => expect(window.localStorage.getItem('sag.update-ready')).toBeNull())
    expect(screen.queryByText(/installed/i)).not.toBeInTheDocument()
  })

  it('reads versions as numbers, so 0.1.10 is later than 0.1.9', async () => {
    // As text, "0.1.10" sorts before "0.1.9", and a waiting update would be
    // thrown away on the next reload.
    window.localStorage.setItem('sag.update-ready', '0.1.10')
    shell('0.1.9')
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    expect(await screen.findByText(/Version 0\.1\.10 is installed/)).toBeInTheDocument()
  })
})
