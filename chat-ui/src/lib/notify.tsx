import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { AlertCircle, CheckCircle2, X } from 'lucide-react'
import { cn } from '@/lib/utils'

/**
 * The chat's own voice for things that happen outside the conversation.
 *
 * Renaming, pinning and deleting a conversation used to report nothing at all:
 * every one of them ran through a helper that caught its own failure, wrote it
 * to the console, and refreshed the list anyway. A delete the server refused
 * therefore looked exactly like a delete that worked, right up until the row
 * was still there, with nothing to say why.
 *
 * This is deliberately the toast half only, and not the console's notify: a
 * question that has to be answered before acting is asked IN the row here (the
 * Yes/No that replaces a conversation's actions), which is better than a dialog
 * over the chat and means there is no confirm to provide.
 *
 * Nothing in the conversation itself belongs here. What the assistant says, what
 * a tool did, an approval to answer: those are the transcript, and a message
 * that fades is the wrong place for anything somebody may need to read twice.
 */

type Tone = 'success' | 'error'

interface Toast {
  id: number
  tone: Tone
  message: string
}

interface Notify {
  success: (message: string) => void
  error: (message: string) => void
}

const NotifyContext = createContext<Notify | null>(null)

/** How long one lingers. A failure stays longer: it is worth reading twice. */
const LINGER: Record<Tone, number> = { success: 4000, error: 8000 }

export function NotifyProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  // A monotonic id: two raised in the same millisecond still differ.
  const nextID = useRef(0)

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id))
  }, [])

  const notify = useMemo<Notify>(() => {
    const push = (tone: Tone, message: string) => {
      const id = (nextID.current += 1)
      setToasts((current) => [...current, { id, tone, message }])
    }
    return {
      success: (message) => push('success', message),
      error: (message) => push('error', message),
    }
  }, [])

  return (
    <NotifyContext.Provider value={notify}>
      {children}
      <div
        aria-live="polite"
        className="pointer-events-none fixed inset-x-0 bottom-0 z-[100] flex flex-col items-end gap-2 p-4 sm:left-auto sm:right-0 sm:max-w-sm"
      >
        {toasts.map((toast) => (
          <ToastItem key={toast.id} toast={toast} onDismiss={() => dismiss(toast.id)} />
        ))}
      </div>
    </NotifyContext.Provider>
  )
}

/**
 * The hook never throws for want of a provider.
 *
 * This chat is a library as much as an app: it is mounted by a shell that may
 * not have wrapped it, and a component tree that crashes because nobody was
 * listening for a toast is worse than a toast nobody hears. Unwrapped, the calls
 * are no-ops.
 */
export function useNotify(): Notify {
  return useContext(NotifyContext) ?? SILENT
}

const SILENT: Notify = { success: () => {}, error: () => {} }

function ToastItem({ toast, onDismiss }: { toast: Toast; onDismiss: () => void }) {
  const Icon = toast.tone === 'success' ? CheckCircle2 : AlertCircle
  // It dismisses itself after its linger, unless the pointer is over it: a
  // person reaching to read or close it should not have it vanish under the
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
      className="pointer-events-auto flex w-full items-start gap-3 rounded-lg border bg-card px-4 py-3 shadow-lg"
    >
      <Icon
        className={cn(
          'mt-0.5 size-4 shrink-0',
          toast.tone === 'success' ? 'text-emerald-600 dark:text-emerald-400' : 'text-destructive',
        )}
      />
      <p className="min-w-0 flex-1 break-words text-sm text-muted-foreground">{toast.message}</p>
      <button
        type="button"
        aria-label="Dismiss"
        onClick={onDismiss}
        className="-mr-1 -mt-0.5 cursor-pointer rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
      >
        <X className="size-3.5" />
      </button>
    </div>
  )
}
