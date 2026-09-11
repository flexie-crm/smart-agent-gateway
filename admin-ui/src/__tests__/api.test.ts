import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, apiFetch, bootstrap, currentSession, json, nothing, setSession, SESSION_EXPIRED } from '@/lib/api'
import type { Session } from '@/lib/api'

// The single door to the orchestrator must never let the same question be in
// flight twice. Development mounts every screen twice to prove its effects
// survive it; two panes may want the same list at the same moment; and two
// requests can expire together. None of those may reach the server as
// duplicates, because a duplicated refresh spends a rotated token and kills
// the session of somebody who did nothing wrong.

const SESSION: Session = {
  accessToken: 'old-access',
  user: { id: 1, email: 'admin@acme.test', name: 'Admin' },
}

function ok(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200 })
}

describe('apiFetch', () => {
  beforeEach(() => setSession({ ...SESSION }))
  afterEach(() => {
    setSession(null)
    vi.unstubAllGlobals()
  })

  it('asks the server once when the same GET is already in flight', async () => {
    const fetch = vi.fn(async () => ok({ answer: 42 }))
    vi.stubGlobal('fetch', fetch)

    const [a, b] = await Promise.all([apiFetch('/v1/things'), apiFetch('/v1/things')])

    expect(fetch).toHaveBeenCalledTimes(1)
    // Both callers get their own copy of the body, not a fight over one stream.
    expect(await a.json()).toEqual({ answer: 42 })
    expect(await b.json()).toEqual({ answer: 42 })
  })

  it('asks again once the previous answer has landed', async () => {
    const fetch = vi.fn(async () => ok({}))
    vi.stubGlobal('fetch', fetch)

    await apiFetch('/v1/things')
    await apiFetch('/v1/things')

    // Sharing is for questions in flight. It is not a cache: a screen that
    // asks after a write must hear what the write changed.
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('does not share answers across different paths', async () => {
    const fetch = vi.fn(async (input: RequestInfo | URL) => ok({ path: String(input) }))
    vi.stubGlobal('fetch', fetch)

    const [a, b] = await Promise.all([apiFetch('/v1/brains'), apiFetch('/v1/tools')])

    expect(fetch).toHaveBeenCalledTimes(2)
    expect(await a.json()).toEqual({ path: '/v1/brains' })
    expect(await b.json()).toEqual({ path: '/v1/tools' })
  })

  it('never shares a write', async () => {
    const fetch = vi.fn(async () => ok({}))
    vi.stubGlobal('fetch', fetch)

    await Promise.all([
      apiFetch('/v1/brains', { method: 'POST', body: '{}' }),
      apiFetch('/v1/brains', { method: 'POST', body: '{}' }),
    ])

    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('refreshes once for requests that expire together, and retries them with the new token', async () => {
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      if (path === '/v1/auth/refresh') {
        // The refresh token comes back only as an HttpOnly cookie now, never in
        // the body; the response carries just the new access token and identity.
        return ok({ access_token: 'new-access', user: SESSION.user })
      }
      const token = new Headers(init?.headers).get('Authorization')
      if (token !== 'Bearer new-access') {
        return new Response('{}', { status: 401 })
      }
      return ok({ path })
    })
    vi.stubGlobal('fetch', fetch)

    const [a, b] = await Promise.all([apiFetch('/v1/brains'), apiFetch('/v1/tools')])

    const refreshes = fetch.mock.calls.filter(([input]) => String(input) === '/v1/auth/refresh')
    expect(refreshes).toHaveLength(1)
    expect(a.status).toBe(200)
    expect(b.status).toBe(200)
  })

  it('refreshes ONCE when the page bootstraps twice', async () => {
    // Development invokes every effect twice to prove it survives it, so
    // bootstrap runs twice on every load. Its retry loop was written against
    // the raw request rather than the single flight, so those two mounts sent
    // two refreshes of the same cookie at once; rotation is single-use, one was
    // refused, and the refusal signed the person out. Reported as "on 4-5
    // refreshes it randomly lands on login".
    setSession(null)
    let inFlight = 0
    let overlapped = false
    // The URL is read back off the recorded calls below, so the mock takes it.
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      if (!String(input).includes('/v1/auth/refresh')) return ok({})
      inFlight++
      if (inFlight > 1) overlapped = true
      await new Promise((r) => setTimeout(r, 5))
      inFlight--
      return ok({ access_token: 'fresh', user: SESSION.user })
    })
    vi.stubGlobal('fetch', fetch)

    await Promise.all([bootstrap(), bootstrap()])

    const refreshes = fetch.mock.calls.filter(([input]) => String(input).includes('/v1/auth/refresh'))
    expect(refreshes).toHaveLength(1)
    expect(overlapped).toBe(false)
  })

  it('does not sign out the loser of a refresh race', async () => {
    // Two refreshes of one cookie can be in the air at once: a page load and a
    // socket reconnect, two tabs, an effect invoked twice. Rotation is
    // single-use, so exactly one wins and the server refuses the other. That
    // loser is a duplicate of a request that just worked, and treating it as a
    // dead session signed people out for succeeding.
    setSession(null)
    const fetch = vi.fn(async () => {
      // While this attempt is in flight, the winner lands its session.
      setSession({ ...SESSION, accessToken: 'won-by-the-other-one' })
      return new Response('{}', { status: 401 })
    })
    vi.stubGlobal('fetch', fetch)

    const expired = vi.fn()
    window.addEventListener(SESSION_EXPIRED, expired)
    const recovered = await bootstrap()
    window.removeEventListener(SESSION_EXPIRED, expired)

    expect(recovered?.accessToken).toBe('won-by-the-other-one')
    expect(expired).not.toHaveBeenCalled()
    expect(currentSession()).not.toBeNull()
  })

  it('still signs out when the cookie is genuinely dead', async () => {
    // The check is precise, not forgiving: nothing arrived while this attempt
    // was in flight, so the refusal is the answer it appears to be.
    setSession(null)
    vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status: 401 })))
    expect(await bootstrap()).toBeNull()
  })

  it('signs the session out when the refresh itself is refused', async () => {
    const fetch = vi.fn(async () => new Response('{}', { status: 401 }))
    vi.stubGlobal('fetch', fetch)
    const expired = vi.fn()
    window.addEventListener(SESSION_EXPIRED, expired)

    const res = await apiFetch('/v1/brains')

    window.removeEventListener(SESSION_EXPIRED, expired)
    // The caller still hears the 401; the app is told the session died so it
    // can show the sign-in instead of a broken screen.
    expect(res.status).toBe(401)
    expect(expired).toHaveBeenCalledTimes(1)
    // The in-memory session is cleared (there is no localStorage to check now).
    expect(currentSession()).toBeNull()
  })
})

// A refusal from the gateway reaches the caller as the gateway stated it:
// the code, the sentence about the whole request, and the field-by-field
// messages. Throwing away the body and guessing from the status is exactly
// what these helpers exist to end.
describe('json and nothing', () => {
  beforeEach(() => setSession({ ...SESSION }))
  afterEach(() => {
    setSession(null)
    vi.unstubAllGlobals()
  })

  it('returns the parsed body when the gateway says yes', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ok({ id: 7 })))

    await expect(json<{ id: number }>('/v1/groups', { method: 'POST', body: '{}' }))
      .resolves.toEqual({ id: 7 })
  })

  it('throws the refusal with both levels intact', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({
              error: 'invalid_request',
              error_description: 'some fields need a change',
              fields: { name: 'a name is required', email: 'a valid email is required' },
            }),
            { status: 400 },
          ),
      ),
    )

    const failure = await json('/v1/users', { method: 'POST', body: '{}' }).catch((e: unknown) => e)

    expect(failure).toBeInstanceOf(ApiError)
    const refusal = failure as ApiError
    expect(refusal.status).toBe(400)
    expect(refusal.code).toBe('invalid_request')
    expect(refusal.description).toBe('some fields need a change')
    expect(refusal.fields).toEqual({
      name: 'a name is required',
      email: 'a valid email is required',
    })
  })

  it('still throws a usable refusal when the answer is not the envelope', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('bad gateway', { status: 502 })))

    const failure = await nothing('/v1/vendors/1', { method: 'DELETE' }).catch((e: unknown) => e)

    expect(failure).toBeInstanceOf(ApiError)
    const refusal = failure as ApiError
    expect(refusal.status).toBe(502)
    expect(refusal.code).toBe('unknown')
    expect(refusal.fields).toEqual({})
    // The fallback still says what was asked, so a log line means something.
    expect(refusal.description).toContain('DELETE /v1/vendors/1')
  })

  it('leaves the fields empty when the refusal is about the whole request', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({ error: 'conflict', error_description: 'resource is still in use' }),
            { status: 409 },
          ),
      ),
    )

    const failure = await nothing('/v1/vendors/1', { method: 'DELETE' }).catch((e: unknown) => e)

    const refusal = failure as ApiError
    expect(refusal.code).toBe('conflict')
    expect(refusal.description).toBe('resource is still in use')
    expect(refusal.fields).toEqual({})
  })
})
