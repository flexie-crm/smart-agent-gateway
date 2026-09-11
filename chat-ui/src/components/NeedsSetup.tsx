import type { SetupState } from '@lib/api'

/**
 * What the chat shows before anything can answer.
 *
 * The chat is downstream of the console: it needs a model, and only the console
 * can give it one. So this replaces the composer rather than sitting above it.
 * Letting somebody type a message that cannot be answered is a worse experience
 * than telling them plainly what is missing, and a disabled box with a tooltip
 * is the same failure with more steps.
 */
export function NeedsSetup({ state }: { state: SetupState }) {
  return (
    <div className="grid min-h-screen place-items-center bg-background px-6">
      <div className="w-full max-w-md">
        <h1 className="text-xl font-semibold tracking-tight">Nothing can answer yet</h1>
        <p className="mt-2 text-sm text-muted-foreground">{reason(state)}</p>

        {/* A link, because the console is the same origin one navigation away.
            It used to be a button that asked the gateway to launch a second
            application; there is one application now, and the endpoint, the
            platform launcher and the bundle identifier they needed all went
            with it. */}
        <a
          href="/setup"
          className="mt-6 flex w-full items-center justify-center rounded-md bg-primary py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
        >
          Open the console
        </a>

        <p className="mt-6 text-xs text-muted-foreground">
          This page will pick it up on its own once a model is set.
        </p>
      </div>
    </div>
  )
}

/**
 * Says which step is outstanding rather than "setup is incomplete".
 *
 * The three states are genuinely different pieces of work, and somebody who has
 * added a model already should not be told to add one.
 */
function reason(state: SetupState): string {
  if (!state.has_vendor) {
    return 'The assistant needs a model to think with. Add a provider in the console, or download a model onto this computer, and point the Gateway at it.'
  }
  if (!state.has_model) {
    return 'A provider is configured, but there is no model yet. Add one in the console and point the Gateway at it.'
  }
  return 'There are models available, but the Gateway is not set to use one yet. Choose one in the console.'
}
