import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
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

  // It used to be hidden on a build that ships no engine, which took the one
  // kind of inference that works there away with it: a machine is a server with
  // a graphics card in it, and a personal installation adds one by exchanging
  // certificates with it. What ships no engine is the LOCAL node, which is
  // decided where it is started and has never been a question for this menu.
  it('offers it on an installation that carries no engine of its own', async () => {
    const nav = await navigationFor(true)
    expect(labels(nav)).toContain('Inference')
    expect(labels(nav)).toContain('Models')
    expect(labels(nav)).toContain('Agents')
  })

  // The posture says what KIND of installation this is. Whether models can run
  // on hardware we own is not one of those things any more, and a field that is
  // always true is a branch waiting to be written by mistake.
  it('has no capability for it left to branch on', async () => {
    const { SERVER_POSTURE, PERSONAL_POSTURE } = await import('@/lib/api')
    expect('local_models' in SERVER_POSTURE).toBe(false)
    expect('local_models' in PERSONAL_POSTURE).toBe(false)
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

/**
 * And the one sentence that does still depend on what the build carries.
 *
 * Not whether there is an Inference screen, which every installation has: a
 * machine is a server with a graphics card in it, somewhere else. Whether THIS
 * computer is also one of them, which it is only where an engine shipped inside
 * the application. Saying so on a build that carries none names a machine the
 * list underneath cannot contain, because the server does not send it
 * (app.Nodes, withoutAMachineThatCannotExistHere).
 *
 * The constant is mocked rather than its environment variable stubbed, which is
 * what the navigation cases above do with the posture and for the same reason:
 * both are read once when the module is first evaluated, so what has to be in
 * place is the module, not the variable it was built from.
 */
describe('what the Inference screen says it is about', () => {
  async function subtitle(engine: boolean) {
    vi.resetModules()
    vi.doMock('@/lib/api', async () => {
      const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
      return { ...actual, LOCAL_ENGINE: engine }
    })
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ nodes: [] }), { status: 200 })),
    )
    // Both from the SAME fresh graph: a provider from the old copy holds a
    // context the new page cannot read.
    const { Nodes } = await import('@/pages/Nodes')
    const { render: fresh, screen: on } = await import('@/test-utils')
    fresh(<Nodes />)
    const said = await on.findByText(/Models that run on hardware we own/)
    return said.textContent ?? ''
  }

  afterEach(() => vi.doUnmock('@/lib/api'))

  it('names this computer where an engine ships with the application', async () => {
    expect(await subtitle(true)).toContain('this computer')
  })

  it('does not, where none does', async () => {
    expect(await subtitle(false)).not.toContain('this computer')
  })
})
