import { describe, expect, it, vi, beforeEach } from 'vitest'
import type { Posture } from '@/lib/api'

/**
 * What the console offers depends on which edition it was built for, and the
 * navigation is where getting it wrong is most visible.
 *
 * The Chat item is the case that was reported: on a deployment the chat is SAG
 * Enterprise, an application on the person's own machine, so a menu item
 * pointing at a chat this server does not serve is a dead end dressed up as a
 * feature. On a personal installation the chat IS the product and the console
 * is the thing one click away from it.
 *
 * The module reads the posture at import time, so each case imports it fresh.
 */
async function navigationFor(personal: boolean, overrides: Partial<Posture> = {}) {
  vi.resetModules()
  vi.doMock('@/lib/api', async () => {
    const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
    const base = personal ? actual.PERSONAL_POSTURE : actual.SERVER_POSTURE
    return { ...actual, POSTURE: { ...base, ...overrides } }
  })
  const { NAVIGATION } = await import('@/components/AppShell')
  return NAVIGATION
}

const labels = (nav: Awaited<ReturnType<typeof navigationFor>>) =>
  nav.flatMap((group) => group.items.map((item) => item.label))

describe('what the console offers, per edition', () => {
  beforeEach(() => vi.resetModules())

  it('offers the chat on a personal installation, where it is served', async () => {
    const nav = await navigationFor(true)
    expect(labels(nav)).toContain('Chat')
    const chat = nav[0].items.find((i) => i.label === 'Chat')
    // A link, not a route: the two applications share no bundle.
    expect(chat?.external).toBe(true)
    expect(chat?.to).toBe('/chat/')
  })

  it('does NOT offer the chat on a deployment, where it is an application', async () => {
    expect(labels(await navigationFor(false))).not.toContain('Chat')
  })

  // Where models run, and the platform that runs none of them.
  //
  // The Windows personal edition ships no engine (KB/36), so there is no machine
  // and nothing for the screen to be about. What must not happen is an item that
  // opens a page reporting that local models are unavailable: that is a promise
  // the product cannot keep, dressed up as a feature.

  // ONE word, the same on both editions. It used to be two, branched on the
  // edition: "Local models" where there is one machine and "Inference Machines"
  // where there are several. The screen does the same job in both, so the nav
  // read differently for no reason a reader could see.
  it('offers inference on a personal installation', async () => {
    expect(labels(await navigationFor(true))).toContain('Inference')
  })

  it('offers the same word on a deployment, where machines join over the network', async () => {
    expect(labels(await navigationFor(false))).toContain('Inference')
  })

  it('offers it on neither, on an installation that carries no engine', async () => {
    const nav = await navigationFor(true, { local_models: false })
    expect(labels(nav)).not.toContain('Inference')
    // And nothing else went with it: the group is still the engine's, and the
    // items either side of the one removed are untouched.
    expect(labels(nav)).toContain('Models')
    expect(labels(nav)).toContain('Agents')
  })

  // A deployment always has machines to talk about, on every platform, because
  // they join it over the network rather than shipping inside it. Pinned because
  // the constant that hides this on Windows is compiled per platform, and a
  // server built there must not hide its own fleet.
  it('keeps the fleet on a deployment even where no engine ships', async () => {
    const { SERVER_POSTURE } = await import('@/lib/api')
    expect(SERVER_POSTURE.local_models).toBe(true)
  })

  // The group must survive losing its first item: an Overview heading with only
  // a Dashboard under it is right, an empty heading is not.
  it('keeps the dashboard either way', async () => {
    for (const personal of [true, false]) {
      const nav = await navigationFor(personal)
      expect(labels(nav), `personal=${personal}`).toContain('Dashboard')
      expect(nav[0].items.length, `personal=${personal}`).toBeGreaterThan(0)
    }
  })
})
