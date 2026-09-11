import { useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import type { Posture } from './api'
import {
  apiFetch,
  bootstrap,
  currentSession,
  SESSION_EXPIRED,
  signIn as apiSignIn,
  signInLocally as apiSignInLocally,
  fetchServerIsDev,
  POSTURE,
  signOut as apiSignOut,
  switchWorkspace as apiSwitchWorkspace,
} from './api'
import { restartSocket, startSocket, stopSocket } from './ws'

/**
 * Who is signed in, and what they are allowed to do.
 *
 * Permissions are fetched from the server on every load, never read out of the
 * token. A token says who you are; the server decides what that means, and it
 * decides it again on every request. What is held here is only what the console
 * needs to draw itself: a menu should not offer a page the API will refuse.
 */

export interface Identity {
  id: number
  email: string
  name: string
}

export interface Workspace {
  id: number
  slug: string
  name: string
}

interface Session {
  identity: Identity
  permissions: string[]
  /** Where the token says they are standing. */
  workspace: Workspace | null
  /** Where else they may stand. Exactly what the switcher offers. */
  workspaces: Workspace[]
}

export interface AuthState {
  identity: Identity | null
  permissions: string[]
  workspace: Workspace | null
  workspaces: Workspace[]
  loading: boolean
  signIn: (email: string, password: string) => Promise<void>
  /** Sign in as the person at this computer, where that is offered. */
  signInLocally: () => Promise<void>
  /** Ask the server who we are again, after something about us changed. */
  refresh: () => Promise<void>
  /** What kind of installation this is, so a screen can ask what it may do. */
  posture: Posture
  /** Whether the SERVER answering this page is a working copy rather than a
   *  deployment. Displayed, and it decides whether one sign-in button is
   *  offered. Nothing else may branch on it: a rule that changes with the
   *  deployment is a rule production never runs. */
  dev: boolean
  signOut: () => Promise<void>
  /** Move to another workspace. Everything on screen is scoped to it, so the
   *  console reloads rather than pretending the old data still applies. */
  switchWorkspace: (workspaceID: number) => Promise<void>
  /** can reports whether the signed-in user holds a permission. */
  can: (permission: string) => boolean
}

import { AuthContext } from '@/lib/auth-context'

/** SUPERUSER passes every check. It is the one permission that is not a list. */
const SUPERUSER = '*'

/**
 * whoAmI asks the server who this is. It only READS: what to do with the answer
 * is the caller's business, which is what lets a caller throw the answer away
 * when it arrives too late to matter.
 */
async function whoAmI(): Promise<Session | null> {
  if (!currentSession()) return null
  try {
    const res = await apiFetch('/v1/auth/me')
    if (!res.ok) return null
    const body = await res.json()
    return {
      identity: { id: body.user.id, email: body.user.email, name: body.user.name },
      permissions: body.permissions ?? [],
      workspace: body.workspace ?? null,
      workspaces: body.workspaces ?? [],
    }
  } catch {
    return null
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [identity, setIdentity] = useState<Identity | null>(null)
  const [permissions, setPermissions] = useState<string[]>([])
  const [workspace, setWorkspace] = useState<Workspace | null>(null)
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  // On load we always check the refresh cookie for a session, so we start in
  // "loading" and the bootstrap below resolves it to signed-in or signed-out.
  const [loading, setLoading] = useState(true)
  // Which server this is. Asked once, and it starts as "not a working copy" so
  // the badge can only ever appear because a server SAID so.
  const [dev, setDev] = useState(false)

  useEffect(() => {
    let live = true
    void fetchServerIsDev().then((isDev) => {
      if (live) setDev(isDev)
    })
    return () => {
      live = false
    }
  }, [])

  const adopt = useCallback((session: Session | null) => {
    setIdentity(session?.identity ?? null)
    setPermissions(session?.permissions ?? [])
    setWorkspace(session?.workspace ?? null)
    setWorkspaces(session?.workspaces ?? [])
    setLoading(false)
    // The socket lives for the signed-in session, whichever screen is open:
    // it is the server's push channel, and a notification must arrive whether
    // or not the dashboard happens to be mounted.
    if (session?.identity) startSocket()
    else stopSocket()
  }, [])

  useEffect(() => {
    // On load, recover the session from the HttpOnly refresh cookie (which mints a
    // fresh access token and rotates the cookie), then ask the server who we are
    // for live permissions. No cookie means signed out. A second answer must not
    // overwrite a newer one: two loads can be in the air (a sign-in landing while
    // the initial check runs), and the first to start is not the first to finish.
    //
    // On a desktop where somebody has ALREADY come in, no cookie means this is
    // the second application rather than a stranger, and it signs itself in
    // here. Doing it in the sign-in screen instead was wrong in a way that only
    // shows on a real machine: the screen paints first and signs in a moment
    // later, so the welcome flashes up and then vanishes. Deciding before the
    // first paint is the whole point of `loading`.
    let current = true
    void bootstrap()
      .then(async (recovered) => {
        if (recovered) return true
        // One application means one cookie jar, so a session recovered in the
        // chat is a session here. Nothing has to be remembered about whether
        // somebody has already come in: if they have, the cookie says so.
        return false
      })
      .catch(() => false)
      .then((signedIn) => {
        if (!signedIn) {
          if (current) adopt(null)
          return
        }
        return whoAmI().then((session) => {
          if (current) adopt(session)
        })
      })
    return () => {
      current = false
    }
  }, [adopt])

  // The refresh token expired while the tab was open. Nothing to salvage: the
  // console is a view of the server's answers, and it has stopped answering.
  useEffect(() => {
    const expired = () => adopt(null)
    window.addEventListener(SESSION_EXPIRED, expired)
    return () => window.removeEventListener(SESSION_EXPIRED, expired)
  }, [adopt])

  const value = useMemo<AuthState>(
    () => ({
      identity,
      permissions,
      workspace,
      workspaces,
      loading,
      can: (permission: string) =>
        permissions.includes(SUPERUSER) || permissions.includes(permission),
      posture: POSTURE,
      dev,
      refresh: async () => {
        adopt(await whoAmI())
      },
      signIn: async (email, password) => {
        await apiSignIn(email, password)
        adopt(await whoAmI())
      },
      signInLocally: async () => {
        await apiSignInLocally()
        const session = await whoAmI()
        if (!session) {
          // Reached only if the sign-in succeeded and the session still cannot
          // be read back. Whatever is wrong, saying so beats a button that
          // stays on "Signing in" for as long as somebody is willing to wait.
          throw new Error('Signed in, but the session could not be read back.')
        }
        adopt(session)
      },
      signOut: async () => {
        stopSocket()
        await apiSignOut()
        adopt(null)
      },
      switchWorkspace: async (workspaceID: number) => {
        await apiSwitchWorkspace(workspaceID)
        adopt(await whoAmI())
        // The old connection speaks for the old workspace, a claim made when it
        // authenticated. A new workspace needs a new connection; the topics are
        // rejoined on it.
        restartSocket()
      },
    }),
    [identity, permissions, workspace, workspaces, loading, dev, adopt],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const context = useContext(AuthContext)
  if (!context) throw new Error('useAuth must be used inside an AuthProvider')
  return context
}
