import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { useVirtualizer } from '@tanstack/react-virtual';
import { ArrowDownIcon } from 'lucide-react';
import type { ComponentProps, ReactNode } from 'react';
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from 'react';

/**
 * The conversation: at the bottom because of how it is LAID OUT, not because
 * something scrolled it there.
 *
 * That distinction is the whole of this file, and getting it wrong cost a day.
 * Every earlier version put the newest message at the top of a normal scroller
 * and then moved the view down to it in JavaScript: on open, again as each row
 * measured itself, again when an answer grew. Each of those corrections is a
 * frame in which the page moves under the reader, and no amount of care makes a
 * hundred of them look like none. It also meant the first thing anybody saw on
 * opening a conversation was the wrong end of it.
 *
 * `flex-direction: column-reverse` moves the problem out of JavaScript
 * altogether. In a reversed column the scroll origin IS the bottom: the browser
 * paints the newest message on the first frame, at rest, with nothing having
 * run. Measured in Chromium and in WebKit (which is what the desktop
 * applications use): at rest `scrollTop` is 0 and the top of the conversation
 * sits 1,699px above the fold. Scrolling up gives NEGATIVE offsets, and both
 * engines agree on that, which is what makes it safe to build on.
 *
 * Two consequences fall out for free, and both were code before:
 *
 *   - An answer growing does not push the view away. The bottom is the anchor,
 *     so a row getting taller grows upward and the newest words stay where they
 *     are. No follow, no re-pin, nothing to get wrong.
 *   - Loading older messages cannot move the reader. Older is FURTHER from the
 *     anchor, so in this coordinate space it is an append, and an append never
 *     shifts what is already placed.
 *
 * The virtual list is given that same coordinate space rather than the
 * browser's: offset 0 is the newest message and it grows as you go back. The two
 * hooks that read and write the scroll position are the only translation, and
 * everything above them (measurement, ranges, anchoring) is ordinary.
 */

const ESTIMATED_ROW = 120;
/** How near the newest message still counts as being at it, for the button. */
const AT_NEWEST = 100;
/** And what counts as being AT the bottom, for deciding whether to stay there.
 *  Strict on purpose: a generous number here is a reader who cannot leave. */
const AT_THE_BOTTOM = 2;
/** Rows built beyond the fold, so arriving at one is not waiting for one. */
const OVERSCAN = 8;
/** How near the far end asks for the page before it. */
const NEAR_THE_END = 5;

type Scrolling = {
  /** Whether the newest message is on screen. */
  atNewest: boolean;
  /** Take the reader to it. */
  toNewest: (behavior?: 'smooth' | 'auto') => void;
  /** The element that scrolls, once it exists. */
  viewport: { current: HTMLDivElement | null };
};

const ScrollingContext = createContext<Scrolling | null>(null);

const useScrolling = (): Scrolling => {
  const ctx = useContext(ScrollingContext);
  if (!ctx) throw new Error('this belongs inside <Conversation>');
  return ctx;
};

export type ConversationProps<T> = {
  /** The rows to show, OLDEST first, as a conversation reads. */
  items: T[];
  /** A row's identity, stable across renders. */
  idOf: (item: T) => string;
  renderItem: (item: T) => ReactNode;
  /** Which conversation these rows belong to. */
  conversation?: string | null;
  /** Whether there is a page before this one. */
  hasOlder?: boolean;
  loadingOlder?: boolean;
  onLoadOlder?: () => void;
  /** Shown over the top of the view while an older page is on its way. */
  notice?: ReactNode;
  /** Shown under the newest row: what the assistant is doing right now. */
  footer?: ReactNode;
  className?: string;
  /** The behaviours that need to know how it is scrolling. */
  children?: ReactNode;
};

export const Conversation = <T,>({
  items,
  idOf,
  renderItem,
  conversation,
  hasOlder = false,
  loadingOlder = false,
  onLoadOlder,
  notice,
  footer,
  className,
  children,
}: ConversationProps<T>) => {
  const viewport = useRef<HTMLDivElement | null>(null);
  const [atNewest, setAtNewest] = useState(true);

  // Oldest first: an ordinary top-down list.
  //
  // Measured from the NEWEST message, every token arriving pushed everything
  // above it and the view had to be scrolled back to hold the reader still:
  // 103 corrective scrollTo calls in 88 seconds while somebody sat perfectly
  // still, some overshooting a few pixels. The scrolling was the design, not a
  // fault on top of it. Counted from the oldest end there is nothing to
  // correct, because nothing above the arriving text moves.
  const rows = items;

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => viewport.current,
    estimateSize: () => ESTIMATED_ROW,
    getItemKey: (i) => {
      const row = rows[i];
      return row ? idOf(row) : `gone-${i}`;
    },
    overscan: OVERSCAN,
    // Holds the reader's row when an older PAGE arrives at the top, which is
    // the one event that still moves what is above them.
    anchorTo: 'end',
    followOnAppend: false,
  });

  const virtualItems = virtualizer.getVirtualItems();
  const total = virtualizer.getTotalSize();

  const toNewest = useCallback(
    (behavior: 'smooth' | 'auto' = 'auto') => {
      const el = viewport.current;
      if (!el) return;
      wasAtBottom.current = true;
      el.scrollTo({ top: el.scrollHeight, behavior });
    },
    [],
  );

  // AT the newest message, stay there: opening lands on it, and an answer
  // arriving carries the reader along. HELD rather than set once, because a
  // virtual list does not know its height until its rows are measured, so a
  // single assignment lands against an estimate and corrects in front of the
  // reader. In useLayoutEffect, so it is applied before the browser paints.
  // Only while atNewest: scrolled up, nothing here writes the scroll at all.
  // Were they at the bottom BEFORE this token was rendered?
  //
  // The whole difficulty is in the word "before". Asked afterwards, in an
  // effect or from React state, the answer is about a conversation that has
  // already grown: the reader is no longer at the bottom because a paragraph
  // was just added below them, so a rule that pins "if at the bottom" either
  // pins nobody, or pins everybody by using a threshold wide enough to cover
  // the new text, and then drags a reader who was deliberately reading back.
  // Both were shipped and both were wrong.
  //
  // This reads the element during RENDER, which is the one moment the DOM still
  // holds the previous commit: the tokens are not in it yet. It is the
  // getSnapshotBeforeUpdate question, asked the way a function component can.
  // A read with no side effect, so it is safe here.
  //
  // And the bottom means the BOTTOM, within a pixel or two, not within a
  // hundred: at a hundred, scrolling gently up from the end never escapes,
  // because every render puts you back. That was the pull somebody hit while
  // nothing was even streaming.
  const wasAtBottom = useRef(true);
  if (viewport.current) {
    const el = viewport.current;
    wasAtBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight <= AT_THE_BOTTOM;
  }
  useLayoutEffect(() => {
    const el = viewport.current;
    if (!el || !wasAtBottom.current || rows.length === 0) return;
    // Only when the conversation actually changed, and only if not already
    // there. Both halves are load-bearing.
    //
    // Run on every render instead, this re-enters: writing the scroll fires a
    // scroll event, which sets the state the jump button renders from, which
    // renders, which runs this again. A long conversation then never finishes
    // opening, which is not a slow page but a hung one, and it timed out six
    // specs on a click. Keyed on the height and the count, a render caused by
    // that state change does not re-run it, and a measurement that really did
    // move the end does.
    const end = el.scrollHeight - el.clientHeight;
    if (Math.abs(el.scrollTop - end) > 1) el.scrollTop = end;
  }, [total, rows.length, conversation]);

  const opened = useRef(conversation);
  useLayoutEffect(() => {
    if (opened.current === conversation) return;
    opened.current = conversation;
    wasAtBottom.current = true;
    setAtNewest(true);
  }, [conversation]);

  useEffect(() => {
    const el = viewport.current;
    if (!el) return;
    // Only what the jump button renders from. Whether to HOLD the bottom is a
    // different and stricter question, asked during render above.
    const read = () =>
      setAtNewest(el.scrollHeight - el.scrollTop - el.clientHeight < AT_NEWEST);
    read();
    el.addEventListener('scroll', read, { passive: true });
    return () => el.removeEventListener('scroll', read);
  }, []);

  // Older messages are at the top now.
  const nearest = virtualItems.length ? virtualItems[0].index : undefined;
  useEffect(() => {
    if (nearest === undefined || !hasOlder || loadingOlder) return;
    if (nearest <= NEAR_THE_END) onLoadOlder?.();
  }, [nearest, hasOlder, loadingOlder, onLoadOlder]);

  const scrolling: Scrolling = { atNewest, toNewest, viewport };

  return (
    <ScrollingContext.Provider value={scrolling}>
      <div className={cn('relative flex min-h-0 flex-col', className)} role="log">
        <div
          ref={viewport}
          className="fx-scroll flex flex-1 flex-col overflow-y-auto"
          style={{ overscrollBehavior: 'contain' }}
        >
          <div className="relative w-full shrink-0" style={{ height: total }}>
            <div
              className="absolute left-0 w-full"
              style={{ top: virtualItems[0]?.start ?? 0 }}
            >
              {virtualItems.map((row) => {
                const item = rows[row.index];
                if (!item) return null;
                return (
                  <div
                    key={row.key}
                    data-index={row.index}
                    ref={virtualizer.measureElement}
                    className="w-full"
                  >
                    <div className="mx-auto w-full max-w-[776px] px-4 pb-3">
                      {renderItem(item)}
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
          {footer && (
            <div className="mx-auto w-full max-w-[776px] shrink-0 px-4 pb-4">{footer}</div>
          )}
        </div>
        {/* Over the view rather than in it: a line that appears and disappears
            inside the scrolling content changes its height, which is a shove
            delivered exactly as the reader arrives at the top. */}
        {loadingOlder && notice && (
          <div className="pointer-events-none absolute inset-x-0 top-0 flex justify-center pt-2">
            {notice}
          </div>
        )}
        {children}
      </div>
    </ScrollingContext.Provider>
  );
};

// On mobile, when the keyboard opens the conversation shrinks to the band above
// the keys. The embed loader fires a 'fx-keyboard-open' window event on that
// rising edge; the newest message is where somebody wants to be. Renders nothing.
export const ConversationKeyboardAutoScroll = () => {
  const { toNewest } = useScrolling();
  useEffect(() => {
    const onKeyboardOpen = () => toNewest('auto');
    window.addEventListener('fx-keyboard-open', onKeyboardOpen);
    return () => window.removeEventListener('fx-keyboard-open', onKeyboardOpen);
  }, [toNewest]);
  return null;
};

/**
 * Saying something takes you to the newest message.
 *
 * Almost everything this used to do is now the layout's job: an answer arriving
 * no longer drags anybody anywhere, because the bottom is the anchor. What is
 * left is the one case that is genuinely a MOVE: you were reading history, you
 * said something, and you should be taken to it. Keyed on the last thing the
 * PERSON said rather than on a button, because there is more than one way to say
 * something (the composer, a suggested prompt, dictation) and all of them end as
 * a message from you.
 */
export const ConversationFollowsWhatYouSend = ({
  lastFromYou,
  conversation,
}: {
  /** The id of the newest message the person sent, if there is one. */
  lastFromYou?: string;
  /** Which conversation that message belongs to, so arriving can be told from sending. */
  conversation?: string | null;
}) => {
  const { toNewest } = useScrolling();
  const seen = useRef(lastFromYou);
  const opened = useRef(conversation);
  // Whether the transcript of the conversation we are in has arrived yet. The id
  // changes first and the messages follow, so "a different conversation" and
  // "its history has landed" are two moments, not one; without this, a history
  // arriving with something you said in it a week ago reads as you saying it
  // now, and the view animates to where it already is.
  const arrived = useRef(false);

  useEffect(() => {
    if (conversation !== opened.current) {
      opened.current = conversation;
      arrived.current = false;
      seen.current = lastFromYou;
      return;
    }
    if (lastFromYou === undefined || lastFromYou === seen.current) return;
    seen.current = lastFromYou;
    if (!arrived.current) {
      arrived.current = true;
      return; // the history landing: the layout has this
    }
    // Instant, and that is the fix rather than a preference.
    //
    // It was `smooth`, which asks WebKit to animate the journey over several
    // hundred milliseconds, and an animation is a scroll that is still happening
    // after the frame that started it. Send something, then reach for the wheel
    // to re-read what is above: the animation is still running and it drags you
    // back down while your hand says up. That is a tug of war, and the browser
    // wins it. It also hides from any measurement that counts scroll writes,
    // because it is ONE write and the pull arrives as native movement, which is
    // how it survived a day of instrumentation.
    //
    // A jump is over within the frame that starts it. Nothing to fight.
    toNewest('auto');
  }, [conversation, lastFromYou, toNewest]);
  return null;
};

/* Two things used to live here and are now the layout's.
 *
 * Holding the bottom when the composer grew, and letting go of it when somebody
 * opened a tool call to read it. Both were corrections for a list that chased
 * the newest message; in a reversed column the bottom is the origin, so a view
 * getting shorter cannot strand anybody and a row growing pushes the older words
 * up rather than pushing the reader down. They were kept as empty components for
 * a while, which meant the chat still rendered them and a tool row still fired an
 * event into nothing. */

export const ConversationScrollButton = ({
  className,
  ...props
}: ComponentProps<typeof Button>) => {
  const { atNewest, toNewest } = useScrolling();
  const go = useCallback(() => toNewest('auto'), [toNewest]);
  return (
    !atNewest && (
      <Button
        className={cn(
          'absolute bottom-4 left-[50%] translate-x-[-50%] rounded-full',
          className
        )}
        onClick={go}
        size="icon"
        type="button"
        variant="outline"
        // An arrow on its own says nothing to anybody who cannot see it, and it
        // was the only control here without a name.
        aria-label="Scroll to the latest message"
        {...props}
      >
        <ArrowDownIcon className="size-4" />
      </Button>
    )
  );
};
