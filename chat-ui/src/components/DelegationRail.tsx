import { useEffect, useState } from 'react'
import { StatusMark } from '@/components/ui/ai/status-mark'
import type { Delegation } from '@lib/delegations'
import { isTerminalDelegation } from '@lib/delegations'

/**
 * DelegationRail is the background-tasks column on the RIGHT (Mode C): its own
 * room, a flex sibling of the chat so it takes width rather than floating over
 * it, no label and no vertical border, just the chip cards.
 *
 * It is a RECORD, not a live view: work that vanished as it finished left
 * somebody with no idea what had been done for them.
 *
 * But a finished task must be CHEAPER to show than a running one, or the record
 * costs as much as the work. So the column has two shapes: anything still going
 * keeps a card, and anything over becomes a LINE. Six identical cards for six
 * finished tasks is not a record, it is wallpaper, and it buries the one thing
 * still happening.
 *
 * Newest sits on top and pushes the rest down. Past a handful, older lines fold
 * behind a count.
 *
 * WHAT A CHIP NEVER SHOWS is the agent's result. That is bare technical fact
 * written for the Gateway, which reads it and tells the person what it means
 * (KB/27). A chip carrying it puts "```json" and "the raw response body:" in
 * front of somebody who asked what the price of gold was.
 */

function formatElapsed(createdAt: number, until: number): string {
  const total = Math.max(0, Math.floor(until - createdAt))
  if (total < 60) return `${total}s`
  const minutes = Math.floor(total / 60)
  const seconds = total % 60
  if (minutes < 60) return `${minutes}m ${seconds}s`
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`
}

// abbrev keeps big token counts short so a whole spend line fits the chip:
// 940 → "940", 12_310 → "12.3k", 1_850_000 → "1.85M".
function abbrev(n: number): string {
  if (n < 1000) return `${n}`
  if (n < 1_000_000) {
    const k = n / 1000
    return `${k < 10 ? k.toFixed(1) : Math.round(k)}k`
  }
  return `${(n / 1_000_000).toFixed(2)}M`
}

// formatCost shows more precision for the small sums a single task runs up.
function formatCost(dollars: number): string {
  return `$${dollars < 1 ? dollars.toFixed(3) : dollars.toFixed(2)}`
}

/**
 * How many finished tasks stay visible before the rest fold away. Enough to see
 * what just happened, not so many that the column becomes a log.
 */
const SHOWN_WHEN_DONE = 4

/**
 * isFleet reports whether this chip stands for a whole batch rather than one
 * agent, in which case it shows a count instead of an activity line: there is
 * no single thing being done to describe, and what somebody wants to know is
 * how far along it is.
 */
function isFleet(d: Delegation): boolean {
  return d.kind === 'fleet'
}

/**
 * How far along a batch is, as a bar. It is a proportion of ROWS, not of time:
 * five agents each doing a different job finish at five different moments, and
 * the only honest thing to draw is how many are back.
 */
function FleetBar({ done, agents }: { done: number; agents: number }) {
  const proportion = agents > 0 ? Math.min(1, done / agents) : 0
  return (
    <div className="h-1 w-full overflow-hidden rounded-full bg-muted">
      <div
        className="h-full rounded-full bg-foreground/40 transition-[width] duration-500"
        style={{ width: `${proportion * 100}%` }}
      />
    </div>
  )
}

export function DelegationRail({
  delegations,
  onCancel,
}: {
  delegations: Delegation[]
  onCancel?: (d: Delegation) => void
}) {
  const [showAll, setShowAll] = useState(false)

  // A ticking clock, only while something is actually running. A column of
  // finished work has nothing to count, and a timer that keeps firing to redraw
  // the same numbers is a tab that never idles.
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000))
  const live = delegations.some((d) => !isTerminalDelegation(d.status))
  useEffect(() => {
    if (!live) return
    const timer = setInterval(() => setNow(Math.floor(Date.now() / 1000)), 1000)
    return () => clearInterval(timer)
  }, [live])

  if (delegations.length === 0) return null

  // Newest first, as the server sent them. Anything still going keeps its card;
  // what has finished becomes a line, and past a handful the older lines fold
  // away. Six identical cards for six finished tasks is not a record, it is
  // wallpaper.
  const running = delegations.filter((d) => !isTerminalDelegation(d.status))
  const done = delegations.filter((d) => isTerminalDelegation(d.status))
  const visible = showAll ? done : done.slice(0, SHOWN_WHEN_DONE)
  const folded = done.length - visible.length

  return (
    // Inside the chat room, below the header: no label, no border, just chips.
    // Hidden below md (~768px), where the chat needs the whole width.
    <aside className="hidden w-64 shrink-0 flex-col gap-1.5 overflow-y-auto p-3 md:flex">
      {running.map((d) => (
        <RunningCard key={d.id} d={d} now={now} onCancel={onCancel} />
      ))}

      {running.length > 0 && done.length > 0 && <div className="my-1 border-t border-border/60" />}

      {visible.map((d) => (
        <DoneRow key={d.id} d={d} />
      ))}

      {folded > 0 && (
        <button
          type="button"
          onClick={() => setShowAll(true)}
          className="px-2 py-1 text-left text-xs text-muted-foreground hover:text-foreground"
        >
          {folded} earlier
        </button>
      )}
    </aside>
  )
}

/**
 * Something still happening: worth a card, because it is the thing somebody is
 * waiting on. The agent, what it is doing, how long it has been, what it has
 * spent.
 *
 * "What it is doing" is the agent's own ACTIVITY line, which is written to be
 * read by a person. Its RESULT never appears here: that is bare technical fact
 * for the Gateway, which reads it and tells the person what it means. A chip
 * carrying it puts "```json" in front of somebody who asked about the price of
 * gold.
 */
function RunningCard({
  d,
  now,
  onCancel,
}: {
  d: Delegation
  now: number
  onCancel?: (d: Delegation) => void
}) {
  const waiting = d.status === 'waiting_approval'
  const p = d.progress
  const fleet = isFleet(d)
  const tokens = p?.tokens
  const tin = p?.tokens_in
  const tout = p?.tokens_out
  const cost = p?.cost
  return (
    <div
      data-chip={fleet ? 'fleet' : 'agent'}
      data-chip-id={d.id}
      // Running or over, said outright. A chip that stays after its work
      // finishes means COUNTING chips cannot tell "it showed up while the agent
      // was working" from "it showed up once the agent was done", which is the
      // difference between the feature working and the bug we shipped. The
      // shapes differ in every visible way and in nothing a test can hold on to.
      data-chip-state="running"
      className="flex flex-col gap-1 rounded-lg border bg-background p-3 text-sm shadow-sm"
    >
      <div className="flex min-w-0 items-center gap-2">
        <StatusMark status={d.status} size={5} />
        <span className="min-w-0 flex-1 truncate font-medium">{d.name}</span>
        {d.created_at ? (
          <span className="shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
            {formatElapsed(d.created_at, now)}
          </span>
        ) : null}
        {onCancel ? (
          <button
            type="button"
            onClick={() => onCancel(d)}
            aria-label={fleet ? 'Stop these agents' : 'Stop this agent'}
            title={fleet ? 'Stop these agents' : 'Stop this agent'}
            className="shrink-0 cursor-pointer rounded p-0.5 text-muted-foreground/60 hover:bg-muted hover:text-foreground"
          >
            <StopMark />
          </button>
        ) : null}
      </div>
      <span className="truncate text-xs text-muted-foreground">
        {fleet
          ? `${d.done ?? 0} of ${d.agents ?? 0} finished`
          : waiting
            ? 'Waiting for your approval'
            : p?.activity || 'Working…'}
      </span>
      {fleet ? <FleetBar done={d.done ?? 0} agents={d.agents ?? 0} /> : null}
      {typeof tokens === 'number' ? (
        <div className="flex items-baseline justify-between gap-2 font-mono text-[11px] tabular-nums text-muted-foreground/80">
          <span className="min-w-0 truncate">
            {abbrev(tokens)} tok
            {typeof tin === 'number' && typeof tout === 'number' ? (
              <span className="text-muted-foreground/60"> ↑{abbrev(tin)} ↓{abbrev(tout)}</span>
            ) : null}
          </span>
          {typeof cost === 'number' && cost > 0 ? (
            <span className="shrink-0 text-muted-foreground">{formatCost(cost)}</span>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}

/**
 * The stop control: a square, because that is what stopping looks like
 * everywhere else and a chip is too small to explain itself.
 */
function StopMark() {
  return (
    <svg viewBox="0 0 12 12" className="h-3 w-3" aria-hidden="true">
      <rect x="2.5" y="2.5" width="7" height="7" rx="1" fill="currentColor" />
    </svg>
  )
}

/**
 * Something that is over: a line, not a card.
 *
 * Still a chip, so it reads as a thing that happened rather than text floating
 * in the gutter, but a SMALLER one: a finished task must be cheaper to show
 * than a running one, or keeping the record costs as much as doing the work,
 * and six full cards is a wall of identical boxes that hides the one thing
 * still happening.
 *
 * What it says is who ran and how long it took. What it did is the Gateway's to
 * tell.
 */
function DoneRow({ d }: { d: Delegation }) {
  const took = d.created_at && d.completed_at ? formatElapsed(d.created_at, d.completed_at) : null
  const fleet = isFleet(d)
  return (
    // px-3 to match the running card, so the marks and the names line up down
    // the column: the chips differ in HEIGHT, which is the point, and agree on
    // every other edge, which is what stops that reading as untidiness.
    <div
      data-chip={fleet ? 'fleet' : 'agent'}
      data-chip-id={d.id}
      data-chip-state="done"
      className="flex min-w-0 items-center gap-2 rounded-lg border border-border/60 bg-muted/20 px-3 py-2 text-xs"
    >
      <StatusMark status={d.status} />
      <span className="min-w-0 flex-1 truncate text-muted-foreground">{d.name}</span>
      {fleet && d.agents ? (
        <span className="shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground/70">
          {d.done ?? 0}/{d.agents}
        </span>
      ) : null}
      {took ? (
        <span className="shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground/70">
          {took}
        </span>
      ) : null}
    </div>
  )
}
