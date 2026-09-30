import type { ReactNode } from "react";
import { Suspense, lazy } from "react";

import { cn } from "@/lib/utils";

// Its own chunk, fetched when somebody opens a file. See CodeHighlighted for
// the measurement that made this worth doing.
const Highlighted = lazy(() => import("./CodeHighlighted"));

/**
 * ONE SET OF METRICS, shared by every way a file is drawn.
 *
 * There are three: the plain text that is on screen while the highlighter's
 * chunk is still arriving, the highlighted text that replaces it, and the
 * textarea somebody types in. They sit in the same frame, beside the same line
 * numbers, and anything they disagree about is a layout shift.
 *
 * That is not a hypothetical. The first version of this let each of the three
 * carry its own gutter and its own padding, and what it did was visible: the
 * line numbers moved sideways about a second after a file opened, which is the
 * highlighted chunk landing and bringing different numbers with it.
 *
 * So the numbers are drawn once, here, beside whichever of the three is
 * showing, and every one of them is told the same line height in the same
 * units. LINE is `leading-5` and SIZE is `text-xs`, written as numbers because
 * the textarea's height has to be computed from them.
 */
const LINE = 20;
const SIZE = 12;
/** Wide enough for four digits, which is past the line cap below. */
const GUTTER = 44;

/**
 * One file of a package, read.
 *
 * Line numbers, colours for the language it is written in, and a range that can
 * be lit up, which is how the passages pane jumps into the file it came from.
 *
 * PLAIN FIRST, COLOURED A BEAT LATER. The highlighter is a chunk of its own and
 * the grammar is another, so neither is here on the frame a file opens. What is
 * on screen until they arrive is the same text at the same size in the same
 * place: only its colour is missing, and only for as long as a fetch takes.
 *
 * It is NOT the chat's code block. That one is a block inside an answer, with a
 * copy button and a deliberate delay so tokenising never runs on a frame a
 * virtualised list is scrolling. This is a file somebody opened and is looking
 * at: there is one, it fills the pane, and nothing is moving. What the two share
 * is the grammar loader and the theme, and those are copied (see
 * lib/code-language.ts).
 */
export function CodeView({
  path,
  text,
  highlight,
  maxLines,
}: {
  path: string;
  text: string;
  /** Lines to light up, 1-based and inclusive. */
  highlight: { from: number; to: number } | null;
  maxLines: number;
}) {
  const all = text.split("\n");
  const lines = all.slice(0, maxLines);

  return (
    <Frame
      count={lines.length}
      highlight={highlight}
      after={
        all.length > maxLines ? (
          <p className="border-t border-border px-4 py-3 text-xs text-muted-foreground">
            Showing the first {maxLines.toLocaleString()} of{" "}
            {all.length.toLocaleString()} lines. Download the file to read the
            rest.
          </p>
        ) : null
      }
    >
      {/* Nothing until it is coloured, rather than the text twice in two
          colours. The fallback is deliberately null: the wait is a chunk and a
          grammar off the same origin, tens of milliseconds warm and paid once
          per language, and an empty pane for that long is invisible where a
          recolour is not. */}
      <Suspense fallback={null}>
        <Highlighted
          path={path}
          text={lines.join("\n")}
          highlight={highlight}
          line={LINE}
          size={SIZE}
        />
      </Suspense>
    </Frame>
  );
}

/**
 * Ask for the highlighter now, before anybody opens a file.
 *
 * Called when a skill is opened, which is a click or two before its first file
 * is read, so by then the chunk is already here and the pane is never empty.
 * Idle work: it is a `import()` whose result is thrown away, because the point
 * is the module cache it fills.
 */
export function warmCodeView(): void {
  void import("./CodeHighlighted");
}

/**
 * A file being edited: the highlighted text, with a box to type in over it.
 *
 * A textarea cannot draw colours, so it does not: the coloured text is behind
 * it and the textarea's own text is transparent, leaving only its caret and
 * selection. This is the ordinary way to do it, and the whole of what makes it
 * work is that the two boxes are metrically identical, which is why the font,
 * the size, the line height and the tab width come from one place and are
 * handed to both.
 *
 * Still not a code editor. CodeMirror or Monaco is hundreds of kilobytes and a
 * set of behaviours to learn, for what somebody does here: fix a line in a
 * script. What a textarea gets wrong for code is Tab, which moves focus, so
 * that is the one key handled.
 *
 * The box is as tall as its content rather than scrolling inside itself, so the
 * frame scrolls the pair of them together and every number stays beside its
 * line. A textarea scrolling on its own would need its position copied to the
 * gutter and to the text behind it on every frame, which is the sort of thing
 * that is almost right.
 */
export function CodeEditor({
  path,
  value,
  onChange,
  label,
}: {
  /** Which file, so the text behind the box is coloured as that language. */
  path: string;
  value: string;
  onChange: (text: string) => void;
  label: string;
}) {
  const count = value.split("\n").length;
  // One place, both boxes. Anything they disagree about shows up as the caret
  // drifting away from the letter it is supposed to be on.
  const metrics = {
    fontSize: `${SIZE}px`,
    lineHeight: `${LINE}px`,
    tabSize: 2,
    fontFamily: "var(--font-mono, ui-monospace, monospace)",
  } as const;

  return (
    <Frame count={count} highlight={null}>
      <div className="relative w-max min-w-full">
        {/* Behind: the same file, coloured. Out of the way of the pointer, so
            every click and drag lands on the box above it. */}
        <div aria-hidden className="pointer-events-none">
          <Suspense fallback={null}>
            <Highlighted
              path={path}
              text={value}
              highlight={null}
              line={LINE}
              size={SIZE}
            />
          </Suspense>
        </div>
        <textarea
          aria-label={label}
          value={value}
          spellCheck={false}
          wrap="off"
          onChange={(event) => onChange(event.target.value)}
          onKeyDown={(event) => {
            if (event.key !== "Tab") return;
            // A textarea moves focus on Tab, which in a file full of
            // indentation is the wrong answer every time.
            event.preventDefault();
            const field = event.currentTarget;
            const { selectionStart: from, selectionEnd: to } = field;
            onChange(value.slice(0, from) + "  " + value.slice(to));
            // Put the caret after what was inserted, on the next frame, because
            // React is about to rewrite the value underneath it.
            requestAnimationFrame(() => {
              field.selectionStart = field.selectionEnd = from + 2;
            });
          }}
          className="absolute inset-0 h-full w-full resize-none overflow-hidden whitespace-pre border-0 bg-transparent p-0 outline-none"
          style={{
            ...metrics,
            // The text is invisible and the caret is not: what is read is the
            // coloured copy underneath. -webkit-text-fill-color as well as
            // color, because WebKit ignores the first for a form control.
            color: "transparent",
            WebkitTextFillColor: "transparent",
            caretColor: "var(--foreground)",
            // One line of slack under the box, so typing a new line at the
            // bottom does not clip before the height catches up.
            minHeight: (count + 1) * LINE,
          }}
        />
      </div>
    </Frame>
  );
}

/**
 * The gutter and the content, in one box that scrolls.
 *
 * One scroller for both, so a long line moves the code sideways while the
 * numbers stay put (the gutter is sticky), and a long file moves both down
 * together. Two scrollers would need syncing, and a synced scroll is a frame
 * behind.
 */
function Frame({
  count,
  highlight,
  children,
  after,
}: {
  count: number;
  highlight: { from: number; to: number } | null;
  children: ReactNode;
  after?: ReactNode;
}) {
  return (
    <div className="min-h-0 flex-1 overflow-auto">
      <div className="flex w-max min-w-full items-stretch py-2">
        <Gutter count={count} highlight={highlight} />
        <div className="min-w-0 flex-1 pr-4">{children}</div>
      </div>
      {after}
    </div>
  );
}

/** The line numbers. Sticky, so they survive a sideways scroll. */
function Gutter({
  count,
  highlight,
}: {
  count: number;
  highlight: { from: number; to: number } | null;
}) {
  return (
    <div
      aria-hidden
      className="sticky left-0 shrink-0 select-none bg-background text-right font-mono text-muted-foreground/60"
      style={{ width: GUTTER, fontSize: `${SIZE}px`, lineHeight: `${LINE}px` }}
    >
      {Array.from({ length: count }, (_, index) => {
        const number = index + 1;
        const lit =
          highlight !== null &&
          number >= highlight.from &&
          number <= highlight.to;
        return (
          <div
            key={number}
            className={cn("pr-3", lit && "bg-primary/10")}
            style={{ height: LINE }}
          >
            {number}
          </div>
        );
      })}
    </div>
  );
}

