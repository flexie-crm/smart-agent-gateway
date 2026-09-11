import { useState, type FormEvent } from 'react'

import type { Workspace } from '@lib/api'

/**
 * SAG adaptation: which workspace to enter.
 *
 * A person in more than one workspace chooses which to open before the chat
 * does. Everything they say is scoped to it, so the choice is made once, here,
 * rather than guessed. A person in exactly one workspace never sees this: there
 * is nothing to ask.
 */
export function WorkspacePicker({
  workspaces,
  current,
  onChoose,
  onCancel,
}: {
  workspaces: Workspace[]
  current?: number
  onChoose: (workspace: Workspace) => Promise<void>
  onCancel: () => void
}) {
  const [selected, setSelected] = useState<number>(current ?? workspaces[0]?.id ?? 0)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const workspace = workspaces.find((w) => w.id === selected)
    if (!workspace) return
    setBusy(true)
    setError(null)
    try {
      await onChoose(workspace)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'That workspace could not be opened.')
      setBusy(false)
    }
  }

  return (
    <div className="grid min-h-screen place-items-center bg-background px-6">
      <form onSubmit={submit} className="w-full max-w-xs">
        <h1 className="text-xl font-semibold tracking-tight">Choose a workspace</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          You can work in more than one. Pick where to start.
        </p>

        <label className="mt-6 block">
          <span className="text-sm font-medium">Workspace</span>
          <select
            value={selected}
            autoFocus
            onChange={(event) => setSelected(Number(event.target.value))}
            className="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus:border-ring"
          >
            {workspaces.map((workspace) => (
              <option key={workspace.id} value={workspace.id}>
                {workspace.name}
              </option>
            ))}
          </select>
        </label>

        {error && <p className="mt-3 text-sm text-destructive">{error}</p>}

        <button
          type="submit"
          disabled={busy || !selected}
          className="mt-6 w-full rounded-md bg-primary py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
        >
          {busy ? 'Opening' : 'Enter'}
        </button>
        <button
          type="button"
          onClick={onCancel}
          className="mt-2 w-full rounded-md py-2 text-sm text-muted-foreground transition-colors hover:text-foreground"
        >
          Sign in as someone else
        </button>
      </form>
    </div>
  )
}
