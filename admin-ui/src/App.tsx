import { useEffect, useState } from 'react'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { Loader2 } from 'lucide-react'
import { AppShell } from '@/components/AppShell'
import { POSTURE } from '@/lib/api'
import { AuthProvider, useAuth } from '@/lib/auth'
import { NotifyProvider } from '@/lib/notify'
import { api } from '@/lib/resources'
import type { SetupState } from '@/lib/resources'
import { SignIn } from '@/pages/SignIn'
import { Dashboard } from '@/pages/Dashboard'
import { Setup } from '@/pages/Setup'
import { Brains } from '@/pages/Brains'
import { Vendors } from '@/pages/Vendors'
import { Models } from '@/pages/Models'
import { Nodes } from '@/pages/Nodes'
import { Tools } from '@/pages/Tools'
import { MCPServers } from '@/pages/MCPServers'
import { Agents } from '@/pages/Agents'
import { Groups, Roles, Users } from '@/pages/Access'
import { Workspaces } from '@/pages/Workspaces'
import { MCPServer } from '@/pages/MCPServer'
import { OAuthClients } from '@/pages/OAuthClients'
import { Licences } from './pages/Licences'

/**
 * The console.
 *
 * Everything behind the sign-in is one route tree under one shell. There is no
 * "public" area: an orchestrator has no anonymous pages.
 */
export function App() {
  return (
    <NotifyProvider>
      <AuthProvider>
        <BrowserRouter>
          <Gate />
        </BrowserRouter>
      </AuthProvider>
    </NotifyProvider>
  )
}

function Gate() {
  const { identity, loading, workspace } = useAuth()
  // Whether there is anything to run yet. Asked once signed in, and again when
  // a step finishes, so the screen advances without a reload.
  const [setup, setSetup] = useState<SetupState | null>(null)
  const [askAgain, setAskAgain] = useState(0)
  useEffect(() => {
    if (!identity) return
    let live = true
    void api.setup
      .state()
      // A console that cannot ask must not lock somebody out of the console
      // itself: the chat is what refuses to run without a model, and it asks
      // separately. A failure here reads as "nothing to do", not "start over".
      .catch(() => ({ ready: true, has_name: true, has_vendor: true, has_model: true }))
      .then((answer) => {
        if (live) setSetup(answer)
      })
    return () => {
      live = false
    }
  }, [identity, askAgain])

  // The first paint decides nothing. Showing the sign-in form while the session
  // is still being checked would flash it at somebody who is already signed in.
  if (loading) {
    return (
      <div className="grid min-h-dvh place-items-center bg-background">
        <Loader2 className="size-5 animate-spin text-muted-foreground" />
      </div>
    )
  }

  if (!identity) {
    return <SignIn />
  }

  // Nothing can answer yet, so nothing else is worth showing. Setup replaces the
  // console rather than sitting beside it as a screen somebody has to find:
  // there is exactly one thing to do, and every other screen depends on it.
  // Nothing is rendered until this is known. Rendering the dashboard first and
  // replacing it a moment later is a layout shift somebody watches happen, and
  // it is avoidable: the answer decides which screen exists, not how one looks.
  if (!setup) {
    return (
      <div className="grid min-h-dvh place-items-center bg-background">
        <Loader2 className="size-5 animate-spin text-muted-foreground" />
      </div>
    )
  }
  // Finishing setup at /setup goes to the chat, which is what somebody came to
  // use. Without this the address that exists to show setup went on showing it
  // after there was nothing left to set up: three ticks and no way forward.
  if (setup.ready && window.location.pathname === '/setup') {
    window.location.replace('/chat/')
    return null
  }

  // /setup is a real address, so arriving from the chat asks for setup and not
  // for a dashboard that is then thrown away.
  if (!setup.ready || window.location.pathname === '/setup') {
    return <Setup state={setup} onDone={() => setAskAgain((n) => n + 1)} />
  }

  return (
    // The workspace is part of every screen's identity: everything drawn below
    // is scoped to it. Keying the tree by workspace makes a switch remount the
    // open screen, which asks the server again as the new workspace.
    <Routes key={workspace?.id ?? 0}>
      <Route element={<AppShell />}>
        <Route path="/" element={<Dashboard />} />

        {/* One address for the whole screen. The server resolves what to show
            in one answer and every click asks it for the next view; selection
            is screen state, the way the CRM's screens work. */}
        <Route path="/brains" element={<Brains />} />

        <Route path="/vendors" element={<Vendors />} />
        <Route path="/models" element={<Models />} />
        {/* Where models run. Absent entirely on an installation that carries no
            engine, so the address falls through to the catch-all below and lands
            on the dashboard: a bookmark or a typed path cannot reach a screen
            about hardware this product does not have. */}
        {POSTURE.local_models && (
          <>
            <Route path="/machines" element={<Nodes />} />
            {/* The machine is in the path, so a refresh stays on it and the
                address can be sent to somebody. */}
            <Route path="/machines/:machineID" element={<Nodes />} />
          </>
        )}
        <Route path="/agents" element={<Agents />} />
        <Route path="/tools" element={<Tools />} />
        <Route path="/mcp-servers" element={<MCPServers />} />

        <Route path="/users" element={<Users />} />
        <Route path="/groups" element={<Groups />} />
        <Route path="/roles" element={<Roles />} />
        <Route path="/workspaces" element={<Workspaces />} />
        <Route path="/mcp-server" element={<MCPServer />} />
        <Route path="/oauth-clients" element={<OAuthClients />} />
        <Route path="/open-source" element={<Licences />} />

        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  )
}
