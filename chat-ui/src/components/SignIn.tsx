import { useState, type FormEvent } from 'react'

import { PERSONAL, signIn, signInLocally, type Session } from '@lib/api'
import { useServerIsDev } from '@lib/use-dev-server'

// A demo account, for a build that wants a one-click way in. It is offered only
// when it is configured (the env vars), or in development, where the seeded
// admin exists; a production build without it never shows the button.
const DEMO_EMAIL = import.meta.env.VITE_SAG_DEMO_EMAIL ?? (import.meta.env.DEV ? 'admin@acme.test' : '')
const DEMO_PASSWORD =
  import.meta.env.VITE_SAG_DEMO_PASSWORD ?? (import.meta.env.DEV ? 'admin1234' : '')

/**
 * SAG adaptation: the chat is its own product now, so it asks who you are.
 * The CRM embedded it in a page that had already signed you in.
 */
export function SignIn({ onSignedIn }: { onSignedIn: (session: Session) => Promise<void> | void }) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const dev = useServerIsDev()

  const attempt = async (withEmail: string, withPassword: string) => {
    setBusy(true)
    setError(null)
    try {
      // Awaited, so the button stays busy through the workspace check that
      // follows, not just the login itself.
      await onSignedIn(await signIn(withEmail.trim(), withPassword))
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'That did not work.')
      setBusy(false)
    }
  }

  const submit = (event: FormEvent) => {
    event.preventDefault()
    void attempt(email, password)
  }


  const enter = async () => {
    setBusy(true)
    setError(null)
    try {
      await onSignedIn(await signInLocally())
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'This computer could not be signed in.')
      setBusy(false)
    }
  }

  // One person on their own computer is not asked for a password they were
  // never given. The fields are absent rather than hidden: a box with no
  // possible answer is worse than no box.
  if (PERSONAL) {
    return (
      <div className="grid min-h-screen place-items-center bg-background px-6">
        <div className="w-full max-w-xs text-center">
          <h1 className="text-xl tracking-tight">
            <span className="font-semibold">SAG</span>{' '}
            <span className="font-light text-muted-foreground">Smart Agent Gateway</span>
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            This is your personal installation. No account or password is needed.
          </p>
          <button
            type="button"
            autoFocus
            disabled={busy}
            onClick={() => void enter()}
            className="mt-6 w-full rounded-md bg-primary py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50 cursor-pointer"
          >
            {busy ? 'Opening' : 'Continue'}
          </button>
          {error && <p className="mt-3 text-sm text-destructive">{error}</p>}
        </div>
      </div>
    )
  }

  return (
    <div className="grid min-h-screen place-items-center bg-background px-6">
      <form onSubmit={submit} className="w-full max-w-xs">
        <h1 className="text-xl tracking-tight">
          <span className="font-semibold">SAG</span>{' '}
          <span className="font-light text-muted-foreground">Smart Agent Gateway</span>
        </h1>
        <p className="mt-1 text-sm text-muted-foreground">Sign in to continue.</p>

        <div className="mt-6 space-y-4">
          <Field label="Email" type="email" value={email} onChange={setEmail} autoFocus />
          <Field label="Password" type="password" value={password} onChange={setPassword} />
        </div>

        {error && <p className="mt-3 text-sm text-destructive">{error}</p>}

        <button
          type="submit"
          disabled={busy || !email || !password}
          className="mt-6 w-full rounded-md bg-primary py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
        >
          {busy ? 'Signing in' : 'Sign in'}
        </button>

        {/* A working copy offers a way in that carries no credential in the
            bundle at all, unlike the demo button below it: the account is named
            in the developer's own environment and the server presents it to the
            ordinary Login. Offered only because the server said it is one. */}
        {dev && (
          <button
            type="button"
            disabled={busy}
            onClick={() => void enter()}
            data-dev-sign-in
            className="mt-2 w-full rounded-md border border-input py-2 text-sm font-medium transition-colors hover:bg-muted disabled:opacity-50 cursor-pointer"
          >
            Sign in as the development user
          </button>
        )}

        {DEMO_EMAIL && DEMO_PASSWORD && (
          <button
            type="button"
            disabled={busy}
            onClick={() => void attempt(DEMO_EMAIL, DEMO_PASSWORD)}
            className="mt-2 w-full rounded-md border border-input py-2 text-sm font-medium transition-colors hover:bg-muted disabled:opacity-50"
          >
            Try the demo
          </button>
        )}
      </form>
    </div>
  )
}

function Field({
  label,
  value,
  onChange,
  type = 'text',
  autoFocus = false,
}: {
  label: string
  value: string
  onChange: (value: string) => void
  type?: string
  autoFocus?: boolean
}) {
  return (
    <label className="block">
      <span className="text-sm font-medium">{label}</span>
      <input
        type={type}
        value={value}
        autoFocus={autoFocus}
        onChange={(event) => onChange(event.target.value)}
        className="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus:border-ring"
      />
    </label>
  )
}
