import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render } from '@testing-library/react'

// The virtual list is faked, because what is under test is the DECISION: when
// does the conversation move itself, now that almost nothing needs it to.
vi.mock('@tanstack/react-virtual', () => ({
  useVirtualizer: () => ({
    getVirtualItems: () => [],
    getTotalSize: () => 0,
    measureElement: () => undefined,
    scrollToOffset: () => undefined,
  }),
}))

const { Conversation, ConversationFollowsWhatYouSend } = await import(
  '@/components/ui/ai/conversation'
)

// The conversation is a reversed column, so it is at the newest message by
// layout: opening one, switching to another, and an answer arriving all leave
// the view exactly where the browser already put it. Nothing calls this.
const moved = vi.fn()
Element.prototype.scrollTo = moved as unknown as Element['scrollTo']

const show = (props: { lastFromYou?: string; conversation?: string | null }) => (
  <Conversation
    items={[]}
    idOf={(x: string) => x}
    renderItem={() => null}
    conversation={props.conversation}
  >
    <ConversationFollowsWhatYouSend {...props} />
  </Conversation>
)

// One case is left, and it is the only one that is genuinely a MOVE: you were
// reading history, you said something, and you should be taken to it. Everything
// else this used to do (following a reply, landing on open, holding the bottom
// while the composer grows) is now the layout's, which is why there is so little
// here.
describe('following what you send', () => {
  beforeEach(() => moved.mockClear())

  it('does not move when a conversation is opened', () => {
    render(show({ conversation: 'a', lastFromYou: 'u1' }))
    expect(moved).not.toHaveBeenCalled()
  })

  it('does not move when the history of a conversation lands', () => {
    const { rerender } = render(show({ conversation: 'a' }))
    rerender(show({ conversation: 'a', lastFromYou: 'last-of-a' }))
    // The first thing the person said in a conversation ARRIVING is not them
    // saying it now.
    expect(moved).toHaveBeenCalledTimes(0)
  })

  it('goes to the newest message when the person says something', () => {
    const { rerender } = render(show({ conversation: 'a' }))
    rerender(show({ conversation: 'a', lastFromYou: 'u1' })) // the history landing
    moved.mockClear()
    rerender(show({ conversation: 'a', lastFromYou: 'u2' })) // them, now
    expect(moved).toHaveBeenCalledTimes(1)
    // Animated, because the person is watching this one happen: they sent
    // something and the view travels to it.
    // Instant, never animated. An animated scroll is still running after the
    // frame that started it, so reaching for the wheel to re-read what is above
    // becomes a tug of war with a browser that wins. A jump cannot be fought.
    expect(moved).toHaveBeenCalledWith({ top: 0, behavior: 'auto' })
  })

  // The reply re-renders this many times over as it streams, and none of those
  // is the person speaking. It stays put on its own besides: the bottom is the
  // anchor, so an answer growing pushes the older words up rather than pushing
  // the view down.
  it('stays put while the answer arrives', () => {
    const { rerender } = render(show({ conversation: 'a', lastFromYou: 'u2' }))
    for (let i = 0; i < 5; i++) rerender(show({ conversation: 'a', lastFromYou: 'u2' }))
    expect(moved).not.toHaveBeenCalled()
  })

  it('does nothing when there is nothing the person has sent', () => {
    const { rerender } = render(show({}))
    rerender(show({}))
    expect(moved).not.toHaveBeenCalled()
  })

  // Switching conversations is not a move either: the new one is already at its
  // newest message, because that is where a reversed column rests.
  it('does not move when you switch conversations', () => {
    const { rerender } = render(show({ conversation: 'a', lastFromYou: 'x' }))
    moved.mockClear()
    rerender(show({ conversation: 'b', lastFromYou: 'y' }))
    expect(moved).not.toHaveBeenCalled()
  })
})
