import { StrictMode, useCallback, useEffect, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'

import App from './App'
import { renewMachineLinkOnRequest, startMachineLink, stopMachineLink } from './lib/machine-link'
import { SignIn } from './components/SignIn'
import { NeedsSetup } from './components/NeedsSetup'
import { WorkspacePicker } from './components/WorkspacePicker'
import {
  PERSONAL,
  fetchSetup,
  signInLocally,
  type SetupState,
  bootstrap,
  currentSession,
  signOut,
  switchWorkspace,
  SESSION_EXPIRED,
  type Session,
  type Workspace,
} from './lib/api'
import { connect as connectPresence, disconnect as disconnectPresence } from './lib/ws'
import { NotifyProvider } from './lib/notify'
import './index.css'
import { makeItFeelNative } from './lib/desktop-chrome'
import { tellTheTruthAboutTouch } from './lib/webview-truth'
import { askTheApplication, paint, rememberedChoice } from './lib/theme'

// Before anything renders, because the virtual list asks once and remembers.
tellTheTruthAboutTouch()

// And before the first paint, or somebody who works at night gets a white flash
// in the face on every reload. This is why it is here rather than in a
// component: by the time React has an opinion, the page has already been drawn.
paint(rememberedChoice())

// And ask the application, which is the half that survives a gateway choosing a
// new port and taking this page's storage with it.
askTheApplication()

// What this build is, readable from the page. A running application could not
// say which version it was, so a fixed bundle and a stale window looked exactly
// alike, and we spent a day on the difference between them.
declare const __SAG_BUILD__: string
;(window as unknown as { __SAG_BUILD__: string }).__SAG_BUILD__ = __SAG_BUILD__

if (PERSONAL) makeItFeelNative()

/**
 * SAG adaptation: the shell.
 *
 * The CRM chat lived inside a page that had already authenticated the user, so
 * it never had to ask who you were. SAG is its own product: it signs you in,
 * and hands the chat a session.
 *
 * Everything below this line is the chat we already have, unchanged.
 */
function Shell() {
  // undefined = still recovering the session from the cookie; null = signed out;
  // Session = signed in. A reload starts as undefined and bootstraps below.
  const [session, setSession] = useState<Session | null | undefined>(undefined)
  // Non-null while a person with more than one workspace picks which to enter.
  const [choices, setChoices] = useState<Workspace[] | null>(null)

  // On load, recover the session from the HttpOnly refresh cookie: a page refresh
  // mints a fresh access token and rotates the cookie. No cookie, no session, and
  // the person signs in. The session's workspace comes back with it, so a reload
  // still goes straight to the chat.
  //
  // On a desktop where somebody has ALREADY come in, no cookie means this is the
  // second application rather than a stranger, and it signs itself in here.
  // Doing it in the sign-in screen instead was wrong in a way only a real
  // machine shows: the screen paints first and signs in a moment later, so the
  // welcome flashes up and then vanishes.
  useEffect(() => {
    let alive = true
    void bootstrap()
      .then((recovered) => recovered)
      .catch(() => null)
      .then((session) => {
        if (alive) setSession(session)
      })
    return () => {
      alive = false
    }
  }, [])

  // While a person is signed in, the desktop application holds a link to the
  // gateway, so a tool configured for a database or a server only this computer
  // can see has a route to it. In a browser this does nothing: there is no
  // application to hand a credential to, and a tab cannot open a connection to
  // anything on this network anyway.
  useEffect(() => {
    if (!session) return
    let alive = true
    let stopListening = () => {}
    void startMachineLink()
    void renewMachineLinkOnRequest().then((stop) => {
      if (alive) stopListening = stop
      else stop()
    })
    return () => {
      alive = false
      stopListening()
    }
  }, [session])

  // While a person is signed in and in a workspace, hold a presence socket open
  // so they count as online. It closes when they leave.
  useEffect(() => {
    if (!session) return
    connectPresence()
    return () => disconnectPresence()
  }, [session])

  // A session can die mid-conversation (a revoked token, a disabled user).
  // When it does, drop to sign-in rather than leaving a chat that silently
  // fails on every turn.
  useEffect(() => {
    const expired = () => {
      setChoices(null)
      setSession(null)
    }
    window.addEventListener(SESSION_EXPIRED, expired)
    return () => window.removeEventListener(SESSION_EXPIRED, expired)
  }, [])

  // enter runs right after sign-in: it decides whether to ask which workspace.
  // The login already told us every workspace the person may enter, so there is
  // no request to make; the token scopes one already, and if that is the only
  // choice the chat just opens.
  const enter = useCallback((signedIn: Session) => {
    if (signedIn.workspaces.length > 1) {
      setChoices(signedIn.workspaces)
      return
    }
    setSession(signedIn)
  }, [])

  // choose opens the workspace the person picked, switching the token to it
  // when it is not the one login already put them in.
  const choose = useCallback(async (workspace: Workspace) => {
    const held = currentSession()
    const next = held && workspace.id === held.workspace?.id ? held : await switchWorkspace(workspace.id)
    setChoices(null)
    setSession(next)
  }, [])

  // switchTo moves an already-signed-in person to another workspace from the
  // chat's header. It re-issues the token for the new workspace and re-renders;
  // the App is keyed by workspace, so it remounts on the new one.
  const switchTo = useCallback(
    async (workspaceID: number) => {
      if (workspaceID === session?.workspace?.id) return
      try {
        setSession(await switchWorkspace(workspaceID))
      } catch {
        // The switch failed; stay where we are rather than blank the chat.
      }
    },
    [session],
  )

  // Re-asked while it is not ready, so finishing setup in the console brings
  // this window to life without anybody restarting it.
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const [setup, setSetup] = useState<SetupState | undefined>(undefined)
  useEffect(() => {
    if (!session) {
      setSetup(undefined)
      return
    }
    let live = true
    const ask = async () => {
      try {
        const answer = await fetchSetup()
        if (live) setSetup(answer)
        return answer.ready
      } catch {
        // A gateway that will not say is treated as not ready: showing a
        // composer that cannot answer is the worse of the two mistakes.
        if (live) setSetup({ ready: false, has_vendor: false, has_model: false })
        return false
      }
    }
    void ask().then((ready) => {
      if (ready || !live) return
      const timer = setInterval(() => {
        void ask().then((now) => {
          if (now) clearInterval(timer)
        })
      }, 3000)
      pollRef.current = timer
    })
    return () => {
      live = false
      if (pollRef.current) clearInterval(pollRef.current)
    }
  }, [session])

  const onSignOut = useCallback(async () => {
    // The link goes first. A route into this network held by a session that has
    // ended is the one thing signing out has to reach.
    await stopMachineLink()
    await signOut()
    setChoices(null)
    setSession(null)
  }, [])

  // Still recovering the cookie session: render nothing rather than flash the
  // sign-in screen for a frame before the session resolves.
  if (session === undefined) {
    return null
  }

  if (!session) {
    if (choices) {
      return (
        <WorkspacePicker
          workspaces={choices}
          current={currentSession()?.workspace?.id}
          onChoose={choose}
          onCancel={() => void onSignOut()}
        />
      )
    }
    return <SignIn onSignedIn={enter} />
  }

  // Signed in, but the assistant may have nothing to think with. Asked here
  // rather than inside the chat, because the answer decides whether there is a
  // chat to draw at all.
  if (setup === undefined) {
    return null
  }
  if (!setup.ready) {
    return <NeedsSetup state={setup} />
  }

  return (
    <App
      // Keyed by workspace: switching is not a filter, it is a different set of
      // conversations and configuration, so the chat remounts fresh rather than
      // showing the old workspace's chats under the new one's name.
      key={session.workspace?.id ?? 0}
      streamEndpoint="/v1/chat/stream"
      fetchEndpoint="/v1/chat/history"
      attachEndpoint="/v1/chat/attach"
      cancelEndpoint="/v1/chat/cancel"
      // The sidebar: your conversations, searchable by what was said in them.
      chatsEndpoint="/v1/chat/chats"
      chatStore="flexie"
      showReasoning
      showTools
      // Host context, posted with every turn. The model is named here until
      // workflows choose it server-side, which is where that decision belongs.
      extraData={{ model_id: Number(import.meta.env.VITE_SAG_MODEL_ID ?? 1) }}
      headerButtons={{ reset: false, expand: false, close: false }}
      onSignOut={onSignOut}
      user={session.user}
      workspaces={session.workspaces}
      currentWorkspaceId={session.workspace?.id}
      onSwitchWorkspace={(id) => void switchTo(id)}
    />
  )
}

const root = document.getElementById('root')
if (!root) {
  throw new Error('the page is missing its mount point')
}

createRoot(root).render(
  <StrictMode>
    {/* Outside Shell, so the sign-in and the workspace picker can speak too. */}
    <NotifyProvider>
      <Shell />
    </NotifyProvider>
  </StrictMode>,
)
