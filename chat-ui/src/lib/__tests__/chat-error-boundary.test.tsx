import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, within, cleanup } from '@testing-library/react'

import { ChatErrorBoundary } from '@/components/ui/ai/chat-error-boundary'

// How the chat FAILS is part of the product.
//
// It used to replace a region that fills the window with a small centred box, so
// the layout collapsed and the composer flew to the top of the screen; and it
// announced itself in the middle of where the conversation had been, as though
// the conversation were gone. It is not gone: the transcript lives above this,
// outside what is caught, and comes straight back.

let boom = true
const Explodes = () => {
  if (boom) throw new Error('a row could not be drawn')
  return <p>the conversation</p>
}

describe('when the conversation cannot be drawn', () => {
  beforeEach(() => {
    boom = true
    // React logs the caught error; the test is about what the person sees.
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })
  afterEach(() => {
    cleanup() // each of these renders a chat; without this they pile up
    vi.restoreAllMocks()
  })

  it('says what happened without claiming the messages are lost', () => {
    render(
      <ChatErrorBoundary>
        <Explodes />
      </ChatErrorBoundary>,
    )
    expect(screen.getByText(/could not be drawn/i)).toBeTruthy()
    expect(screen.getByText(/messages are safe/i)).toBeTruthy()
  })

  it('keeps the shape of what it replaced, so nothing else on the screen moves', () => {
    const { container } = render(
      <ChatErrorBoundary>
        <Explodes />
      </ChatErrorBoundary>,
    )
    // jsdom does no layout, so this asserts the rule rather than the pixels:
    // the replacement still FILLS its parent (flex-1) and puts its notice at the
    // bottom, which is what stops the composer being pulled up to the top.
    const root = container.firstElementChild as HTMLElement
    expect(root.className).toContain('flex-1')
    expect(root.className).toContain('justify-end')
  })

  it('brings the conversation back when asked', () => {
    const { container } = render(
      <ChatErrorBoundary>
        <Explodes />
      </ChatErrorBoundary>,
    )
    boom = false
    fireEvent.click(within(container).getByRole('button', { name: /show it again/i }))
    expect(within(container).getByText('the conversation')).toBeTruthy()
  })
})
