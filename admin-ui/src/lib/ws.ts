import { useEffect, useState } from 'react'
import { apiOrigin, currentSession, refreshSession, SESSION_EXPIRED } from './api'
import { ReconnectingSocket, type SocketMessage } from './reconnecting-socket'

/**
 * The console's WebSocket, built on the tested ReconnectingSocket core.
 *
 * The console holds this connection for the WHOLE signed-in session, not per
 * screen: it is the server's push channel (workflow notifications, presence,
 * whatever comes next), and a notification must arrive whichever screen is
 * open. The auth provider starts it on sign-in, stops it on sign-out, and
 * restarts it on a workspace switch, because the connection's workspace is a
 * claim made when it authenticated: a new workspace needs a new connection.
 *
 * Screens never touch the connection itself; they subscribe to topics and
 * listen for messages.
 */

/**
 * LiveDashboard is the workspace's live picture, pushed on the 'dashboard' topic
 * whenever a number moves. It is the SAME shape /stats returns on load, so a
 * screen can seed from the endpoint and then let the socket keep it current.
 */
export interface LiveDashboard {
  connected: number
  sessions: number
  running: number
  waiting_approval: number
}

const socket = new ReconnectingSocket({
  url: apiOrigin.replace(/^http/, 'ws') + '/v1/ws',
  source: 'console',
  token: () => currentSession()?.accessToken ?? null,
  refresh: refreshSession,
  onExpired: () => window.dispatchEvent(new CustomEvent(SESSION_EXPIRED)),
  onMessage: (message) => {
    for (const listener of listeners) listener(message)
  },
})

const listeners = new Set<(message: SocketMessage) => void>()

/** startSocket opens the app-wide connection (idempotent). */
export function startSocket(): void {
  socket.start()
}

/** stopSocket closes it and stops reconnecting (sign-out, session death). */
export function stopSocket(): void {
  socket.stop()
}

/**
 * restartSocket reconnects with the current token. A workspace switch calls
 * this: the old connection speaks for the old workspace and cannot be kept.
 * Topic subscriptions survive; they are rejoined on the new connection.
 */
export function restartSocket(): void {
  socket.stop()
  socket.start()
}

/** subscribeTopic joins a topic, now and on every reconnect. */
export function subscribeTopic(topic: string): void {
  socket.subscribe(topic)
}

/** listen registers a handler for every server message; returns the undo. */
export function listen(handler: (message: SocketMessage) => void): () => void {
  listeners.add(handler)
  return () => {
    listeners.delete(handler)
  }
}

/**
 * useLiveDashboard subscribes to the workspace's live picture and returns the
 * latest push, updated the instant a turn starts or ends, an approval is raised
 * or answered, or a person connects or leaves. It seeds nothing itself: a screen
 * loads the first snapshot from /stats and passes it as `initial`, so the tile is
 * whole on first paint and the socket only keeps it current. No timer, no poll.
 */
export function useLiveDashboard(initial: LiveDashboard | null = null): LiveDashboard | null {
  // The latest socket push, null until the first one arrives. Until then the
  // caller's loaded snapshot stands in, so the tiles are whole from first paint
  // and simply become live once the socket speaks. No effect syncs the two.
  const [pushed, setPushed] = useState<LiveDashboard | null>(null)
  useEffect(() => {
    subscribeTopic('dashboard')
    return listen((message) => {
      if (message.type === 'topic' && message.topic === 'dashboard' && message.payload) {
        setPushed(message.payload as LiveDashboard)
      }
    })
  }, [])
  return pushed ?? initial
}
