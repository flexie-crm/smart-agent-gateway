import { describe, it, expect } from 'vitest'
import { dropEmptyPlaceholder } from '../placeholder'
import type { ChatMessage } from '../chat-types'

const msg = (over: Partial<ChatMessage>): ChatMessage => ({
  id: 'x',
  role: 'assistant',
  content: '',
  ...over,
})

// The reported failure, as a test: an answer was on screen and then it was not.
//
// A rejoin puts an empty bubble up before it knows whether there is anything to
// hear, and takes it back when there is not. The bubble was taken back BY ID,
// whatever had been written into it in the meantime, and an attach can paint an
// answer and then fail: the outcome describes the request, not the content.
describe('taking back the bubble a rejoin raised', () => {
  it('removes it while it is still empty, which is what it is for', () => {
    const after = dropEmptyPlaceholder(
      [msg({ id: 'a', content: 'hello' }), msg({ id: 'ph', isStreaming: true })],
      'ph',
    )
    expect(after.map((m) => m.id)).toEqual(['a'])
  })

  it('KEEPS it once an answer has been written into it', () => {
    const painted = msg({ id: 'ph', content: 'the report: 10 projects', isStreaming: true })
    const after = dropEmptyPlaceholder([msg({ id: 'a', content: 'hi' }), painted], 'ph')
    expect(after).toHaveLength(2)
    expect(after[1].content).toBe('the report: 10 projects')
  })

  // A turn can do its work without saying anything, and that is still something
  // somebody watched happen. Judging emptiness on prose alone would delete it.
  it('keeps a turn that only ran a tool, or only reasoned', () => {
    for (const over of [
      { tools: [{ name: 'ssh_box', friendly_name: 'Run a command' }] as ChatMessage['tools'] },
      { parts: [{ type: 'text', text: 'x' }] as unknown as ChatMessage['parts'] },
      { reasoning: 'thinking about it' },
    ]) {
      const after = dropEmptyPlaceholder([msg({ id: 'ph', ...over })], 'ph')
      expect(after, `${JSON.stringify(over)} was deleted`).toHaveLength(1)
    }
  })

  it('touches nothing else, whatever state the rest is in', () => {
    const before = [
      msg({ id: 'a', content: '' }),
      msg({ id: 'b', content: 'kept' }),
      msg({ id: 'ph' }),
    ]
    const after = dropEmptyPlaceholder(before, 'ph')
    // An unrelated empty message is not this rejoin's to remove: only the id it
    // raised itself.
    expect(after.map((m) => m.id)).toEqual(['a', 'b'])
  })

  it('does nothing when the id is not there at all', () => {
    const before = [msg({ id: 'a', content: 'hi' })]
    expect(dropEmptyPlaceholder(before, 'ph')).toEqual(before)
  })
})
