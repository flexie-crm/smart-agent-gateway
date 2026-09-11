import { describe, it, expect, vi } from 'vitest'
import { ReconnectingSocket, type SocketLike, type SocketOptions } from '../reconnecting-socket'

// A fake WebSocket the test drives by hand, and exactly as strict as the real
// one: send() THROWS unless the socket is OPEN, which is precisely the bug a
// looser fake once let through to production.
const CONNECTING = 0
const OPEN = 1
const CLOSED = 3

class FakeSocket implements SocketLike {
  sent: string[] = []
  readyState = CONNECTING
  onopen: (() => void) | null = null
  onmessage: ((event: { data: string }) => void) | null = null
  onclose: ((event: { code: number }) => void) | null = null
  onerror: (() => void) | null = null

  send(data: string): void {
    if (this.readyState !== OPEN) {
      throw new Error("InvalidStateError: send on a socket that is not OPEN")
    }
    this.sent.push(data)
  }
  close(code = 1000): void {
    this.fireClose(code)
  }
  // Test controls:
  open(): void {
    this.readyState = OPEN
    this.onopen?.()
  }
  message(obj: unknown): void {
    this.onmessage?.({ data: JSON.stringify(obj) })
  }
  fireClose(code: number): void {
    if (this.readyState === CLOSED) return
    this.readyState = CLOSED
    this.onclose?.({ code })
  }
  authFrame(): { type: string; token: string; source: string } {
    return JSON.parse(this.sent[0])
  }
  get closed(): boolean {
    return this.readyState === CLOSED
  }
}

interface Harness {
  client: ReconnectingSocket
  sockets: FakeSocket[]
  /** Run the single pending timer (the deferred open, or a reconnect). */
  runTimer: () => void
  pendingDelay: () => number
  refresh: ReturnType<typeof vi.fn>
  onExpired: ReturnType<typeof vi.fn>
  messages: unknown[]
  setToken: (t: string | null) => void
}

function harness(overrides: Partial<SocketOptions> = {}): Harness {
  const sockets: FakeSocket[] = []
  let token: string | null = 'tok-1'
  let timerFn: (() => void) | null = null
  let delay = 0
  const refresh = vi.fn(async () => true)
  const onExpired = vi.fn()
  const messages: unknown[] = []

  const client = new ReconnectingSocket({
    url: 'ws://test/ws',
    source: 'console',
    token: () => token,
    refresh,
    onExpired,
    onMessage: (m) => messages.push(m),
    socketFactory: () => {
      const s = new FakeSocket()
      sockets.push(s)
      return s
    },
    setTimer: (fn, ms) => {
      timerFn = fn
      delay = ms
      return 1
    },
    clearTimer: () => {
      timerFn = null
    },
    random: () => 0.5,
    ...overrides,
  })

  return {
    client,
    sockets,
    runTimer: () => {
      const fn = timerFn
      timerFn = null
      fn?.()
    },
    pendingDelay: () => delay,
    refresh,
    onExpired,
    messages,
    setToken: (t) => {
      token = t
    },
  }
}

// Let queued microtasks (an awaited refresh) settle.
const settle = () => new Promise((r) => setTimeout(r, 0))

const last = <T>(arr: T[]): T => arr[arr.length - 1]

describe('ReconnectingSocket', () => {
  it('authenticates with the token and source, then joins its topics', () => {
    const h = harness()
    h.client.subscribe('presence')
    h.runTimer() // the deferred open
    expect(h.sockets).toHaveLength(1)

    h.sockets[0].open()
    expect(h.sockets[0].authFrame()).toEqual({ type: 'authenticate', token: 'tok-1', source: 'console' })
    expect(JSON.parse(h.sockets[0].sent[1])).toEqual({ type: 'subscribe', topic: 'presence' })
  })

  it('never sends on a socket that is still connecting (the Dashboard crash)', () => {
    const h = harness()
    h.client.subscribe('presence')
    h.runTimer()
    // The socket exists but is CONNECTING; a second subscribe must not throw.
    expect(() => h.client.subscribe('another')).not.toThrow()

    // Both topics are joined once the socket actually opens.
    h.sockets[0].open()
    const joined = h.sockets[0].sent.slice(1).map((raw) => JSON.parse(raw).topic)
    expect(joined).toEqual(['presence', 'another'])
  })

  it('collapses the StrictMode start/stop/start into a single socket', () => {
    const h = harness()
    h.client.start() // mount: schedules the open
    h.client.stop() // strict-mode cleanup: cancels it before it ever connects
    h.client.start() // remount: schedules again
    h.runTimer()
    expect(h.sockets).toHaveLength(1) // exactly one, no throwaway connection
  })

  it('delivers server messages, but not the authenticated ack, to onMessage', () => {
    const h = harness()
    h.client.start()
    h.runTimer()
    h.sockets[0].open()
    h.sockets[0].message({ type: 'authenticated' })
    h.sockets[0].message({ type: 'presence', payload: { users: 3 } })

    expect(h.messages).toEqual([{ type: 'presence', payload: { users: 3 } }])
    expect(h.client.currentState()).toBe('open')
  })

  it('reconnects on a network drop, and the backoff grows and is capped', () => {
    const h = harness()
    h.client.start()
    h.runTimer()
    h.sockets[0].open()
    h.sockets[0].message({ type: 'authenticated' }) // resets the backoff

    // A network drop (not 1008): reconnect with backoff.
    h.sockets[0].fireClose(1006)
    expect(h.pendingDelay()).toBe(750) // exp=1000 → [500,1000], random 0.5 → 750
    h.runTimer()
    expect(h.sockets).toHaveLength(2)

    // Keep dropping without authenticating: the backoff climbs...
    h.sockets[1].open()
    h.sockets[1].fireClose(1006)
    expect(h.pendingDelay()).toBe(1500) // exp=2000
    h.runTimer()
    h.sockets[2].open()
    h.sockets[2].fireClose(1006)
    expect(h.pendingDelay()).toBe(3000) // exp=4000

    // ...and is capped: after enough attempts exp hits 30000 → [15000,30000].
    for (let i = 0; i < 10; i++) {
      h.runTimer()
      last(h.sockets).open()
      last(h.sockets).fireClose(1006)
    }
    expect(h.pendingDelay()).toBe(22500) // capped exp=30000 → 15000 + 0.5*15000
  })

  it('refreshes the token on an auth rejection and retries with the new one', async () => {
    const h = harness()
    h.refresh.mockImplementation(async () => {
      h.setToken('tok-2')
      return true
    })
    h.client.start()
    h.runTimer()
    h.sockets[0].open()

    h.sockets[0].fireClose(1008) // the server rejected the token
    await settle()
    expect(h.refresh).toHaveBeenCalledTimes(1)
    expect(h.pendingDelay()).toBe(250) // a prompt retry, not a full backoff

    h.runTimer()
    h.sockets[1].open()
    expect(h.sockets[1].authFrame().token).toBe('tok-2') // the fresh token
  })

  it('gives up and reports the session dead when refresh fails', async () => {
    const h = harness()
    h.refresh.mockResolvedValue(false)
    h.client.start()
    h.runTimer()
    h.sockets[0].open()

    h.sockets[0].fireClose(1008)
    await settle()

    expect(h.onExpired).toHaveBeenCalledTimes(1)
    expect(h.client.currentState()).toBe('stopped')
    // No reconnect is pending.
    h.runTimer()
    expect(h.sockets).toHaveLength(1)
  })

  it('stops after repeated auth rejections even when refresh keeps succeeding', async () => {
    const h = harness() // refresh returns true by default
    h.client.start()
    h.runTimer()
    h.sockets[0].open()

    // The server keeps rejecting; refresh keeps "working" but the token never
    // takes. It must not loop forever.
    for (let i = 0; i < 3; i++) {
      last(h.sockets).fireClose(1008)
      await settle()
      h.runTimer()
      last(h.sockets).open()
    }
    last(h.sockets).fireClose(1008) // the fourth rejection
    await settle()

    expect(h.onExpired).toHaveBeenCalledTimes(1)
    expect(h.client.currentState()).toBe('stopped')
  })

  it('stops cleanly, and a stopped socket never reconnects', () => {
    const h = harness()
    h.client.start()
    h.runTimer()
    h.sockets[0].open()
    h.sockets[0].message({ type: 'authenticated' })

    h.client.stop()
    expect(h.client.currentState()).toBe('stopped')
    expect(h.sockets[0].closed).toBe(true)
    // The close that stop() triggered does not schedule anything.
    h.runTimer()
    expect(h.sockets).toHaveLength(1)
  })

  it('ignores a stale socket entirely', () => {
    const h = harness()
    h.client.start()
    h.runTimer() // socket 0 exists (connecting)
    const stale = h.sockets[0]
    h.client.stop() // closes socket 0
    h.client.start()
    h.runTimer() // socket 1
    expect(h.sockets).toHaveLength(2)

    // The old socket fires late events: they must not touch the live one.
    stale.message({ type: 'presence' })
    stale.fireClose(1006)
    expect(h.messages).toHaveLength(0)
    h.runTimer()
    expect(h.sockets).toHaveLength(2) // no extra reconnect from the stale close
  })

  it('does not open a socket when signed out', () => {
    const h = harness({ token: () => null })
    h.client.start()
    h.runTimer()
    expect(h.sockets).toHaveLength(0)
    expect(h.client.currentState()).toBe('idle')
  })

  it('resets the backoff only once the connection is confirmed working', () => {
    const h = harness()
    h.client.start()
    h.runTimer()
    h.sockets[0].open()
    // Drop before the authenticated ack: this is NOT a working connection, so
    // the backoff must advance, not reset.
    h.sockets[0].fireClose(1006)
    expect(h.pendingDelay()).toBe(750) // attempt 0
    h.runTimer()
    h.sockets[1].open()
    h.sockets[1].fireClose(1006)
    expect(h.pendingDelay()).toBe(1500) // attempt 1, i.e. it did NOT reset
  })
})
