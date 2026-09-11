// SAG adaptation: authentication.
//
// The CRM authenticated the chat with a session cookie and an opaque token in
// the request body. SAG has a real API: a bearer token on the Authorization
// header, refreshed when it expires. Rather than thread that through every
// call site, every request the chat makes goes through here.

export interface Session {
  accessToken: string
  user: { id: number; email: string; name: string }
}

/**
 * The console talks to the gateway directly, no middleman serving as a relay.
 * In development the gateway runs on its own port, so the base comes from the
 * build environment; in production the console is served from the same origin
 * and the base is empty.
 */
const API_BASE: string = import.meta.env.VITE_SAG_API_URL ?? ''

/**
 * The origin the gateway is served from, which is where its OAuth callback page
 * lives. A popup message that does not come from here is not the gateway's, so
 * the console can ignore it. Empty base means same origin (production).
 */
export const apiOrigin: string = API_BASE ? new URL(API_BASE).origin : window.location.origin

function apiURL(path: string): string {
  return `${API_BASE}${path}`
}

// The session lives ONLY in memory. The access token is short-lived, and the
// durable half of the session, the refresh token, is an HttpOnly cookie the
// browser keeps and JavaScript cannot read. So a reload starts with nothing here
// and recovers the session from the cookie (see bootstrap), and there is no token
// sitting in localStorage for a cross-site script to steal.
let current: Session | null = null

export function currentSession(): Session | null {
  return current
}

export function setSession(session: Session | null): void {
  current = session
}

/** Signals that the session died mid-request, so the app can show sign-in. */
export const SESSION_EXPIRED = 'sag:session-expired'

/**
 * What kind of installation this is.
 *
 * Asked rather than compiled in, so ONE build of this console serves a
 * deployment and a desktop alike: there is one artifact to sign and no way to
 * ship the wrong one. It is also the rule this codebase already follows, that a
 * server rule is answered by the server (KB/19).
 *
 * The fields are capabilities, not a name. A screen asks whether users can be
 * administered here, never whether it is "a desktop", so a third kind of
 * installation does not mean revisiting every screen.
 */
export interface Posture {
  single_user: boolean
  local_sign_in: boolean
  single_machine: boolean
  /**
   * Whether a model can run on hardware this installation owns, and therefore
   * whether there is anything for the Inference screen to be about.
   */
  local_models: boolean
}

/** A deployment, which is what anything unrecognised is treated as. */
export const SERVER_POSTURE: Posture = {
  single_user: false,
  local_sign_in: false,
  single_machine: false,
  // A deployment always has this, on every platform: machines JOIN a server
  // over the network rather than shipping inside it.
  local_models: true,
}


/** One person on their own computer. */
export const PERSONAL_POSTURE: Posture = {
  single_user: true,
  local_sign_in: true,
  single_machine: true,
  // The one capability a desktop does not always have, because here the fleet
  // IS this computer and it can only run a model if an engine shipped with the
  // application. The Windows installer carries none (KB/36), so its build sets
  // this to 0 and the menu, the screen and the "add local model" button all go
  // with it. macOS ships an engine and is unaffected.
  //
  // Decided by the build for the same reason the rest of the posture is: a
  // bundle simply IS what it was compiled as, and asking at run time leaves a
  // moment where the product does not know what it offers.
  local_models: import.meta.env.VITE_SAG_LOCAL_MODELS !== '0',
}

/**
 * Which one this build is.
 *
 * Decided WHEN THIS IS BUILT, by the desktop build passing VITE_SAG_PERSONAL=1,
 * rather than asked for at run time. The difference matters: a build that asks
 * has a moment where it does not know, and a request that fails leaves it
 * showing the wrong product. A desktop bundle simply IS a desktop bundle.
 *
 * It costs nothing, because the desktop already builds these pages separately:
 * the chat has to be built for its own sub-path regardless. The Go binary and
 * the shell stay single builds; only the pages differ, and only in what they
 * offer.
 */
export const POSTURE: Posture =
  import.meta.env.VITE_SAG_PERSONAL === '1' ? PERSONAL_POSTURE : SERVER_POSTURE

/**
 * Whether the server this page is talking to is somebody's working copy.
 *
 * Asked at RUN TIME, which is the opposite of POSTURE above, for a reason that
 * does not apply to any field there: this page is SERVED by whichever server it
 * was deployed to, and the enterprise shell points at one somebody typed. A
 * build cannot know which server picked it up, and which server you are looking
 * at is exactly the question being asked.
 *
 * Anything other than a clear yes means production. A server that cannot be
 * reached, or an older one that does not answer this at all, says nothing rather
 * than claiming to be a working copy: the badge is a warning, and a warning
 * shown where it does not belong is worse than one that never appears.
 */
export async function fetchServerIsDev(): Promise<boolean> {
  try {
    const res = await fetch(apiURL('/v1/meta'), { credentials: 'include' })
    if (!res.ok) return false
    const body = (await res.json()) as { dev?: boolean }
    return body.dev === true
  } catch {
    return false
  }
}

/**
 * Sign in as the person at this computer, with nothing to type.
 *
 * The server checks the same password as every other sign-in, holds it on this
 * caller's behalf, and issues an ordinary session. There is no weaker path here:
 * what is removed is the typing.
 */
export async function signInLocally(): Promise<Session> {
  const res = await fetch(apiURL('/v1/auth/local'), {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
  })
  if (!res.ok) {
    const body = await res.json().catch(() => null)
    throw new Error(
      (body && (body.error_description as string)) || 'This computer could not be signed in.',
    )
  }
  // keep(), exactly as signIn does, and forgetting it was a real bug rather
  // than an untidiness: the cookie was set and the access token thrown away, so
  // the very next request was unauthenticated, whoAmI answered "nobody", and
  // the console sat on "Signing in" forever having successfully signed in.
  return keep(await res.json())
}

/**
 * Sign in with an email and a password. No workspace is asked for: a person is
 * not asked to remember one. The server puts them back where they were, and the
 * switcher in the sidebar moves them.
 */
export async function signIn(email: string, password: string): Promise<Session> {
  const res = await fetch(apiURL('/v1/auth/login'), {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  })
  if (!res.ok) {
    // A wrong password and an account nobody has placed in a workspace are
    // different problems, and sending the second one to check their password
    // would be sending them to fix the thing that works.
    const body = await res.json().catch(() => null)
    throw new Error(
      body?.error === 'no_workspace'
        ? 'This account is not in any workspace yet. An administrator has to add it to one.'
        : 'Invalid email or password.',
    )
  }
  return keep(await res.json())
}

/**
 * Move to another workspace. The workspace is a CLAIM on the token, so it can
 * only change by getting a new one: the server checks the membership and issues
 * a pair that speaks for the new workspace, and the old refresh token dies.
 */
export async function switchWorkspace(workspaceID: number): Promise<Session> {
  const res = await apiFetch('/v1/auth/workspace', {
    method: 'POST',
    body: JSON.stringify({ workspace_id: workspaceID }),
  })
  if (!res.ok) throw new Error('That workspace could not be opened.')
  return keep(await res.json())
}

function keep(body: { access_token: string; user: Session['user'] }): Session {
  const session: Session = {
    accessToken: body.access_token,
    user: body.user,
  }
  setSession(session)
  return session
}

/**
 * apiFetch is the single door to the orchestrator. It carries the bearer
 * token, and on a 401 it refreshes once and retries: an access token expiring
 * mid-conversation should be invisible, not a lost turn.
 *
 * A GET that is already in flight is never asked again: the second caller
 * waits on the first answer and gets its own copy. React mounts a screen
 * twice in development to prove its effects survive it, and two panes may
 * legitimately want the same list at the same moment; neither is a reason
 * to make the server say the same thing twice.
 */
export async function apiFetch(path: string, init: RequestInit = {}): Promise<Response> {
  const method = (init.method ?? 'GET').toUpperCase()
  if (method !== 'GET') return dispatch(path, init)

  let shared = inflight.get(path)
  if (!shared) {
    shared = dispatch(path, init).finally(() => inflight.delete(path))
    inflight.set(path, shared)
  }
  // Every caller reads a clone; the shared response itself stays unread, so
  // no caller can consume the body out from under another.
  return (await shared).clone()
}

const inflight = new Map<string, Promise<Response>>()

async function dispatch(path: string, init: RequestInit): Promise<Response> {
  const session = currentSession()
  const headers = new Headers(init.headers)
  headers.set('Content-Type', 'application/json')
  if (session) headers.set('Authorization', `Bearer ${session.accessToken}`)

  // credentials:include so the browser attaches the refresh cookie on the auth
  // endpoints (it is path-scoped, so it rides no other call) and can store a
  // rotated one from the response.
  let res = await fetch(apiURL(path), { ...init, headers, credentials: 'include' })
  if (res.status !== 401) return res

  // The access token expired (or there was none): mint a fresh one from the
  // refresh cookie and retry once.
  //
  // A dead cookie drops us to sign-in. The server NOT ANSWERING does not: it
  // says nothing about the session, so the 401 goes back to the caller as a
  // failed request and the session is left alone to be tried again. Treating
  // silence as a refusal is what signed somebody out every time the server
  // restarted underneath them.
  let refreshed: Session | null
  try {
    refreshed = await refresh()
  } catch {
    return res
  }
  if (!refreshed) {
    setSession(null)
    window.dispatchEvent(new CustomEvent(SESSION_EXPIRED))
    return res
  }

  headers.set('Authorization', `Bearer ${refreshed.accessToken}`)
  res = await fetch(apiURL(path), { ...init, headers, credentials: 'include' })
  return res
}

/**
 * One refresh at a time. Refresh tokens rotate: if two requests hit 401
 * together and each ran its own refresh, the second would spend a token the
 * first had already rotated away, and the session would die under a user who
 * did nothing wrong. Everyone waits on the same attempt instead.
 */
// refresh is the ONLY way to refresh. Everything goes through here, including
// the page-load bootstrap: reaching past it to refreshFromCookie means two
// requests can present the same single-use cookie, and the loser of that race
// looks exactly like an expired session.
function refresh(): Promise<Session | null> {
  refreshing ??= refreshFromCookie().finally(() => {
    refreshing = null
  })
  return refreshing
}

/**
 * bootstrap recovers a session on page load from the HttpOnly refresh cookie: it
 * asks the server for a fresh access token, which also rotates the cookie. It
 * returns the session, or null when there is no valid cookie and the person must
 * sign in. This is why nothing is kept in localStorage: the durable half of the
 * session is a cookie JavaScript cannot read, but the browser always presents.
 */
export async function bootstrap(): Promise<Session | null> {
  // A gateway that is not answering YET is not a gateway that says no.
  //
  // This used to catch Offline and return null, with a comment saying that is
  // not "please sign in" while returning the one value the caller reads as
  // exactly that. So reloading the page during a restart put the person on the
  // sign-in screen with a perfectly good session in their cookie jar.
  //
  // A restart is seconds, so it WAITS for one, briefly and with a widening gap,
  // and only gives up if the gateway is properly gone. Bounded: a page that
  // never resolves is its own kind of broken, and after this the sign-in screen
  // is the honest answer.
  for (let attempt = 0; ; attempt++) {
    try {
      // Through the SINGLE FLIGHT, not the raw request. This loop was written
      // against the raw one, so two mounts (development invokes every effect
      // twice) sent two refreshes of the same cookie at once. Rotation is
      // single-use: one won, one was refused, and the refusal signed the person
      // out. It was reported as "on 4-5 refreshes it randomly lands on login".
      return await refresh()
    } catch {
      if (attempt >= BOOTSTRAP_WAITS.length) return null
      await new Promise((done) => setTimeout(done, BOOTSTRAP_WAITS[attempt]))
    }
  }
}

/**
 * How long to keep waiting for a gateway that is restarting, in ms.
 *
 * It sums to about half a minute, which is not arbitrary: a development rebuild
 * measures ~15 seconds of downtime (the Go build is most of it), and a
 * production restart is quicker. Waiting through it costs a person a loading
 * state; not waiting costs them their session and everything they had typed.
 *
 * It widens rather than polling flat, so a gateway that is properly gone is not
 * hammered a hundred times on the way to the same answer.
 */
const BOOTSTRAP_WAITS = [250, 500, 1000, 2000, 3000, 4000, 5000, 5000, 5000, 5000]

/**
 * refreshSession refreshes the access token, sharing the single-flight above
 * with the REST client, and reports whether a fresh token is now available. It
 * is what the WebSocket calls when the server rejects its token.
 */
export async function refreshSession(): Promise<boolean> {
  try {
    return (await refresh()) !== null
  } catch {
    // Silence is not a refusal: the socket reconnects and asks again.
    return false
  }
}

let refreshing: Promise<Session | null> | null = null

/**
 * Offline is the server not answering: it is down, restarting, or the network
 * went away for a moment.
 *
 * It is a DIFFERENT thing from a refusal, and telling them apart is the whole
 * point of this class. A refusal means the session is over and the person has
 * to sign in. Silence means nothing at all about the session, and treating it
 * as a refusal throws away a perfectly good one: every restart of the server,
 * and every flaky moment of wifi, used to sign somebody out mid-sentence.
 */
export class Offline extends Error {
  constructor() {
    super('the gateway did not answer')
    this.name = 'Offline'
  }
}

async function refreshFromCookie(): Promise<Session | null> {
  // What we held when this attempt began, so a refusal can be told from a race.
  const before = currentSession()
  let res: Response
  try {
    // No body: the refresh token is the HttpOnly cookie the browser sends on its
    // own. The response mints a new access token and rotates the cookie.
    res = await fetch(apiURL('/v1/auth/refresh'), {
      method: 'POST',
      credentials: 'include',
    })
  } catch {
    // The server did not answer. That says nothing about whether the session is
    // still good, so nothing is concluded and nothing is thrown away.
    throw new Offline()
  }
  // A SERVER ERROR IS NOT A REFUSAL. Only the server saying no ends a session.
  //
  // Anything 5xx is the gateway (or whatever sits in front of it) failing to
  // answer, with a status code attached: a restart, a proxy that turns a refused
  // connection into a 500 or a 502, a moment of trouble. Treating that as "your
  // session is over" is the same bug as treating silence as a refusal, only
  // harder to see, because this time an answer really did come back. It signed
  // people out mid-sentence every time the server was restarted behind a proxy.
  if (res.status >= 500) throw new Offline()
  if (!res.ok) {
    // REFUSED, but somebody else may have already succeeded.
    //
    // Two refreshes of the same cookie can be in the air at once (a page load
    // and a socket reconnect, two tabs, a double-invoked effect). Rotation is
    // single-use, so exactly one wins and the other is refused. That loser is
    // not a dead session: it is a duplicate of a request that just worked, and
    // treating it as proof of anything signs somebody out for succeeding.
    //
    // The check is precise rather than forgiving: only a session that ARRIVED
    // while this attempt was in flight counts, so a genuinely expired cookie
    // still ends the session on the first answer.
    const now = currentSession()
    if (now && now !== before) return now
    return null
  }
  return keep(await res.json())
}

/**
 * A refusal from the gateway, as the gateway states it: a code, a sentence
 * about the request as a whole, and, when the problems belong to named
 * fields, a message per field keyed by the JSON field name the request used.
 *
 * The two levels are the point. A form shows `description` as its own error
 * and each entry of `fields` on the input it is about, instead of guessing
 * from a status code what the gateway might have meant.
 */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly description: string,
    readonly fields: Record<string, string> = {},
  ) {
    super(description)
    this.name = 'ApiError'
  }
}

async function refusal(res: Response, method: string, path: string): Promise<ApiError> {
  const body = (await res.json().catch(() => null)) as {
    error?: string
    error_description?: string
    fields?: Record<string, string>
  } | null
  return new ApiError(
    res.status,
    body?.error ?? 'unknown',
    body?.error_description ?? `${method} ${path} failed (${res.status})`,
    body?.fields ?? {},
  )
}

/** A request whose answer is a JSON body. A refusal throws the ApiError. */
export async function json<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await apiFetch(path, init)
  if (!res.ok) throw await refusal(res, (init.method ?? 'GET').toUpperCase(), path)
  return (await res.json()) as T
}

/** A request whose answer is only its status. A refusal throws the ApiError. */
export async function nothing(path: string, init: RequestInit): Promise<void> {
  const res = await apiFetch(path, init)
  if (!res.ok) throw await refusal(res, (init.method ?? 'GET').toUpperCase(), path)
}

export const send = (method: string, body: unknown): RequestInit => ({
  method,
  body: JSON.stringify(body),
})

export async function signOut(): Promise<void> {
  setSession(null)
  try {
    // The refresh cookie carries the token: logout revokes the session and clears
    // the cookie server-side.
    await fetch(apiURL('/v1/auth/logout'), {
      method: 'POST',
      credentials: 'include',
    })
  } catch {
    // The session is gone locally either way; a failed logout is not worth
    // blocking the user on.
  }
}
