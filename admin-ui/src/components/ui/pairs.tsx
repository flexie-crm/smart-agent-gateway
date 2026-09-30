import { useEffect, useRef, useState } from 'react'
import { Plus, X } from 'lucide-react'
import { Input } from '@/components/ui/input'

type Row = { name: string; value: string }

/**
 * Any number of name/value rows, with a button to add one.
 *
 * For a setting that is a LIST of pairs rather than one value: the extra
 * headers an API wants beside its key. A fixed pair of inputs ("second name",
 * "second value") could only ever carry one of them.
 *
 * The stored value is one string, "name: value" per line, like every other
 * declared field. That is not a compromise for this renderer: a secret is
 * sealed at rest BY PATH and the sealer only ever seals a string leaf, so a
 * list stored there would be skipped and every value written in plaintext. The
 * rows are the interface; the string is the setting. `tool.ReadPairs` is the
 * same rule on the server, including that the FIRST colon separates, so a value
 * may contain one.
 *
 * WHY THE ROWS ARE STATE and not simply read from the string each render: an
 * EMPTY row has no representation in the string. Deriving the rows from the
 * value meant pressing Add wrote the same value back, so nothing appeared and
 * the button looked broken. So the rows are held here, and the string is what
 * is emitted; a value that changes underneath us (a secret blanked when the
 * form is reopened) is read back in.
 */
export function PairRows({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [rows, setRows] = useState<Row[]>(() => atLeastOne(readPairs(value)))
  // What we last emitted, so a value arriving from outside can be told from
  // our own echo and does not wipe a half-typed row.
  const emitted = useRef(value)

  useEffect(() => {
    if (value === emitted.current) return
    emitted.current = value
    setRows(atLeastOne(readPairs(value)))
  }, [value])

  const write = (next: Row[]) => {
    setRows(next)
    const text = serialise(next)
    emitted.current = text
    onChange(text)
  }

  const edit = (at: number, part: Partial<Row>) =>
    write(rows.map((row, i) => (i === at ? { ...row, ...part } : row)))

  return (
    <div className="space-y-2">
      {rows.map((row, i) => {
        // One button per row, on the row. The LAST row adds another and every
        // row above it removes itself, so the control is always where the eye
        // already is and there is no button sitting under the field waiting to
        // be found. A single row shows only the plus: there is nothing to
        // remove when it is the only one.
        const last = i === rows.length - 1
        return (
          <div key={i} className="flex items-center gap-2">
            <Input
              aria-label="Name"
              placeholder="Name"
              value={row.name}
              onChange={(e) => edit(i, { name: e.target.value })}
            />
            <Input
              aria-label="Value"
              placeholder="Value"
              value={row.value}
              onChange={(e) => edit(i, { value: e.target.value })}
            />
            {last ? (
              <button
                type="button"
                aria-label="Add"
                title="Add another"
                className="shrink-0 cursor-pointer rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
                onClick={() => write([...rows, { name: '', value: '' }])}
              >
                <Plus className="size-4" />
              </button>
            ) : (
              <button
                type="button"
                aria-label="Remove"
                title="Remove"
                className="shrink-0 cursor-pointer rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
                onClick={() => write(atLeastOne(rows.filter((_, at) => at !== i)))}
              >
                <X className="size-4" />
              </button>
            )}
          </div>
        )
      })}
    </div>
  )
}

/** One row to type into, so the field is usable before Add is pressed. */
function atLeastOne(rows: Row[]): Row[] {
  return rows.length > 0 ? rows : [{ name: '', value: '' }]
}

/**
 * The rows as they are stored. A row with neither a name nor a value is not a
 * pair, so it contributes nothing: pressing Add and saving must not store a
 * blank line the server would then refuse.
 */
function serialise(rows: Row[]): string {
  return rows
    .filter((r) => r.name.trim() !== '' || r.value.trim() !== '')
    .map((r) => `${r.name.trim()}: ${r.value.trim()}`)
    .join('\n')
}

/** The stored string, read as rows. The first colon separates. */
function readPairs(value: string): Row[] {
  return value
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
    .map((line) => {
      const at = line.indexOf(':')
      if (at < 0) return { name: line, value: '' }
      return { name: line.slice(0, at).trim(), value: line.slice(at + 1).trim() }
    })
}
