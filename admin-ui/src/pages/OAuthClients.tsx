import { useState } from 'react'
import { Plus, X } from 'lucide-react'
import { Page } from '@/components/AppShell'
import { Badge, DataTable } from '@/components/DataTable'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Field, Modal } from '@/components/ui/modal'
import { NativeSelect } from '@/components/ui/native-select'
import { useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { api, useResource } from '@/lib/resources'
import type { OAuthClient } from '@/lib/resources'

/**
 * OAuth clients: who may connect to our surfaces (the CRM's screen, kept).
 *
 * Three kinds, one modal: a browser or native app (PKCE, no secret), a
 * server-side app (issued a secret, shown once), and a service token,
 * machine to machine, minted the moment you save, acting as you, shown
 * once. Deleting a client takes every token it owns, which for a service
 * client is the kill switch.
 */
export function OAuthClients() {
  const { data: clients, reload } = useResource(() => api.oauthClients.list())
  const [editing, setEditing] = useState<{ client?: OAuthClient } | null>(null)

  return (
    <Page
      title="OAuth Clients"
      description="Applications and machine tokens that may connect to this workspace."
      actions={
        <Button size="sm" onClick={() => setEditing({})}>
          <Plus className="size-4" />
          New OAuth Client
        </Button>
      }
    >
      <DataTable
        items={clients}
        empty="No client yet. External agents register themselves, or you mint one here."
        onEdit={(client) => setEditing({ client })}
        remove={{
          run: async (client) => {
            await api.oauthClients.remove(client.id)
            await reload()
          },
          confirm: (c) =>
            `Delete "${c.name}"? Every token it owns dies with it` +
            (c.client_type === 'service' ? ', including its service token.' : '.'),
          done: (c) => `${c.name} was deleted, and every token it held.`,
        }}
        columns={[
          { header: 'Name', cell: (c) => <span className="font-medium">{c.name}</span> },
          {
            header: 'Client ID',
            cell: (c) => <span className="font-mono text-xs text-muted-foreground">{c.client_id}</span>,
          },
          {
            header: 'Type',
            cell: (c) =>
              c.client_type === 'service' ? (
                <Badge tone="warn">service token</Badge>
              ) : c.client_type === 'confidential' ? (
                <Badge>server-side</Badge>
              ) : (
                <Badge>browser / native</Badge>
              ),
          },
          {
            header: 'Scopes',
            cell: (c) => <span className="text-xs text-muted-foreground">{c.scopes.join(', ')}</span>,
          },
          {
            header: 'Source',
            cell: (c) => (c.is_dcr ? <Badge>self-registered</Badge> : <Badge>manual</Badge>),
          },
          {
            header: 'Status',
            cell: (c) =>
              c.status === 'active' ? <Badge tone="good">active</Badge> : <Badge>disabled</Badge>,
          },
        ]}
      />

      {editing && (
        <ClientForm
          client={editing.client}
          onClose={() => setEditing(null)}
          onSaved={async () => {
            await reload()
          }}
          onDone={() => setEditing(null)}
        />
      )}
    </Page>
  )
}

const TYPE_HINTS: Record<string, string> = {
  public:
    "Runs on a user's device and cannot keep a secret, so it uses PKCE. Signs the user in and returns to a redirect address.",
  confidential:
    'Runs on a backend you control and is issued a client secret. Signs the user in and returns to a redirect address.',
  service:
    'Machine to machine, with no login and no redirect. A long-lived token is issued the moment you save. It acts as you and carries your access. Copy it once; it cannot be shown again.',
}

function ClientForm({
  client,
  onClose,
  onSaved,
  onDone,
}: {
  client?: OAuthClient
  onClose: () => void
  onSaved: () => Promise<void>
  onDone: () => void
}) {
  const notify = useNotify()
  const [name, setName] = useState(client?.name ?? '')
  const [clientType, setClientType] = useState(client?.client_type ?? 'public')
  const [redirects, setRedirects] = useState<string[]>(
    client?.redirect_uris.length ? client.redirect_uris : [''],
  )
  const [status, setStatus] = useState(client?.status ?? 'active')
  const [busy, setBusy] = useState(false)
  // The shown-once panel: after a save that minted something, the modal
  // stays and shows it. Closing it is the last time anyone sees the value.
  const [minted, setMinted] = useState<{ label: string; value: string } | null>(null)
  const errors = useFormErrors()

  async function save() {
    const problems: Record<string, string> = {}
    if (!name.trim()) problems.name = 'A client needs a name.'
    if (Object.keys(problems).length > 0) {
      errors.reject(problems)
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const body = {
        name: name.trim(),
        client_type: clientType,
        redirect_uris: redirects.map((r) => r.trim()).filter(Boolean),
      }
      const saved = client
        ? await api.oauthClients.update(client.id, { ...body, status })
        : await api.oauthClients.create(body)
      notify.success('The client was saved.')
      await onSaved()
      if (saved.service_token) {
        setMinted({ label: 'Service token', value: saved.service_token })
      } else if (saved.client_secret) {
        setMinted({ label: 'Client secret', value: saved.client_secret })
      } else {
        onDone()
      }
    } catch (failure) {
      errors.fail(failure)
    } finally {
      setBusy(false)
    }
  }

  if (minted) {
    return (
      <Modal
        open
        onOpenChange={(o) => !o && onDone()}
        title={`${minted.label} for ${name.trim()}`}
        onSubmit={onDone}
        submitLabel="Done"
      >
        <p className="text-sm text-muted-foreground">
          Copy it now. It is stored only as a hash and can never be shown again; if it is
          lost, delete the client and create a new one.
        </p>
        <Input readOnly value={minted.value} className="font-mono text-xs" onFocus={(e) => e.target.select()} />
      </Modal>
    )
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={client ? 'Edit OAuth client' : 'New OAuth Client'}
      onSubmit={save}
      submitting={busy}
    >
      <Field label="Client name" required error={errors.fields.name}>
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus placeholder="e.g. Acme Integration" />
      </Field>

      <Field label="Client type" hint={TYPE_HINTS[clientType]} error={errors.fields.client_type}>
        <NativeSelect value={clientType} onChange={(e) => setClientType(e.target.value)}>
          <option value="public">Browser or native application</option>
          <option value="confidential">Server-side application</option>
          <option value="service">Service token</option>
        </NativeSelect>
      </Field>

      <Field label="Scopes" hint="What a token from this client may reach.">
        <Input readOnly value="mcp" className="font-mono text-xs" />
      </Field>

      {clientType !== 'service' && (
        <Field
          label="Redirect URIs"
          required
          hint="The callback address the app is sent back to after sign-in, matched exactly."
          error={errors.fields.redirect_uris}
        >
          <div className="space-y-1.5">
            {redirects.map((value, index) => (
              <div key={index} className="flex items-center gap-2">
                <Input
                  value={value}
                  onChange={(e) =>
                    setRedirects((prev) => prev.map((v, i) => (i === index ? e.target.value : v)))
                  }
                  placeholder="https://app.example.com/callback"
                  className="font-mono text-xs"
                />
                {redirects.length > 1 && (
                  <button
                    type="button"
                    aria-label="Remove redirect URI"
                    onClick={() => setRedirects((prev) => prev.filter((_, i) => i !== index))}
                    className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                  >
                    <X className="size-4" />
                  </button>
                )}
              </div>
            ))}
            <button
              type="button"
              onClick={() => setRedirects((prev) => [...prev, ''])}
              className="flex items-center gap-1.5 text-sm font-medium text-primary hover:underline"
            >
              <Plus className="size-4" />
              Add URI
            </button>
          </div>
        </Field>
      )}

      {client && (
        <Field label="Status" error={errors.fields.status}>
          <NativeSelect value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="active">Active</option>
            <option value="disabled">Disabled</option>
          </NativeSelect>
        </Field>
      )}

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}
