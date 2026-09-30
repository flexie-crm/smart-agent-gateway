import { useRef, type ReactNode } from "react";
import { Pencil, Plus, Search, Trash2, X } from "lucide-react";
import { cn } from "@/lib/utils";
import { describeError } from "@/lib/form";
import { useNotify } from "@/lib/notify";

/**
 * The parts a three-pane screen is built from: a search band across the top,
 * columns under it, and rows in the columns.
 *
 * Shared by the screens laid out this way (brains, skills) because these ARE
 * the same things, and two copies of them drift: the header height and the
 * hairline between columns have to agree across a screen, and they cannot if
 * every column invents them. The same goes for the band and the rows, which is
 * why they moved here rather than being written twice.
 *
 * What is common is the frame. What is in a row is always the caller's.
 */

/** A column of a three-pane screen: a fixed header, and rows that scroll. */
export function Pane({
  title,
  note,
  onAdd,
  children,
}: {
  title: string;
  /**
   * What this column is showing, beside the word for what it is: "v3 · by John
   * Doe" next to PACKAGE.
   *
   * Its own slot rather than part of the title, because the title is set in
   * capitals and a person's name is not a label. Appending it would print
   * somebody as JOHN DOE.
   */
  note?: ReactNode;
  /** Absent means there is nothing to add here, and no button is drawn. */
  onAdd?: () => void;
  children: ReactNode;
}) {
  return (
    <div className="flex min-h-0 flex-col border-r border-border">
      <header className="flex h-11 shrink-0 items-center gap-2 border-b border-border px-4">
        <h2 className="shrink-0 text-xs font-medium uppercase tracking-wider text-muted-foreground">
          {title}
        </h2>
        {note && (
          <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">
            {note}
          </span>
        )}
        {!note && <span className="flex-1" />}
        {onAdd && (
          <button
            type="button"
            onClick={onAdd}
            className="cursor-pointer rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
            aria-label={`New ${title.toLowerCase().replace(/s$/, "")}`}
          >
            <Plus className="size-4" />
          </button>
        )}
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto">{children}</div>
    </div>
  );
}

/** What a pane says when it holds nothing. */
export function Empty({ children }: { children: ReactNode }) {
  return (
    <p className="px-4 py-8 text-center text-xs text-muted-foreground">
      {children}
    </p>
  );
}

/**
 * The search across the top of the screen, in a band of its own.
 *
 * Its own band and not part of a column: what is typed here narrows the panes
 * BELOW it, all of them, so it belongs to the screen rather than to any one
 * column. The columns keep their own borders and gain nothing around them.
 */
export function SearchBand({
  value,
  onChange,
  placeholder,
  label,
  summary,
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder: string;
  /** What a screen reader calls the box. */
  label: string;
  /** What came back, when something has. Empty says nothing rather than zero. */
  summary?: string;
}) {
  const box = useRef<HTMLInputElement>(null);
  return (
    <div className="flex h-11 shrink-0 items-center gap-2.5 border-b border-border px-4">
      <Search className="pointer-events-none size-4 shrink-0 text-muted-foreground" />
      <input
        // The cursor is in here on arrival. This is the screen you come to to
        // find something, and the alternative is a click that does nothing
        // except put it where it was always going to go.
        autoFocus
        ref={box}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        aria-label={label}
        className="h-full w-full min-w-0 bg-transparent text-sm outline-none placeholder:text-muted-foreground"
      />
      {value !== "" && (
        <>
          {summary && (
            <span className="shrink-0 text-xs text-muted-foreground">
              {summary}
            </span>
          )}
          {/* Clearing puts the cursor back where it was: somebody who wants the
              whole list again usually wants to look for something else, and a
              focus left on the button means their next keystroke goes
              nowhere. */}
          <button
            type="button"
            onClick={() => {
              onChange("");
              box.current?.focus();
            }}
            aria-label="Clear search"
            title="Clear search"
            className="shrink-0 cursor-pointer rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
          >
            <X className="size-3.5" />
          </button>
        </>
      )}
    </div>
  );
}

/**
 * Row is one item in a pane. The edit and delete actions appear on hover: they
 * are always reachable but never in the way of the thing you came here to read.
 *
 * Three elements and not one: the frame holds the selection and the hover, the
 * BODY is a real button, and the actions sit beside it rather than inside it.
 * That shape is forced, because a button inside a button is not valid HTML and
 * browsers do not agree on what to do with it. The alternative, a clickable
 * div, is what this looked like when it was only the brains screen, and it
 * costs the row its place in the tab order and its response to the space bar:
 * the pane becomes something only a mouse can use.
 */
export function Row({
  selected,
  onClick,
  icon,
  title,
  subtitle,
  meta,
  onEdit,
  onDelete,
  confirm,
  confirmBody,
  deleted,
}: {
  selected: boolean;
  onClick: () => void;
  icon: ReactNode;
  title: ReactNode;
  subtitle?: string;
  meta?: string;
  onEdit: () => void;
  onDelete: () => Promise<void>;
  /** The question asked before deleting. Say what will be lost. */
  confirm: string;
  /** The rest of what will be lost, when one line cannot hold it. */
  confirmBody?: string;
  /** What is said afterwards. A row vanishing is not a confirmation. */
  deleted: string;
}) {
  const notify = useNotify();
  return (
    <div
      className={cn(
        "group relative border-b border-border/60 transition-colors",
        // The selected row carries an accent bar on its right edge, the way the
        // CRM marks the open item: the tint alone reads as hover, the bar reads
        // as "you are here".
        selected
          ? "bg-accent after:absolute after:inset-y-0 after:right-0 after:w-[3px] after:bg-primary"
          : "hover:bg-accent",
      )}
    >
      <button
        type="button"
        onClick={onClick}
        className="flex w-full cursor-pointer gap-2.5 px-4 py-3 text-left"
      >
        {/* The icon centers on the TITLE'S line, not on the row: next to a
            multi-line row it would float between the lines, which reads as
            misaligned next to every one of them. The slot is one title line
            tall (h-5 matches the text-sm leading) and the icon centers in it. */}
        <span className="flex h-5 shrink-0 items-center">{icon}</span>
        {/* Spans throughout, because this is inside a button and a button may
            not contain block content. They are given `block` where they need
            to stack; `line-clamp` sets its own display and is left alone. */}
        <span className="min-w-0 flex-1">
          <span className="block truncate text-sm font-medium">{title}</span>
          {subtitle && (
            <span className="mt-0.5 line-clamp-2 text-xs leading-snug text-muted-foreground">
              {subtitle}
            </span>
          )}
          {meta && (
            <span className="mt-1 block text-xs text-muted-foreground">
              {meta}
            </span>
          )}
        </span>
      </button>

      {/* Beside the body, never inside it, so neither of these needs to stop a
          click from reaching the row: there is no handler above them to reach.
          Absolutely positioned, so the body keeps the full width and the text
          in it does not reflow as the actions appear. */}
      <div className="absolute right-2 top-2 hidden gap-1 group-hover:flex">
        <IconButton label="Edit" onClick={onEdit}>
          <Pencil className="size-3.5" />
        </IconButton>
        <IconButton
          label="Delete"
          destructive
          onClick={async () => {
            // Deleting one of these takes everything under it. That is worth
            // one question.
            if (
              !(await notify.confirm({
                title: confirm,
                body: confirmBody,
                confirmLabel: "Delete",
                destructive: true,
              }))
            )
              return;
            try {
              await onDelete();
              notify.success(deleted);
            } catch (failure) {
              notify.error(describeError(failure));
            }
          }}
        >
          <Trash2 className="size-3.5" />
        </IconButton>
      </div>
    </div>
  );
}

export function IconButton({
  label,
  destructive,
  onClick,
  children,
}: {
  label: string;
  destructive?: boolean;
  onClick: () => void | Promise<void>;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      onClick={() => void onClick()}
      className={cn(
        "cursor-pointer rounded-md bg-card p-1.5 text-muted-foreground shadow-sm transition-colors",
        destructive
          ? "hover:bg-destructive hover:text-white"
          : "hover:bg-accent hover:text-foreground",
      )}
    >
      {children}
    </button>
  );
}
