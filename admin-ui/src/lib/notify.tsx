import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import { AlertCircle, CheckCircle2, Info, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

/**
 * Notifications: the console's own voice.
 *
 * The browser's alert() and confirm() are not ours: they name the origin
 * ("localhost says"), they cannot be styled, and they freeze the page. So the
 * console speaks for itself. A passing outcome is a toast that says what
 * happened and fades; a question that must be answered before acting is a
 * confirm dialog, and it hands back a promise so the caller reads it as a plain
 * yes or no.
 *
 * One provider, mounted once, gives both. Nothing below has to render a viewport
 * or hold dialog state.
 */

type Tone = 'success' | 'error' | 'info'

interface Toast {
  id: number
  tone: Tone
  title?: string
  message: string
}

export interface ConfirmOptions {
  /** The question, stated plainly. */
  title: string
  /** What is at stake, when it is worth spelling out. */
  body?: ReactNode
  confirmLabel?: string
  cancelLabel?: string
  /** A destructive confirm paints its action red and defaults focus to Cancel. */
  destructive?: boolean
}

interface Notify {
  success: (message: string, title?: string) => void
  error: (message: string, title?: string) => void
  info: (message: string, title?: string) => void
  /** Ask before acting. Resolves true when confirmed, false on cancel or Escape. */
  confirm: (options: ConfirmOptions) => Promise<boolean>
}

const NotifyContext = createContext<Notify | null>(null)

/** How long a toast lingers before it fades. An error stays longer: it is worth reading twice. */
const LINGER: Record<Tone, number> = { success: 4500, info: 4500, error: 8000 }

export function NotifyProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const [pending, setPending] = useState<{
    options: ConfirmOptions
    resolve: (confirmed: boolean) => void
  } | null>(null)
  // A monotonic id: two toasts raised in the same millisecond still differ.
  const nextID = useRef(0)

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id))
  }, [])

  const push = useCallback((tone: Tone, message: string, title?: string) => {
    const id = (nextID.current += 1)
    setToasts((current) => [...current, { id, tone, message, title }])
  }, [])

  const confirm = useCallback(
    (options: ConfirmOptions) =>
      new Promise<boolean>((resolve) => {
        setPending({ options, resolve })
      }),
    [],
  )

  const settle = useCallback(
    (confirmed: boolean) => {
      pending?.resolve(confirmed)
      setPending(null)
    },
    [pending],
  )

  const notify = useMemo<Notify>(
    () => ({
      success: (message, title) => push('success', message, title),
      error: (message, title) => push('error', message, title),
      info: (message, title) => push('info', message, title),
      confirm,
    }),
    [push, confirm],
  )

  return (
    <NotifyContext.Provider value={notify}>
      {children}
      <Toaster toasts={toasts} onDismiss={dismiss} />
      {pending && (
        <ConfirmDialog options={pending.options} onSettle={settle} />
      )}
    </NotifyContext.Provider>
  )
}

export function useNotify(): Notify {
  const context = useContext(NotifyContext)
  if (!context) throw new Error('useNotify must be used inside a NotifyProvider')
  return context
}

const TONE_ICON: Record<Tone, typeof CheckCircle2> = {
  success: CheckCircle2,
  error: AlertCircle,
  info: Info,
}

const TONE_ACCENT: Record<Tone, string> = {
  success: 'text-emerald-600 dark:text-emerald-400',
  error: 'text-destructive',
  info: 'text-muted-foreground',
}

/**
 * The stack of toasts, bottom-right, above everything (a toast that a modal
 * covered would be a toast nobody read). The region announces its own contents
 * to a screen reader; each toast times out on its own.
 */
function Toaster({ toasts, onDismiss }: { toasts: Toast[]; onDismiss: (id: number) => void }) {
  return (
    <div
      aria-live="polite"
      className="pointer-events-none fixed inset-x-0 bottom-0 z-[100] flex flex-col items-end gap-2 p-4 sm:max-w-sm sm:left-auto sm:right-0"
    >
      {toasts.map((toast) => (
        <ToastItem key={toast.id} toast={toast} onDismiss={() => onDismiss(toast.id)} />
      ))}
    </div>
  )
}

function ToastItem({ toast, onDismiss }: { toast: Toast; onDismiss: () => void }) {
  const Icon = TONE_ICON[toast.tone]
  // The toast dismisses itself after its linger, unless the pointer is over it:
  // a person reaching to read or close it should not have it vanish under the
  // cursor. Hovering clears the arming; leaving arms it again.
  const [hovering, setHovering] = useState(false)
  useEffect(() => {
    if (hovering) return
    const handle = setTimeout(onDismiss, LINGER[toast.tone])
    return () => clearTimeout(handle)
  }, [hovering, onDismiss, toast.tone])

  return (
    <div
      role={toast.tone === 'error' ? 'alert' : 'status'}
      onMouseEnter={() => setHovering(true)}
      onMouseLeave={() => setHovering(false)}
      className="pointer-events-auto flex w-full items-start gap-3 rounded-lg border border-border bg-card px-4 py-3 shadow-lg"
    >
      <Icon className={cn('mt-0.5 size-4 shrink-0', TONE_ACCENT[toast.tone])} />
      <div className="min-w-0 flex-1 space-y-0.5">
        {toast.title && <p className="text-sm font-medium">{toast.title}</p>}
        <p className="text-sm text-muted-foreground break-words">{toast.message}</p>
      </div>
      <button
        type="button"
        aria-label="Dismiss"
        onClick={onDismiss}
        className="-mr-1 -mt-0.5 rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
      >
        <X className="size-3.5" />
      </button>
    </div>
  )
}

/**
 * The confirm dialog. It is a form so Enter answers yes and Escape answers no,
 * the way every dialog in this console behaves. It never scrolls inside itself.
 */
function ConfirmDialog({
  options,
  onSettle,
}: {
  options: ConfirmOptions
  onSettle: (confirmed: boolean) => void
}) {
  const { title, body, confirmLabel, cancelLabel, destructive } = options

  return (
    <Dialog.Root open onOpenChange={(open) => !open && onSettle(false)}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/50" />
        <Dialog.Content
          className="fixed inset-0 z-50 overflow-y-auto"
          onPointerDownOutside={() => onSettle(false)}
        >
          <div
            onPointerDown={(event) => event.target === event.currentTarget && onSettle(false)}
            className="flex min-h-full items-center justify-center p-4 sm:p-8"
          >
            <form
              onSubmit={(event) => {
                event.preventDefault()
                onSettle(true)
              }}
              className="w-full max-w-md rounded-lg border border-border bg-card shadow-lg"
            >
              <div className="space-y-2 px-5 py-4">
                <Dialog.Title className="text-sm font-semibold">{title}</Dialog.Title>
                {body && (
                  <Dialog.Description asChild>
                    <div className="text-sm text-muted-foreground">{body}</div>
                  </Dialog.Description>
                )}
              </div>
              <footer className="flex justify-end gap-2 border-t border-border px-5 py-3.5">
                <Button
                  type="button"
                  variant="ghost"
                  autoFocus={destructive}
                  onClick={() => onSettle(false)}
                >
                  {cancelLabel ?? 'Cancel'}
                </Button>
                <Button
                  type="submit"
                  variant={destructive ? 'destructive' : 'default'}
                  autoFocus={!destructive}
                >
                  {confirmLabel ?? 'Confirm'}
                </Button>
              </footer>
            </form>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
