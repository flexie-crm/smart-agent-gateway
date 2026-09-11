import { apiFetch } from './api';
// lib/use-chat-list.ts
// ─── Multi-thread chat list + CRUD (mirrors use-chat-stream, no streaming) ───
//
// Talks to the DB-backed chat endpoints:
//   GET  {base}                 -> { chats: ChatListItem[] }   (?q=&limit=)
//   POST {base}/create          -> { id, title }               (form: title?)
//   POST {base}/update/{id}     -> { ok:true }                  (form: title|is_pinned)
//   POST {base}/delete/{id}     -> { ok:true }
//
// Same-origin (main app) requests carry the session cookie automatically, which
// is what the /s firewall authenticates on. No token/extraData is sent here.
//
// Pagination grows the window from the top: "Load more" bumps `limit` by PAGE and
// re-reads the top N. Cheap for a sidebar, and mutations (create/rename/pin/
// delete) refresh the same N so the loaded set never collapses to page one.

import { useCallback, useEffect, useRef, useState } from 'react'
import { useNotify } from './notify'

const PAGE = 30

export interface ChatListItem {
  id: string
  title: string | null
  last_message_at: string | null
  is_pinned: boolean
  message_count: number
}

// SAG: the chat authenticates itself, and the API speaks JSON. The CRM posted
// forms to an endpoint that already knew who you were from a cookie.
async function postJson(url: string, fields: Record<string, unknown>): Promise<Response> {
  return apiFetch(url, { method: 'POST', body: JSON.stringify(fields) })
}

export function useChatList(chatsEndpoint?: string) {
  const notify = useNotify()
  const [chats, setChats] = useState<ChatListItem[]>([])
  /**
   * Whether this person may delete a conversation at all (`chats:delete`).
   *
   * A fact about the whole list rather than about a row: the permission is
   * theirs and every conversation in it is theirs, so the server answers it
   * once, with the list. It is NOT a second request, and it is not read from a
   * token either, because a role edited while somebody sits on this screen has
   * to bite on their next request.
   *
   * It starts false. An action that turns out to be refused should never have
   * been offered, and offering it for one paint and withdrawing it is the same
   * mistake with worse timing.
   */
  const [canDelete, setCanDelete] = useState(false)
  const [loading, setLoading] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [query, setQueryRaw] = useState('')
  const [limit, setLimit] = useState(PAGE)
  const [hasMore, setHasMore] = useState(false)
  const reqSeq = useRef(0)

  const enabled = typeof chatsEndpoint === 'string' && chatsEndpoint.length > 0
  const base = chatsEndpoint || ''

  const refresh = useCallback(async () => {
    if (!enabled) return
    const seq = ++reqSeq.current
    setLoading(true)
    try {
      const params = new URLSearchParams()
      if (query.trim()) params.set('q', query.trim())
      params.set('limit', String(limit))
      const res = await apiFetch(`${base}?${params.toString()}`, { method: 'GET' })
      if (!res.ok) throw new Error('Failed to load chats')
      const data = await res.json()
      // Ignore out-of-order responses (a newer request already landed).
      if (seq !== reqSeq.current) return
      const rows: ChatListItem[] = Array.isArray(data?.chats) ? data.chats : []
      setChats(rows)
      setCanDelete(data?.can_delete === true)
      // A full page back means the server may be holding more beyond the window.
      setHasMore(rows.length >= limit)
    } catch (err) {
      if (seq === reqSeq.current) console.error('[useChatList] load failed', err)
    } finally {
      if (seq === reqSeq.current) { setLoading(false); setLoaded(true) }
    }
  }, [enabled, base, query, limit])

  // Reload when the filter or window changes. The query is debounced
  // (search-as-you-type) — a genuine input-debounce, not a sequencing hack.
  useEffect(() => {
    if (!enabled) return
    const h = window.setTimeout(refresh, query ? 250 : 0)
    return () => window.clearTimeout(h)
  }, [enabled, refresh, query])

  // New search starts a fresh window so results are not paginated by a stale,
  // grown limit from browsing the full list.
  const setQuery = useCallback((q: string) => {
    setLimit(PAGE)
    setQueryRaw(q)
  }, [])

  const loadMore = useCallback(() => {
    setLimit((l) => l + PAGE)
  }, [])

  // Optimistically add a freshly created chat (from the SSE chat_created frame)
  // so the sidebar shows the row immediately; a later refresh() reconciles it and
  // fills in the generated title.
  const addChatOptimistic = useCallback((id: string, title: string | null = null) => {
    setChats((prev) => prev.some((c) => c.id === id)
      ? prev
      : [{ id, title, last_message_at: null, is_pinned: false, message_count: 0 }, ...prev])
  }, [])

  const createChat = useCallback(async (title?: string): Promise<string | null> => {
    if (!enabled) return null
    try {
      const res = await postJson(`${base}/create`, title ? { title } : {})
      if (!res.ok) throw new Error('create failed')
      const data = await res.json()
      await refresh()
      return typeof data?.id === 'string' && data.id ? data.id : null
    } catch {
      notify.error('A new conversation could not be started.')
      return null
    }
  }, [enabled, base, refresh, notify])

  /**
   * Every write the sidebar makes, and what is said about it.
   *
   * It used to catch its own failure, write it to the console and refresh the
   * list anyway. So a delete the server refused looked exactly like a delete
   * that worked, right up until the row was still there with nothing to say
   * why, and a rename that failed silently put the old title back as if the
   * person had mistyped it.
   *
   * `said` is what a person is told when it WORKS, and is left out where the
   * screen already answers: renaming shows the new title and pinning moves the
   * row, and a toast repeating what somebody just watched happen is noise. A
   * failure is always said, because nothing else says it.
   */
  const runMutation = useCallback(async (
    url: string,
    fields: Record<string, unknown>,
    failed: string,
    said?: string,
  ) => {
    if (!enabled) return
    try {
      const res = await postJson(url, fields)
      if (!res.ok) notify.error(failed)
      else if (said) notify.success(said)
    } catch {
      notify.error(failed)
    }
    await refresh()
  }, [enabled, refresh, notify])

  const renameChat = useCallback((id: string, title: string) =>
    runMutation(`${base}/update/${id}`, { title }, 'The conversation could not be renamed.'),
    [runMutation, base])

  const setPinned = useCallback((id: string, pinned: boolean) =>
    runMutation(`${base}/update/${id}`, { is_pinned: pinned },
      pinned ? 'The conversation could not be pinned.' : 'The conversation could not be unpinned.'),
    [runMutation, base])

  // A conversation going is the one that has to be said. A row disappearing is
  // not a confirmation (it looks the same as a row disappearing for any other
  // reason), and this is the only one of the three that cannot be undone.
  const deleteChat = useCallback((id: string) =>
    runMutation(`${base}/delete/${id}`, {},
      'The conversation could not be deleted.', 'The conversation was deleted.'),
    [runMutation, base])

  return {
    enabled,
    chats,
    canDelete,
    loading,
    loaded,
    query,
    setQuery,
    hasMore,
    loadMore,
    refresh,
    createChat,
    addChatOptimistic,
    renameChat,
    setPinned,
    deleteChat,
  }
}
