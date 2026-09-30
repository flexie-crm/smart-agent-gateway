import { Loader2 } from 'lucide-react'
import { cn, t } from '@/lib/utils'
import { filled, showsMeter, type ContextMeter } from '@lib/context-meter'

/**
 * The line above the message box that says how full the conversation is, with
 * the way to make room at its right.
 *
 * It is part of the composer rather than of the conversation, so it never
 * scrolls away: it is about what the next message will be sent with, and that
 * is decided here. It appears from the threshold up (the server's, carried on
 * the meter), stays while a summary is being written, and says so when one
 * could not be.
 *
 * One row, no box of its own: a hairline bar, a number and a button, sitting on
 * the composer's own surface the way its toolbar does.
 */
export function ContextLine({
  meter,
  onCompact,
  busy,
  lang,
}: {
  meter: ContextMeter
  onCompact: () => void
  /** A turn is being answered: the server would refuse a compaction now. */
  busy: boolean
  lang?: Record<string, string>
}) {
  if (!showsMeter(meter)) return null
  const width = filled(meter)
  // Past full, the oldest messages are already being dropped to fit.
  const dropping = meter.percent !== null && meter.percent > 100

  let said: string
  if (meter.compacting) {
    said = t('compacting', lang, 'Compacting…')
  } else if (meter.failed) {
    said = meter.failed
  } else if (dropping) {
    said = t('context_full', lang, 'Context full, oldest messages dropped')
  } else {
    said = t('context_used', lang, '{percent}% of context used', { percent: meter.percent ?? 0 })
  }

  return (
    <div
      className="flex items-center gap-3 px-3 py-2"
      data-context-line={meter.compacting ? 'compacting' : dropping ? 'full' : 'shown'}
    >
      <div
        className="h-1 min-w-0 flex-1 overflow-hidden rounded-full bg-muted"
        role="progressbar"
        aria-label={t('context_used_label', lang, 'Context used')}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={width}
      >
        <div
          className={cn(
            'h-full rounded-full transition-[width] duration-300',
            dropping ? 'bg-destructive' : 'bg-foreground/70',
            meter.compacting && 'animate-pulse',
          )}
          style={{ width: `${width}%` }}
        />
      </div>
      <span
        className={cn(
          'min-w-0 shrink truncate text-xs tabular-nums',
          meter.failed && !meter.compacting ? 'text-destructive' : 'text-muted-foreground',
        )}
        title={said}
      >
        {said}
      </span>
      <button
        type="button"
        onClick={onCompact}
        disabled={meter.compacting || busy}
        className="flex shrink-0 cursor-pointer items-center gap-1.5 rounded-md px-2 py-0.5 text-xs font-medium text-foreground transition-colors hover:bg-muted disabled:cursor-default disabled:opacity-40 disabled:hover:bg-transparent"
        title={t('compact_hint', lang, 'Summarize the conversation so far, so it takes less room')}
      >
        {meter.compacting && <Loader2 className="size-3 animate-spin" />}
        {t('compact', lang, 'Compact')}
      </button>
    </div>
  )
}
