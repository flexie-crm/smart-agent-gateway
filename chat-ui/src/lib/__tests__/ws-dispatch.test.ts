import { describe, it, expect, vi } from 'vitest'
import { dispatchSocketMessage } from '../ws'
import { CARD_EVENT, DELEGATION_EVENT, TURN_EVENT } from '../delegations'
import type { SocketMessage } from '../reconnecting-socket'

describe('dispatchSocketMessage', () => {
  it('forwards a delegation push as a window event carrying its payload', () => {
    const handler = vi.fn()
    window.addEventListener(DELEGATION_EVENT, handler)
    dispatchSocketMessage({ type: 'delegation', payload: { chat_uid: 'c1', id: '5', status: 'running' } } as SocketMessage)
    expect(handler).toHaveBeenCalledOnce()
    expect((handler.mock.calls[0][0] as CustomEvent).detail).toMatchObject({ chat_uid: 'c1', id: '5', status: 'running' })
    window.removeEventListener(DELEGATION_EVENT, handler)
  })

  it('forwards a turn push as a window event', () => {
    const handler = vi.fn()
    window.addEventListener(TURN_EVENT, handler)
    dispatchSocketMessage({ type: 'turn', payload: { chat_uid: 'c1', run: 'r1' } } as SocketMessage)
    expect(handler).toHaveBeenCalledOnce()
    window.removeEventListener(TURN_EVENT, handler)
  })

  it('ignores unrelated types and messages with no payload', () => {
    const handler = vi.fn()
    window.addEventListener(DELEGATION_EVENT, handler)
    dispatchSocketMessage({ type: 'presence', payload: { users: 3 } } as SocketMessage)
    dispatchSocketMessage({ type: 'delegation' } as SocketMessage)
    expect(handler).not.toHaveBeenCalled()
    window.removeEventListener(DELEGATION_EVENT, handler)
  })

  // The hub wraps every push in a notification envelope, so the message the
  // server actually sends is one layer in. This is the shape that reaches a real
  // browser; without unwrapping, a background card and completion never surface.
  it('unwraps a notification envelope to route the message inside it', () => {
    const delegation = vi.fn()
    const turn = vi.fn()
    window.addEventListener(DELEGATION_EVENT, delegation)
    window.addEventListener(TURN_EVENT, turn)

    dispatchSocketMessage({
      type: 'notification',
      payload: { type: 'delegation', payload: { chat_uid: 'c1', id: '7', status: 'waiting_approval' } },
    } as SocketMessage)
    dispatchSocketMessage({
      type: 'notification',
      payload: { type: 'turn', payload: { chat_uid: 'c1', run: 'r9' } },
    } as SocketMessage)

    expect(delegation).toHaveBeenCalledOnce()
    expect((delegation.mock.calls[0][0] as CustomEvent).detail).toMatchObject({ id: '7', status: 'waiting_approval' })
    expect(turn).toHaveBeenCalledOnce()
    expect((turn.mock.calls[0][0] as CustomEvent).detail).toMatchObject({ chat_uid: 'c1', run: 'r9' })

    window.removeEventListener(DELEGATION_EVENT, delegation)
    window.removeEventListener(TURN_EVENT, turn)
  })

  it('unwraps a pushed approval card (KB/27), carried whole over the socket', () => {
    const card = vi.fn()
    window.addEventListener(CARD_EVENT, card)
    dispatchSocketMessage({
      type: 'notification',
      payload: {
        type: 'card',
        payload: {
          chat_uid: 'c1',
          id: 'confirm_call_1',
          confirmation: { token: 'tok', title: 'Fetch data from a website', description: 'x', severity: 'external_communication', status: 'pending' },
        },
      },
    } as SocketMessage)
    expect(card).toHaveBeenCalledOnce()
    expect((card.mock.calls[0][0] as CustomEvent).detail).toMatchObject({
      id: 'confirm_call_1',
      confirmation: { token: 'tok' },
    })
    window.removeEventListener(CARD_EVENT, card)
  })
})
