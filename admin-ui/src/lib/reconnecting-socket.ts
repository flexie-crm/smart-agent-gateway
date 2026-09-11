/**
 * A reconnecting WebSocket client, done properly.
 *
 * This is deliberately framework-free and dependency-injected (the socket
 * factory, timers, and clock are all injectable) so it can be unit-tested in
 * full without a real network. Both apps use it; the wiring around it (token,
 * refresh, source) is all that differs.
 *
 * What it guarantees:
 *   - AT MOST ONE socket and ONE pending reconnect, ever. A stale socket that
 *     has been replaced can never disturb the live one.
 *   - The backoff resets ONLY when the server confirms the connection works
 *     (the authenticated ack), not merely when the TCP socket opens, so a
 *     connection that opens and then fails auth backs off instead of looping.
 *   - Backoff is exponential with jitter and a cap (no thundering herd after a
 *     server restart, no unbounded delay).
 *   - Auth is distinguished from a network drop: the server rejects a bad token
 *     with WebSocket close code 1008, which triggers a token REFRESH and one
 *     retry with the new token. After a bounded number of auth failures it
 *     stops and reports the session dead, rather than looping on a token that
 *     will never work.
 */

export type SocketState = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'stopped'

export interface SocketMessage {
  type: string
  [key: string]: unknown
}

/** The minimal WebSocket surface used, so a test can supply a fake. */
export interface SocketLike {
  send(data: string): void
  close(code?: number, reason?: string): void
  onopen: (() => void) | null
  onmessage: ((event: { data: string }) => void) | null
  onclose: ((event: { code: number }) => void) | null
  onerror: (() => void) | null
  /** A real WebSocket THROWS on send() unless OPEN; every send checks this. */
  readonly readyState: number
}

export interface SocketOptions {
  url: string
  source: string
  /** The current access token, or null when signed out. */
  token: () => string | null
  /** Refresh the access token (shared with the REST client). True when a fresh one is available. */
  refresh: () => Promise<boolean>
  /** The session is dead (refresh failed after an auth rejection); drop to sign-in. */
  onExpired: () => void
  /** Every server message after authentication. */
  onMessage?: (message: SocketMessage) => void
  /** State transitions, for observability. */
  onState?: (state: SocketState) => void

  // Injected for tests; real implementations by default.
  socketFactory?: (url: string) => SocketLike
  setTimer?: (fn: () => void, ms: number) => number
  clearTimer?: (id: number) => void
  random?: () => number
}

const WS_CLOSE_NORMAL = 1000
const WS_CLOSE_POLICY_VIOLATION = 1008 // the server's code for a rejected token
const SOCKET_OPEN = 1 // WebSocket.OPEN; send() on any other state throws

const BACKOFF_BASE_MS = 1000
const BACKOFF_CAP_MS = 30000
const AUTH_RETRY_DELAY_MS = 250
const MAX_AUTH_FAILURES = 3
// The first open is deferred by a tick: React StrictMode mounts, cleans up, and
// mounts again synchronously, and the deferral lets that dance cancel the first
// attempt, so exactly one socket opens instead of a throwaway plus a real one.
const CONNECT_DELAY_MS = 10

export class ReconnectingSocket {
  private state: SocketState = 'idle'
  private socket: SocketLike | null = null
  private timer: number | null = null
  private attempt = 0 // consecutive reconnects, for the backoff curve
  private authFailures = 0 // consecutive auth rejections, bounded
  private readonly topics = new Set<string>()

  private readonly url: string
  private readonly source: string
  private readonly getToken: () => string | null
  private readonly refresh: () => Promise<boolean>
  private readonly onExpired: () => void
  private readonly onMessage?: (message: SocketMessage) => void
  private readonly onState?: (state: SocketState) => void
  private readonly makeSocket: (url: string) => SocketLike
  private readonly setTimer: (fn: () => void, ms: number) => number
  private readonly clearTimer: (id: number) => void
  private readonly random: () => number

  constructor(opts: SocketOptions) {
    this.url = opts.url
    this.source = opts.source
    this.getToken = opts.token
    this.refresh = opts.refresh
    this.onExpired = opts.onExpired
    this.onMessage = opts.onMessage
    this.onState = opts.onState
    this.makeSocket = opts.socketFactory ?? ((url) => new WebSocket(url) as unknown as SocketLike)
    this.setTimer = opts.setTimer ?? ((fn, ms) => window.setTimeout(fn, ms))
    this.clearTimer = opts.clearTimer ?? ((id) => window.clearTimeout(id))
    this.random = opts.random ?? Math.random
  }

  /** start opens the connection and keeps it open (idempotent). */
  start(): void {
    if (this.state === 'stopped') this.state = 'idle'
    if (this.socket || this.timer !== null) return
    this.timer = this.setTimer(() => {
      this.timer = null
      this.open()
    }, CONNECT_DELAY_MS)
  }

  /** stop closes the connection and prevents any further reconnect. */
  stop(): void {
    this.setState('stopped')
    this.cancelTimer()
    const closing = this.socket
    this.socket = null
    this.attempt = 0
    this.authFailures = 0
    closing?.close(WS_CLOSE_NORMAL, 'client stop')
  }

  /** subscribe joins a topic now and on every future reconnect. */
  subscribe(topic: string): void {
    this.topics.add(topic)
    this.sendRaw({ type: 'subscribe', topic })
    this.start()
  }

  currentState(): SocketState {
    return this.state
  }

  private open(): void {
    if (this.state === 'stopped') return
    if (this.socket || this.timer !== null) return
    const token = this.getToken()
    if (!token) {
      // Signed out. A later start()/subscribe reconnects once there is a token.
      this.setState('idle')
      return
    }

    this.setState('connecting')
    const socket = this.makeSocket(this.url)
    this.socket = socket

    socket.onopen = () => {
      // Authenticate first, then (re)join every topic. Ordered on the wire, so
      // the server reads authenticate before anything else.
      socket.send(JSON.stringify({ type: 'authenticate', token, source: this.source }))
      for (const topic of this.topics) {
        socket.send(JSON.stringify({ type: 'subscribe', topic }))
      }
    }
    socket.onmessage = (event) => this.handleMessage(socket, event.data)
    socket.onclose = (event) => this.handleClose(socket, event.code)
    socket.onerror = () => socket.close()
  }

  private handleMessage(socket: SocketLike, data: string): void {
    if (socket !== this.socket) return // a stale socket; ignore it entirely
    let message: SocketMessage
    try {
      message = JSON.parse(data)
    } catch {
      return
    }
    if (message.type === 'authenticated') {
      // Proven working: NOW reset the backoff and the auth-failure count.
      this.setState('open')
      this.attempt = 0
      this.authFailures = 0
      return
    }
    this.onMessage?.(message)
  }

  private handleClose(socket: SocketLike, code: number): void {
    if (socket !== this.socket) return // a stale socket closing cannot drive anything
    this.socket = null
    if (this.state === 'stopped') return

    if (code === WS_CLOSE_POLICY_VIOLATION) {
      void this.handleAuthRejection()
      return
    }
    this.scheduleReconnect(this.backoffDelay())
  }

  private async handleAuthRejection(): Promise<void> {
    this.authFailures += 1
    if (this.authFailures > MAX_AUTH_FAILURES) {
      // The token keeps being rejected even after refreshing; the session is
      // not going to recover on its own. Stop, and let the app sign in again.
      this.stop()
      this.onExpired()
      return
    }
    const refreshed = await this.refresh()
    if (this.state === 'stopped') return
    if (!refreshed) {
      this.stop()
      this.onExpired()
      return
    }
    // A fresh token is available: retry promptly, but not instantly.
    this.scheduleReconnect(AUTH_RETRY_DELAY_MS)
  }

  private scheduleReconnect(delay: number): void {
    if (this.state === 'stopped' || this.timer !== null || this.socket) return
    this.setState('reconnecting')
    this.timer = this.setTimer(() => {
      this.timer = null
      this.open()
    }, delay)
  }

  /** Exponential backoff with equal jitter: [exp/2, exp], capped. */
  private backoffDelay(): number {
    const exp = Math.min(BACKOFF_BASE_MS * 2 ** this.attempt, BACKOFF_CAP_MS)
    this.attempt += 1
    return Math.floor(exp / 2 + this.random() * (exp / 2))
  }

  private sendRaw(message: unknown): void {
    // Only on an OPEN socket: a real WebSocket THROWS on send while it is still
    // connecting. Nothing is lost by skipping: every topic is (re)joined by the
    // onopen handler when the socket does open.
    if (this.socket && this.socket.readyState === SOCKET_OPEN) {
      this.socket.send(JSON.stringify(message))
    }
  }

  private cancelTimer(): void {
    if (this.timer !== null) {
      this.clearTimer(this.timer)
      this.timer = null
    }
  }

  private setState(state: SocketState): void {
    if (this.state === state) return
    this.state = state
    this.onState?.(state)
  }
}
