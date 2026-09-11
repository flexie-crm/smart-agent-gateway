import { useState } from 'react'
import { KeyRound, Plus } from 'lucide-react'
import { Page } from '@/components/AppShell'
import { Badge, DataTable } from '@/components/DataTable'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Field, Modal } from '@/components/ui/modal'
import { NativeSelect } from '@/components/ui/native-select'
import { POSTURE } from '@/lib/api'
import { useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { api, useResource } from '@/lib/resources'
import type { Vendor } from '@/lib/resources'

/**
 * Vendors: the provider accounts the gateway can call.
 *
 * The API key is WRITE-ONLY. It is sealed on arrival and never read back, so
 * this screen can say whether a key is stored and never what it is. A console
 * that can show you a secret is a console that can leak one.
 */
export function Vendors() {
  // ONE request for the screen: the vendors, and the kinds one can be.
  const { data: screen, reload } = useResource(() => api.vendors.list())
  const vendors = screen?.vendors ?? null
  const kinds = screen?.kinds ?? null
  const [editing, setEditing] = useState<{ vendor?: Vendor } | null>(null)

  return (
    <Page
      title="Vendors"
      description="The provider accounts the gateway can call."
      actions={
        <Button size="sm" onClick={() => setEditing({})}>
          <Plus className="size-4" />
          New vendor
        </Button>
      }
    >
      <DataTable
        items={vendors}
        empty="No vendor yet. Without one, nothing can answer a question."
        onEdit={(vendor) => setEditing({ vendor })}
        remove={{
          run: async (vendor) => {
            await api.vendors.remove(vendor.id)
            await reload()
          },
          confirm: (v) => `Delete "${v.name}"? Its models go with it.`,
          done: (v) => `${v.name} was deleted.`,
        }}
        columns={[
          {
            // The name the person gave it, and nothing else. The kind underneath
            // it (`anthropic`) was the key we dispatch on: ours, unchangeable
            // after creation, and printed under a row whose name usually said
            // the same word in the language people speak. It is on the form,
            // where it is a choice somebody makes.
            header: 'Name',
            cell: (v) => <div className="font-medium">{v.name}</div>,
          },
          {
            header: 'Endpoint',
            cell: (v) => (
              <span className="font-mono text-xs text-muted-foreground">
                {v.base_url || 'the vendor default'}
              </span>
            ),
          },
          {
            header: 'Key',
            cell: (v) =>
              v.has_credentials ? (
                <Badge tone="good">
                  <KeyRound className="mr-1 size-3" />
                  stored
                </Badge>
              ) : (
                // A local model needs no key at all, so this is not an error.
                <Badge>none</Badge>
              ),
          },
          {
            header: 'Status',
            cell: (v) =>
              v.status === 'active' ? <Badge tone="good">active</Badge> : <Badge>disabled</Badge>,
          },
        ]}
      />

      {editing && (
        <VendorForm
          vendor={editing.vendor}
          kinds={kinds ?? []}
          usedKeys={(vendors ?? []).map((v) => v.vendor_key)}
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

// A self-hosted endpoint can be added many times: each is a different server.
// Every other kind is one account, so once it exists it is not offered again.
//
// This is somebody ELSE's server. Our own machines are not added here at all:
// they join themselves and appear under Inference, and a model from one is added
// on the Models screen (KB/35).
const SELF_HOSTED_KIND = 'openai-compatible'

function VendorForm({
  vendor,
  kinds,
  usedKeys,
  onClose,
  onSaved,
}: {
  vendor?: Vendor
  kinds: { key: string; name: string; requires_base_url?: boolean }[]
  usedKeys: string[]
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  // A kind is offered when it is not already configured, unless it is the one
  // being edited (which must still show) or the repeatable local kind.
  const available = kinds.filter(
    (k) => k.key === vendor?.vendor_key || k.key === SELF_HOSTED_KIND || !usedKeys.includes(k.key),
  )
  const needsEndpoint = (key: string) => kinds.find((k) => k.key === key)?.requires_base_url ?? false

  const [name, setName] = useState(vendor?.name ?? '')
  const [kind, setKind] = useState(vendor?.vendor_key ?? available[0]?.key ?? 'openai')
  const [baseURL, setBaseURL] = useState(vendor?.base_url ?? '')
  const [credentials, setCredentials] = useState('')
  const [status, setStatus] = useState(vendor?.status ?? 'active')
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  async function save() {
    if (!name.trim()) {
      errors.reject({ name: 'A vendor needs a name.' })
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const body = {
        name: name.trim(),
        vendor_key: kind,
        base_url: baseURL.trim(),
        status,
        // Empty means "leave the stored secret alone". It never means "erase it":
        // somebody editing a name must not have to retype an API key.
        ...(credentials ? { credentials } : {}),
      }
      if (vendor) await api.vendors.update(vendor.id, body)
      else await api.vendors.create(body)
      notify.success('The vendor was saved.')
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
      title={vendor ? 'Edit vendor' : 'New vendor'}
      onSubmit={save}
      submitting={busy}
    >
      <Field label="Name" required error={errors.fields.name}>
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
      </Field>

      <Field
        label="Vendor"
        hint="This decides how the gateway talks to it on the wire."
        error={errors.fields.vendor_key}
      >
        <NativeSelect
          value={kind}
          onChange={(e) => {
            const next = e.target.value
            setKind(next)
            // A kind that runs on the vendor's own API has no endpoint to set, so
            // drop anything typed for one that did.
            if (!needsEndpoint(next)) setBaseURL('')
          }}
          disabled={Boolean(vendor)}
        >
          {available.map((k) => (
            <option key={k.key} value={k.key}>
              {k.name}
            </option>
          ))}
        </NativeSelect>
      </Field>

      {/* The endpoint is only asked of a vendor defined by where it runs (a
          self-hosted server, or Azure's per-customer endpoint); a fixed-API
          vendor uses its own. Shown exactly when validation will require it. */}
      {needsEndpoint(kind) && (
        <Field
          label="Endpoint"
          hint={
            kind === SELF_HOSTED_KIND
              ? // Named for the server most people point it at, but it is the
                // generic protocol, so the ones that are not Ollama are said
                // here rather than left to be discovered.
                // The last sentence sends the reader to a screen, so it is said
                // only where that screen exists. An installation with no engine
                // has no machines, and naming one is worse than saying nothing:
                // it describes a way of doing this that is not there.
                'The address of the server. Anything speaking the same protocol works: Ollama, vLLM, LM Studio.' +
                (POSTURE.local_models
                  ? ' Our own machines are not added here, they appear under Inference.'
                  : '')
              : 'The address of your endpoint.'
          }
          error={errors.fields.base_url}
        >
          <Input
            value={baseURL}
            onChange={(e) => setBaseURL(e.target.value)}
            placeholder="https://..."
            className="font-mono text-xs"
          />
        </Field>
      )}

      <Field
        label="API key"
        hint={
          vendor?.has_credentials
            ? 'A key is stored. Type a new one to replace it, or leave this empty to keep it.'
            : kind === SELF_HOSTED_KIND
              ? 'A self-hosted model may need none.'
              : 'Needed to reach this vendor.'
        }
        error={errors.fields.credentials}
      >
        <Input
          type="password"
          value={credentials}
          onChange={(e) => setCredentials(e.target.value)}
          placeholder={vendor?.has_credentials ? '••••••••••••••••' : ''}
          autoComplete="off"
        />
      </Field>

      <Field label="Status" error={errors.fields.status}>
        <NativeSelect
          value={status}
          onChange={(e) => setStatus(e.target.value)}
        >
          <option value="active">Active</option>
          <option value="disabled">Disabled</option>
        </NativeSelect>
      </Field>

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}
