import { useState } from 'react'
import { Plus } from 'lucide-react'
import { Page } from '@/components/AppShell'
import { Badge, DataTable } from '@/components/DataTable'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Field, Modal } from '@/components/ui/modal'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { api, useResource } from '@/lib/resources'
import type { Workspace } from '@/lib/resources'

/**
 * Workspaces: the partitions of the tenant.
 *
 * Everything an administrator configures (vendors, agents, brains, groups,
 * roles) lives inside one, and deleting one takes all of that with it. The
 * server refuses to delete the workspace you are standing in: switch somewhere
 * else first, then take the ground away.
 */
export function Workspaces() {
  const { data: workspaces, reload } = useResource(() => api.workspaces.list())
  const [editing, setEditing] = useState<{ workspace?: Workspace } | null>(null)

  return (
    <Page
      title="Workspaces"
      description="The partitions of the company. Everything else lives inside one."
      actions={
        <Button size="sm" onClick={() => setEditing({})}>
          <Plus className="size-4" />
          New workspace
        </Button>
      }
    >
      <DataTable
        items={workspaces}
        empty="No workspace. Nobody can sign in without one."
        onEdit={(workspace) => setEditing({ workspace })}
        remove={{
          run: async (workspace) => {
            await api.workspaces.remove(workspace.id)
            await reload()
          },
          confirm: (w) =>
            `Delete "${w.name}"? Its vendors, agents, brains, groups, roles and every conversation go with it.`,
          done: (w) => `${w.name} was deleted, and everything in it.`,
        }}
        columns={[
          { header: 'Name', cell: (w) => <span className="font-medium">{w.name}</span> },
          {
            header: 'Description',
            cell: (w) =>
              w.description ? (
                <span className="text-sm text-muted-foreground">{w.description}</span>
              ) : (
                <span className="text-sm text-muted-foreground/60">—</span>
              ),
          },
          {
            header: 'Status',
            cell: (w) =>
              w.status === 'active' ? (
                <Badge tone="good">active</Badge>
              ) : (
                <Badge tone="warn">suspended</Badge>
              ),
          },
        ]}
      />

      {editing && (
        <WorkspaceForm
          workspace={editing.workspace}
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

function WorkspaceForm({
  workspace,
  onClose,
  onSaved,
}: {
  workspace?: Workspace
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  const [name, setName] = useState(workspace?.name ?? '')
  const [description, setDescription] = useState(workspace?.description ?? '')
  const [status, setStatus] = useState(workspace?.status ?? 'active')
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  async function save() {
    if (!name.trim()) {
      errors.reject({ name: 'A workspace needs a name.' })
      return
    }
    errors.clear()
    setBusy(true)
    try {
      if (workspace) {
        await api.workspaces.update(workspace.id, { name: name.trim(), description: description.trim(), status })
      } else {
        await api.workspaces.create({ name: name.trim(), description: description.trim() })
      }
      notify.success('The workspace was saved.')
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
      title={workspace ? 'Edit workspace' : 'New workspace'}
      onSubmit={save}
      submitting={busy}
    >
      <Field label="Name" required error={errors.fields.name}>
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
      </Field>

      <Field
        label="Description"
        hint="What this workspace is for, in a few words."
        error={errors.fields.description}
      >
        <Textarea
          rows={3}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          placeholder="The sales team's vendors, agents and conversations."
        />
      </Field>

      {workspace && (
        <Field
          label="Status"
          hint="Suspended, it grants nothing: nobody can switch into it until it is active again."
          error={errors.fields.status}
        >
          <NativeSelect value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="active">Active</option>
            <option value="suspended">Suspended</option>
          </NativeSelect>
        </Field>
      )}

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}
