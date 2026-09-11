import { StrictMode } from 'react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { UpdateReady } from '../UpdateReady'

// The chat carries the same chip as the console and had no test for it at all,
// which is how it reached a release drawn in a colour nobody could see: the
// console's suite proved the text and the wiring, and neither is what went
// wrong. These assert the behaviours that fail silently.
//
// No jest-dom: this project's vitest has no setup file, so the assertions are
// on what render() returns rather than on custom matchers.

type Handler = (event: { payload?: { version?: string } }) => void

/**
 * A shell that records what was listened for, and can send an event.
 *
 * The last handler is kept even after its subscription is dropped, which is not
 * laziness: StrictMode mounts, unmounts and remounts, so a map keyed by event
 * name can be emptied by the first mount's cleanup in between a test asserting
 * that something is listening and that test firing the event. Keeping the
 * handler makes the test measure the component rather than that race.
 */
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
    fire: (version?: string) => {
      if (!last) throw new Error('nothing was listening')
      last({ payload: version === undefined ? {} : { version } })
    },
  }
}

describe('the update chip in the chat', () => {
  beforeEach(() => window.localStorage.clear())
  // Explicit, because this project's vitest runs without `globals`, so
  // Testing Library cannot register its own afterEach and every render
  // stays in the document: the next test then finds two chips and fails
  // for a reason that has nothing to do with the chip.
  afterEach(cleanup)

  it('names the version and puts the instruction on its own line', async () => {
    const tauri = shell()
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    await waitFor(() => expect(tauri.heard('sag://update-ready')).toBe(true))
    tauri.fire('0.1.5')

    expect(await screen.findByText(/Version 0\.1\.5 is installed/)).toBeTruthy()
    // A second line rather than a trailing sentence: the element carries
    // `block`, which is what makes it one. Asserting the text alone passes just
    // as well when both halves run together on one line.
    expect(screen.getByText(/Reopen SAG to use it/).className).toContain('block')
  })

  it('is still there after a reload, because the update still is', async () => {
    const tauri = shell()
    const { unmount } = render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    await waitFor(() => expect(tauri.heard('sag://update-ready')).toBe(true))
    tauri.fire('0.1.5')
    expect(await screen.findByText(/Version 0\.1\.5 is installed/)).toBeTruthy()

    unmount()
    shell()
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    expect(await screen.findByText(/Version 0\.1\.5 is installed/)).toBeTruthy()
  })

  it('says nothing on a fresh launch, which is a new origin with an empty store', () => {
    // The gateway takes a free port every launch (gateway.rs:513), so a
    // reopened application cannot see what the previous one wrote. That is what
    // clears the chip, rather than any code here.
    shell()
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    expect(screen.queryByText(/installed/i)).toBeNull()
  })

  it('shows nothing in a browser, where there is no shell to hear', () => {
    delete (window as unknown as { __TAURI__?: unknown }).__TAURI__
    window.localStorage.setItem('sag.update-ready', '0.1.5')
    render(
      <StrictMode>
        <UpdateReady />
      </StrictMode>,
    )
    // Even with something remembered: a web chat is not an installation that
    // could have been updated, and must never claim to be one.
    expect(screen.queryByText(/installed/i)).toBeNull()
  })
})
