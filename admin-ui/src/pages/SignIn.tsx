import { useState } from 'react'
import type { FormEvent } from 'react'
import { Loader2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useAuth } from '@/lib/auth'

/**
 * Sign in.
 *
 * Email and password. The workspace is NOT asked for: a person is not asked to
 * remember one, and the server already knows where they were. The sidebar moves
 * them if they want to be somewhere else. No 2FA yet, and nothing here pretends
 * otherwise: a form that shows a disabled "two-factor" field is a promise the
 * product has not made.
 *
 * The error says the credentials were refused and nothing more, except for the
 * one failure that is not about credentials at all: an account nobody has put in
 * a workspace yet, which the server names, because "check your password" would
 * send them to fix the thing that works.
 */
export function SignIn() {
  const { signIn, signInLocally, posture, dev } = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError('')
    setBusy(true)
    try {
      await signIn(email.trim(), password)
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : 'Invalid email or password.')
      setBusy(false)
    }
  }


  async function enter() {
    setError('')
    setBusy(true)
    try {
      await signInLocally()
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : 'This computer could not be signed in.')
      setBusy(false)
    }
  }

  return (
    <div className="min-h-dvh grid place-items-center bg-background px-6">
      <div className="w-full max-w-sm">
        <div className="mb-10">
          <div className="flex items-baseline gap-2">
            <span className="text-2xl font-semibold tracking-tight">SAG</span>
            <span className="text-2xl font-light tracking-tight text-muted-foreground">
              Smart Agent Gateway
            </span>
          </div>
          <p className="mt-1.5 text-sm text-muted-foreground">
            {posture.local_sign_in
              ? 'This is your personal installation. No account or password is needed.'
              : 'Sign in to the orchestrator.'}
          </p>
        </div>

        {posture.local_sign_in ? (
          <div className="space-y-4">
            <Button onClick={enter} className="w-full" disabled={busy} autoFocus>
              {busy && <Loader2 className="size-4 animate-spin" />}
              {busy ? 'Opening' : 'Continue'}
            </Button>
            {error && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </div>
        ) : (
        <form onSubmit={submit} className="space-y-4" noValidate>
          <Field label="Email" htmlFor="email">
            <Input
              id="email"
              type="email"
              autoComplete="username"
              autoFocus
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
            />
          </Field>

          <Field label="Password" htmlFor="password">
            <Input
              id="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </Field>

          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}

          <Button type="submit" className="w-full" disabled={busy}>
            {busy && <Loader2 className="size-4 animate-spin" />}
            Sign in
          </Button>

          {/* Only a working copy offers this, and only because the server said
              so. It is the same route and the same Login the button above uses:
              the account is named in this developer's own environment instead of
              typed, so a password nobody needs to remember is not a password
              anybody writes on a sticky note either. */}
          {dev && (
            <Button
              type="button"
              variant="outline"
              className="w-full"
              disabled={busy}
              onClick={enter}
              data-dev-sign-in
            >
              Sign in as the development user
            </Button>
          )}
        </form>
        )}
      </div>
    </div>
  )
}

function Field({
  label,
  htmlFor,
  children,
}: {
  label: string
  htmlFor: string
  children: React.ReactNode
}) {
  return (
    <div className="space-y-1.5">
      <label htmlFor={htmlFor} className="text-sm font-medium">
        {label}
      </label>
      {children}
    </div>
  )
}
