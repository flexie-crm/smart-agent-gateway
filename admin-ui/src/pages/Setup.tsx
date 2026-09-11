import { useEffect, useState } from 'react'
import { Loader2 } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { api } from '@/lib/resources'
import type { SetupState, VendorKind } from '@/lib/resources'
import { useNotify } from '@/lib/notify'
import { useAuth } from '@/lib/auth'

/**
 * The first thing somebody sees, once.
 *
 * Two steps, because there are two decisions: where models come from, and which
 * one. Pointing the Gateway at it is not a third: it was a screen that offered a
 * list of the one model just chosen and asked somebody to choose it again.
 *
 * They are not one form either, because the second question cannot be asked
 * until the first is answered: which models exist is something the provider is
 * asked, with the key just given for it.
 *
 * It is not a tutorial and it does not explain the product. It asks for what is
 * missing, in order, and gets out of the way: once the Gateway has a model this
 * screen is never shown again.
 */
export function Setup({ state, onDone }: { state: SetupState; onDone: () => void }) {
  return (
    <div className="mx-auto w-full max-w-lg px-6 py-16">
      <h1 className="text-2xl font-semibold tracking-tight">Set up the assistant</h1>
      <p className="mt-2 text-sm text-muted-foreground">
        The assistant needs a model to think with. This takes a minute and is asked once.
      </p>

      <div className="mt-10 divide-y">
        <Step n={1} title="Your name" done={state.has_name} current={!state.has_name}>
          <SetName ownerID={state.owner_id} onNamed={onDone} />
        </Step>

        <Step
          n={2}
          title="Where models come from"
          done={state.has_vendor}
          current={state.has_name && !state.has_vendor}
        >
          <AddVendor onAdded={onDone} />
        </Step>

        <Step
          n={3}
          title="The model it thinks with"
          done={state.ready}
          current={state.has_name && state.has_vendor && !state.ready}
        >
          <AddModel onAdded={onDone} />
        </Step>
      </div>
    </div>
  )
}

function Step({
  n,
  title,
  done,
  current,
  children,
}: {
  n: number
  title: string
  done: boolean
  current: boolean
  children: React.ReactNode
}) {
  return (
    <div className="py-6">
      <div className="flex items-baseline gap-3">
        <span
          className={`grid size-6 shrink-0 place-items-center rounded-full text-xs font-medium ${
            done ? 'bg-foreground text-background' : 'border text-muted-foreground'
          }`}
        >
          {done ? '✓' : n}
        </span>
        <h2 className={`text-sm font-medium ${done && !current ? 'text-muted-foreground' : ''}`}>
          {title}
        </h2>
      </div>
      {/* Only the step being worked on is open. The finished ones stay visible
          as a record of what was done, and the ones ahead are not asked yet
          because their answers depend on this one. */}
      {current && <div className="mt-4 pl-9">{children}</div>}
    </div>
  )
}

/**
 * What to call the person.
 *
 * It is the ordinary users row, the only one there is here, so nothing about
 * identity is special-cased: the assistant greets them by the same name a
 * deployment would show in a member list.
 */
function SetName({ ownerID, onNamed }: { ownerID?: number; onNamed: () => void }) {
  const notify = useNotify()
  const { identity, workspace } = useAuth()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)

  const id = ownerID ?? identity?.id

  const save = async () => {
    if (!id || !identity || !name.trim()) return
    setBusy(true)
    try {
      // Whole, because this endpoint replaces rather than patches. Sending the
      // one field being changed is how the model step first failed, with "a
      // name is required" from an endpoint that meant it.
      await api.users.update(id, {
        email: identity.email,
        name: name.trim(),
        status: 'active',
        workspaces: workspace ? [workspace.id] : [],
      })
      onNamed()
    } catch (failure) {
      notify.error(failure instanceof Error ? failure.message : 'That name could not be saved.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-3">
      <Input
        placeholder="What should the assistant call you?"
        value={name}
        onChange={(e) => setName(e.target.value)}
        onKeyDown={(e) => e.key === 'Enter' && void save()}
        autoFocus
      />
      <Button onClick={() => void save()} disabled={busy || !name.trim() || !id}>
        {busy && <Loader2 className="size-4 animate-spin" />}
        Continue
      </Button>
    </div>
  )
}

function AddVendor({ onAdded }: { onAdded: () => void }) {
  const notify = useNotify()
  const [kinds, setKinds] = useState<VendorKind[]>([])
  const [kind, setKind] = useState('')
  const [key, setKey] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    void api.vendors.list().then((screen) => {
      setKinds(screen.kinds)
      setKind((current) => current || screen.kinds[0]?.key || '')
    })
  }, [])

  const chosen = kinds.find((k) => k.key === kind)

  const save = async () => {
    setBusy(true)
    try {
      await api.vendors.create({
        name: chosen?.name ?? kind,
        vendor_key: kind,
        ...(key.trim() ? { credentials: key.trim() } : {}),
      })
      onAdded()
    } catch (failure) {
      notify.error(failure instanceof Error ? failure.message : 'That provider could not be added.')
      setBusy(false)
    }
  }

  return (
    <div className="space-y-3">
      <NativeSelect value={kind} onChange={(e) => setKind(e.target.value)}>
        {kinds.map((k) => (
          <option key={k.key} value={k.key}>
            {k.name}
          </option>
        ))}
      </NativeSelect>
      <Input
        type="password"
        placeholder="API key"
        value={key}
        onChange={(e) => setKey(e.target.value)}
      />
      <p className="text-xs text-muted-foreground">
        The key is stored on this computer, encrypted, and is never shown again.
      </p>
      <Button onClick={() => void save()} disabled={busy || !kind || !key.trim()}>
        {busy && <Loader2 className="size-4 animate-spin" />}
        Add provider
      </Button>
    </div>
  )
}

function AddModel({ onAdded }: { onAdded: () => void }) {
  const notify = useNotify()
  const [vendorID, setVendorID] = useState<number | null>(null)
  // What the provider says it publishes. Null while it is being asked.
  const [catalog, setCatalog] = useState<{ id: string; name?: string }[] | null>(null)
  // Typed instead, for a provider that cannot list. Empty until it has to be.
  const [typed, setTyped] = useState('')
  const [chosen, setChosen] = useState('')
  const [reasoning, setReasoning] = useState(false)
  const [busy, setBusy] = useState(false)

  const [gatewayID, setGatewayID] = useState<number | null>(null)

  useEffect(() => {
    void api.agents.screen().then((screen) => setGatewayID(screen.gateway?.id ?? null))
    void api.models.list().then(async (screen) => {
      const vendor = screen.vendors[0]
      if (!vendor) return
      setVendorID(vendor.id)
      // Asking the provider what it offers beats asking a person to remember
      // how it spells things. Not every provider can be asked, and one with a
      // bad key cannot answer at all, so a failure falls back to typing rather
      // than to an empty menu with no way past it.
      const answer = await api.vendors.catalog(vendor.id).catch(() => null)
      const models = answer?.listed ? answer.models : []
      setCatalog(models)
      setChosen(models[0]?.id ?? '')
    })
  }, [])

  const name = catalog && catalog.length > 0 ? chosen : typed.trim()

  const save = async () => {
    if (!vendorID || !name) return
    setBusy(true)
    try {
      const created = await api.models.create({
        vendor_id: vendorID,
        model_key: name,
        type: 'chat',
        // The console's own default elsewhere. It is editable on the Models
        // screen, and guessing per model would be guessing.
        context_window: 128000,
      })
      // And it is what the assistant thinks with. Choosing a model here and
      // then being asked which model to use is a question with one answer.
      //
      // No Gateway means this step cannot finish, and saying so beats leaving
      // the button turning: without one the model is created, nothing points at
      // it, and the screen sits exactly where it was.
      if (!gatewayID) {
        throw new Error('This installation has no Gateway to point at the model.')
      }
      // The whole agent, not just the field being changed: this endpoint
      // REPLACES rather than patches, and sending one field fails with "a name
      // is required". Read it back and send it whole, so nothing else about the
      // Gateway is lost on the way through.
      const form = await api.agents.form(gatewayID)
      if (!form.agent) {
        throw new Error('This installation has no Gateway to point at the model.')
      }
      await api.agents.update(gatewayID, {
        ...form.agent,
        model_id: created.id,
        reasoning,
      })
      onAdded()
    } catch (failure) {
      notify.error(failure instanceof Error ? failure.message : 'That model could not be added.')
    } finally {
      // ALWAYS, including on success. This screen stays mounted until readiness
      // changes, so leaving busy set on the happy path meant a spinner that
      // never stopped whenever the step did not actually complete.
      setBusy(false)
    }
  }

  if (catalog === null) {
    return (
      <p className="text-sm text-muted-foreground">
        <Loader2 className="mr-2 inline size-4 animate-spin" />
        Asking the provider which models it offers
      </p>
    )
  }

  return (
    <div className="space-y-3">
      {catalog.length > 0 ? (
        <NativeSelect value={chosen} onChange={(e) => setChosen(e.target.value)}>
          {catalog.map((m) => (
            <option key={m.id} value={m.id}>
              {m.name ? `${m.name} (${m.id})` : m.id}
            </option>
          ))}
        </NativeSelect>
      ) : (
        <>
          <Input
            placeholder="Model name, as the provider spells it"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            autoFocus
          />
          <p className="text-xs text-muted-foreground">
            This provider does not publish a list, so the name has to match it exactly.
          </p>
        </>
      )}
      <label className="flex cursor-pointer items-start gap-2.5 text-sm">
        <input
          type="checkbox"
          checked={reasoning}
          onChange={(e) => setReasoning(e.target.checked)}
          className="mt-0.5 size-4 cursor-pointer"
        />
        <span>
          Think before answering
          <span className="block text-xs text-muted-foreground">
            Slower and more expensive, and only some models can. One that cannot will say so.
          </span>
        </span>
      </label>

      <Button onClick={() => void save()} disabled={busy || !name || !vendorID}>
        {busy && <Loader2 className="size-4 animate-spin" />}
        Add model
      </Button>
    </div>
  )
}
