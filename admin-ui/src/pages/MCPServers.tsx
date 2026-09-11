import { useEffect, useRef, useState } from 'react'
import { ExternalLink, Plus, RefreshCw } from 'lucide-react'
import { Page } from '@/components/AppShell'
import { Badge, DataTable } from '@/components/DataTable'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Field, Modal } from '@/components/ui/modal'
import { NativeSelect } from '@/components/ui/native-select'
import { describeError, useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { api, useResource } from '@/lib/resources'
import type { MCPServer } from '@/lib/resources'
import { listen } from '@/lib/ws'
import type { SocketMessage } from '@/lib/reconnecting-socket'

/**
 * What the gateway pushes when a connection's consent has finished, whichever
 * way it went. It arrives on the socket rather than from the page the person
 * came back to, because that page is very often not in this application at all.
 */
export interface ConnectionOutcome {
  id: number
  name?: string
  connected: boolean
  detail: string
}

/**
 * connectionOutcome reads one off the wire, or returns null for every other
 * message the socket carries.
 */
export function connectionOutcome(message: SocketMessage): ConnectionOutcome | null {
  if (message.type !== 'notification') return null
  const inner = message.payload as { type?: string; payload?: ConnectionOutcome } | undefined
  if (inner?.type !== 'mcp_connection' || !inner.payload) return null
  return inner.payload
}

/**
 * MCP servers: the third-party tool servers this workspace consumes (SAG as
 * the client; our own MCP server lives under Access).
 *
 * A server's tools are projected into the Tools screen, where the usual
 * governance applies: on or off, approval, grants, and which agents carry
 * them. Here lives only the connection itself: where it is, how it
 * authenticates, and whether the last sync agreed with the remote.
 */
export function MCPServers() {
  const { data: servers, reload } = useResource(() => api.mcpServers.list())
  const [editing, setEditing] = useState<{ server?: MCPServer } | null>(null)
  const [consent, setConsent] = useState<{ server: MCPServer; url: string } | null>(null)
  const [busyRow, setBusyRow] = useState(0)
  const notify = useNotify()

  // The screen listens for as long as it is open, rather than only while a
  // connection it started is outstanding. The consent finishes in a browser,
  // which may not be this application and may not even be this machine, so the
  // only thing that can be relied upon to report it is the gateway itself.
  const latest = useRef({ reload, notify })
  useEffect(() => {
    latest.current = { reload, notify }
  })
  useEffect(
    () =>
      listen((message) => {
        const outcome = connectionOutcome(message)
        if (!outcome) return
        const { reload: refresh, notify: say } = latest.current
        if (outcome.connected) say.success(outcome.detail, outcome.name)
        else say.error(outcome.detail, outcome.name)
        setConsent(null)
        void refresh()
      }),
    [],
  )

  async function sync(server: MCPServer) {
    setBusyRow(server.id)
    try {
      const result = await api.mcpServers.sync(server.id)
      // "Nothing changed" is an ANSWER to the question that was asked, and it
      // used to be silence: a click on Sync that found no drift said nothing at
      // all, which reads exactly like a click that did nothing.
      const changed = result.added || result.changed || result.missing || result.skipped
      notify.success(
        changed
          ? `${result.added} new, ${result.changed} changed, ${result.missing} gone.` +
              (result.skipped ? ` ${result.skipped} could not be projected.` : '')
          : `Checked, and its ${result.offered} tools are unchanged.`,
        server.name,
      )
    } catch (failure) {
      notify.error(describeError(failure))
    } finally {
      setBusyRow(0)
      await reload()
    }
  }

  /**
   * Ask the gateway to prepare the consent, then show the person where they are
   * about to be sent.
   *
   * It used to open a popup and point it at the address once the gateway had
   * minted one. That cannot work: preparing the consent means discovery and, on
   * a server that offers it, registering ourselves, which is most of a second of
   * real work. A window opened after that wait is a window opened outside the
   * click that asked for it, and no webview allows one. In the desktop
   * application it is not blocked so much as ignored: nothing opens, nothing
   * fails, and the button simply sits there.
   *
   * So the address is shown instead, and the person opens it themselves. Their
   * click is a click, wherever they are, and they get to see whose sign-in page
   * they are about to be sent to before they go.
   */
  async function connect(server: MCPServer) {
    setBusyRow(server.id)
    try {
      const { authorize_url } = await api.mcpServers.connect(server.id)
      setConsent({ server, url: authorize_url })
    } catch (failure) {
      notify.error(describeError(failure))
    } finally {
      setBusyRow(0)
    }
  }

  return (
    <Page
      title="MCP Servers"
      description="External tool servers this workspace consumes. Their tools land on the Tools screen, governed like any other."
      actions={
        <Button size="sm" onClick={() => setEditing({})}>
          <Plus className="size-4" />
          Add MCP Server
        </Button>
      }
    >
      <DataTable
        items={servers}
        empty="No MCP server yet. Add one and its tools become grantable."
        onEdit={(server) => setEditing({ server })}
        remove={{
          run: async (server) => {
            await api.mcpServers.remove(server.id)
            await reload()
          },
          confirm: (s) =>
            `Delete "${s.name}"? Its tools disappear from every agent that carries them.`,
          done: (s) => `${s.name} was disconnected, and its tools are gone.`,
        }}
        columns={[
          {
            header: 'Service',
            cell: (s) => (
              <div>
                <div className="font-medium">{s.name}</div>
                <div className="font-mono text-xs text-muted-foreground">{s.url}</div>
              </div>
            ),
          },
          {
            // The prefix used to be here, as `crm.*`. It is our namespacing and
            // no longer appears anywhere a person reads: the catalogue groups
            // tools under this service's NAME instead. What is worth a column
            // is when this connection was made.
            header: 'Created',
            cell: (s) => (
              <span className="text-xs text-muted-foreground">
                {new Date(s.created_at).toLocaleDateString()}
              </span>
            ),
          },
          {
            header: 'Access',
            cell: (s) =>
              s.auth_type === 'none' ? (
                <Badge>open</Badge>
              ) : s.auth_type === 'api_key' ? (
                s.has_api_key ? (
                  <Badge tone="good">key stored</Badge>
                ) : (
                  <Badge tone="warn">no key</Badge>
                )
              ) : s.connected ? (
                <Badge tone="good">connected</Badge>
              ) : (
                <Button size="sm" variant="outline" disabled={busyRow === s.id} onClick={() => void connect(s)}>
                  <ExternalLink className="size-4" />
                  Connect
                </Button>
              ),
          },
          {
            header: 'Last sync',
            cell: (s) => (
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  aria-label={`Sync ${s.name}`}
                  disabled={busyRow === s.id}
                  onClick={() => void sync(s)}
                  className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:opacity-50"
                >
                  <RefreshCw className={busyRow === s.id ? 'size-3.5 animate-spin' : 'size-3.5'} />
                </button>
                {s.last_error ? (
                  <Badge tone="bad" title={s.last_error}>
                    failed
                  </Badge>
                ) : s.last_synced_at ? (
                  <span className="text-xs text-muted-foreground">
                    {new Date(s.last_synced_at).toLocaleString()}
                  </span>
                ) : (
                  <span className="text-xs text-muted-foreground">never</span>
                )}
              </div>
            ),
          },
          {
            header: 'Status',
            cell: (s) =>
              s.status === 'active' ? <Badge tone="good">active</Badge> : <Badge>disabled</Badge>,
          },
        ]}
      />

      {consent && (
        <Modal
          open
          onOpenChange={(open) => !open && setConsent(null)}
          title={`Connect to ${consent.server.name}`}
          submitLabel="Continue"
          // Opened from the click itself, with nothing awaited in between: that
          // is the whole reason this dialog exists. Where it opens is the
          // surface's business, not this screen's, and on the desktop it is the
          // person's own browser.
          onSubmit={() => {
            window.open(consent.url, '_blank', 'noopener')
          }}
        >
          <p className="text-sm text-muted-foreground">
            You will be asked to sign in and approve the connection. Come back here when you are
            done: this screen updates itself.
          </p>
          <Field label="Where you are going">
            {/* Selectable, and the whole address: if a browser refuses to open
                it, it can still be carried across by hand. */}
            <p className="break-all font-mono text-xs text-muted-foreground">{consent.url}</p>
          </Field>
        </Modal>
      )}

      {editing && (
        <MCPServerForm
          server={editing.server}
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

function MCPServerForm({
  server,
  onClose,
  onSaved,
}: {
  server?: MCPServer
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const [name, setName] = useState(server?.name ?? '')
  const [url, setURL] = useState(server?.url ?? '')
  const [authType, setAuthType] = useState(server?.auth_type ?? 'api_key')
  const [apiKey, setAPIKey] = useState('')
  // Only for a service that will not issue a client of its own. Most do, and
  // these stay empty; the id is not a secret so it is prefilled, the secret is
  // write-only like the API key.
  const [clientID, setClientID] = useState(server?.oauth_client_id ?? '')
  const [clientSecret, setClientSecret] = useState('')
  const [status, setStatus] = useState(server?.status ?? 'active')
  const [busy, setBusy] = useState(false)
  const notify = useNotify()
  const errors = useFormErrors()

  async function save() {
    const problems: Record<string, string> = {}
    if (!name.trim()) problems.name = 'An MCP server needs a name.'
    if (!url.trim()) problems.url = "The service's address is required."
    if (Object.keys(problems).length > 0) {
      errors.reject(problems)
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const body = {
        name: name.trim(),
        url: url.trim(),
        auth_type: authType,
        // Empty means "leave the stored key alone", never "erase it".
        ...(apiKey ? { api_key: apiKey } : {}),
        // The id is an ordinary field: sent every time, so clearing it works and
        // the connection falls back to a client the service issues itself.
        ...(authType === 'oauth' ? { oauth_client_id: clientID.trim() } : {}),
        ...(clientSecret ? { oauth_client_secret: clientSecret } : {}),
      }
      if (server) await api.mcpServers.update(server.id, { ...body, status })
      else await api.mcpServers.create(body)
      notify.success(
        server
          ? `${name.trim()} was saved.`
          : `${name.trim()} was added. Sync it to bring its tools in.`,
      )
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
      title={server ? 'Edit MCP server' : 'New MCP server'}
      onSubmit={save}
      submitting={busy}
    >
      <Field
        label="Name"
        required
        hint={
          server
            ? 'Its tools are listed under this name. Renaming moves the heading, not the tools.'
            : 'Its tools are listed under this name.'
        }
        error={errors.fields.name}
      >
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
      </Field>

      <Field label="Address" required hint="Where the service listens." error={errors.fields.url}>
        <Input
          value={url}
          onChange={(e) => setURL(e.target.value)}
          placeholder="https://..."
          className="font-mono text-xs"
        />
      </Field>

      <Field
        label="Authentication"
        hint="How the gateway identifies itself to the service."
        error={errors.fields.auth_type}
      >
        <NativeSelect value={authType} onChange={(e) => setAuthType(e.target.value)}>
          <option value="none">None</option>
          <option value="api_key">API key</option>
          <option value="oauth">OAuth (connect after saving)</option>
        </NativeSelect>
      </Field>

      {authType === 'api_key' && (
        <Field
          label="API key"
          hint={
            server?.has_api_key
              ? 'A key is stored. Type a new one to replace it, or leave this empty to keep it.'
              : 'Sent as a bearer token on every request.'
          }
          error={errors.fields.api_key}
        >
          <Input
            type="password"
            value={apiKey}
            onChange={(e) => setAPIKey(e.target.value)}
            placeholder={server?.has_api_key ? '••••••••••••••••' : ''}
            autoComplete="off"
          />
        </Field>
      )}

      {authType === 'oauth' && (
        <>
          <Field
            label="Client ID"
            hint="Leave empty if the service registers applications itself, which most do. If it does not, create the application on its side and paste the id here."
            error={errors.fields.oauth_client_id}
          >
            <Input
              value={clientID}
              onChange={(e) => setClientID(e.target.value)}
              placeholder="only if the service does not register clients"
              autoComplete="off"
            />
          </Field>
          <Field
            label="Client secret"
            hint={
              server?.has_oauth_client_secret
                ? 'A secret is stored. Type a new one to replace it, or leave this empty to keep it.'
                : 'Only if the service issued one with the client id. Public clients have none.'
            }
            error={errors.fields.oauth_client_secret}
          >
            <Input
              type="password"
              value={clientSecret}
              onChange={(e) => setClientSecret(e.target.value)}
              placeholder={server?.has_oauth_client_secret ? '••••••••••••••••' : ''}
              autoComplete="off"
            />
          </Field>
        </>
      )}

      {server && (
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
