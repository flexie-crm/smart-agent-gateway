import { LogOut, Monitor, Moon, Sun } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'

import { cn } from '@/lib/utils'
import {
  followTheMachine,
  paint,
  remember,
  rememberedChoice,
  type ThemeChoice,
} from '@/lib/theme'

/**
 * Light, night, or whatever the computer is set to.
 *
 * Three positions rather than a toggle, because following the computer is a
 * real answer and the one most people want: the screen goes dark in the evening
 * along with everything else they have open. A two-position switch cannot say
 * that, and hiding it as the state you get by never touching anything makes it
 * invisible.
 *
 * Small, quiet, and in the corner where the account is, because it is a setting
 * somebody changes twice and then forgets. The same control as the chat's, so a
 * person meets one switch in one place across both halves of the product.
 */
export function ThemeSwitch({
  onSignOut,
}: {
  /** Signing out, when there is a session to leave. Absent on the personal
   * edition, where there is nobody to sign out AS. */
  onSignOut?: () => void
} = {}) {
  const [choice, setChoice] = useState<ThemeChoice>(() => rememberedChoice())
  // Read when the machine changes its mind rather than when this was mounted.
  const now = useRef(choice)
  now.current = choice

  useEffect(() => followTheMachine(() => now.current), [])

  const pick = useCallback((next: ThemeChoice) => {
    setChoice(next)
    remember(next)
    paint(next)
  }, [])

  const SIGN_OUT = 'Sign out'

  const options: Array<{ value: ThemeChoice; icon: typeof Sun; label: string }> = [
    { value: 'system', icon: Monitor, label: 'Match the computer' },
    { value: 'light', icon: Sun, label: 'Light' },
    { value: 'dark', icon: Moon, label: 'Night' },
  ]

  return (
    <div
      className="flex shrink-0 items-center gap-0.5 rounded-md border border-border p-0.5"
      // A radiogroup of three, because they are three positions of one setting
      // and only one can hold. Signing out is not a fourth position of it: it
      // sits in the same frame, where the hand already is, and is marked as
      // what it is rather than as a choice of appearance.
      role={onSignOut ? 'group' : 'radiogroup'}
      aria-label={'Appearance'}
    >
      {options.map(({ value, icon: Icon, label }) => (
        <button
          key={value}
          type="button"
          role="radio"
          aria-checked={choice === value}
          aria-label={label}
          title={label}
          onClick={() => pick(value)}
          className={cn(
            'cursor-pointer rounded-[5px] p-1.5 transition-colors',
            choice === value
              ? 'bg-sidebar-accent text-foreground'
              : 'text-muted-foreground hover:text-foreground',
          )}
        >
          <Icon className="size-3.5" />
        </button>
      ))}
      {onSignOut && (
        <>
          <span aria-hidden className="mx-0.5 h-4 w-px bg-border" />
          <button
            type="button"
            onClick={onSignOut}
            aria-label={SIGN_OUT}
            title={SIGN_OUT}
            className="cursor-pointer rounded-[5px] p-1.5 text-muted-foreground transition-colors hover:text-foreground"
          >
            <LogOut className="size-3.5" />
          </button>
        </>
      )}
    </div>
  )
}
