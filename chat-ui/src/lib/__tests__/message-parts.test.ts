import { describe, expect, it } from 'vitest';
import { assistantTimelineItems, rendersNothing } from '../message-parts';
import type { ChatMessage } from '../chat-types';

const assistant = (m: Partial<ChatMessage>): ChatMessage =>
  ({ id: 'm1', role: 'assistant', content: '', ...m }) as ChatMessage;

// An empty assistant turn is not free. The conversation spaces its children with
// `space-y-3`, so a zero-height child costs 12px above it and 12px below.
//
// The bug: answering an approval appends a placeholder for the resume stream to
// write into, and a background agent's card answers with ONE frame and no prose,
// so nothing ever arrived. Every answered approval left a ghost behind, and a
// column of them rendered at exactly double the spacing of the tool rows above.
// The guard existed but only covered the STREAMING case, so the finished empties
// stayed on the page for good.
describe('a turn that would show nothing', () => {
  it('renders nothing when it is empty and finished, not only while streaming', () => {
    expect(rendersNothing(assistant({}))).toBe(true);
    expect(rendersNothing(assistant({ isStreaming: true }))).toBe(true);
    // The exact wreckage: the placeholder appended when a card is answered.
    expect(rendersNothing(assistant({ content: '', isStreaming: false, parts: [] }))).toBe(true);
  });

  it('keeps a turn that is only tool calls', () => {
    // No content and no reasoning, and it must still be shown. A rule written
    // against the FIELDS rather than against what renders would hide it.
    const toolsOnly = assistant({
      tools: [{ name: 'http_request', friendly_name: 'Fetch data', status: 'done' } as any],
    });
    expect(assistantTimelineItems(toolsOnly)).toHaveLength(1);
    expect(rendersNothing(toolsOnly)).toBe(false);
  });

  it('keeps a turn whose only content is an error', () => {
    // Not a timeline item, and the whole point of the message it arrives on.
    expect(rendersNothing(assistant({ error: 'the vendor refused' }))).toBe(false);
  });

  it('keeps an ordinary answer', () => {
    expect(rendersNothing(assistant({ content: 'Here is the price.' }))).toBe(false);
    expect(rendersNothing(assistant({ reasoning: 'thinking' }))).toBe(false);
  });
});

describe('the timeline a turn renders', () => {
  it('prefers the streamed parts, which carry the real order', () => {
    const m = assistant({
      content: 'flattened',
      parts: [{ kind: 'text', text: 'streamed' }],
    });
    expect(assistantTimelineItems(m)).toEqual([{ kind: 'text', text: 'streamed' }]);
  });

  // A live segment that never got prose. The stream makes the message object
  // before a word arrives, and an empty part is not free: the timeline spaces
  // its children with gap-3, so a 730x0 div costs 12px above and 12px below,
  // which is the uneven rhythm between reasoning and tool rows. Measured in the
  // DOM, not guessed. The server is not at fault: of 184 assistant steps, none
  // is genuinely empty (40 are tool-only, 2 reasoning-only).
  it('drops a live part that would put nothing on the page', () => {
    const m = assistant({
      parts: [
        { kind: 'reasoning', text: 'why' },
        { kind: 'text', text: '' },
        { kind: 'text', text: '   ' },
      ],
    });
    expect(assistantTimelineItems(m).map((p) => p.kind)).toEqual(['reasoning']);
  });

  it('counts a turn of nothing but empty parts as showing nothing', () => {
    // rendersNothing counted parts that EXIST rather than parts that SHOW, so a
    // turn holding one empty text part was rendered as an empty message.
    const m = assistant({ parts: [{ kind: 'text', text: '' }] });
    expect(rendersNothing(m)).toBe(true);
  });

  it('rebuilds the same list from a reloaded message', () => {
    // Live and reloaded must produce the SAME list, or a turn looks different
    // after a refresh than it did while it streamed.
    const reloaded = assistant({
      reasoning: 'why',
      content: 'answer',
      tools: [{ name: 'a', friendly_name: 'A', status: 'done' } as any],
    });
    expect(assistantTimelineItems(reloaded).map((p) => p.kind)).toEqual(['reasoning', 'text', 'tool']);
  });
});
