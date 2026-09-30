import { useEffect, useLayoutEffect, useRef, useState, type PointerEvent, type ReactNode } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import { ChevronLeft, ChevronRight, Loader2, X } from 'lucide-react'
import { StatusMark } from '@/components/ui/ai/status-mark'
import { MessageItem, IDLE, type ChatTheme } from '@/components/MessageItem'
import { abbrev, formatCost, formatElapsed } from '@/components/DelegationRail'
import type { ChatMessage } from '@lib/chat-types'
import type { Delegation } from '@lib/delegations'
import { batchSpend, fleetOf, isFinished, type AgentRun } from '@lib/agent-watch'
import { useAgentWork, useFleetRuns } from '@lib/use-agent-watch'

/**
 * An agent opened from its chip, to read and not to talk to: what it was asked,
 * everything it did as the chat draws a conversation, and what it handed back.
 * A batch opens as a list with a row per agent, and a row opens that agent.
 *
 * ONE geometry for every view, because moving between them is the whole
 * interaction: the same 24px edge on both sides of everything, and a header of
 * one fixed height whatever it holds, so opening an agent from its batch
 * changes the content and never the frame around it. Every mark is centred on
 * the whole row it stands for rather than on its first line.
 *
 * The window scrolls it, never an inner region, so there is no scrollbar inside
 * a scrollbar. The header stays put while the work scrolls under it, and like
 * the chat it opens at the latest and keeps up while you are there.
 */

/** The facts of a subtitle, in one line, the empty ones left out. */
function line(...parts: (string | null | undefined | false)[]): string {
  return parts.filter(Boolean).join(' · ')
}

/**
 * What something has spent, as the chip says it ("364k tokens · $0.12"), or
 * nothing when nothing was counted: a run from before its agent counted its
 * spend says nothing rather than "0 tokens", which would be untrue.
 */
function spent(tokens?: number, cost?: number): string | false {
  if (!tokens) return false
  return line(`${abbrev(tokens)} tokens`, typeof cost === 'number' && cost > 0 && formatCost(cost))
}

/** A batch's line: how many are back, how long, and what they have spent between them. */
function fleetLine(done: number, agents: number, time: string | null, runs: AgentRun[] | null): string {
  const total = runs ? batchSpend(runs) : null
  return line(`${done} of ${agents} finished`, time, total && spent(total.tokens, total.cost))
}

/**
 * One agent of a batch, as a row. Led by its task, because one agent given
 * several tasks has the same name on every row, and numbered so two with
 * similar tasks can still be told apart.
 */
function RunRow({ run, number, named, time, onOpen }: { run: AgentRun; number: number; named: boolean; time: string | null; onOpen: () => void }) {
  return (
    <button
      type="button"
      onClick={onOpen}
      data-agent-block={run.id}
      className="flex w-full cursor-pointer items-center gap-3 px-6 py-3 text-left transition-colors hover:bg-muted/40"
    >
      {/* In a slot as wide as the header's 20px mark, so every mark in the
          modal stands on one centre line and every title starts at one edge.
          Left-aligned, the 16px marks sat 2px left of the header's centre
          and their text 4px left of its (measured on the installed app). */}
      <span className="flex w-5 shrink-0 justify-center">
        <StatusMark status={run.status} />
      </span>
      <span className="min-w-0 flex-1">
        <span className="block truncate text-sm leading-5">{run.task.replace(/\s+/g, ' ')}</span>
        <span className="block truncate text-xs leading-4 text-muted-foreground">
          {line(`Agent ${number}`, named && run.name, time, spent(run.tokens))}
        </span>
      </span>
      <ChevronRight className="size-4 shrink-0 text-muted-foreground/70" />
    </button>
  )
}

/** One step of an agent's work, drawn exactly as the chat draws it. */
function Step({ message, shown }: { message: ChatMessage; shown: Shown }) {
  return (
    <MessageItem
      message={message}
      activity={IDLE}
      showReasoning={shown.showReasoning}
      showTools={shown.showTools}
      theme={shown.theme}
      lang={shown.lang}
    />
  )
}

interface AgentViewProps {
  /** The chip that was opened, kept current by the socket; null when closed. */
  chip: Delegation | null
  chatId: string | null
  historyEndpoint: string
  showReasoning: boolean
  showTools: boolean
  theme?: ChatTheme
  lang?: Record<string, string>
  onClose: () => void
}

export function AgentView({ chip, chatId, historyEndpoint, showReasoning, showTools, theme, lang, onClose }: AgentViewProps) {
  const scroller = useRef<HTMLDivElement>(null)
  const fleetId = chip ? fleetOf(chip) : null

  // Radix closes on a press outside the content, and the content fills the
  // window, so the empty space around the panel says so itself.
  function pressedTheBackdrop(event: PointerEvent<HTMLDivElement>) {
    if (event.target === event.currentTarget) onClose()
  }

  return (
    <Dialog.Root open={!!chip && !!chatId} onOpenChange={(open) => !open && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/50" />
        <Dialog.Content
          ref={scroller}
          aria-describedby={undefined}
          className="fixed inset-0 z-50 overflow-y-auto outline-none"
        >
          <div
            onPointerDown={pressedTheBackdrop}
            className="flex min-h-full items-start justify-center p-4 sm:px-8 sm:py-[8vh]"
          >
            {/* Clipped to its own corners, so nothing inside (a row's hover on
                the last row) can paint square over the rounded edge. `clip`
                and not `hidden`: hidden would make the panel a scroll
                container, and the header would stop being pinned to the
                window as it scrolls. The corners are 8px, written out: this
                theme sets --radius to 0.625rem (index.css), so rounded-lg is
                10px here, measured so on the installed application. */}
            <div
              data-agent-view
              // The raised surface, as the chat's other floating panels are
              // (the select menu, the chart tooltip): in the dark theme it is
              // lighter than the page, which is what lifts it off the page, and
              // the page's own colour left it looking cut out of the backdrop.
              // Two shadows: a tight one that draws the edge and a deep one
              // that lifts it. The dark theme's are stronger, because black on
              // a dimmed dark page shows far less.
              className="w-full max-w-[720px] overflow-clip rounded-[8px] border border-border bg-popover text-popover-foreground shadow-[0_2px_6px_-1px_rgb(0_0_0/0.2),0_32px_64px_-12px_rgb(0_0_0/0.55)] dark:shadow-[0_2px_6px_-1px_rgb(0_0_0/0.6),0_32px_64px_-12px_rgb(0_0_0/0.85)]"
            >
              {chip && chatId ? (
                fleetId ? (
                  <FleetPanel
                    key={chip.id}
                    chip={chip}
                    fleetId={fleetId}
                    chatId={chatId}
                    historyEndpoint={historyEndpoint}
                    scroller={scroller}
                    shown={{ showReasoning, showTools, theme, lang }}
                  />
                ) : (
                  <AgentPanel
                    key={chip.id}
                    runId={chip.id}
                    chatId={chatId}
                    historyEndpoint={historyEndpoint}
                    status={chip.status}
                    name={chip.name}
                    createdAt={chip.created_at}
                    completedAt={chip.completed_at}
                    activity={chip.progress?.activity}
                    spend={chip.progress}
                    scroller={scroller}
                    shown={{ showReasoning, showTools, theme, lang }}
                  />
                )
              ) : null}
            </div>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}

interface Shown {
  showReasoning: boolean
  showTools: boolean
  theme?: ChatTheme
  lang?: Record<string, string>
}

/** A clock that ticks only while something is running. */
function useNow(live: boolean): number {
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000))
  useEffect(() => {
    if (!live) return
    const timer = setInterval(() => setNow(Math.floor(Date.now() / 1000)), 1000)
    return () => clearInterval(timer)
  }, [live])
  return now
}

/** How long it has been going, or how long it took. */
function took(now: number, createdAt?: number, completedAt?: number): string | null {
  if (!createdAt) return null
  return formatElapsed(createdAt, completedAt || now)
}

/** What a run is doing, in the words the chip uses. */
function standing(status: string, activity?: string): string {
  switch (status) {
    case 'waiting_approval':
      return 'Waiting for your approval in the chat'
    case 'done':
      return 'Finished'
    case 'failed':
      return 'Did not finish'
    case 'cancelled':
      return 'Stopped'
    default:
      return activity || 'Working…'
  }
}

/**
 * Keeps the window at the end while the reader has put it there: new work as
 * it arrives stays in view for somebody reading the end, and nobody else is
 * moved. It opens at the TOP and stays there: scrolling to the end once the
 * work had arrived moved everything on screen the moment it was drawn (the
 * test "opens at the top and stays there when its work arrives").
 */
function useFollow(scroller: React.RefObject<HTMLDivElement | null>, content: React.RefObject<HTMLDivElement | null>, ready: boolean) {
  // Only once the reader scrolls there themselves.
  const atEnd = useRef(false)
  useEffect(() => {
    const el = scroller.current
    if (!el) return
    const onScroll = () => {
      atEnd.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [scroller])
  useEffect(() => {
    const el = scroller.current
    const body = content.current
    if (!ready || !el || !body) return
    const follow = new ResizeObserver(() => {
      if (atEnd.current) el.scrollTop = el.scrollHeight
    })
    follow.observe(body)
    return () => follow.disconnect()
  }, [ready, scroller, content])
}

const iconButton =
  'flex size-8 shrink-0 cursor-pointer items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground'

/**
 * The one header every view has: a fixed 64px, whatever it holds. The mark and
 * the two lines beside it are centred on the header as a whole, the back arrow
 * (when there is one) and the close button mirror each other at the two edges,
 * and a batch's progress is drawn ON the header's bottom line, so showing it
 * costs no height at all.
 */
function Header({
  status,
  title,
  subtitle,
  onBack,
  progress,
  loading,
}: {
  status: string
  title: string
  subtitle: string
  onBack?: () => void
  /** A batch's share of agents back, 0 to 1. */
  progress?: number
  /**
   * What the modal shows has not arrived yet. The header is whole already (all
   * of it comes from the chip) and its bottom line sweeps, the way the
   * console's modals say they are loading, so nothing is written into the body
   * for the work to push away when it lands.
   */
  loading?: boolean
}) {
  return (
    // The bottom line is an inset shadow and not a border, so all 64px are the
    // header's own and its middle is a whole pixel (32). With a 1px border the
    // middle was 31.5, and the running spinner, centred there, turned on a
    // half pixel (measured at y 94.5 on the installed application).
    <header
      data-agent-header
      className="sticky top-0 z-10 flex h-16 items-center gap-3 bg-popover px-6 shadow-[inset_0_-1px_0_var(--border)]"
    >
      {loading ? (
        <span role="progressbar" aria-label="Loading" className="absolute inset-x-0 bottom-0 h-px overflow-hidden">
          <span className="block h-full w-1/3 animate-modal-loading bg-foreground/50" />
        </span>
      ) : null}
      {onBack ? (
        <button type="button" onClick={onBack} aria-label="Back to all agents" className={`-ml-2 ${iconButton}`}>
          <ChevronLeft className="size-4" />
        </button>
      ) : null}
      <StatusMark status={status} size={5} />
      <div className="min-w-0 flex-1">
        <Dialog.Title className="truncate text-[15px] font-semibold leading-5">{title}</Dialog.Title>
        <p className="truncate text-xs leading-4 text-muted-foreground">{subtitle}</p>
      </div>
      <Dialog.Close className={`-mr-2 ${iconButton}`}>
        <X className="size-4" />
        <span className="sr-only">Close</span>
      </Dialog.Close>
      {!loading && progress !== undefined ? (
        <span
          role="progressbar"
          aria-label="Agents finished"
          aria-valuenow={Math.round(progress * 100)}
          // On the header's own bottom line, the inset shadow above, where the
          // loader is: -bottom-px was for the border this replaced, and would
          // now draw the bar a pixel below the header.
          className="absolute inset-x-0 bottom-0 h-px overflow-hidden"
        >
          {/* Exactly the border's own line, one pixel, a shade darker where
              it is filled: progress, not a second border. */}
          <span
            className="block h-full bg-foreground/25 transition-[width] duration-500"
            style={{ width: `${Math.min(1, progress) * 100}%` }}
          />
        </span>
      ) : null}
    </header>
  )
}

/** One of an agent's three sections: the same label, the same edges. */
function Section({ label, tone, result, children }: { label: string; tone?: 'failed' | 'waiting'; result?: boolean; children: ReactNode }) {
  const colour =
    tone === 'failed' ? 'text-destructive' : tone === 'waiting' ? 'text-amber-700 dark:text-amber-400' : 'text-muted-foreground'
  return (
    <section data-agent-result={result ? '' : undefined} className="border-b border-border px-6 py-5 last:border-b-0">
      <h3 className={`mb-2.5 text-xs font-medium ${colour}`}>{label}</h3>
      {children}
    </section>
  )
}

/** A long task, three lines of it until asked for the rest. */
function Task({ text }: { text: string }) {
  const [whole, setWhole] = useState(false)
  const [long, setLong] = useState(false)
  const box = useRef<HTMLParagraphElement>(null)
  useLayoutEffect(() => {
    const el = box.current
    if (el && !whole) setLong(el.scrollHeight > el.clientHeight + 1)
  }, [text, whole])
  return (
    <>
      <p ref={box} className={`whitespace-pre-wrap text-sm leading-relaxed text-foreground/90 ${whole ? '' : 'line-clamp-3'}`}>
        {text}
      </p>
      {long || whole ? (
        <button
          type="button"
          onClick={() => setWhole(!whole)}
          className="mt-1.5 cursor-pointer text-xs font-medium text-muted-foreground hover:text-foreground"
        >
          {whole ? 'Show less' : 'Show all'}
        </button>
      ) : null}
    </>
  )
}

interface AgentPanelProps {
  runId: string
  chatId: string
  historyEndpoint: string
  status: string
  name: string
  createdAt?: number
  completedAt?: number
  activity?: string
  spend?: Delegation['progress']
  scroller: React.RefObject<HTMLDivElement | null>
  shown: Shown
  /** Set when this agent is one of a batch: which of how many, and the way back. */
  member?: { number: number; of: number; onBack: () => void }
}

/** One agent: Task, Work, Result. */
function AgentPanel({ runId, chatId, historyEndpoint, status, name, createdAt, completedAt, activity, spend, scroller, shown, member }: AgentPanelProps) {
  const { work, failed } = useAgentWork(historyEndpoint, chatId, runId, status)
  const content = useRef<HTMLDivElement>(null)
  useFollow(scroller, content, work !== null)
  const live = !isFinished(status)
  const now = useNow(live)
  const run = work?.run
  const time = took(now, createdAt ?? run?.created_at, completedAt ?? run?.completed_at)

  const subtitle = line(
    member && `Agent ${member.number} of ${member.of}`,
    standing(status, activity),
    time,
    spent(spend?.tokens, spend?.cost),
  )

  // What it handed back is shown as the Result, and not also as the last of
  // its work: the same words twice is exactly what this layout avoids.
  const result = work?.messages.find((m) => m.id === work.result_id)
  const steps = work?.messages.filter((m) => m.id !== work.result_id) ?? []

  return (
    <>
      <Header status={status} title={name} subtitle={subtitle} onBack={member?.onBack} loading={!work && !failed} />
      <div ref={content}>
        {run?.task ? (
          <Section label="Task">
            <Task text={run.task} />
          </Section>
        ) : null}

        {steps.length > 0 ? (
          <Section label="Work">
            <div className="flex flex-col gap-3">
              {steps.map((message) => (
                <div key={message.id} data-agent-step={message.id}>
                  <Step message={message} shown={shown} />
                </div>
              ))}
            </div>
          </Section>
        ) : null}

        {/* Nothing until the work is here: the header's line is saying so. */}
        {work || failed ? (
          <Outcome status={status} activity={activity} failed={failed} result={result} error={run?.error} shown={shown} />
        ) : null}
      </div>
    </>
  )
}

/**
 * The last section, always there and always in the same place: what the agent
 * handed back, or why it did not, or that it is still working on it.
 */
function Outcome({
  status,
  activity,
  failed,
  result,
  error,
  shown,
}: {
  status: string
  activity?: string
  failed: boolean
  result?: ChatMessage
  error?: string
  shown: Shown
}) {
  if (failed) {
    return (
      <Section label="Result">
        <p className="text-sm text-muted-foreground">This agent's work could not be loaded.</p>
      </Section>
    )
  }
  if (result) {
    return (
      <Section label="Result" result>
        <div data-agent-step={result.id}>
          <Step message={result} shown={shown} />
        </div>
      </Section>
    )
  }
  if (status === 'failed' || status === 'cancelled') {
    return (
      <Section label={standing(status)} tone="failed" result>
        <p className="text-sm leading-relaxed text-destructive">{error || 'It stopped before it returned anything.'}</p>
      </Section>
    )
  }
  if (status === 'waiting_approval') {
    // Paused, not working: no spinner, and where to go to let it go on.
    return (
      <Section label="Waiting" tone="waiting">
        <p className="text-sm text-amber-700 dark:text-amber-400">{standing(status)}</p>
      </Section>
    )
  }
  if (!isFinished(status)) {
    return (
      <Section label="Result">
        <Working text={standing(status, activity)} />
      </Section>
    )
  }
  return (
    <Section label="Result">
      <p className="text-sm text-muted-foreground">It finished without handing anything back.</p>
    </Section>
  )
}

function Working({ text }: { text: string }) {
  return (
    <div className="flex items-center gap-2 text-sm text-muted-foreground">
      <Loader2 className="size-4 animate-spin" />
      <span>{text}</span>
    </div>
  )
}

interface FleetPanelProps {
  chip: Delegation
  fleetId: string
  chatId: string
  historyEndpoint: string
  scroller: React.RefObject<HTMLDivElement | null>
  shown: Shown
}

/** A batch: a row per agent, and any row opens that agent. */
function FleetPanel({ chip, fleetId, chatId, historyEndpoint, scroller, shown }: FleetPanelProps) {
  const { runs, failed } = useFleetRuns(historyEndpoint, chatId, fleetId)
  const [open, setOpen] = useState<string | null>(null)
  const live = !isFinished(chip.status) || (runs ?? []).some((r) => !isFinished(r.status))
  const now = useNow(live)

  const at = open && runs ? runs.findIndex((r) => r.id === open) : -1
  if (runs && at >= 0) {
    const member = runs[at]
    return (
      <AgentPanel
        key={member.id}
        runId={member.id}
        chatId={chatId}
        historyEndpoint={historyEndpoint}
        status={member.status}
        name={member.name}
        createdAt={member.created_at}
        completedAt={member.completed_at}
        // What it is doing and what it has spent, off its own row, kept
        // current by the batch's pushes while it works.
        activity={member.activity}
        spend={{ tokens: member.tokens, cost: member.cost }}
        scroller={scroller}
        shown={shown}
        member={{ number: at + 1, of: runs.length, onBack: () => setOpen(null) }}
      />
    )
  }

  const agents = chip.agents ?? runs?.length ?? 0
  const done = chip.done ?? 0
  // One kind of agent reads the same on every row, so it is said once, in the
  // header; a mixed batch says on each row which agent it was.
  const mixed = !!runs && new Set(runs.map((r) => r.name)).size > 1

  return (
    <>
      <Header
        status={chip.status}
        title={chip.name}
        subtitle={fleetLine(done, agents, took(now, chip.created_at, chip.completed_at), runs)}
        // Only while it is going: a finished batch's full bar is just a
        // darker border, and the count beside the name already says so.
        progress={isFinished(chip.status) ? undefined : agents > 0 ? done / agents : 0}
        // Until its agents have arrived the same line says it is loading,
        // and the body is left empty rather than filled and then replaced.
        loading={!runs && !failed}
      />
      {failed ? (
        <p className="px-6 py-5 text-sm text-muted-foreground">These agents could not be loaded.</p>
      ) : !runs ? null : (
        <ul className="divide-y divide-border">
          {runs.map((run, i) => (
            <li key={run.id}>
              <RunRow
                run={run}
                number={i + 1}
                named={mixed}
                time={took(now, run.created_at, run.completed_at)}
                onOpen={() => setOpen(run.id)}
              />
            </li>
          ))}
        </ul>
      )}
    </>
  )
}
