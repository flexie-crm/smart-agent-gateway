import { useState } from 'react'
import { ThemeSwitch } from './ThemeSwitch'
import { UpdateReady } from './UpdateReady'
import { NavLink, Outlet } from 'react-router-dom'
import { POSTURE } from '@/lib/api'
import { api } from '@/lib/resources'
import { Input } from '@/components/ui/input'
import {
  Bot,
  Brain,
  Building2,
  MessageSquare,
  Pencil,
  Cable,
  Cpu,
  KeyRound,
  KeySquare,
  Scale,
  LayoutDashboard,
  Plug,
  Server,
  Users as UsersIcon,
  UsersRound,
  Wrench,
} from 'lucide-react'
import { cn } from '@/lib/utils'
import { useAuth } from '@/lib/auth'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

/**
 * The console shell: a permanent sidebar, a thin header, and the page.
 *
 * The navigation is filtered by what the signed-in user may actually do. A menu
 * that offers a page the API will refuse is a menu that lies, and the person
 * following it learns to distrust the whole thing.
 *
 * Sections that have no server behind them yet are not here. An empty screen
 * with a "coming soon" is a worse promise than no link at all.
 */

interface Section {
  to: string
  label: string
  icon: typeof LayoutDashboard
  /** The permission the API will demand. Absent means everyone signed in. */
  permission?: string
  /** An address outside this application, opened as a page rather than routed. */
  external?: boolean
}

// Exported so a test can ask what this build offers without rendering a shell
// that wants a router, an auth context and a socket.
export const NAVIGATION: { heading: string; items: Section[]; personalHides?: boolean }[] = [
  {
    heading: 'Overview',
    items: [
      // The chat, on a personal installation ONLY.
      //
      // There it is what the application is FOR and the console is where you go
      // to adjust it, one click away on the same origin: a link rather than a
      // route, because the two share no logic and are deliberately not one
      // bundle.
      //
      // On a deployment there is nothing behind this. The chat ships as SAG
      // Enterprise, an application on the person's own machine, and the console
      // is the web surface. A menu item pointing at a chat that is not served
      // here is a dead end that looks like a feature.
      ...(POSTURE.single_user
        ? [{ to: '/chat/', label: 'Chat', icon: MessageSquare, external: true }]
        : []),
      { to: '/', label: 'Dashboard', icon: LayoutDashboard },
    ],
  },
  {
    heading: 'The engine',
    items: [
      { to: '/vendors', label: 'Vendors', icon: Plug, permission: 'vendors:view' },
      { to: '/models', label: 'Models', icon: Cpu, permission: 'models:view' },
      // Where models run, if anywhere. An installation with no engine has no
      // machine, no screen and no menu item: the entry is absent rather than
      // present-and-empty, because a screen that can only ever say "nothing
      // here" is a promise the product does not keep. Its route goes with it
      // (App.tsx), so a bookmarked address lands on the dashboard instead of a
      // page reporting a machine that was never going to exist.
      // Inference: the models this deployment runs itself, on hardware it owns.
      //
      // One entry, one word, the same on every edition. It used to be two, and
      // the branch was on the EDITION: "Local models" where there is one machine
      // and "Inference Machines" where there are several. That made the nav read
      // differently depending on which installer somebody had, for a screen that
      // does the same job in both, and "Machines" was the wrong noun anyway,
      // since nothing else on the deployment is called one and the reader had to
      // work out which machines were meant.
      //
      // The only condition left is whether this build carries an engine at all,
      // which is a fact about the product rather than about the edition: the
      // Windows installer ships none (KB/36), and a screen that can only ever
      // say "nothing here" is a promise the product does not keep.
      ...(!POSTURE.local_models
        ? []
        : [
            {
              to: '/machines',
              label: 'Inference',
              icon: Server,
              permission: 'machines:view',
            },
          ]),
      { to: '/agents', label: 'Agents', icon: Bot, permission: 'agents:view' },
      { to: '/tools', label: 'Tools', icon: Wrench, permission: 'tools:view' },
      { to: '/brains', label: 'Brains', icon: Brain, permission: 'brains:view' },
      { to: '/mcp-servers', label: 'MCP Servers', icon: Cable, permission: 'mcp-servers:view' },
    ],
  },
  {
    heading: 'Access',
    // Nobody to administer on a personal installation: one person, one
    // workspace, and every permission held by them. The system underneath still
    // runs, so this is hidden rather than removed.
    personalHides: true,
    items: [
      { to: '/users', label: 'Users', icon: UsersIcon, permission: 'users:view' },
      { to: '/groups', label: 'Groups', icon: UsersRound, permission: 'groups:view' },
      { to: '/roles', label: 'Roles', icon: KeyRound, permission: 'roles:view' },
      { to: '/workspaces', label: 'Workspaces', icon: Building2, permission: 'workspaces:view' },
      { to: '/mcp-server', label: 'MCP Server', icon: Server, permission: 'mcp-server:view' },
      { to: '/oauth-clients', label: 'OAuth Clients', icon: KeySquare, permission: 'oauth-clients:view' },
    ],
  },
  {
    heading: 'About',
    // Attribution for the open source this product is built on, and the licence
    // each part travels under.
    //
    // No permission, and shown on every edition, because it is not an
    // administrative screen: it is where a notice several licences require
    // actually reaches somebody. Putting it behind a role would mean the notice
    // travels as far as the administrators and no further, and the obligation
    // is the same whether this is the AGPL build or a commercial one.
    items: [
      { to: '/open-source', label: 'Open Source', icon: Scale },
    ],
  },
]

/**
 * Which workspace you are working in.
 *
 * Everything on every screen is scoped to it, so switching is not a filter: it
 * asks the server for a new token, and what comes back is a different workspace's
 * console. A person who belongs to one workspace has nothing to choose, so they
 * are shown where they are and not asked.
 */
/**
 * The person's own name, renamed in place.
 *
 * A pencil rather than a screen, because renaming yourself on an installation
 * with one person is not administration: it is the same gesture as renaming a
 * conversation, and it belongs in the same shape.
 */
function OwnName() {
  const { identity, workspace, refresh } = useAuth()
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)

  const save = async () => {
    if (!identity || !name.trim() || name.trim() === identity.name) {
      setEditing(false)
      return
    }
    setBusy(true)
    try {
      // Whole, because this endpoint replaces rather than patches.
      await api.users.update(identity.id, {
        email: identity.email,
        name: name.trim(),
        status: 'active',
        workspaces: workspace ? [workspace.id] : [],
      })
      // Ask who we are again rather than reloading the page. A reload for a
      // renamed label throws away the whole screen and everything on it, which
      // is a shift somebody watches happen for no reason.
      await refresh()
    } finally {
      setBusy(false)
      setEditing(false)
    }
  }

  if (editing) {
    return (
      <Input
        autoFocus
        disabled={busy}
        value={name}
        onChange={(e) => setName(e.target.value)}
        onBlur={() => void save()}
        onKeyDown={(e) => {
          if (e.key === 'Enter') void save()
          if (e.key === 'Escape') setEditing(false)
        }}
        className="h-8"
      />
    )
  }

  return (
    <div className="group flex items-center gap-2.5 px-2 py-1.5">
      <p className="min-w-0 flex-1 truncate text-sm font-medium">{identity?.name}</p>
      <button
        type="button"
        onClick={() => {
          setName(identity?.name ?? '')
          setEditing(true)
        }}
        title="Change your name"
        aria-label="Change your name"
        className="rounded-md p-1.5 text-muted-foreground opacity-0 transition-opacity hover:bg-sidebar-accent hover:text-foreground focus-visible:opacity-100 group-hover:opacity-100"
      >
        <Pencil className="size-3.5" />
      </button>
    </div>
  )
}

function WorkspaceSwitcher() {
  const { workspace, workspaces, switchWorkspace } = useAuth()
  const [busy, setBusy] = useState(false)

  if (!workspace) return null

  async function choose(value: string) {
    const id = Number(value)
    if (!workspace || id === workspace.id) return
    setBusy(true)
    try {
      await switchWorkspace(id)
    } finally {
      setBusy(false)
    }
  }

  if (workspaces.length < 2) {
    return (
      <div className="flex h-14 shrink-0 items-center gap-2 border-b border-border px-5 text-sm text-muted-foreground">
        <Building2 className="size-4 shrink-0" />
        <span className="truncate">{workspace.name}</span>
      </div>
    )
  }

  return (
    // h-14 to match the main header across the fold: the sidebar's first rule
    // and the content's are the same line, and 8px of disagreement between them
    // is the sort of thing you see before you can name.
    <div className="flex h-14 shrink-0 items-center border-b border-border px-3">
      <Select value={String(workspace.id)} onValueChange={(v) => void choose(v)} disabled={busy}>
        <SelectTrigger
          size="sm"
          // px-2, not the trigger's own px-3: the nav below is px-3 on the list
          // plus px-2 on each row, so its icons start 20px in. A control that
          // sits 4px further right than everything it sits above reads as a
          // mistake even when nobody can say what is wrong with it.
          className="w-full border-transparent bg-transparent px-2 shadow-none hover:bg-sidebar-accent focus-visible:border-input"
        >
          <span className="flex min-w-0 items-center gap-2">
            <Building2 className="size-4 shrink-0 text-muted-foreground" />
            <SelectValue />
          </span>
        </SelectTrigger>
        <SelectContent>
          {workspaces.map((ws) => (
            <SelectItem key={ws.id} value={String(ws.id)}>
              {ws.name}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  )
}

export function AppShell() {
  const { identity, can, signOut } = useAuth()

  return (
    <div className="min-h-dvh bg-background text-foreground">
      {/* 280px, which is the chat's. One product, one sidebar. */}
      <aside className="fixed inset-y-0 left-0 z-20 flex w-[280px] flex-col border-r border-border bg-sidebar">
        {/* No brand row. A product name at the top of its own console tells
            somebody where they are only if they could have been somewhere else,
            and the workspace they are working in is the first thing that
            genuinely varies. The list starts with it. */}
        <WorkspaceSwitcher />

        <nav className="flex-1 overflow-y-auto px-3 py-4">
          {NAVIGATION.map((group) => {
            // A personal installation has nobody to administer, and permissions
            // cannot hide it: the one person holds all of them. So the group is
            // dropped outright rather than filtered.
            if (group.personalHides && POSTURE.single_user) return null
            const visible = group.items.filter((item) => !item.permission || can(item.permission))
            if (visible.length === 0) return null

            return (
              <div key={group.heading} className="mb-6 last:mb-0">
                <p className="mb-2 px-2 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                  {group.heading}
                </p>
                <ul className="space-y-0.5">
                  {visible.map((item) =>
                    // The chat is a different application on the same origin.
                    // A router link would try to route to a page this bundle
                    // does not have.
                    item.external ? (
                      <li key={item.to}>
                        <a
                          href={item.to}
                          className="flex items-center gap-2.5 rounded-md px-2 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-sidebar-accent hover:text-foreground"
                        >
                          <item.icon className="size-4 shrink-0" />
                          {item.label}
                        </a>
                      </li>
                    ) : (
                    <li key={item.to}>
                      <NavLink
                        to={item.to}
                        end={item.to === '/'}
                        className={({ isActive }) =>
                          // Selection is a background, never a weight. Bolding the
                          // current item makes the whole list twitch as you move
                          // through it, and the chat sidebar already settled this.
                          cn(
                            'flex items-center gap-2.5 rounded-md px-2 py-1.5 text-sm transition-colors',
                            isActive
                              ? 'bg-sidebar-accent text-sidebar-accent-foreground'
                              : 'text-muted-foreground hover:bg-sidebar-accent hover:text-foreground',
                          )
                        }
                      >
                        <item.icon className="size-4 shrink-0" />
                        {item.label}
                      </NavLink>
                    </li>
                    ),
                  )}
                </ul>
              </div>
            )
          })}
        </nav>

        {/* h-16, and the SAME string as the chat's footer.
            The two were both p-3 and were still different heights, because what
            is inside them is not the same shape: an icon button at p-1.5 makes a
            26px row and a line of text makes 32, so the console's foot sat six
            pixels below the chat's. A height stated here settles it whatever is
            put inside, and 64 is what the tallest of them needs: the identity
            block is two lines. */}
        <UpdateReady />
        <div className="flex h-16 shrink-0 items-center gap-2 border-t border-border px-3">
          {POSTURE.single_user ? (
            // One person, so there is nothing to sign out of and no address
            // worth showing: an account they never made, at a domain that does
            // not resolve. Just the name, editable in place the way a chat's
            // title is.
            <div className="flex min-w-0 flex-1 items-center gap-2">
              <div className="min-w-0 flex-1">
                <OwnName />
              </div>
              <ThemeSwitch />
            </div>
          ) : (
            <div className="flex min-w-0 flex-1 items-center gap-2.5 px-2">
              <div className="min-w-0 flex-1">
                <p className="truncate text-sm font-medium">{identity?.name}</p>
              </div>
              {/* Leaving sits INSIDE the appearance group rather than beside
                  it: two controls of the same size, one framed and one not, read
                  as one thing somebody has not finished aligning. */}
              <ThemeSwitch onSignOut={() => void signOut()} />
            </div>
          )}
        </div>
      </aside>

      <main className="pl-[280px]">
        <Outlet />
      </main>
    </div>
  )
}

/**
 * Page is the frame every screen sits in, so they cannot drift apart.
 *
 * `flush` gives the screen the whole area, with no padding and no frame. A
 * layout that IS the page (the three panes of the brains manager) must not be
 * wrapped in a rounded box inside a padded body: a container inside a container
 * is noise, and it steals the space the content needed.
 */
export function Page({
  title,
  description,
  actions,
  flush,
  children,
}: {
  title: string
  description?: string
  actions?: React.ReactNode
  flush?: boolean
  children: React.ReactNode
}) {
  return (
    <div className={cn('flex flex-col', flush && 'h-dvh')}>
      <header className="flex h-14 shrink-0 items-center justify-between border-b border-border px-6">
        <div>
          <h1 className="text-sm font-semibold">{title}</h1>
          {description && <p className="text-xs text-muted-foreground">{description}</p>}
        </div>
        {actions}
      </header>
      <div className={cn(flush ? 'min-h-0 flex-1' : 'px-6 py-6')}>{children}</div>
    </div>
  )
}
