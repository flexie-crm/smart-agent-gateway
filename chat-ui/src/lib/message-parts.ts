import type { ChatMessage, MessagePart } from './chat-types'

/**
 * What an assistant turn actually renders, and whether that is anything at all.
 *
 * Its own module rather than two helpers inside the component, because "would
 * this message show anything" is a rule with a bug history and no way to test it
 * from inside a 1500-line component.
 */

/**
 * The flat timeline for one assistant turn: reasoning, text and tool calls in
 * the order they happened, each a sibling under the message's single gap.
 *
 * A live message carries `parts` (the stream processor builds them in order); a
 * message from history carries the flattened fields. Both produce the SAME list,
 * so a turn looks identical whether it just streamed or came back from a reload.
 */
export function assistantTimelineItems(m: ChatMessage): MessagePart[] {
  if (m.parts && m.parts.length) return m.parts.filter(showsSomething)
  const items: MessagePart[] = []
  // Trimmed, because a string of whitespace is truthy and draws a row of
  // nothing. The live path is filtered the same way just above, and the two
  // must agree: a turn looks the same reloaded as it did streaming.
  if (m.reasoning?.trim()) items.push({ kind: 'reasoning', text: m.reasoning, model: m.reasoning_model })
  if (m.content?.trim()) items.push({ kind: 'text', text: m.content })
  if (m.tools && m.tools.length) for (const tool of m.tools) items.push({ kind: 'tool', tool })
  return items
}

/**
 * Whether one part puts anything on the page.
 *
 * The live list was taken verbatim, and the stream makes an empty text part the
 * moment a turn begins so there is somewhere to write. Until a word arrives it
 * holds nothing, so it drew a 730x0 div, and a zero-height child is NOT free:
 * the timeline spaces its children with gap-3, so it cost 12px above and 12px
 * below. That is the uneven rhythm, and it read as arbitrary because the thing
 * making the space was invisible.
 *
 * The history path had always been careful (`if (m.content)`) and the live path
 * was not, which is exactly the split this module exists to close: both must
 * produce the same list for the same turn.
 */
function showsSomething(part: MessagePart): boolean {
  switch (part.kind) {
    case 'text':
    case 'reasoning':
    case 'agent':
      return part.text.trim() !== ''
    case 'tool':
      return true
  }
}

/**
 * Whether this assistant turn would put nothing on the page.
 *
 * An empty turn is NOT free. The conversation spaces its children with
 * `space-y-3`, so a zero-height child still costs 12px above it and 12px below.
 * Answering an approval appends exactly such a placeholder for the resume stream
 * to write into, and a background agent's card answers with one frame and no
 * prose, so nothing ever arrives: every answered approval left a ghost behind,
 * and a column of them read at double the spacing of the tool rows above.
 *
 * The test is what would RENDER, not which fields happen to be set. A turn that
 * is only tool calls has no content and no reasoning and must still be shown,
 * and an error is the whole point of the message it arrives on.
 */
export function rendersNothing(m: ChatMessage): boolean {
  return assistantTimelineItems(m).length === 0 && !m.error
}
