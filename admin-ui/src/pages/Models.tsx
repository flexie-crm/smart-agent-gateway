import { useEffect, useState } from 'react'
import { Plus, Server } from 'lucide-react'
import { Page } from '@/components/AppShell'
import { Badge, DataTable } from '@/components/DataTable'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { Field, Modal } from '@/components/ui/modal'
import { NativeSelect } from '@/components/ui/native-select'
import { POSTURE } from '@/lib/api'
import { useAuth } from '@/lib/auth'
import { useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { api, useResource } from '@/lib/resources'
import { cn } from '@/lib/utils'
import type { Model, Vendor, VendorCatalog } from '@/lib/resources'

/**
 * Models: what each vendor offers.
 *
 * A model carries no capability flags. Whether it can call tools or reason
 * varies per model with no reliable machine source, so that is the AGENT's
 * choice; a model that cannot honour it returns a vendor error the chat surfaces.
 * Whether the data leaves the building is derived from the vendor (a local, self
 * hosted endpoint), never stored on the model. What the model keeps is a note.
 */
/**
 * What a model is for, in the words somebody choosing one would use.
 *
 * The values are the server's own model types. Reading a file is not a kind of
 * its own: a model that reads a PDF is a chat model handed a file, which is why
 * "Chatting and reading files" is one entry rather than two.
 */
const MODEL_TYPES = [
  { value: 'chat', label: 'Chatting, and reading files' },
  { value: 'stt', label: 'Turning audio into words' },
  { value: 'tts', label: 'Turning words into audio' },
  { value: 'embedding', label: 'Embeddings' },
  { value: 'rerank', label: 'Reranking' },
]

export function Models() {
  // ONE request for the screen: the models and the vendors they belong to come
  // back together, so nothing arrives late and rewrites the page.
  const { data: screen, reload } = useResource(() => api.models.list())
  const models = screen?.models ?? null
  const vendors = screen?.vendors ?? null
  const [editing, setEditing] = useState<{ model?: Model } | null>(null)
  const [addingLocal, setAddingLocal] = useState(false)
  const { can } = useAuth()
  // Two conditions, and they answer different questions: whether this
  // installation runs models on its own hardware at all, and whether this person
  // may see them. Neither implies the other, and an installation with no engine
  // must not offer the button to an administrator who holds every permission.
  const localModels = POSTURE.local_models && can('machines:view')

  // A local model's source is the machine on its own row; a hosted one's is the
  // vendor account. The vendor list here is accounts only, because that is what
  // the cloud form picks from.
  const sourceName = (m: Model) => m.machine || vendors?.find((v) => v.id === m.vendor_id)?.name || '—'
  // The data stays in the building when the model runs on a machine of ours. It
  // is a property of WHERE it runs, which is why it is not a flag on the model.
  const staysHere = (m: Model) => Boolean(m.machine)

  return (
    <Page
      title="Models"
      description="What each vendor can be asked to do."
      actions={
        <div className="flex gap-2">
          {/* Two ways in, because they are two different things. A cloud model
              is an id you type against an account you configured. A local one
              is already on a machine: nothing to type, only to pick. */}
          {localModels && (
            <Button size="sm" variant="outline" onClick={() => setAddingLocal(true)}>
              <Server className="size-4" />
              Add local model
            </Button>
          )}
          <Button size="sm" disabled={!vendors?.length} onClick={() => setEditing({})}>
            <Plus className="size-4" />
            Add cloud model
          </Button>
        </div>
      }
    >
      <DataTable
        items={models}
        empty={
          localModels
            ? 'No model yet. Add one from a machine of ours, or from a vendor account.'
            : vendors?.length
              ? 'No model yet. Add one from a vendor account.'
              : 'Add a vendor first: a cloud model belongs to one.'
        }
        onEdit={(model) => setEditing({ model })}
        remove={{
          run: async (model) => {
            await api.models.remove(model.id)
            await reload()
          },
          confirm: (m) => `Delete "${m.model_key}"?`,
          done: (m) => `${m.model_key} was deleted.`,
        }}
        columns={[
          {
            // A model has one name and this is it: `claude-opus-4-8` is what
            // the vendor calls it and what people call it too. It was set in a
            // monospace face, which reads as an identifier somebody is being
            // shown rather than the name of the thing in the row.
            header: 'Model',
            cell: (m) => (
              <div>
                <div className="text-sm font-medium">{m.model_key}</div>
                <div className="text-xs text-muted-foreground">{sourceName(m)}</div>
              </div>
            ),
          },
          {
            header: 'Context',
            numeric: true,
            cell: (m) => (m.context_window ? m.context_window.toLocaleString() : '—'),
          },
          {
            header: 'Data',
            cell: (m) =>
              staysHere(m) ? (
                // Worth saying out loud: this one does not leave the building.
                <Badge tone="good">stays local</Badge>
              ) : (
                <Badge tone="warn">leaves the building</Badge>
              ),
          },
          {
            header: 'Status',
            cell: (m) =>
              m.status === 'active' ? <Badge tone="good">active</Badge> : <Badge>disabled</Badge>,
          },
        ]}
      />

      {addingLocal && (
        <AddLocalModelModal
          onClose={() => setAddingLocal(false)}
          onAdded={async () => {
            setAddingLocal(false)
            await reload()
          }}
        />
      )}
      {editing && vendors && (
        <ModelForm
          model={editing.model}
          vendors={vendors}
          onClose={() => setEditing(null)}
          onSaved={async () => {
            setEditing(null)
            await reload()
          }}
        />
      )}
    </Page>
  )
}

function ModelForm({
  model,
  vendors,
  onClose,
  onSaved,
}: {
  model?: Model
  vendors: Vendor[]
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  const [vendorID, setVendorID] = useState(model?.vendor_id ?? vendors[0]?.id ?? 0)
  // A model that runs on a machine of ours. Its source is the machine, not an
  // account somebody configured, and several fields here are about accounts.
  const local = Boolean(model?.machine)
  const [key, setKey] = useState(model?.model_key ?? '')
  const [context, setContext] = useState(String(model?.context_window ?? 128000))
  const [description, setDescription] = useState(model?.description ?? '')
  const [status, setStatus] = useState(model?.status ?? 'active')
  // What this model is FOR. It is a declaration by whoever set it up, not
  // something we work out: the Gateway's Files and Audio settings offer only
  // models of the right kind, so a transcriber saved as a chat model cannot be
  // chosen for audio and nothing says why.
  const [type, setType] = useState(model?.type ?? 'chat')
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  // What this vendor says it offers, asked of the vendor itself. A model id is
  // typed from memory otherwise, and a typo saves happily and then fails on the
  // first real question somebody asks it.
  //
  // A vendor that will not say (no key yet, an endpoint that is down, an Azure
  // resource where the name is a deployment somebody made) leaves the field a
  // text box. Presenting an empty menu instead would be telling the person the
  // vendor has no models, which is not what happened.
  // The answer is stamped with the vendor it is about. An answer for the vendor
  // you have just navigated away from is not an answer to the question on the
  // screen, and showing it would offer one vendor's models under another's name.
  const [asked, setAsked] = useState<{ vendor: number; catalog: VendorCatalog } | null>(null)
  useEffect(() => {
    if (!vendorID) return
    let current = true
    const remember = (catalog: VendorCatalog) => {
      if (current) setAsked({ vendor: vendorID, catalog })
    }
    void api.vendors
      .catalog(vendorID)
      .then(remember)
      // A vendor that will not answer is not a vendor with nothing to offer, and
      // the form treats it as such: the field falls back to a text box.
      .catch(() => remember({ listed: false, models: [] }))
    return () => {
      current = false
    }
  }, [vendorID])

  const catalog = asked?.vendor === vendorID ? asked.catalog : null
  const offered = catalog?.listed ? catalog.models : []

  async function save() {
    if (!key.trim()) {
      errors.reject({ model_key: "A model needs the vendor's own name for it." })
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const body = {
        vendor_id: vendorID,
        model_key: key.trim(),
        type,
        context_window: Number(context) || 0,
        description: description.trim(),
        status,
      }
      if (model) await api.models.update(model.id, body)
      else await api.models.create(body)
      notify.success('The model was saved.')
      await onSaved()
    } catch (failure) {
      errors.fail(failure)
      setBusy(false)
    }
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={model ? 'Edit model' : 'New model'}
      onSubmit={save}
      submitting={busy}
    >
      <Field
        label="What it is for"
        required
        hint="The Gateway offers each setting only the models of the kind it needs, so this is what makes a model choosable for chatting, for reading a file, or for audio."
        error={errors.fields.type}
      >
        <NativeSelect value={type} onChange={(e) => setType(e.target.value)}>
          {MODEL_TYPES.map((k) => (
            <option key={k.value} value={k.value}>
              {k.label}
            </option>
          ))}
        </NativeSelect>
      </Field>

      {/* Not shown for a model on one of our machines, where it is neither a
          choice nor even true. The row a local model points at is the MACHINE,
          and a machine pointer is deliberately kept out of the vendor list
          (Vendors.tsx: an administrator should never find a vendor they did not
          make). So the select could not find its own value and fell back to
          rendering the first option, which showed a Qwen model as DeepSeek: a
          disabled field stating something false. Which machine it runs on is on
          its row already, and cannot be changed here anyway. */}
      {!local && (
        <Field label="Vendor" required error={errors.fields.vendor_id}>
          <NativeSelect
            value={vendorID}
            onChange={(e) => setVendorID(Number(e.target.value))}
            disabled={Boolean(model)}
          >
            {vendors.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}
              </option>
            ))}
          </NativeSelect>
        </Field>
      )}

      <Field
        label="Model"
        required
        hint={
          local
            ? 'The name this model answers to on the machine.'
            : offered.length > 0
              ? 'Offered by this vendor right now.'
              : catalog === null
                ? 'Asking the vendor what it offers...'
                : 'This vendor does not publish a list, so the name is not checked here.'
        }
        error={errors.fields.model_key}
      >
        {offered.length > 0 ? (
          <NativeSelect value={key} onChange={(e) => setKey(e.target.value)} autoFocus>
            {/* The option reads as the friendly name; the value saved is the
                vendor's own alias (m.id). A model with no friendly name falls
                back to showing the alias. */}
            {key === '' && <option value="">Choose a model</option>}
            {/* An edit whose model has since been withdrawn must still show what
                it is, rather than silently reading as a different one. */}
            {!offered.some((m) => m.id === key) && key !== '' && (
              <option value={key}>{key} (no longer offered)</option>
            )}
            {offered.map((m) => (
              <option key={m.id} value={m.id}>
                {m.name || m.id}
              </option>
            ))}
          </NativeSelect>
        ) : (
          <Input
            value={key}
            onChange={(e) => setKey(e.target.value)}
            placeholder="claude-sonnet-5"
            className="font-mono text-xs"
            autoFocus
          />
        )}
      </Field>

      <Field
        label="Context window"
        hint="Tokens. The gateway trims a conversation to fit it."
        error={errors.fields.context_window}
      >
        <Input
          value={context}
          onChange={(e) => setContext(e.target.value.replace(/\D/g, ''))}
          inputMode="numeric"
        />
      </Field>

      <Field
        label="Notes"
        hint="Anything worth remembering about this model. How hard it thinks is set on the agent that uses it."
      >
        <Textarea
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          rows={3}
          placeholder="Fast and cheap; no tool calling; good for drafting."
        />
      </Field>

      <Field label="Status" error={errors.fields.status}>
        <NativeSelect value={status} onChange={(e) => setStatus(e.target.value)}>
          <option value="active">Active</option>
          <option value="disabled">Disabled</option>
        </NativeSelect>
      </Field>

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}

/**
 * Adding a model that is already on one of our machines.
 *
 * There is nothing to type. A name, a kind and a context length are properties
 * of the weights, and the price of hardware you own is zero, so this is a picker
 * and not a form. What it writes is a ROUTE: the weights stay where they are and
 * nothing is downloaded (KB/35).
 *
 * When what somebody wants is not on any machine yet, the way to it is the
 * Inference screen, because that is where the disk and the room are. Downloading
 * from here would let a workspace-scoped decision fill a shared disk.
 */
function AddLocalModelModal({
  onClose,
  onAdded,
}: {
  onClose: () => void
  onAdded: () => Promise<void>
}) {
  const { data } = useResource(() => api.localModels.machines())
  const machines = data?.machines ?? []
  const [machineID, setMachineID] = useState<number | null>(null)
  const [uid, setUID] = useState('')
  const [busy, setBusy] = useState(false)
  const notify = useNotify()

  // The first reachable machine, so the common case (one machine) needs no
  // choosing at all. Falling back to the first of ANY is what makes a single
  // unreachable machine say so, instead of the dialog showing nothing and
  // leaving somebody to wonder which part is broken.
  const chosen =
    machines.find((m) => m.id === machineID) ?? machines.find((m) => m.reachable) ?? machines[0]
  const available = chosen?.models?.filter((m) => !m.here) ?? []

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="Add local model"
      loading={!data}
      submitLabel="Add it"
      submitting={busy}
      submitDisabled={!uid || !chosen}
      onSubmit={async () => {
        if (!chosen || !uid) return
        setBusy(true)
        try {
          await api.localModels.add(chosen.id, uid)
          notify.success('The model was added to this workspace.')
          await onAdded()
        } catch (failure) {
          notify.error(failure instanceof Error ? failure.message : 'That did not work.')
        } finally {
          setBusy(false)
        }
      }}
    >
      <p className="text-sm text-muted-foreground">
        The weights stay on the machine. This workspace gets a route to the copy already there, and
        nothing is downloaded.
      </p>

      {machines.length > 1 && (
        <Field label="Machine">
          <NativeSelect
            value={String(chosen?.id ?? '')}
            onChange={(e) => {
              setMachineID(Number(e.target.value))
              setUID('')
            }}
          >
            {machines.map((m) => (
              <option key={m.id} value={m.id} disabled={!m.reachable}>
                {m.name}
                {m.reachable ? '' : ' (unreachable)'}
              </option>
            ))}
          </NativeSelect>
        </Field>
      )}

      {machines.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No machine has registered yet. Machines add themselves: see the Inference screen.
        </p>
      )}
      {chosen && !chosen.reachable && (
        <p className="text-sm text-destructive">{chosen.problem || 'That machine did not answer.'}</p>
      )}
      {chosen?.reachable && available.length === 0 && (
        <p className="text-sm text-muted-foreground">
          {(chosen.models?.length ?? 0) === 0
            ? 'Nothing on that machine yet. Download one on the Inference screen.'
            : 'This workspace already has everything on that machine.'}
        </p>
      )}

      {available.length > 0 && (
        <ul className="-mx-5 -mb-4 mt-0! divide-y divide-border">
          {available.map((m) => (
            <li key={m.uid}>
              <button
                type="button"
                className={cn(
                  'flex w-full cursor-pointer items-center justify-between gap-4 px-5 py-2.5 text-left leading-5',
                  uid === m.uid ? 'bg-muted/60' : 'hover:bg-muted/30',
                )}
                onClick={() => setUID(m.uid)}
              >
                <span className="text-sm/5 font-medium">{m.handle}</span>
                <span className="shrink-0 text-xs/5 text-muted-foreground">
                  {m.kind}
                  {m.context_length ? ` · ${m.context_length.toLocaleString()} tokens` : ''}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </Modal>
  )
}
