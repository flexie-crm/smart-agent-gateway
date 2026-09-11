import * as Dialog from '@radix-ui/react-dialog'
import { X } from 'lucide-react'
import type { FormEvent, PointerEvent, ReactNode } from 'react'
import { Button } from './button'

/**
 * A modal that is a FORM.
 *
 * Every dialog in this console exists to save something, so it is a form: Enter
 * submits, Escape cancels, the first field takes focus. A dialog whose "Save"
 * is a button you have to reach for with the mouse is a dialog people dread.
 *
 * It opens at once, before what it shows has arrived. A form that waits for its
 * data before appearing reads as a click that did nothing, so the dialog is
 * there immediately and says it is still loading: the hairline under its header
 * becomes a moving bar, and the body stays empty until the answer lands. One
 * request, one bar, then the whole form at once, never a form that fills in in
 * pieces while somebody is already typing into it.
 *
 * The FORM is there from the start; only its values are waiting. That is what
 * keeps it still: a dialog whose body is empty until the answer lands is a
 * header sitting on a footer, and when the form arrives the footer drops half a
 * screen. Rendering the form immediately gives the dialog its height at once,
 * and the values landing in it move nothing.
 *
 * So `loading` says "the values are not here yet": the hairline under the header
 * becomes a moving bar and the submit button waits. It does not empty the body,
 * and it must not, because there is no honest height to reserve for content you
 * have not received, and any number picked would be wrong for most forms.
 *
 * The panel is anchored near the top for the same reason: what growth there is
 * extends downward instead of pushing its own header up the screen.
 *
 * It never scrolls inside itself. The dialog is as tall as what it holds, and
 * when that is taller than the window, the window scrolls it. An inner scrollbar
 * turns a form into a peephole: you cannot see the field you are filling and the
 * button that saves it at the same time. So the dialog IS the scroll container
 * (a full-viewport one), the panel sits inside it, and the space around the panel
 * behaves like the backdrop it looks like.
 */
export function Modal({
  open,
  onOpenChange,
  title,
  onSubmit,
  submitting,
  loading,
  submitLabel = 'Save',
  submitDisabled,
  children,
  wide,
  footerStart,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  onSubmit: () => void | Promise<void>
  submitting?: boolean
  /**
   * What this form shows has not arrived yet. The header's hairline becomes a
   * moving bar and the body is left empty, so an open dialog with nothing in it
   * reads as loading rather than as broken.
   */
  loading?: boolean
  submitLabel?: string
  /**
   * The form is complete enough to render but cannot go ahead, and the reason is
   * already on screen. Different from `submitting`: nothing is in flight, this
   * simply must not be pressed.
   */
  submitDisabled?: boolean
  children: ReactNode
  wide?: boolean
  /**
   * An action pinned to the LEFT of the footer, opposite Cancel and the submit
   * button. This is where a destructive verb belongs (Delete), set apart from
   * the affirmative pair so the two are never a slip of the mouse from each other.
   */
  footerStart?: ReactNode
}) {
  function submit(event: FormEvent) {
    event.preventDefault()
    void onSubmit()
  }

  // Radix only closes on a press outside the content, and the content now fills
  // the window. So the empty space around the panel has to say so itself.
  function pressedTheBackdrop(event: PointerEvent<HTMLDivElement>) {
    if (event.target === event.currentTarget) onOpenChange(false)
  }

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        {/* No entrance animation, on the panel or the backdrop: a form you
            asked for should be there when you asked for it. */}
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/50" />
        <Dialog.Content className="fixed inset-0 z-50 overflow-y-auto">
          <div
            onPointerDown={pressedTheBackdrop}
            className="flex min-h-full items-start justify-center p-4 sm:p-8 sm:pt-[12vh]"
          >
            <div
              className={[
                'w-full rounded-lg border border-border bg-card shadow-lg',
                wide ? 'max-w-2xl' : 'max-w-md',
              ].join(' ')}
            >
              {/* The bar IS the header's bottom border, not a thing added under
                  it: nothing moves when it goes away. */}
              <header className="relative flex items-center justify-between border-b border-border px-5 py-3.5">
                <Dialog.Title className="text-sm font-semibold">{title}</Dialog.Title>
                <Dialog.Close className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground">
                  <X className="size-4" />
                  <span className="sr-only">Close</span>
                </Dialog.Close>
                {loading && (
                  <div
                    role="progressbar"
                    aria-label="Loading"
                    className="absolute inset-x-0 -bottom-px h-px overflow-hidden"
                  >
                    <div className="h-full w-1/3 animate-modal-loading bg-foreground/60" />
                  </div>
                )}
              </header>

              <form onSubmit={submit}>
                {/* Empty while it loads: a form that appears field by field is one
                    somebody starts typing into before it is finished arriving. */}
                <div className="space-y-4 px-5 py-4">{children}</div>
                <footer className="flex items-center justify-between gap-2 border-t border-border px-5 py-3.5">
                  <div>{footerStart}</div>
                  <div className="flex gap-2">
                    <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
                      Cancel
                    </Button>
                    <Button type="submit" disabled={submitting || loading || submitDisabled}>
                      {submitLabel}
                    </Button>
                  </div>
                </footer>
              </form>
            </div>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}

/** Field is a label and its input, so no form in the console spaces them differently. */
export function Field({
  label,
  hint,
  required,
  error,
  children,
}: {
  label: string
  hint?: string
  required?: boolean
  /** What is wrong with this field. It takes the hint's place: while the input is refused, the correction is the guidance. */
  error?: string
  children: ReactNode
}) {
  return (
    <div className="space-y-1.5">
      <label className="flex items-center gap-1 text-sm font-medium">
        {label}
        {required && <span className="text-destructive">*</span>}
      </label>
      {children}
      {error ? (
        <p className="text-xs text-destructive">{error}</p>
      ) : (
        hint && <p className="text-xs text-muted-foreground">{hint}</p>
      )}
    </div>
  )
}
