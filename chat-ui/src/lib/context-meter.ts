/**
 * How full a conversation is, as the line above the message box shows it.
 *
 * The number is the server's: the same measure its trim uses, against the room
 * the model's context window gives. At 100 the oldest messages start to be
 * dropped, so the line is there to say so before it happens, with the offer to
 * compact the conversation instead, which keeps what it meant rather than
 * cutting it off.
 *
 * It arrives in two ways and both are read here: on the history answer when a
 * conversation is opened (meta.context), and pushed over the socket after every
 * step (a `context` message), so it is right on a reload and moves while the
 * assistant works.
 */

/** The window event a socket `context` message becomes (see ws.ts). */
export const CONTEXT_EVENT = 'sag:context'

export type ContextMeter = {
  /** Percent of the room used, or null when there is nothing to measure against. */
  percent: number | null
  /** From here up, the line is shown. */
  warnAt: number
  /** A summary is being written: the chat is frozen until it is done. */
  compacting: boolean
  /** Why the last compaction did not happen, when it did not. */
  failed: string
}

/** Where a conversation starts: nothing measured, nothing under way. */
export const NO_METER: ContextMeter = { percent: null, warnAt: 80, compacting: false, failed: '' }

/**
 * readMeter turns what the server sent into a meter. Anything missing or of the
 * wrong type reads as not known rather than as a number, so a server a release
 * behind shows no line instead of a wrong one.
 */
export function readMeter(raw: unknown): ContextMeter {
  if (typeof raw !== 'object' || raw === null) return NO_METER
  const r = raw as Record<string, unknown>
  return {
    percent: typeof r.percent === 'number' && Number.isFinite(r.percent) ? r.percent : null,
    warnAt: typeof r.warn_at === 'number' && r.warn_at > 0 ? r.warn_at : NO_METER.warnAt,
    compacting: r.compacting === true,
    failed: typeof r.failed === 'string' ? r.failed : '',
  }
}

/**
 * showsMeter is whether the line is on screen: while compacting, when there is
 * something to say about a compaction that failed, and from the threshold up.
 */
export function showsMeter(m: ContextMeter): boolean {
  if (m.compacting || m.failed !== '') return true
  return m.percent !== null && m.percent >= m.warnAt
}

/** How much of the line is filled, 0 to 100: past full is still full. */
export function filled(m: ContextMeter): number {
  if (m.percent === null) return 0
  return Math.max(0, Math.min(100, m.percent))
}
