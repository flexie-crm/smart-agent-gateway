import { describe, it, expect, vi } from 'vitest'
import { render } from '@testing-library/react'

// The list asks by INDEX, and it asks using what it knew a render ago.
//
// This fakes exactly that: a virtualizer still holding rows 0..4 while the
// conversation underneath it has become two rows long, or empty. It happens for
// real when a session is lost and the transcript is cleared, which is what a
// gateway restart did to somebody mid-conversation: their requests started
// failing, the messages went, and a list still holding five hundred indices
// asked about the five hundredth. The whole chat went to an error boundary.
const stale = (count: number) => ({
  useVirtualizer: () => ({
    getVirtualItems: () =>
      Array.from({ length: count }, (_, index) => ({
        key: `k${index}`,
        index,
        start: index * 100,
        end: index * 100 + 100,
        size: 100,
        lane: 0,
      })),
    getTotalSize: () => count * 100,
    measureElement: () => undefined,
    scrollToOffset: () => undefined,
    scrollToIndex: () => undefined,
  }),
})

vi.mock('@tanstack/react-virtual', () => stale(5))

const { Conversation } = await import('@/components/ui/ai/conversation')

type Row = { id: string }

describe('a conversation that gets shorter under the list', () => {
  const show = (items: Row[]) => (
    <Conversation
      items={items}
      idOf={(m: Row) => m.id}
      renderItem={(m: Row) => <div>{m.id.toUpperCase()}</div>}
    />
  )

  it('draws what is there and ignores what is gone', () => {
    // Five indices asked for, two rows left.
    const { container } = render(show([{ id: 'a' }, { id: 'b' }]))
    expect(container.textContent).toContain('A')
    expect(container.textContent).toContain('B')
  })

  it('survives the conversation emptying completely', () => {
    // The case that broke: every index the list holds is gone.
    expect(() => render(show([]))).not.toThrow()
  })

  it('survives a long conversation being replaced by a short one', () => {
    const { rerender, container } = render(
      show([{ id: 'a' }, { id: 'b' }, { id: 'c' }, { id: 'd' }, { id: 'e' }]),
    )
    expect(() => rerender(show([{ id: 'z' }]))).not.toThrow()
    expect(container.textContent).toContain('Z')
  })
})
