import { Loader2, Clock, Check, X, Minus } from 'lucide-react'

/**
 * The mark that says how something stands, at a glance and before any reading.
 *
 * ONE of these in the app, shared by the background-task chips and the answered
 * approval lines in the transcript, because they are saying the same thing and a
 * person should not have to learn two vocabularies of tick.
 *
 * A FILLED badge, not an outline. At this size an outlined circle spends most of
 * its box on the ring and leaves the glyph inside it smaller still, which is why
 * an outlined check read as a speck: 16 pixels of icon were about 8 pixels of
 * check. Filling the circle and knocking the glyph out in white puts the whole
 * box to work, so the mark carries at a glance instead of needing to be looked
 * for.
 *
 * SIZED TO THE LINE IT SITS ON: 20px against the 20px line of text-sm, 16px
 * against the 16px line of text-xs. The glyph is a fixed fraction of the badge,
 * so the proportion holds at either size rather than being tuned twice.
 *
 * A spinner stays an outline, because it is motion rather than a state, and it
 * keeps the same box so nothing shifts when the thing finishes.
 */
export function StatusMark({ status, size = 4 }: { status: string; size?: 4 | 5 }) {
  const box = size === 5 ? 'size-5' : 'size-4'
  const glyph = size === 5 ? 'size-3' : 'size-2.5'

  if (status === 'running' || status === '') {
    return <Loader2 className={`${box} shrink-0 animate-spin text-muted-foreground`} />
  }

  const tone =
    status === 'done'
      ? 'bg-emerald-600'
      : status === 'failed'
        ? 'bg-destructive'
        : status === 'waiting_approval'
          ? 'bg-amber-500'
          : 'bg-muted-foreground/60'

  return (
    <span className={`${box} ${tone} flex shrink-0 items-center justify-center rounded-full`}>
      <Mark status={status} className={`${glyph} text-white`} />
    </span>
  )
}

/** The glyph inside a badge. Heavier stroke, because it is small and knocked out. */
function Mark({ status, className }: { status: string; className: string }) {
  switch (status) {
    case 'done':
      return <Check className={className} strokeWidth={3.5} />
    case 'failed':
      return <X className={className} strokeWidth={3.5} />
    case 'waiting_approval':
      return <Clock className={className} strokeWidth={3} />
    default:
      return <Minus className={className} strokeWidth={3.5} />
  }
}
