import { useState } from 'react'
import { Plus } from 'lucide-react'
import { Page } from '@/components/AppShell'
import { Badge, DataTable } from '@/components/DataTable'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { CheckboxField } from '@/components/ui/checkbox'
import { Field, Modal } from '@/components/ui/modal'
import { NativeSelect } from '@/components/ui/native-select'
import { useFormErrors } from '@/lib/form'
import { useNotify } from '@/lib/notify'
import { api, useResource } from '@/lib/resources'
import type { Group, Permission, Role, User } from '@/lib/resources'
import { useAuth } from '@/lib/auth'

/**
 * Who may do what.
 *
 * A user belongs to groups, a group holds roles, and a role is a bundle of
 * permissions. Nothing is granted to a person directly, which is what makes
 * "why can they do that" a question with one answer instead of a search.
 *
 * A person belongs to the company, not to a workspace. Which workspaces they may
 * work in is a membership, and this is where it is granted: a colleague with no
 * membership has an account and nowhere to use it.
 */

export function Users() {
  const { data: users, reload } = useResource(() => api.users.list())
  const { workspaces } = useAuth()
  const [editing, setEditing] = useState<{ user?: User } | null>(null)

  const workspaceName = (id: number) => workspaces.find((w) => w.id === id)?.name

  return (
    <Page
      title="Users"
      description="Who can sign in."
      actions={
        <Button size="sm" onClick={() => setEditing({})}>
          <Plus className="size-4" />
          New user
        </Button>
      }
    >
      <DataTable
        items={users}
        empty="No user but you."
        onEdit={(user) => setEditing({ user })}
        remove={{
          run: async (user) => {
            await api.users.remove(user.id)
            await reload()
          },
          confirm: (u) => `Delete ${u.email}? Their conversations go with them.`,
          done: (u) => `${u.name} was deleted and can no longer sign in.`,
        }}
        columns={[
          { header: 'Name', cell: (u) => <span className="font-medium">{u.name}</span> },
          { header: 'Email', cell: (u) => <span className="text-muted-foreground">{u.email}</span> },
          {
            header: 'Workspaces',
            cell: (u) =>
              u.workspaces.length === 0 ? (
                // An account with nowhere to go cannot sign in at all, so it is
                // said plainly rather than shown as an empty cell.
                <Badge tone="warn">nowhere yet</Badge>
              ) : (
                <span className="text-xs text-muted-foreground">
                  {u.workspaces.map((id) => workspaceName(id)).filter(Boolean).join(', ') ||
                    `${u.workspaces.length}`}
                </span>
              ),
          },
          {
            header: 'Status',
            cell: (u) =>
              u.status === 'active' ? <Badge tone="good">active</Badge> : <Badge>disabled</Badge>,
          },
        ]}
      />

      {editing && (
        <UserForm
          user={editing.user}
          workspaces={workspaces}
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

function UserForm({
  user,
  workspaces,
  onClose,
  onSaved,
}: {
  user?: User
  workspaces: { id: number; name: string }[]
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  /**
   * The dialog's own answer, asked for when it opens: this workspace's groups,
   * each already saying whether this person is in it.
   *
   * It used to be two requests joined here, one of them on the page. The
   * workspace's whole group list was fetched on every view of the user table on
   * the chance that somebody opened a dialog, and the dialog then fetched every
   * group this person belongs to ANYWHERE and kept the ones appearing in both.
   * That intersection is the workspace scoping rule, and the rule is the
   * server's: a console re-deriving it from two lists is a console that will
   * eventually disagree with the server about who is in what.
   *
   * The dialog still opens at once and says it is loading, rather than showing
   * checkboxes that are all empty until the answer lands, which reads as "this
   * person is in no groups" and is not true yet. It can, because everything
   * else it shows came from the row that was clicked.
   */
  const { data: form } = useResource(() => api.users.form(user?.id))
  const groups = form?.groups ?? []
  const [name, setName] = useState(user?.name ?? '')
  const [email, setEmail] = useState(user?.email ?? '')
  const [password, setPassword] = useState('')
  const [status, setStatus] = useState(user?.status ?? 'active')
  const [member, setMember] = useState<number[]>(user?.workspaces ?? workspaces.map((w) => w.id).slice(0, 1))
  // The memberships as they arrived, against the ones ticked since. Holding the
  // EDITS rather than a copy of the answer means no effect writing state when it
  // lands, and nothing to go stale: a box nobody has touched simply reads what
  // the server said.
  const initialGroups = groups.filter((g) => g.member).map((g) => g.id)
  const [edits, setEdits] = useState<Record<number, boolean>>({})
  const chosen = groups.filter((g) => edits[g.id] ?? g.member).map((g) => g.id)
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  async function save() {
    const problems: Record<string, string> = {}
    if (!name.trim()) problems.name = 'A user needs a name.'
    if (!email.trim()) problems.email = 'A user needs an email.'
    if (member.length === 0) {
      problems.workspaces = 'A person has to belong to at least one workspace, or they cannot sign in.'
    }
    if (Object.keys(problems).length > 0) {
      errors.reject(problems)
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const saved = user
        ? await api.users.update(user.id, {
            name: name.trim(),
            email: email.trim(),
            status,
            workspaces: member,
          })
        : await api.users.create({
            name: name.trim(),
            email: email.trim(),
            password,
            workspaces: member,
          })
      if (!sameSet(chosen, initialGroups)) {
        await api.users.setGroups(saved.id, chosen)
      }
      notify.success('The person was saved.')
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
      title={user ? 'Edit user' : 'New user'}
      onSubmit={save}
      submitting={busy}
      loading={form === null}
    >
      <Field label="Name" required error={errors.fields.name}>
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
      </Field>
      <Field label="Email" required error={errors.fields.email}>
        <Input type="email" value={email} onChange={(e) => setEmail(e.target.value)} />
      </Field>

      {!user && (
        <Field
          label="Password"
          required
          hint="They can change it once they are in."
          error={errors.fields.password}
        >
          <Input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
          />
        </Field>
      )}

      <Field
        label="Workspaces"
        required
        hint="Where this person may work. They sign in with an email, and the sidebar moves them between the ones they are given."
        error={errors.fields.workspaces}
      >
        <div className="space-y-1.5">
          {workspaces.map((ws) => (
            <CheckboxField
              key={ws.id}
              checked={member.includes(ws.id)}
              onChange={(on) =>
                setMember((prev) => (on ? [...prev, ws.id] : prev.filter((id) => id !== ws.id)))
              }
              label={ws.name}
            />
          ))}
        </div>
      </Field>

      {groups.length > 0 && (
        <Field
          label="Groups"
          hint="A person can be in one or many. Groups hold roles, and roles are where permissions come from."
          error={errors.fields.groups}
        >
          <div className="space-y-1.5">
            {groups.map((group) => (
              <CheckboxField
                key={group.id}
                checked={edits[group.id] ?? group.member}
                onChange={(on) => setEdits((prev) => ({ ...prev, [group.id]: on }))}
                label={group.name}
              />
            ))}
          </div>
        </Field>
      )}

      {user && (
        <Field label="Status" error={errors.fields.status}>
          <NativeSelect
            value={status}
            onChange={(e) => setStatus(e.target.value)}
          >
            <option value="active">Active</option>
            <option value="disabled">Disabled</option>
          </NativeSelect>
        </Field>
      )}

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}

export function Roles() {
  // ONE request: the roles, and the permissions a role can hold. The catalogue
  // is a constant this screen cannot draw a role's form without.
  const { data: screen, reload } = useResource(() => api.roles.list())
  const roles = screen?.roles ?? null
  const permissions = screen?.permissions ?? null
  const [editing, setEditing] = useState<{ role?: Role } | null>(null)

  return (
    <Page
      title="Roles"
      description="A bundle of permissions, granted to a group."
      actions={
        <Button size="sm" onClick={() => setEditing({})}>
          <Plus className="size-4" />
          New role
        </Button>
      }
    >
      <DataTable
        items={roles}
        empty="No role yet."
        onEdit={(role) => setEditing({ role })}
        remove={{
          run: async (role) => {
            await api.roles.remove(role.id)
            await reload()
          },
          confirm: (r) => `Delete "${r.name}"? Anyone holding it loses what it granted.`,
          done: (r) => `${r.name} was deleted, and nobody holds it any more.`,
        }}
        columns={[
          { header: 'Role', cell: (r) => <span className="font-medium">{r.name}</span> },
          {
            header: 'Grants',
            cell: (r) =>
              r.permissions.includes('*') ? (
                // Everything. Worth saying loudly rather than listing.
                <Badge tone="bad">everything</Badge>
              ) : (
                <span className="text-xs text-muted-foreground">
                  {r.permissions.length} {r.permissions.length === 1 ? 'permission' : 'permissions'}
                </span>
              ),
          },
        ]}
      />

      {editing && (
        <RoleForm
          role={editing.role}
          permissions={permissions ?? []}
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

/** Two lists hold the same ids, order aside. */
function sameSet(a: number[], b: number[]): boolean {
  return a.length === b.length && a.every((id) => b.includes(id))
}

function RoleForm({
  role,
  permissions,
  onClose,
  onSaved,
}: {
  role?: Role
  permissions: Permission[]
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  const [name, setName] = useState(role?.name ?? '')
  const [held, setHeld] = useState<string[]>(role?.permissions ?? [])
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  async function save() {
    if (!name.trim()) {
      errors.reject({ name: 'A role needs a name.' })
      return
    }
    errors.clear()
    setBusy(true)
    try {
      const body = { name: name.trim(), permissions: held }
      if (role) await api.roles.update(role.id, body)
      else await api.roles.create(body)
      notify.success('The role was saved.')
      await onSaved()
    } catch (failure) {
      errors.fail(failure)
      setBusy(false)
    }
  }

  // Grouped by area, in the catalog's own order, because a flat list of forty
  // checkboxes is a list nobody reads. The server sends the areas and the
  // words; this screen only lays them out.
  const areas: { area: string; list: Permission[] }[] = []
  for (const permission of permissions) {
    const last = areas[areas.length - 1]
    if (last && last.area === permission.area) last.list.push(permission)
    else areas.push({ area: permission.area, list: [permission] })
  }

  return (
    <Modal
      open
      wide
      onOpenChange={(o) => !o && onClose()}
      title={role ? `Edit ${role.name}` : 'New role'}
      onSubmit={save}
      submitting={busy}
    >
      <Field label="Name" required error={errors.fields.name}>
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
      </Field>

      <Field label="Permissions" error={errors.fields.permissions}>
        <div className="space-y-4">
          {areas.map(({ area, list }) => (
            <div key={area}>
              <p className="mb-1.5 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                {area}
              </p>
              <div className="grid grid-cols-2 gap-1.5">
                {list.map((permission) => (
                  <CheckboxField
                    key={permission.key}
                    checked={held.includes(permission.key)}
                    onChange={(on) =>
                      setHeld((prev) =>
                        on ? [...prev, permission.key] : prev.filter((p) => p !== permission.key),
                      )
                    }
                    label={permission.label}
                  />
                ))}
              </div>
            </div>
          ))}
        </div>
      </Field>

      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}

export function Groups() {
  const { data: groups, reload } = useResource(() => api.groups.list())
  const [editing, setEditing] = useState<{ group?: Group } | null>(null)

  return (
    <Page
      title="Groups"
      description="Users belong to groups. Groups hold roles."
      actions={
        <Button size="sm" onClick={() => setEditing({})}>
          <Plus className="size-4" />
          New group
        </Button>
      }
    >
      <DataTable
        items={groups}
        empty="No group yet."
        onEdit={(group) => setEditing({ group })}
        remove={{
          run: async (group) => {
            await api.groups.remove(group.id)
            await reload()
          },
          confirm: (g) => `Delete "${g.name}"? Its members lose what it granted them.`,
          done: (g) => `${g.name} was deleted.`,
        }}
        columns={[{ header: 'Group', cell: (g) => <span className="font-medium">{g.name}</span> }]}
      />

      {editing && (
        <GroupForm
          group={editing.group}
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

function GroupForm({
  group,
  onClose,
  onSaved,
}: {
  group?: Group
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const notify = useNotify()
  const [name, setName] = useState(group?.name ?? '')
  const [busy, setBusy] = useState(false)
  const errors = useFormErrors()

  async function save() {
    if (!name.trim()) {
      errors.reject({ name: 'A group needs a name.' })
      return
    }
    errors.clear()
    setBusy(true)
    try {
      if (group) await api.groups.update(group.id, { name: name.trim() })
      else await api.groups.create({ name: name.trim() })
      notify.success('The group was saved.')
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
      title={group ? 'Edit group' : 'New group'}
      onSubmit={save}
      submitting={busy}
    >
      <Field label="Name" required error={errors.fields.name}>
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
      </Field>
      {errors.form && <p className="text-sm text-destructive">{errors.form}</p>}
    </Modal>
  )
}
