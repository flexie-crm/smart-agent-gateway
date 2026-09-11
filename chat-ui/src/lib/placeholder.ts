import type { ChatMessage } from './chat-types'

/**
 * Take back the empty bubble a rejoin put up, and ONLY if it is still empty.
 *
 * Rejoining raises a bubble before it knows whether there is anything to hear,
 * because the frames that follow are written into it. When there turns out to be
 * nothing, it has to come back down or the conversation ends on a blank line
 * that never fills.
 *
 * The bug this exists to make impossible: the bubble was removed by id, whatever
 * was in it. An attach can paint an answer and THEN fail, which is not exotic
 * (the stream is long-lived, and a failure after the last frame is as likely as
 * one before the first), and the outcome is reported for the request rather than
 * for the content. So a person watched an answer arrive and watched it vanish a
 * moment later, and the only copy on screen was destroyed by the code meant to
 * tidy up after a request that found nothing.
 *
 * The rule is one line and it is the whole fix: WHAT THE SERVER SENT IS NEVER
 * OURS TO DELETE. An empty placeholder is ours, because we invented it. Anything
 * with content in it came from the other end and stays, whatever the request
 * that carried it went on to report about itself.
 */
export function dropEmptyPlaceholder(messages: ChatMessage[], id: string): ChatMessage[] {
  return messages.filter((m) => m.id !== id || hasContent(m))
}

/**
 * Whether anything arrived in this message. Text is the ordinary case; a turn
 * that only ran a tool has its work in `parts` or `tools` and no prose at all,
 * and that is still something a person watched happen.
 */
function hasContent(m: ChatMessage): boolean {
  return (
    m.content.length > 0 ||
    (m.parts?.length ?? 0) > 0 ||
    (m.tools?.length ?? 0) > 0 ||
    (m.reasoning?.length ?? 0) > 0
  )
}
