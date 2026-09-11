// SAG adaptation: authentication.
//
// The CRM authenticated the chat with a session cookie and an opaque token in
// the request body. SAG has a real API: a bearer token on the Authorization
// header, refreshed when it expires. Rather than thread that through every
// call site, every request the chat makes goes through here.

export interface Workspace {
  id: number
  slug: string
  name: string
}

export interface Session {
  accessToken: string
  user: { id: number; email: string; name: string }
  // The workspace this token speaks for. It is a CLAIM on the token, so it
  // travels with every request without a header of its own; it is kept here
  // only so the app can name where it is standing and remember the choice.
  workspace: Workspace | null
  // Every workspace the person may enter, sent with the login itself so the
  // picker has its choices without a second request. The current one is above.
  workspaces: Workspace[]
}

// The chat talks to the orchestrator DIRECTLY, the same way the console does,
// never through a dev proxy: VITE_SAG_API_URL points at it in dev
// (http://localhost:8080), and in production it is empty and the API is
// same-origin. Every request goes through apiUrl() so the base is applied once.
const API_BASE: string = import.meta.env.VITE_SAG_API_URL ?? ''

/** apiUrl prefixes a /v1 path with the orchestrator's base (empty = same-origin). */
export function apiUrl(path: string): string {
  return `${API_BASE}${path}`
}

/** apiOrigin is the orchestrator's origin, for the WebSocket to build its URL. */
export const apiOrigin: string = API_BASE ? new URL(API_BASE).origin : window.location.origin

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

/** Whether this installation can answer anything yet, and what is missing. */
export interface SetupState {
  ready: boolean
  has_vendor: boolean
  has_model: boolean
  gateway_model?: string
}

/**
 * Asked after signing in, before a composer is drawn.
 *
 * Runtime, unlike DESKTOP below: what kind of build this is never changes,
 * whereas whether somebody has finished setting it up changes the moment they
 * do.
 */
export async function fetchSetup(): Promise<SetupState> {
  const res = await apiFetch('/v1/setup')
  if (!res.ok) throw new Error('The gateway did not say whether it is ready.')
  return (await res.json()) as SetupState
}


/**
 * Whether this build is the desktop one.
 *
 * Decided WHEN THIS IS BUILT, by the desktop build passing VITE_SAG_PERSONAL=1,
 * rather than asked for at run time. A build that asks has a moment where it
 * does not know, and a request that fails leaves it showing the wrong product.
 */
export const PERSONAL: boolean = import.meta.env.VITE_SAG_PERSONAL === '1'

/**
 * Whether the server this page is talking to is somebody's working copy.
 *
 * Asked at RUN TIME, unlike PERSONAL above, and for a reason that does not apply
 * to it: this page is SERVED by whichever server it was deployed to, and the
 * enterprise application points at one somebody typed. A build cannot know which
 * server picked it up, and which server you are looking at is the question.
 *
 * Anything other than a clear yes means production. A server that cannot be
 * reached, or an older one that does not answer this, says nothing rather than
 * claiming to be a working copy: a warning shown where it does not belong is
 * worse than one that never appears.
 */
export async function fetchServerIsDev(): Promise<boolean> {
  try {
    const res = await fetch(apiUrl('/v1/meta'), { credentials: 'include' })
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
 * The server checks the same password as every other sign-in, holding it on this
 * caller's behalf, and issues an ordinary session. What is removed is the
 * typing, not the check.
 */
export async function signInLocally(): Promise<Session> {
  const res = await fetch(apiUrl('/v1/auth/local'), {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
  })
  if (!res.ok) {
    const body = await res.json().catch(() => null)
    throw new Error(body?.error_description || 'This computer could not be signed in.')
  }
  return keep(await res.json())
}

/**
 * Sign in with an email and a password. The workspace is not asked for: the
 * server puts a person back where they were working, and one they have never
 * been placed in is told so plainly rather than looking like a typo.
 */
export async function signIn(email: string, password: string): Promise<Session> {
  const res = await fetch(apiUrl('/v1/auth/login'), {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  })
  if (!res.ok) {
    const body = await res.json().catch(() => null)
    throw new Error(
      body?.error === 'no_workspace'
        ? 'This account is not in any workspace yet. An administrator has to add it to one.'
        : 'Invalid email or password.',
    )
  }
  return keep(await res.json())
}

/** keep turns a token response into a session and stores it. */
function keep(body: {
  access_token: string
  user: Session['user']
  workspace: Workspace | null
  workspaces?: Workspace[]
}): Session {
  const session: Session = {
    accessToken: body.access_token,
    user: body.user,
    workspace: body.workspace ?? null,
    workspaces: body.workspaces ?? [],
  }
  setSession(session)
  return session
}

/**
 * switchWorkspace moves the person to another workspace they belong to. The
 * workspace is a claim, so it can only change by getting a new token: the
 * server checks the membership and issues a pair that speaks for the new one.
 */
export async function switchWorkspace(workspaceID: number): Promise<Session> {
  const res = await apiFetch('/v1/auth/workspace', {
    method: 'POST',
    body: JSON.stringify({ workspace_id: workspaceID }),
  })
  if (!res.ok) throw new Error('That workspace could not be opened.')
  return keep(await res.json())
}

/**
 * apiFetch is the single door to the orchestrator. It carries the bearer
 * token, and on a 401 it refreshes once and retries: an access token expiring
 * mid-conversation should be invisible, not a lost turn.
 */
export async function apiFetch(path: string, init: RequestInit = {}): Promise<Response> {
  const session = currentSession()
  const headers = new Headers(init.headers)
  headers.set('Content-Type', 'application/json')
  if (session) headers.set('Authorization', `Bearer ${session.accessToken}`)

  // credentials:include so the browser attaches the refresh cookie on the auth
  // endpoints (it is path-scoped, so it rides no other call) and can store a
  // rotated one from the response.
  let res = await fetch(apiUrl(path), { ...init, headers, credentials: 'include' })
  if (res.status !== 401) return res

  // The access token expired (or there was none): mint a fresh one from the
  // refresh cookie and retry once. A dead cookie drops us to sign-in.
  // A dead cookie drops us to sign-in. The server NOT ANSWERING does not: it
  // says nothing about the session, so the 401 goes back to the caller and the
  // session is left alone to be tried again.
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
  res = await fetch(apiUrl(path), { ...init, headers, credentials: 'include' })
  return res
}

// One refresh at a time. Refresh tokens rotate, so two concurrent refreshes
// (an apiFetch 401 and the socket's auth rejection at once) would each spend a
// token the other rotated away, killing a session that did nothing wrong.
let refreshing: Promise<Session | null> | null = null

export function refresh(): Promise<Session | null> {
  refreshing ??= refreshOnce().finally(() => {
    refreshing = null
  })
  return refreshing
}

/**
 * Offline is the server not answering: down, restarting, or a moment of network.
 *
 * It is a DIFFERENT thing from a refusal, and telling them apart is the point.
 * A refusal means the session is over. Silence means nothing at all about the
 * session, and treating it as a refusal throws away a good one: every restart
 * of the server signed somebody out mid-sentence.
 */
export class Offline extends Error {
  constructor() {
    super('the gateway did not answer')
    this.name = 'Offline'
  }
}

async function refreshOnce(): Promise<Session | null> {
  // What we held when this attempt began, so a refusal can be told from a race.
  const before = currentSession()
  let res: Response
  try {
    // No body: the refresh token is the HttpOnly cookie the browser sends on its
    // own. The response mints a new access token and rotates the cookie.
    res = await fetch(apiUrl('/v1/auth/refresh'), {
      method: 'POST',
      credentials: 'include',
    })
  } catch {
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
    // and a socket reconnect, two tabs, an effect invoked twice). Rotation is
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
 * refreshSession refreshes the access token (single-flighted, shared with the
 * REST client) and reports whether a fresh one is now available. It is what the
 * WebSocket calls when the server rejects its token.
 */
export async function refreshSession(): Promise<boolean> {
  try {
    return (await refresh()) !== null
  } catch {
    // Silence is not a refusal: the socket reconnects and asks again.
    return false
  }
}

export async function signOut(): Promise<void> {
  setSession(null)
  try {
    // The refresh cookie carries the token: logout revokes the session and clears
    // the cookie server-side.
    await fetch(apiUrl('/v1/auth/logout'), {
      method: 'POST',
      credentials: 'include',
    })
  } catch {
    // The session is gone locally either way; a failed logout is not worth
    // blocking the user on.
  }
}

/**
 * apiUpload sends one file as multipart, with the session on it.
 *
 * It is separate from apiFetch for two reasons, and the second is the one that
 * matters. The Content-Type must NOT be set: the browser writes it itself,
 * including the multipart boundary, and a hand-written one has no boundary and
 * makes the body unparseable.
 *
 * And the body has to be REBUILT for the retry. apiFetch retries a 401 by
 * re-sending the same init, which is fine for a string and wrong for a stream:
 * a FormData body is consumed by the first attempt, so replaying it sends
 * nothing at all. Here the FormData is constructed per attempt from the File,
 * which can be read again.
 */
export async function apiUpload(path: string, file: File): Promise<Response> {
  const send = (accessToken: string | undefined) => {
    const body = new FormData()
    body.append('file', file)
    const headers = new Headers()
    if (accessToken) headers.set('Authorization', `Bearer ${accessToken}`)
    return fetch(apiUrl(path), { method: 'POST', body, headers, credentials: 'include' })
  }

  const res = await send(currentSession()?.accessToken)
  if (res.status !== 401) return res

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
  return send(refreshed.accessToken)
}

/** What a tool was sent and what it answered, as the chat shows it back. */
/**
 * One field of a call, and what it IS rather than only what it is called. The
 * server decides both: which fields are worth reading comes from the tool's own
 * declaration, so nothing here knows a tool by name.
 */
export interface ToolCallField {
  name: string;
  value: unknown;
  /**
   * How to draw it. It describes the DRAWING, not the side: a query's
   * statement and a terminal's output are both text kept as it came, and they
   * sit on opposite halves of a call.
   */
  as?: 'command' | 'text' | 'sql' | 'table' | 'body';
}

export interface ToolCallRecord {
  name: string;
  friendly_name: string;
  status: string;
  duration_ms: number;
  /** Where it ran, shown once above both halves. */
  where?: ToolCallField[];
  sent?: ToolCallField[];
  answered?: ToolCallField[];
  error?: string;
}

/**
 * Open one tool call.
 *
 * Asked for when somebody expands the row rather than carried on every turn:
 * what a tool was sent and answered is the largest thing in a conversation, and
 * the one thing a person may not be permitted to see. A refusal here is a
 * refusal, not a hidden button — the check is the server's.
 */
export async function fetchToolCall(id: string): Promise<ToolCallRecord> {
  const res = await apiFetch('/v1/chat/tool-call', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id }),
  });
  if (!res.ok) {
    throw new Error(res.status === 403 ? 'not_permitted' : 'unavailable');
  }
  return (await res.json()) as ToolCallRecord;
}
