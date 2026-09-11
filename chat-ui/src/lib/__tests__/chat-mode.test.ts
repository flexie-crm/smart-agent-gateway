import { describe, it, expect } from 'vitest'
import { resolveChatStore, chatsEndpointFor, isMultiChatEnabled } from '../chat-mode'

describe('resolveChatStore', () => {
  it('defaults to "flexie" when nothing is set (the product is the default context)', () => {
    expect(resolveChatStore(undefined, undefined)).toBe('flexie')
  })

  it('uses the window config when no prop is given', () => {
    expect(resolveChatStore(undefined, 'external')).toBe('external')
    expect(resolveChatStore(undefined, 'flexie')).toBe('flexie')
  })

  it('lets the prop win over the window config', () => {
    expect(resolveChatStore('external', 'flexie')).toBe('external')
    expect(resolveChatStore('flexie', 'external')).toBe('flexie')
  })
})

describe('chatsEndpointFor', () => {
  it('passes the endpoint through in flexie mode', () => {
    expect(chatsEndpointFor('flexie', '/s/ai/chats')).toBe('/s/ai/chats')
  })

  it('is undefined in flexie mode when no endpoint is configured', () => {
    expect(chatsEndpointFor('flexie', undefined)).toBeUndefined()
  })

  it('suppresses the endpoint entirely in external mode (no list request is made)', () => {
    expect(chatsEndpointFor('external', '/s/ai/chats')).toBeUndefined()
  })
})

describe('isMultiChatEnabled', () => {
  it('flexie + chats endpoint => sidebar on', () => {
    expect(isMultiChatEnabled('flexie', true)).toBe(true)
  })

  it('flexie + no endpoint => off (single chat)', () => {
    expect(isMultiChatEnabled('flexie', false)).toBe(false)
  })

  it('external + endpoint present => off (the hard guard: external is never multi-thread)', () => {
    expect(isMultiChatEnabled('external', true)).toBe(false)
  })

  it('external + no endpoint => off', () => {
    expect(isMultiChatEnabled('external', false)).toBe(false)
  })
})
