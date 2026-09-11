import type { ReactNode } from 'react'
import { Loader2, Pencil, Trash2 } from 'lucide-react'
import { describeError } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { cn } from '@/lib/utils'

/**
 * A table of things you can edit.
 *
 * Every management screen in this console is the same shape, so it is the same
 * component: a list, a row per thing, edit and delete on each. Seven pages that
 * each invent their own table become seven pages that each behave slightly
 * differently, and a person who learns one has not learned any of the others.
 */

export interface Column<T> {
  header: string
  /** The cell. Anything React can draw. */
  cell: (item: T) => ReactNode
  /** Numbers right-align, because that is how numbers are read. */
  numeric?: boolean
  className?: string
}

export function DataTable<T extends { id: number | string }>({
  items,
  columns,
  loading,
  empty,
  onEdit,
  remove,
}: {
  items: T[] | null
  columns: Column<T>[]
  loading?: boolean
  empty: ReactNode
  onEdit?: (item: T) => void
  /**
   * Deleting, as one thing: what to ask, what to do, and what to say afterwards.
   *
   * The three used to be separate props, and `confirm` was optional while
   * nothing said the deletion had happened at all — a row vanished and that was
   * the whole confirmation, which is indistinguishable from a row vanishing for
   * some other reason. Together they cannot come apart: there is no way to add
   * a delete to a table without saying what is being risked and what was done.
   */
  remove?: {
    run: (item: T) => void | Promise<void>
    /** The question asked first. Say what will be lost. */
    confirm: (item: T) => string
    /** What is said afterwards. Name the thing, not the action. */
    done: (item: T) => string
  }
}) {
  const notify = useNotify()

  if (loading || items === null) {
    return (
      <div className="grid place-items-center py-16">
        <Loader2 className="size-4 animate-spin text-muted-foreground" />
      </div>
    )
  }

  if (items.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-border py-16 text-center">
        <p className="text-sm text-muted-foreground">{empty}</p>
      </div>
    )
  }

  const actions = Boolean(onEdit || remove)

  return (
    <div className="overflow-hidden rounded-lg border border-border">
      <table className="w-full text-sm">
        <thead className="bg-muted/40">
          <tr className="border-b border-border text-left">
            {columns.map((column) => (
              <th
                key={column.header}
                className={cn(
                  'px-4 py-2.5 text-xs font-medium text-muted-foreground',
                  column.numeric && 'text-right',
                  column.className,
                )}
              >
                {column.header}
              </th>
            ))}
            {actions && <th className="w-20 px-4 py-2.5" />}
          </tr>
        </thead>
        <tbody>
          {items.map((item) => (
            <tr
              key={item.id}
              className="group border-b border-border/60 transition-colors last:border-0 hover:bg-muted/30"
            >
              {columns.map((column) => (
                <td
                  key={column.header}
                  className={cn('px-4 py-3', column.numeric && 'text-right tabular-nums', column.className)}
                >
                  {column.cell(item)}
                </td>
              ))}
              {actions && (
                <td className="px-4 py-3">
                  <div className="flex justify-end gap-1 opacity-0 transition-opacity group-hover:opacity-100">
                    {onEdit && (
                      <IconButton label="Edit" onClick={() => onEdit(item)}>
                        <Pencil className="size-3.5" />
                      </IconButton>
                    )}
                    {remove && (
                      <IconButton
                        label="Delete"
                        destructive
                        onClick={async () => {
                          if (
                            !(await notify.confirm({
                              title: remove.confirm(item),
                              confirmLabel: 'Delete',
                              destructive: true,
                            }))
                          )
                            return
                          try {
                            await remove.run(item)
                            // A row disappearing is not a confirmation: it looks
                            // the same as a row disappearing for any other
                            // reason, and on a screen where deleting can be
                            // refused, silence is the ambiguous answer.
                            notify.success(remove.done(item))
                          } catch (failure) {
                            // A refused delete must be heard, not swallowed:
                            // the gateway says why (still in use, the ground
                            // you stand on), and the person reads that.
                            notify.error(describeError(failure))
                          }
                        }}
                      >
                        <Trash2 className="size-3.5" />
                      </IconButton>
                    )}
                  </div>
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function IconButton({
  label,
  destructive,
  onClick,
  children,
}: {
  label: string
  destructive?: boolean
  onClick: () => void | Promise<void>
  children: ReactNode
}) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      onClick={() => void onClick()}
      className={cn(
        'rounded-md p-1.5 text-muted-foreground transition-colors',
        destructive ? 'hover:bg-destructive hover:text-white' : 'hover:bg-muted hover:text-foreground',
      )}
    >
      {children}
    </button>
  )
}

/** Badge states a fact in one word: active, disabled, locked, published. */
export function Badge({
  children,
  tone = 'neutral',
  title,
}: {
  children: ReactNode
  tone?: 'neutral' | 'good' | 'warn' | 'bad'
  /** Hover text, for badges that summarize something longer (an error). */
  title?: string
}) {
  return (
    <span
      title={title}
      className={cn(
        'inline-flex items-center rounded-md px-1.5 py-0.5 text-xs font-medium',
        tone === 'neutral' && 'bg-muted text-muted-foreground',
        tone === 'good' && 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
        tone === 'warn' && 'bg-amber-500/10 text-amber-600 dark:text-amber-400',
        tone === 'bad' && 'bg-destructive/10 text-destructive',
      )}
    >
      {children}
    </span>
  )
}
