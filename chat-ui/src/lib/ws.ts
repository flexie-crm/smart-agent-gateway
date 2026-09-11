import { apiOrigin, currentSession, refreshSession, SESSION_EXPIRED } from './api'
import { ReconnectingSocket, type SocketMessage } from './reconnecting-socket'
import { CARD_EVENT, DELEGATION_EVENT, SOCKET_READY_EVENT, TURN_EVENT } from './delegations'

/**
 * dispatchSocketMessage turns a server push into a window event the chat reacts
 * to (mirroring the SESSION_EXPIRED pattern), so the module singleton here stays
 * free of React while the hook that owns chat state listens. A `delegation`
 * message moves a chip; a `turn` message means a server-initiated turn (a
 * background completion) is streaming, so the chat attaches and hears it.
 *
 * The hub wraps every push in a notification envelope ({type:"notification",
 * payload:<the real message>}), so the message that carries its own type is one
 * layer in. Unwrap it before routing, or a background card and completion would
 * never surface live and the person would have to reload to see them.
 */
export function dispatchSocketMessage(message: SocketMessage): void {
  const inner = (message.type === 'notification' && isSocketMessage(message.payload)
    ? message.payload
    : message) as SocketMessage
  if (inner.type === 'delegation' && inner.payload) {
    window.dispatchEvent(new CustomEvent(DELEGATION_EVENT, { detail: inner.payload }))
  } else if (inner.type === 'turn' && inner.payload) {
    window.dispatchEvent(new CustomEvent(TURN_EVENT, { detail: inner.payload }))
  } else if (inner.type === 'card' && inner.payload) {
    window.dispatchEvent(new CustomEvent(CARD_EVENT, { detail: inner.payload }))
  }
}

/** isSocketMessage narrows an envelope's payload to a nested, self-typed message. */
function isSocketMessage(v: unknown): v is SocketMessage {
  return typeof v === 'object' && v !== null && typeof (v as { type?: unknown }).type === 'string'
}

/**
 * The chat's WebSocket presence connection, built on the tested
 * ReconnectingSocket core.
 *
 * The chat itself streams a turn's answer over SSE; this socket is the other
 * half: while a person is here they hold a connection, tagged as the chat so it
 * counts toward the online total, and it is where server-pushed notifications
 * will arrive. This module only wires the core up.
 */

const socket = new ReconnectingSocket({
  // Straight to the orchestrator, like the REST client: apiOrigin is the API's
  // origin (VITE_SAG_API_URL in dev, same-origin in prod); http->ws, https->wss.
  url: `${apiOrigin.replace(/^http/, 'ws')}/v1/ws`,
  source: 'chat',
  token: () => currentSession()?.accessToken ?? null,
  refresh: refreshSession,
  onExpired: () => window.dispatchEvent(new CustomEvent(SESSION_EXPIRED)),
  onMessage: dispatchSocketMessage,
  // The line is up. Anything the server pushed while it was not went to
  // nobody, so whoever holds state that arrives by push re-reads it now.
  //
  // On the FIRST connection too, and that is not belt-and-braces. A push goes
  // to whoever is CONNECTED, and connected is a fact about sockets rather than
  // about people: a tab that has just opened has read the conversation over
  // HTTP but has no socket yet, while the tab it replaced can still have one
  // registered. Anything raised in that window is delivered to a socket nobody
  // is looking at, and lands nowhere.
  onReady: () => window.dispatchEvent(new CustomEvent(SOCKET_READY_EVENT)),
})

/** connect opens the presence socket and keeps it open until disconnect(). */
export function connect(): void {
  socket.start()
}

/** disconnect closes the socket and stops reconnecting (on sign-out). */
export function disconnect(): void {
  socket.stop()
}
