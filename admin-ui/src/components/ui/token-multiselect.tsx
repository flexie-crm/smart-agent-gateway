import { useMemo, useRef, useState } from 'react'
import { Lock, X } from 'lucide-react'
import { cn } from '@/lib/utils'

export interface TokenOption {
  value: string
  label: string
  /** A second, dimmer line under the label (e.g. the tool's internal name). */
  hint?: string
  /**
   * A fixed option is always shown as a token and cannot be removed. The agent
   * form uses it for a tool the code already forces to confirm: it is in the
   * set by law, not by choice, so the person sees it but cannot take it out.
   */
  fixed?: boolean
  /** A heading this option is filed under. Options carrying one are shown in
   *  labelled runs, so a list of sixty tools from three services is read by
   *  where they came from rather than scrolled through. */
  group?: string
}

/**
 * A tokenised multi-select: the chosen values sit inline as removable tokens,
 * and typing filters the rest into a dropdown to add. Modelled on the CRM's own
 * "Require Confirmation" control. Fixed options render as locked tokens and are
 * never part of the value the caller owns.
 */
export function TokenMultiSelect({
  options,
  value,
  onChange,
  placeholder = 'Add…',
  empty = 'Nothing to choose from.',
}: {
  options: TokenOption[]
  value: string[]
  onChange: (values: string[]) => void
  placeholder?: string
  empty?: string
}) {
  const [query, setQuery] = useState('')
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)

  const byValue = useMemo(() => new Map(options.map((o) => [o.value, o])), [options])
  const fixed = useMemo(() => options.filter((o) => o.fixed), [options])
  // Tokens: the fixed ones first, then the chosen (non-fixed) ones in order.
  const chosen = value.map((v) => byValue.get(v)).filter((o): o is TokenOption => Boolean(o) && !o!.fixed)

  const q = query.trim().toLowerCase()
  const candidates = options.filter(
    (o) =>
      !o.fixed &&
      !value.includes(o.value) &&
      (q === '' || o.label.toLowerCase().includes(q) || o.value.toLowerCase().includes(q)),
  )

  function add(v: string) {
    if (!value.includes(v)) onChange([...value, v])
    setQuery('')
    setActive(0)
    inputRef.current?.focus()
  }

  function remove(v: string) {
    onChange(value.filter((x) => x !== v))
    inputRef.current?.focus()
  }

  function onKeyDown(e: React.KeyboardEvent) {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setOpen(true)
      setActive((i) => Math.min(i + 1, candidates.length - 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActive((i) => Math.max(i - 1, 0))
    } else if (e.key === 'Enter' && open && candidates[active]) {
      e.preventDefault()
      add(candidates[active].value)
    } else if (e.key === 'Backspace' && query === '' && chosen.length > 0) {
      remove(chosen[chosen.length - 1].value)
    } else if (e.key === 'Escape') {
      setOpen(false)
    }
  }

  const noOptions = options.filter((o) => !o.fixed).length === 0

  return (
    <div className="relative">
      <div
        className={cn(
          'flex min-h-9 flex-wrap items-center gap-1.5 rounded-md border border-input bg-transparent px-2 py-1.5',
          'focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/50',
        )}
        onClick={() => inputRef.current?.focus()}
      >
        {fixed.map((o) => (
          <span
            key={o.value}
            title="Always confirms: the code requires it."
            className="inline-flex items-center gap-1 rounded bg-muted px-2 py-0.5 text-xs text-muted-foreground"
          >
            <Lock className="size-3" />
            {o.label}
          </span>
        ))}
        {chosen.map((o) => (
          <span
            key={o.value}
            className="inline-flex items-center gap-1 rounded bg-primary/10 px-2 py-0.5 text-xs text-foreground"
          >
            {o.label}
            <button
              type="button"
              className="cursor-pointer text-muted-foreground hover:text-foreground"
              onClick={(e) => {
                e.stopPropagation()
                remove(o.value)
              }}
              aria-label={`Remove ${o.label}`}
            >
              <X className="size-3" />
            </button>
          </span>
        ))}
        <input
          ref={inputRef}
          value={query}
          disabled={noOptions}
          onChange={(e) => {
            setQuery(e.target.value)
            setOpen(true)
            setActive(0)
          }}
          onFocus={() => setOpen(true)}
          onBlur={() => setTimeout(() => setOpen(false), 120)}
          onKeyDown={onKeyDown}
          placeholder={placeholder}
          className="h-6 min-w-24 flex-1 bg-transparent px-1 text-sm outline-none placeholder:text-muted-foreground disabled:cursor-not-allowed"
        />
      </div>

      {open && !noOptions && (
        <div className="absolute z-20 mt-1 max-h-56 w-full overflow-y-auto rounded-md border border-border bg-popover py-1 shadow-md">
          {candidates.length === 0 ? (
            <div className="px-3 py-2 text-xs text-muted-foreground">{q ? 'No match.' : 'Everything is added.'}</div>
          ) : (
            candidates.map((o, i) => (
              <div key={o.value}>
              {/* A heading whenever the run changes. Rendered inside the same
                  map so the index the keyboard moves through stays the flat
                  one: a separate grouped structure would have to keep two
                  orders in step, and they would drift the first time one of
                  them was filtered. */}
              {o.group && o.group !== candidates[i - 1]?.group && (
                <div className="px-3 pb-0.5 pt-2 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                  {o.group}
                </div>
              )}
              <button
                type="button"
                // Chosen on mousedown, not on click.
                //
                // preventDefault here keeps the input focused so the list is not
                // closed out from under the press. That much was always right.
                // What it cannot do is guarantee a click ever follows: the press
                // and the release are two events, and anything that moves,
                // rerenders or re-lays-out the row between them means the release
                // lands somewhere else and the click never fires. The keyboard
                // path acts on one event and always worked, which is exactly the
                // shape of "I can only select by typing".
                //
                // Acting on the press removes the gap entirely. The selection is
                // made from the same event that stops the blur, so there is no
                // second event to lose.
                onMouseDown={(e) => {
                  e.preventDefault()
                  add(o.value)
                }}
                onMouseEnter={() => setActive(i)}
                className={cn(
                  'flex w-full flex-col items-start px-3 py-1.5 text-left text-sm',
                  i === active ? 'bg-primary text-primary-foreground' : 'hover:bg-muted',
                )}
              >
                <span>{o.label}</span>
                {o.hint && (
                  <span
                    className={cn(
                      'font-mono text-xs',
                      i === active ? 'text-primary-foreground/80' : 'text-muted-foreground',
                    )}
                  >
                    {o.hint}
                  </span>
                )}
              </button>
            </div>
            ))
          )}
        </div>
      )}

      {noOptions && empty && <p className="mt-1 text-xs text-muted-foreground">{empty}</p>}
    </div>
  )
}
