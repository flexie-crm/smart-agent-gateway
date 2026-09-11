import { Fragment, useState } from 'react'
import {
  Check,
  Loader2,
  Eye,
  PencilLine,
  Globe,
  CreditCard,
  Trash2,
  KeyRound,
  ChevronRight,
  ShieldCheck,
} from 'lucide-react'
import type { ConfirmationRequest } from '@/lib/chat-types'
import { t } from '@lib/utils'
import { principalOf, secondaryOf, lookOf, type Accent, type RiskLook } from '@lib/confirm-proposal'
import { StatusMark } from './status-mark'

/**
 * Asking a person to approve something the assistant wants to do.
 *
 * The card answers three questions in the order somebody asks them: WHAT is
 * about to happen, HOW FAR does it reach, and exactly WHAT WOULD IT DO. It was
 * the last one the old card got wrong. It printed every argument as
 * `key: value` in break-all monospace, so a URL arrived shattered across five
 * ragged lines with no word left intact, and a headers blob buried the one
 * thing worth reading.
 *
 * # Colour is the colour of the consequence
 *
 * The card is not a severity rainbow and it is not black and white either.
 * Colour answers ONE question, and it is not which of the six levels this is:
 * that is what the words are for, and they are exact. Colour says whether to
 * stop. Blue is everything ordinary enough to read and get on with, red is what
 * cannot be taken back, and neutral is the case where nothing happens at all.
 *
 * # Nothing wraps anything it does not have to
 *
 * The icon had a rounded tile of its own, inside a heading band, inside the
 * card: three nested boxes to show one glyph. The tile is gone and the icon
 * sits on the band, which is a SECTION of the card rather than a box within it,
 * separated by a hairline. The card's own corners came down with it: a large
 * radius on an outer box makes everything it holds look like it is being held.
 *
 * # It measures ITSELF, not the window
 *
 * The card is a `@container`, and everything that moves keys off the card's own
 * width. A viewport breakpoint would be answering the wrong question: this card
 * sits in a chat column beside a sidebar and a task rail, so the same phone-sized
 * card can perfectly well appear on a 27 inch screen, and a rule that asked the
 * WINDOW how wide things are would lay it out for room it does not have.
 *
 * Wide enough and it is one row, title and scope side by side, three buttons
 * with the widest choice pushed to the far right. Narrower than its contents
 * need, it stacks: scope under the title, the two answers splitting a row at
 * thumb size, the third on a line of its own. Each part flips at the width where
 * that part stops fitting, which is why there are two thresholds and not one.
 *
 * # Nothing is filled
 *
 * The accent stops at the buttons. A solid block of colour down there is the
 * loudest thing on the card, and what it would be shouting is "press me", on a
 * card whose entire job is to make somebody stop and read. So Approve is an
 * OUTLINE: green, because that is what saying yes looks like, and quiet,
 * because agreeing should not be easier than the reading that precedes it.
 *
 * The vocabulary is the SERVER's (see confirm-proposal): the six risk levels a
 * tool declares, not the three the reference CRM used and this card used to
 * switch on while nothing ever matched.
 *
 * # Every control says it is a control
 *
 * Tailwind v4 dropped the preflight `cursor: pointer` on `<button>`, so a bare
 * button here read as dead text under the cursor. Restored once in the base
 * layer (index.css) rather than as a class on every button in the app, so the
 * buttons below carry nothing for it.
 */

interface ConfirmBlockProps {
  confirmation: ConfirmationRequest
  onRespond: (token: string, action: 'approved' | 'rejected' | 'approved_all') => void
  lang?: any
}

/** How much of the proposal is shown before it is folded. */
const PREVIEW_LINES = 6
const PREVIEW_CHARS = 460

/** The glyph for a kind of action. Named by what it does, not by the tool. */
const ICONS = {
  read: Eye,
  write: PencilLine,
  outside: Globe,
  money: CreditCard,
  delete: Trash2,
  access: KeyRound,
}

/**
 * One accent, everywhere it lands: the card's edge, the heading band, and the
 * icon and scope line that share a colour because they say the same thing.
 *
 * Written out per accent rather than composed from a colour name, because
 * Tailwind resolves class names at build time and a string built at runtime
 * produces no CSS at all.
 */
const ACCENTS: Record<Accent, { edge: string; head: string; word: string }> = {
  neutral: {
    edge: 'border-border',
    head: 'bg-muted/40',
    word: 'text-muted-foreground',
  },
  blue: {
    edge: 'border-blue-200 dark:border-blue-900/60',
    head: 'bg-blue-50 dark:bg-blue-950/30',
    word: 'text-blue-700 dark:text-blue-400',
  },
  rose: {
    edge: 'border-rose-200 dark:border-rose-900/60',
    head: 'bg-rose-50 dark:bg-rose-950/30',
    word: 'text-rose-700 dark:text-rose-400',
  },
}

/**
 * The proposal itself, wrapped rather than shattered.
 *
 * `overflow-wrap: anywhere` breaks a long token only where it must;
 * `break-all` breaks wherever the line happens to end, which is what turned a
 * URL into five fragments. Clamped by LINES and by CHARACTERS both: a
 * one-paragraph body has no newlines at all, and a line-only clamp lets it grow
 * to the height of the conversation.
 */
function Preview({ text, lang }: { text: string; lang?: any }) {
  const [all, setAll] = useState(false)
  const lines = text.split('\n')
  const long = lines.length > PREVIEW_LINES || text.length > PREVIEW_CHARS

  let shown = text
  if (!all && long) {
    shown = lines.slice(0, PREVIEW_LINES).join('\n')
    if (shown.length > PREVIEW_CHARS) shown = shown.slice(0, PREVIEW_CHARS).trimEnd() + '…'
  }

  return (
    <div className="mt-3">
      <pre className="max-h-80 overflow-y-auto whitespace-pre-wrap rounded-md border bg-muted/40 px-3 py-2.5 font-mono text-xs leading-relaxed text-foreground/80 [overflow-wrap:anywhere]">
        {shown}
      </pre>
      {long && (
        <button
          type="button"
          onClick={() => setAll((v) => !v)}
          className="mt-1.5 text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
        >
          {all
            ? t('show_less', lang, 'Show less')
            : lines.length > PREVIEW_LINES
              ? `${t('show_all_lines', lang, 'Show all')} ${lines.length} ${t('lines', lang, 'lines')}`
              : t('show_full', lang, 'Show the whole thing')}
        </button>
      )}
    </div>
  )
}

/**
 * Everything that is not the proposal itself: the method, the headers, the
 * settings around it.
 *
 * FOLDED, because it is not the decision. It used to sit open under the URL as
 * a faint mono line reading `method=GET headers={"User-Agent":"Mozilla/5.0"…}`,
 * which is a request somebody is being asked to approve rendered as a debug
 * dump: it says nothing at a glance, and it competes with the one line that
 * does. So it is a count and a toggle, and it opens when somebody wants it.
 *
 * It is never REMOVED, only folded. What is about to run has to be readable in
 * full by the person authorising it, or the card is asking them to take our
 * word for it.
 */
function Rest({ details, skip, lang }: { details: Record<string, unknown>; skip?: string; lang?: any }) {
  const [open, setOpen] = useState(false)
  const rest = secondaryOf(details, skip)
  if (rest.length === 0) return null

  return (
    <div className="mt-2.5">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="inline-flex items-center gap-1 text-xs text-muted-foreground transition-colors hover:text-foreground"
      >
        <ChevronRight className={`size-3.5 transition-transform ${open ? 'rotate-90' : ''}`} />
        {t('confirm_details', lang, 'Details')}
        <span className="text-muted-foreground/60">({rest.length})</span>
      </button>

      {open && (
        // Label left, value right, aligned down the column so the shape of the
        // request is visible before any of it is read. The value wraps rather
        // than shattering, same rule as the proposal above it.
        <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 rounded-md border bg-muted/30 px-3 py-2.5">
          {rest.map(([k, v]) => (
            <Fragment key={k}>
              <dt className="font-mono text-[11px] leading-relaxed text-muted-foreground">{k}</dt>
              <dd className="min-w-0 font-mono text-[11px] leading-relaxed text-foreground/80 [overflow-wrap:anywhere]">
                {v}
              </dd>
            </Fragment>
          ))}
        </dl>
      )}
    </div>
  )
}

export function ConfirmBlock({ confirmation, onRespond, lang }: ConfirmBlockProps) {
  const [submitting, setSubmitting] = useState(false)
  const { token, title, description, severity, details, status } = confirmation

  const act = (action: 'approved' | 'rejected' | 'approved_all') => {
    setSubmitting(true)
    onRespond(token, action)
  }

  // Already answered: the same shape as a tool row, because that is what it now
  // is. A decision is a fact about the past, and it belongs in the transcript's
  // ordinary rhythm rather than in a bordered pill of its own with margins on
  // top of the rhythm: three answered approvals in a row used to be three
  // floating capsules where three quiet lines were wanted.
  //
  // A LEAF, carrying no vertical margin. The conversation's own `space-y-3` is
  // the only source of spacing between it and its neighbours, exactly as for a
  // tool row, so an approval and a tool call sit on the same grid.
  if (status && status !== 'pending') {
    const answer =
      status === 'approved'
        ? { mark: 'done', word: t('confirm_approved', lang, 'Approved'), tone: 'text-emerald-600 dark:text-emerald-500' }
        : status === 'rejected'
          ? { mark: 'failed', word: t('confirm_rejected', lang, 'Declined'), tone: 'text-muted-foreground' }
          : { mark: 'expired', word: t('confirm_timeout', lang, 'Expired'), tone: 'text-muted-foreground' }
    return (
      <div className="flex items-center gap-1.5 text-sm text-muted-foreground">
        <StatusMark status={answer.mark} />
        <span className={`shrink-0 font-medium ${answer.tone}`}>{answer.word}</span>
        <span className="min-w-0 truncate">{title}</span>
      </div>
    )
  }

  const look: RiskLook = lookOf(severity)
  const c = ACCENTS[look.accent]
  const Icon = ICONS[look.icon]
  const principal = principalOf(details)

  return (
    <div className={`@container my-3 overflow-hidden rounded-lg border ${c.edge} bg-background shadow-sm`}>
      {/* A SECTION of the card, not a box inside it: a band of colour closed by
          a hairline, with the icon sitting on it rather than in a tile of its
          own.

          Title over scope, ALWAYS, at every width. They shared a row and wrapped
          when they had to, and on a phone that came out cracked down the middle:
          the title folded into a narrow column on the left while the scope ran
          the full width on the right, two ragged blocks at odd sizes with no
          relation between them. A heading with a line under it has one shape,
          and it is the same shape on a phone and on a desktop. */}
      <div
        className={`flex items-start gap-2.5 border-b ${c.edge} ${c.head} px-3.5 py-2.5 @lg:items-center`}
      >
        <Icon className={`mt-0.5 size-4 shrink-0 ${c.word} @lg:mt-0`} strokeWidth={2.25} />
        {/* One row where both fit, stacked where they do not. It wraps rather
            than crushing either: on a long title and a long scope the scope
            simply falls to the second line, which is the stacked shape again. */}
        <div className="min-w-0 flex-1 @lg:flex @lg:flex-wrap @lg:items-center @lg:justify-between @lg:gap-x-3">
          <h4 className="text-sm font-semibold leading-5">{title}</h4>
          <p className={`mt-0.5 text-[11px] font-medium leading-4 ${c.word} @lg:mt-0`}>
            {t(look.scopeKey, lang, look.scope)}
          </p>
        </div>
      </div>

      <div className="p-3.5">
        {description && (
          <p className="text-sm leading-relaxed text-foreground/80">{description}</p>
        )}

        {principal ? (
          <>
            <Preview text={principal[1]} lang={lang} />
            {details && <Rest details={details} skip={principal[0]} lang={lang} />}
          </>
        ) : (
          details && Object.keys(details).length > 0 && <Rest details={details} lang={lang} />
        )}

        {/* Where all three fit, the two answers take the room their words need
            and the third is pushed to the far right, as far from Approve as the
            card allows. Where they do not, the two answers SPLIT a row, so they
            come out the same size, the same shape and big enough to hit with a
            thumb, and the third takes a line of its own rather than being
            stranded against the right edge of an empty one.

            They wrap rather than overflow: three buttons on a narrow card used
            to push the third straight through the card's own border. */}
        <div className="mt-4 flex flex-wrap items-center gap-2">
          {/* An outline, not a fill. Filled, it becomes the loudest thing on a
              card whose whole job is to make somebody stop and read. */}
          <button
            type="button"
            disabled={submitting}
            onClick={() => act('approved')}
            className="inline-flex h-10 flex-1 items-center justify-center gap-1.5 rounded-md border border-emerald-600/70 px-4 text-sm font-semibold text-emerald-700 @md:h-9 @md:flex-none transition-colors hover:bg-emerald-50 disabled:opacity-50 dark:border-emerald-500/60 dark:text-emerald-400 dark:hover:bg-emerald-950/40"
          >
            {submitting ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Check className="size-4" strokeWidth={2.75} />
            )}
            {t('confirm_approve', lang, 'Approve')}
          </button>

          <button
            type="button"
            disabled={submitting}
            onClick={() => act('rejected')}
            className="inline-flex h-10 flex-1 items-center justify-center gap-1.5 rounded-md border px-3.5 text-sm font-medium text-muted-foreground @md:h-9 @md:flex-none transition-colors hover:border-destructive/40 hover:text-destructive disabled:opacity-50"
          >
            {t('confirm_reject', lang, 'Decline')}
          </button>

          {/* The widest choice, and the quietest button, because it is the one
              that turns the asking off rather than answering a question.

              It says BOTH halves of what it does. "Stop asking this
              conversation" said only the second, in words that read like a
              setting rather than an answer, so the one button that also
              approves the card in front of somebody did not say so. It is the
              same switch as the shield in the composer, flipped from here. */}
          <button
            type="button"
            disabled={submitting}
            onClick={() => act('approved_all')}
            title={t(
              'confirm_approve_all_hint',
              lang,
              'This one goes ahead, and nothing else in this conversation stops to ask you. You can turn asking back on at the top of the chat.',
            )}
            className="group inline-flex h-9 w-full items-center justify-center gap-1.5 rounded-md px-2.5 text-xs font-medium text-muted-foreground @md:ml-auto @md:w-auto transition-colors hover:bg-muted hover:text-foreground disabled:opacity-50"
          >
            {/* The shield is the one in the composer: this button flips that
                switch, and wearing its icon says so without a sentence. */}
            <ShieldCheck className="size-3.5 shrink-0" />
            {/* Underlined, because with no border and no fill this was a line of
                small grey text sitting under two buttons, and it read as a
                caption on them rather than as a third thing you can press. An
                underline is the one mark that says "clickable" on its own. */}
            <span className="underline decoration-current/30 underline-offset-4 transition-colors group-hover:decoration-current">
              {t('confirm_approve_all', lang, 'Approve, and stop asking')}
            </span>
          </button>
        </div>
      </div>
    </div>
  )
}
