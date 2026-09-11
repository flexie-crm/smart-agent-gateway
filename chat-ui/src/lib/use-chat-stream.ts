// lib/use-chat-stream.ts
// ─── Thin React hook — delegates frame processing to StreamProcessor ────────

import { useState, useRef, useEffect, useCallback, useMemo } from 'react'
import { ChatStreamClient, type ClientToolDef } from './chat-stream-client'
import { nanoid } from 'nanoid'
import { StreamProcessor, type StateCommand } from './stream-processor'
import type { ChatMessage, SSEFrame, AgentActivity, FileAttachment } from './chat-types'
import { invokeLocalClientTool, resolveLocalRegistry, type ClientToolRegistry } from './client-tool-dispatcher'
import { useNotify } from './notify'
import { NO_ACCEPTS, readAccepts, type ChatAccepts } from './use-chat-accepts'
import { resolveDynamic } from './utils'
import { applyDelegationEvent, CARD_EVENT, DELEGATION_EVENT, SOCKET_READY_EVENT, TURN_EVENT, type Delegation } from './delegations'
import { traceCompletion } from './completion-trace'
import { dropEmptyPlaceholder } from './placeholder'
import type { ConfirmationRequest } from './chat-types'

// Re-export ChatMessage so existing imports from this module keep working
import { apiFetch } from './api';
export type { ChatMessage } from './chat-types'

/**
 * Build the per-turn POST snapshot of client-tool defs from the host registry.
 * Strips handler functions — only metadata travels to the server.
 */
function buildClientToolDefs(registry: ClientToolRegistry): ClientToolDef[] {
  if (!registry || typeof registry !== 'object') return [];
  const out: ClientToolDef[] = [];
  for (const [name, entry] of Object.entries(registry)) {
    if (!entry || typeof entry !== 'object') continue;
    if (typeof entry.description !== 'string' || !entry.parameters) continue;
    out.push({
      name,
      description: entry.description,
      parameters: entry.parameters as Record<string, unknown>,
      friendly_name: entry.friendlyName,
      requires_confirm: !!entry.requiresConfirm,
    });
  }
  return out;
}

export function useChatStream(
  streamEndpoint: string,
  fetchEndpoint: string,
  attachEndpoint?: string,
  cancelEndpoint?: string,
  resetEndpoint?: string,
  token?: string | (() => string | Promise<string>),
  extraData?: any,
  _lang?: any,
  clientTools?: ClientToolRegistry,
  // Opaque DB-backed chat id (uid). Undefined/null keeps the legacy per-user
  // Redis behaviour untouched; when set it is sent as chat_id on history + turns.
  chatId?: string | null,
  // Draft mode: when true (multi-chat with no chat resolved yet) the next send
  // asks the server to create a fresh chat. The new uid arrives mid-stream via
  // the chat_created frame -> onChatCreated.
  newChatOnSend?: boolean,
  onChatCreated?: (uid: string) => void,
) {
  const [activity, setActivity] = useState<AgentActivity>({ kind: 'idle' });
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [isStreaming, setIsStreaming] = useState(false)
  // Whether this conversation auto-approves risky actions. Read from the server
  // on load and switched by the in-session toggle; also flipped to 'auto' when
  // the person picks "approve all" on a card.
  const notify = useNotify()
  const [approvalMode, setApprovalModeState] = useState<'manual' | 'auto'>('manual')
  // What may be attached and whether anything may be spoken, from the history
  // answer rather than an endpoint of its own.
  const [accepts, setAccepts] = useState<ChatAccepts>(NO_ACCEPTS)
  // Background delegations still in flight for this conversation, rendered as
  // chips (Mode C). Seeded from history on load, moved live over the socket.
  const [delegations, setDelegations] = useState<Delegation[]>([])
  // The approval-mode and delegation-cancel endpoints are siblings of history.
  const approvalEndpoint = useMemo(
    () => fetchEndpoint.replace(/\/[^/]+$/, '/approval-mode'),
    [fetchEndpoint],
  )
  const cancelDelegationEndpoint = useMemo(
    () => fetchEndpoint.replace(/\/[^/]+$/, '/delegation/cancel'),
    [fetchEndpoint],
  )
  // Where something said mid-answer goes: into the turn, not behind it.
  const sayEndpoint = useMemo(
    () => fetchEndpoint.replace(/\/[^/]+$/, '/say'),
    [fetchEndpoint],
  )
  const isStreamingRef = useRef(false)
  // Whether a card is on screen, for the socket reconcile above. Kept in step
  // with the rendered messages rather than set at each of the places a card can
  // appear or be answered, because there are several and one of them would be
  // missed.
  const showingCardRef = useRef(false)
  const rafRef = useRef<number | null>(null)
  // Ref-indirected: the history effect runs before rejoin is defined, and a
  // conversation being opened is exactly when it needs to be called.
  // Returns whether the run is SETTLED: heard, or genuinely not there. False
  // means the attempt was interrupted and the run still has an answer nobody
  // has heard, so the caller must try it again.
  const rejoinRef = useRef<((opts?: { fromStart?: boolean; replay?: boolean; run?: string }) => Promise<boolean>) | null>(null)
  // Completion turns (Mode C) can be nudged while a stream is already in flight
  // (a fast auto-approved task, or several background tasks finishing at once).
  // Rejoining then would collide with the live stream, so each nudge's run is
  // queued here and heard, in order, as the lane frees, rather than dropped
  // (which left the narration invisible until a reload) or collapsed onto the
  // latest (which lost the earlier ones).
  const pendingRunsRef = useRef<string[]>([])
  // Drains pendingRunsRef reactively, and a flag so only one attach is in flight.
  // Ref-indirected because openStream, setStreaming, and the socket effect are
  // defined before the drain and reach it through the ref.
  const drainCompletionsRef = useRef<(() => void) | null>(null)
  // What was said while the assistant was busy, in the order it was said.
  const waitingRef = useRef<Array<{ id: string; text: string; fileIds?: string[] }>>([])
  const drainSaidRef = useRef<(() => void) | null>(null)
  const drainingRef = useRef(false)
  const processorRef = useRef<StreamProcessor | null>(null)
  const lastFetchKeyRef = useRef<string>('')
  const skipNextFetchRef = useRef(false)
  const clientRef = useRef<ChatStreamClient | null>(null)
  // Live registry — prop wins over window.FlexieClientTools (lib mode); same
  // window fallback. Iframe mode resolves through the bridge inside
  // invokeLocalClientTool when the registry is empty here.
  const registry = useMemo(() => resolveLocalRegistry(clientTools), [clientTools]);
  const registryRef = useRef<ClientToolRegistry>(registry);
  registryRef.current = registry;
  const clientToolDefs = useMemo(() => buildClientToolDefs(registry), [registry]);
  const clientToolDefsRef = useRef<ClientToolDef[]>(clientToolDefs);
  clientToolDefsRef.current = clientToolDefs;

  // Always-current chat id so a client rebuild (endpoint/token/extraData change)
  // re-applies it without depending on chatId.
  const chatIdRef = useRef<string | null | undefined>(chatId)
  chatIdRef.current = chatId
  const newChatOnSendRef = useRef<boolean | undefined>(newChatOnSend)
  newChatOnSendRef.current = newChatOnSend
  const onChatCreatedRef = useRef(onChatCreated)
  onChatCreatedRef.current = onChatCreated

  if (!clientRef.current) {
    clientRef.current = new ChatStreamClient(streamEndpoint, token, extraData, clientToolDefs, { attach: attachEndpoint, cancel: cancelEndpoint })
    clientRef.current.setChatId(chatId ?? null)
    clientRef.current.setPendingNewChat(!!newChatOnSend)
  }

  useEffect(() => {
    clientRef.current = new ChatStreamClient(streamEndpoint, token, extraData, clientToolDefsRef.current, { attach: attachEndpoint, cancel: cancelEndpoint })
    clientRef.current.setChatId(chatIdRef.current ?? null)
    clientRef.current.setPendingNewChat(!!newChatOnSendRef.current)
  }, [streamEndpoint, attachEndpoint, cancelEndpoint, token, extraData])

  // Keep the live client's chat id current when the user switches threads.
  useEffect(() => {
    clientRef.current?.setChatId(chatId ?? null)
  }, [chatId])

  // Keep the live client's draft flag current.
  useEffect(() => {
    clientRef.current?.setPendingNewChat(!!newChatOnSend)
  }, [newChatOnSend])

  // Keep the live client in sync with the latest registry snapshot — no need
  // to rebuild the client; the defs are read at start() time per turn.
  useEffect(() => {
    clientRef.current?.setClientToolDefs(clientToolDefs);
  }, [clientToolDefs]);

  const client = clientRef.current
  const sessionKey: string = (() => {
    let key = localStorage.getItem('fx_chat_session')

    if (!key) {
      key = crypto.randomUUID()
      localStorage.setItem('fx_chat_session', key)
    }

    return key
  })()

  /**
   * Read a conversation's transcript, and everything that renders alongside it.
   *
   * Its own function rather than a body inside the load effect, because a
   * transcript has to be re-read at moments that have nothing to do with
   * opening a chat. The one that matters here: the server refusing an approval
   * because the card we were holding is no longer the one waiting. That refusal
   * says our view of the conversation is out of date, and the only honest answer
   * to being out of date is to go and read it again.
   *
   * It does not clear anything first and does not touch the fetch-key guard: the
   * effect owns opening a chat, this owns bringing an open one up to date.
   */
  // What is behind what is on screen: whether there is more, and where it
  // starts. Empty when a conversation arrived whole.
  const [older, setOlder] = useState<{ more: boolean; before: number }>({ more: false, before: 0 });
  const [loadingOlder, setLoadingOlder] = useState(false);

  /**
   * Fetches the page before the one on screen and puts it in front.
   *
   * Prepending rather than replacing, because the person is READING: what they
   * are looking at must stay where it is, and the older rows appear above it.
   * Nothing is refetched, so a conversation is walked backwards once however
   * far somebody scrolls.
   */
  const loadOlder = useCallback(async () => {
    if (!older.more || !older.before || loadingOlder) return;
    setLoadingOlder(true);
    try {
      const res = await apiFetch(fetchEndpoint, {
        method: 'POST',
        body: JSON.stringify({ chat_id: chatIdRef.current, before: older.before }),
      });
      if (!res.ok) return;
      const data = await res.json();
      const page: ChatMessage[] = data.messages ?? [];
      if (page.length) {
        setMessages((prev) => [...page, ...prev]);
      }
      setOlder({
        more: !!data.meta?.more,
        before: typeof data.meta?.oldest === 'number' ? data.meta.oldest : 0,
      });
    } finally {
      setLoadingOlder(false);
    }
  }, [fetchEndpoint, older.more, older.before, loadingOlder]);

  const loadHistory = useCallback(async (chat?: string, signal?: AbortSignal) => {
    // SAG: history is loaded for a session the server owns. There is no
    // session key to trust from the client: the token says who you are.
    const body: Record<string, any> = {};
    if (chat) body.chat_id = chat;
    const res = await apiFetch(fetchEndpoint, {
      method: 'POST',
      signal,
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error('Failed to load history');

    // The history response carries the conversation's metadata (its approval
    // mode, and whatever else is added to `meta` later) alongside the
    // messages, so one request loads everything the chat needs to render.
    const data = await res.json();
    setMessages(data.messages ?? []);
    // Where the page before this one starts, and whether there is one. A
    // conversation arrives newest-first in pages counted in rows, because
    // sending a long one whole costs on every open: the payload, the memory,
    // and the page it builds.
    setOlder({
      more: !!data.meta?.more,
      before: typeof data.meta?.oldest === 'number' ? data.meta.oldest : 0,
    });
    const mode = data.meta?.approval_mode;
    setApprovalModeState(mode === 'auto' ? 'auto' : 'manual');
    setDelegations(Array.isArray(data.meta?.running_delegations) ? data.meta.running_delegations : []);
    // What the composer may offer, from the same answer. It used to be a
    // request of its own beside this one, for a fact needed exactly when a
    // conversation opens and at no other time.
    setAccepts(readAccepts(data.meta?.accepts));
    // And whether there is a turn still being answered here. Returned so the
    // caller rejoins only when there is something to rejoin: it used to POST to
    // /chat/attach after every history load and be told 204, nothing is
    // happening, which is a round trip to hear no.
    return typeof data.meta?.live_run === 'string' ? data.meta.live_run : '';
  }, [fetchEndpoint]);
  const loadHistoryRef = useRef(loadHistory);
  loadHistoryRef.current = loadHistory;

  // Get existing messages from this chat — re-fetches on config changes.
  // Token isn't part of the fingerprint: it identifies the caller/auth, not the
  // conversation, so a token swap alone shouldn't trigger a history re-fetch.
  useEffect(() => {
    // Multi-thread mode with no chat resolved yet (chatId === null): this is a
    // draft "New chat" (or bootstrap). Show an empty transcript and do NOT fire a
    // legacy per-user history fetch that would be superseded once a chat id
    // arrives. Legacy mode passes undefined (not null) and fetches as before.
    // A draft belongs to no conversation, so it carries none of the previous
    // chat's per-conversation state: reset the approval mode to its default and
    // drop any running-delegation chips, or they leak into the new chat until the
    // first message's history load replaces them.
    if (chatId === null) {
      setMessages([]);
      setApprovalModeState('manual');
      setDelegations([]);
      lastFetchKeyRef.current = '';
      // And then ASK, because a draft still has a composer.
      //
      // What may be sent is a fact about the GATEWAY, not about a conversation:
      // which files it can read, whether anything can be spoken. A draft has no
      // conversation and every right to know it, and the server has always said
      // so, putting `accepts` on the empty answer for exactly this case. Nobody
      // asked, so a new chat sat on NO_ACCEPTS and showed neither the microphone
      // nor the attach button until after the first message had been sent.
      //
      // It is also how configuring one takes effect: set an audio model in the
      // console, come back, and the answer to this is different. Nothing has to
      // be invalidated or pushed, because the question is asked again.
      //
      // Cheap and safe: with no chat id the endpoint short-circuits to that
      // empty answer, which is the same reset performed above plus the one thing
      // that cannot be guessed from here.
      void loadHistoryRef.current?.()
      return
    }

    const hasChatId = typeof chatId === 'string' && chatId.length > 0
    const identity = hasChatId ? `c${chatId}` : sessionKey
    const fetchKey = `${fetchEndpoint}|${identity}`
    if (fetchKey === lastFetchKeyRef.current) return
    lastFetchKeyRef.current = fetchKey

    // After reset we already cleared messages — skip the redundant fetch
    if (skipNextFetchRef.current) {
      skipNextFetchRef.current = false
      return
    }

    // Switching threads: clear immediately so the previous chat's transcript
    // doesn't linger under the incoming one while it loads. Legacy (no chatId)
    // path is untouched — it never cleared before a fetch.
    if (hasChatId) setMessages([])

    const controller = new AbortController()

    ;(async () => {
      try {
        const live = await loadHistoryRef.current?.(hasChatId ? chatId : undefined, controller.signal);
        // A turn outlives the page that asked for it (KB/17), so opening this
        // chat means rejoining whatever is in flight. The history just said
        // whether there IS anything, so this asks only when there is.
        if (live) void rejoinRef.current?.({ run: live });
      } catch (err: any) {
        if (err?.name !== 'AbortError') {
          console.error('[useChatStream] Failed to fetch chat history: ', err);
        }
      }
    })();

    return () => controller.abort()
  }, [fetchEndpoint, sessionKey, token, extraData, chatId]);

  /**
   * Switch this conversation between asking every time and auto-approving.
   *
   * Optimistic, and it puts the switch BACK if the write does not land. It used
   * to leave it flipped and wait for the next load to re-read the truth, which
   * on this particular setting is the wrong way round: somebody who turned
   * asking off, and was not told it had failed, believes the assistant will
   * stop for them and it will not. The failure is worth interrupting for.
   */
  const setApprovalMode = useCallback(async (mode: 'manual' | 'auto') => {
    if (!chatId) return;
    const before = approvalMode;
    setApprovalModeState(mode);
    try {
      const res = await apiFetch(approvalEndpoint, { method: 'POST', body: JSON.stringify({ chat_id: chatId, mode }) });
      if (!res.ok) throw new Error(`approval mode ${res.status}`);
    } catch {
      setApprovalModeState(before);
      notify.error(
        mode === 'auto'
          ? 'That did not save, so you will still be asked before anything runs.'
          : 'That did not save, so this conversation still runs without asking.',
      );
    }
  }, [approvalEndpoint, chatId, approvalMode, notify]);

  /**
   * Stop background work: one agent, or a whole batch.
   *
   * Which of the two is decided by the chip's own kind rather than by the
   * caller, because they are ids from different tables and a number meant as a
   * batch and read as an agent would stop somebody else's work.
   *
   * Nothing is flipped optimistically. Cancelling a batch has to reach the
   * machines running it, so what the chip should say next is what the server
   * pushes back, not what this guessed.
   */
  const cancelDelegation = useCallback(async (chip: Delegation) => {
    if (!chatId) return;
    const fleet = chip.kind === 'fleet';
    const id = Number(fleet ? chip.id.replace(/^fleet-/, '') : chip.id);
    if (!Number.isFinite(id) || id === 0) return;
    try {
      const res = await apiFetch(cancelDelegationEndpoint, {
        method: 'POST',
        body: JSON.stringify(
          fleet
            ? { chat_id: chatId, fleet_id: id }
            : { chat_id: chatId, delegation_id: id },
        ),
      });
      if (!res.ok) throw new Error(`cancel ${res.status}`);
    } catch {
      notify.error('That could not be stopped. It may still be running.');
    }
  }, [cancelDelegationEndpoint, chatId, notify]);

  // A background agent stopped for approval: the server pushes its card
  // WHOLE over the socket (KB/27), so it is rendered straight from the push with
  // no API round-trip. The card is REPLACED when it is already shown (the server
  // re-mints its token on a fresh surfacing), so it always carries the live token.
  const showCard = useCallback((id: string, confirmation: ConfirmationRequest) => {
    const card: ChatMessage = { id, role: 'confirm', content: '', confirmation };
    setMessages(prev => {
      const idx = prev.findIndex(m => m.id === id);
      if (idx === -1) return [...prev, card];
      const next = [...prev];
      next[idx] = card;
      return next;
    });
  }, []);
  const showCardRef = useRef(showCard);
  showCardRef.current = showCard;

  // Ref-indirected so applyCommand can dispatch to it without a definition
  // cycle (runClientTool is built below and needs applyCommand to exist).
  const runClientToolRef = useRef<((payload: {
    name: string;
    friendly_name?: string;
    args: Record<string, unknown>;
    requires_confirm?: boolean;
  }) => void) | null>(null);

  // The one place a turn's streaming state flips. Turning it OFF means the
  // conversation's lane is free again, so any completion turns (Mode C) that were
  // queued while it was busy are drained now. The drain reacts to this event; it
  // never polls a flag waiting for the lane to clear.
  const setStreaming = useCallback((streaming: boolean) => {
    isStreamingRef.current = streaming;
    setIsStreaming(streaming);
    // Deferred out of the ending stream's own teardown, and that is the whole
    // point of the microtask. A stream ends with `onEnd(); stop()`: running the
    // drain inside onEnd starts a NEW request on the same client, and the `stop()`
    // one line later aborts the request it just opened. So the next attach waits
    // until the last thing the old stream does has been done.
    if (!streaming) {
      queueMicrotask(() => {
        drainCompletionsRef.current?.();
        // And then whatever the person said while all that was going on. It
        // checks the lane itself, so a completion taking it just means this
        // comes round again when THAT ends.
        drainSaidRef.current?.();
      });
    }
  }, []);

  // ─── Apply commands produced by StreamProcessor to React state ──────────
  const applyCommand = useCallback((cmd: StateCommand) => {
    switch (cmd.kind) {
      case 'set_streaming':
        setStreaming(cmd.value);
        break;
      case 'set_activity':
        setActivity(cmd.value);
        break;
      case 'update_messages':
        setMessages(cmd.updater);
        break;
      case 'invoke_client_tool':
        // Fire and forget — the dispatcher runs the host handler and then
        // opens a fresh SSE turn carrying the result.
        runClientToolRef.current?.(cmd.payload);
        break;
    }
  }, [setStreaming]);

  const applyCommands = useCallback((cmds: StateCommand[]) => {
    for (const cmd of cmds) applyCommand(cmd);
  }, [applyCommand]);

  /** Cancel any in-flight RAF loop and drop processor reference. */
  const cleanupStream = useCallback(() => {
    if (rafRef.current !== null) {
      cancelAnimationFrame(rafRef.current)
      rafRef.current = null
    }
    processorRef.current = null
  }, [])

  // Cleanup on unmount — cancel RAF and stop client
  useEffect(() => {
    return () => {
      cleanupStream()
      clientRef.current?.stop()
    }
  }, [cleanupStream])

  /**
   * Wire a processor up to a stream that is about to arrive, and hand back the
   * three callbacks it needs. A new turn and a rejoined one render through
   * exactly the same path, because they are the same frames.
   */
  const openStream = useCallback((assistantId: string) => {
    setStreaming(true)
    cleanupStream()

    const processor = new StreamProcessor(assistantId)
    processorRef.current = processor

    const flushLoop = () => {
      const cmd = processor.flush()
      if (cmd) applyCommand(cmd)
      rafRef.current = requestAnimationFrame(flushLoop)
    }
    rafRef.current = requestAnimationFrame(flushLoop)

    return {
      onDelta: (delta: string, isAgent?: boolean) => processor.processDelta(delta, isAgent),
      onFrame: (frame: SSEFrame) => {
        try {
          const f = frame as { type?: string; chat_id?: unknown }
          if (f.type === 'chat_created' && typeof f.chat_id === 'string' && f.chat_id) {
            skipNextFetchRef.current = true
            // Adopt the new id on the ref SYNCHRONOUSLY, before the parent's state
            // update re-renders us: a background delegation started later in this
            // same turn pushes its chip over the socket tagged with this id, and
            // the socket listener matches on the ref. Without this the first chip
            // push would be dropped until a reload (or an extra fetch).
            chatIdRef.current = f.chat_id
            onChatCreatedRef.current?.(f.chat_id)
            return
          }
          applyCommands(processor.processFrame(frame))
        } catch (err) {
          console.error('[useChatStream] Frame processing error:', err)
          setActivity({ kind: 'idle' })
        }
      },
      onEnd: () => {
        cleanupStream()
        // finalize emits set_streaming(false), which frees the lane and, through
        // setStreaming, drains any completion nudge that arrived while this stream
        // ran (Mode C). No polling: the drain reacts to the lane clearing.
        applyCommands(processor.finalize())
      },
    }
  }, [applyCommand, applyCommands, cleanupStream, setStreaming])

  /**
   * Rejoin a turn that is still running.
   *
   * The answer kept being written while this page was away (a reload, a closed
   * laptop, a dropped connection). It asks for what it missed and follows the
   * rest live. When nothing is in flight, this does nothing at all, which is
   * the ordinary case.
   */
  const rejoin = useCallback(async (opts?: { fromStart?: boolean; replay?: boolean; run?: string }): Promise<boolean> => {
    const client = clientRef.current
    if (!client || isStreamingRef.current) return false

    // A server-initiated completion turn (Mode C) is a DIFFERENT run than the one
    // this client last streamed, and frame indices are per-run: without resetting,
    // attach would ask for frames after a stale index and miss the narration.
    if (opts?.fromStart) client.resetIndex()

    // The bubble goes up first, because the frames that follow are written
    // into it. If there turns out to be nothing to rejoin, it comes back down.
    const assistantId = nanoid()
    setMessages(prev => [...prev, { id: assistantId, role: 'assistant', content: '', isStreaming: true }])

    const { onDelta, onFrame, onEnd } = openStream(assistantId)

    // replay: a nudged completion may have finished in the moment before this
    // request lands, so ask for its frames even if the run is already done. run:
    // attach to the exact run the nudge named, so simultaneous completions do not
    // collapse onto the latest.
    const outcome = await client.attach(onDelta, onFrame, onEnd, 0, opts?.replay, opts?.run)
    traceCompletion('attach result', { run: opts?.run, replay: opts?.replay, outcome })
    if (outcome === 'attached') return true

    // Interrupted: this client stopped its own request. It says NOTHING about
    // the run, whose answer is saved and waiting, so the caller puts it back and
    // tries again rather than treating the interruption as an absence. Dropping
    // it here is precisely how a narration the server had already written never
    // reached the person until they reloaded.
    //
    // And the bubble only comes back down if it is still EMPTY. The outcome
    // above describes the REQUEST, not what arrived: an attach can paint an
    // answer and then fail, and removing it by id then destroys the only copy on
    // screen. Somebody watched a report arrive and watched it vanish a moment
    // later, which is the same lesson as the paragraph above, one step further
    // along.
    setMessages(prev => dropEmptyPlaceholder(prev, assistantId))
    cleanupStream()
    setStreaming(false)
    setActivity({ kind: 'idle' })
    return outcome !== 'interrupted'
  }, [openStream, cleanupStream, setStreaming])

  rejoinRef.current = rejoin

  // Drain queued completion nudges (Mode C), one at a time. It is purely
  // event-driven, never a poll: it is called when a nudge is queued (onTurn) and
  // when a stream ends (setStreaming(false) frees the lane). Each call attaches to
  // at most ONE run and hands control back; that run's own end re-invokes it for
  // the next. So completions never overlap a live stream or one another, and none
  // is collapsed onto the latest. drainingRef keeps the single in-flight attach
  // from being started twice.
  const drainCompletions = useCallback(() => {
    // A stream owns the lane, or an attach is already in flight: do nothing. The
    // end of whatever is streaming will call us again.
    if (drainingRef.current || isStreamingRef.current) {
      traceCompletion('drain deferred', {
        draining: drainingRef.current, streaming: isStreamingRef.current,
        queueLen: pendingRunsRef.current.length,
      })
      return
    }
    const run = pendingRunsRef.current.shift()
    if (run === undefined) return
    traceCompletion('drain → attaching', { run })
    drainingRef.current = true
    const next = (settled: boolean) => {
      if (!settled) {
        // Nothing was heard and the run is still out there. Back on the front of
        // the queue, so it is the next thing tried rather than the last.
        traceCompletion('re-queued after an interrupted attach', { run })
        pendingRunsRef.current.unshift(run)
      }
      drainingRef.current = false
      drainCompletionsRef.current?.()
    }
    const attaching = rejoinRef.current?.({ fromStart: true, replay: true, run })
    if (attaching) void attaching.then(next, () => next(false))
    else next(true)
  }, [])
  drainCompletionsRef.current = () => { drainCompletions() }

  // React to the socket's background-delegation pushes (Mode C). A `delegation`
  // event moves this conversation's chips (and surfaces the card when an agent
  // stops for approval); a `turn` event means a completion turn is streaming, so
  // we attach and hear it. Both are scoped to THIS conversation by its public id.
  //
  // Registered ONCE and matched against the live chatId ref, deliberately not
  // re-subscribed per chatId: a brand-new chat resolves its id mid-stream (the
  // chat_created frame), and re-subscribing on that change would leave a gap in
  // which the agent's very first push (its card, or its completion turn) has
  // no listener and is lost, so the person only sees it on a manual reload.
  // The one place the card ref is updated: what is rendered decides it.
  useEffect(() => {
    showingCardRef.current = messages.some(
      m => m.confirmation && m.confirmation.status === 'pending',
    )
  }, [messages])

  useEffect(() => {
    const onDelegation = (e: Event) => {
      const detail = (e as CustomEvent).detail as (Delegation & { chat_uid?: string }) | undefined
      if (!detail || !chatIdRef.current || detail.chat_uid !== chatIdRef.current) return
      setDelegations(prev => applyDelegationEvent(prev, detail))
    }
    // The server pushes the approval card itself (KB/27); render it directly.
    const onCard = (e: Event) => {
      const detail = (e as CustomEvent).detail as { chat_uid?: string; id?: string; confirmation?: ConfirmationRequest } | undefined
      if (!detail || !chatIdRef.current || detail.chat_uid !== chatIdRef.current) return
      if (detail.id && detail.confirmation) showCardRef.current?.(detail.id, detail.confirmation)
    }
    const onTurn = (e: Event) => {
      const detail = (e as CustomEvent).detail as { chat_uid?: string; run?: string } | undefined
      const matched = !!detail && !!chatIdRef.current && detail.chat_uid === chatIdRef.current
      traceCompletion('nudge received', {
        run: detail?.run, chat_uid: detail?.chat_uid, current: chatIdRef.current,
        matched, queueLen: pendingRunsRef.current.length,
      })
      if (!detail || !chatIdRef.current || detail.chat_uid !== chatIdRef.current) return
      // Queue this completion's run and let the drain loop attach to it when the
      // lane is free. Everything goes through the one loop, so nothing races the
      // in-flight stream or an earlier completion still rendering.
      pendingRunsRef.current.push(detail.run ?? '')
      void drainCompletionsRef.current?.()
    }
    // The socket is up. Anything pushed while it was NOT was pushed to nobody:
    // a card an agent raised, a chip that moved, a completion turn announced.
    // So the conversation is read again, which restores all three from the
    // record (a card lives in its park, chips in their rows).
    //
    // Skipped while a stream is running, because a history read replaces the
    // message list wholesale and would paint over an answer arriving right now.
    // The reconcile that matters is the one after a drop, and a tab that is
    // streaming did not have a drop.
    const onSocketReady = () => {
      if (!chatIdRef.current || isStreamingRef.current) return
      // NOT while a card is on screen, and that is the whole subtlety.
      //
      // A card's token is minted when the card is delivered and never stored in
      // the clear, so every delivery mints a fresh one and invalidates the last.
      // Re-reading the conversation IS a delivery. Do it while somebody is
      // looking at a card and their click is refused with a token rotated out
      // from under them, which is worse than the gap this closes.
      //
      // With no card shown there is nothing to invalidate and everything to
      // gain: whatever was raised while this tab had no socket is read from the
      // record it was written to.
      if (showingCardRef.current) return
      void loadHistoryRef.current?.(chatIdRef.current)
    }
    window.addEventListener(DELEGATION_EVENT, onDelegation)
    window.addEventListener(TURN_EVENT, onTurn)
    window.addEventListener(CARD_EVENT, onCard)
    window.addEventListener(SOCKET_READY_EVENT, onSocketReady)
    return () => {
      window.removeEventListener(DELEGATION_EVENT, onDelegation)
      window.removeEventListener(TURN_EVENT, onTurn)
      window.removeEventListener(CARD_EVENT, onCard)
      window.removeEventListener(SOCKET_READY_EVENT, onSocketReady)
    }
  }, [])

  // Opens the turn: the assistant's row, the lane, the request. Shared by
  // saying something now and by saying something that had to wait.
  const beginTurn = useCallback((text: string, fileIds?: string[]) => {
    const assistantId = nanoid()
    setMessages(prev => [
      ...prev,
      { id: assistantId, role: 'assistant', content: '', isStreaming: true },
    ])
    setStreaming(true)
    const { onDelta, onFrame, onEnd } = openStream(assistantId)
    clientRef.current!.start(text, onDelta, sessionKey, onFrame, onEnd, 0, fileIds)
  }, [openStream, sessionKey, setStreaming])

  /**
   * Says the next thing that was said while the assistant was busy.
   *
   * The same shape as the completion drain above and for the same reason: a
   * conversation answers ONE thing at a time. The server is explicit about it,
   * a second turn is refused with "this conversation is already answering", so
   * this is not a limit worth pretending around. It is event-driven rather than
   * polled, called when the lane frees, and it takes ONE message and hands
   * control back; that turn's own end brings it round again for the next.
   *
   * Completions go first. They narrate what an agent did in the background, and
   * hearing that before the answer to a new question is the order that makes
   * sense of both. If a completion takes the lane, this simply defers: it is
   * called again when that ends.
   */
  const drainSaid = useCallback(() => {
    if (isStreamingRef.current) return
    const next = waitingRef.current.shift()
    if (!next) return
    // It is being said now, so it stops being marked as waiting.
    setMessages(prev => prev.map(m => (m.id === next.id ? { ...m, waiting: false } : m)))
    beginTurn(next.text, next.fileIds)
  }, [beginTurn])

  drainSaidRef.current = drainSaid

  const sendMessage = useCallback((text: string, attachments?: FileAttachment[]) => {
    if (!text.trim()) return

    const userMessage: ChatMessage = {
      id: nanoid(),
      content: text.trim(),
      role: 'user',
      ...(attachments && attachments.length > 0 ? { attachments } : {}),
    }
    const fileIds = attachments?.map(f => f.id);

    // Said while the assistant is still answering.
    //
    // It used to be dropped on the floor here, silently: you typed, pressed
    // enter, and nothing at all happened. It now goes INTO the running turn,
    // where the loop folds it in at its next step and the model sees it before
    // deciding what to do next. So a correction arrives in time to change what
    // happens rather than after it has happened.
    //
    // Shown straight away, because it has been said. Marked as waiting until the
    // server confirms it was handed over, which is a moment, not a wait for the
    // answer.
    if (isStreamingRef.current) {
      setMessages(prev => [...prev, { ...userMessage, waiting: true }])
      void apiFetch(sayEndpoint, {
        method: 'POST',
        body: JSON.stringify({ chat_id: chatIdRef.current ?? '', text: text.trim() }),
      })
        .then((res) => {
          if (res.ok) {
            // Accepted, and NOT placed here. The turn says where it landed, with
            // a `said` frame, and that is what stops being marked as waiting.
            // Clearing it here as well put the message on screen twice: the
            // frame arrived, found nothing waiting to move, and added its own.
            return
          }
          // The turn ended between the key being pressed and this arriving,
          // which is a race nobody can close. Say it as a new one instead.
          waitingRef.current.push({ id: userMessage.id, text: text.trim(), fileIds })
          drainSaidRef.current?.()
        })
        .catch(() => {
          waitingRef.current.push({ id: userMessage.id, text: text.trim(), fileIds })
          drainSaidRef.current?.()
        })
      return
    }

    setMessages(prev => [...prev, userMessage])
    beginTurn(text, fileIds)
  }, [beginTurn])

  const stop = useCallback(() => {
    // Detaching no longer stops the turn: the run has a life of its own. So
    // stopping has to be said out loud, and this is where the user says it.
    void clientRef.current?.cancel()

    // Finalize processor so the last assistant message gets isStreaming: false
    const processor = processorRef.current
    if (processor) {
      applyCommands(processor.finalize())
    }

    cleanupStream()

    // Reset streaming state so the UI unlocks
    setStreaming(false)
    setActivity({ kind: 'idle' })
  }, [cleanupStream, applyCommands, setStreaming])
  const reset = useCallback(() => {
    // Abort stream + RAF without finalizing (we're wiping messages anyway)
    clientRef.current?.stop()
    cleanupStream()

    setMessages([])
    // A fresh conversation inherits none of the last one's queued completions.
    pendingRunsRef.current = []
    // Nor anything said into the last one that had not gone yet. A message
    // waiting its turn belongs to the conversation it was typed into, and
    // sending it into a different one would be putting words somewhere nobody
    // said them.
    waitingRef.current = []
    setStreaming(false)
    setActivity({ kind: 'idle' })

    // Remove the session key, so we start fresh
    // Skip the history fetch that the new session key would trigger
    skipNextFetchRef.current = true
    localStorage.removeItem('fx_chat_session')

    // If there is any reset endpoint, we have to fire it
    if (resetEndpoint) {
      (async () => {
        try {
          const resolvedExtra = await resolveDynamic(extraData);
          const resolvedToken = await resolveDynamic(token);
          const body: Record<string, any> = {
            k: sessionKey,
            e: resolvedExtra || {},
          };
          if (resolvedToken) body.t = String(resolvedToken);
          const res = await fetch(resetEndpoint, {
            mode: 'cors',
            method: 'POST',
            body: JSON.stringify(body),
          });

          if (!res.ok) throw new Error('Reset request failed on endpoint ' + resetEndpoint);
        } catch (err) {
          console.error('[useChatStream] Failed to reset chat', err);
        }
      })();
    }
  }, [cleanupStream, resetEndpoint, sessionKey, extraData, token, setStreaming])

  /**
   * Send a confirmation response (approve/reject) to the backend.
   *
   * Both Approve and Reject open a fresh SSE on the main /ai/agent endpoint
   * with `resume_token` + `resume_action`. The backend:
   *   - Approve → replays the parked tool with its stored args, appends the
   *     real tool_result to the paused conversation, continues with full
   *     tools so the agent narrates the outcome naturally.
   *   - Reject → synthesizes a {success:false, rejected:true} tool result so
   *     the agent acknowledges the rejection and continues conversationally
   *     instead of dead-ending the chat.
   *
   * The card flips to its final state immediately (optimistic) and the agent's
   * response streams underneath.
   *
   * Network abort: ChatStreamClient.start() always calls this.stop() first,
   * which aborts any in-flight AbortController via abortController.abort().
   * cleanupStream() here only cancels the RAF flush loop — the network abort
   * is the client's responsibility (see chat-stream-client.ts:23).
   */
  const respondToConfirmation = useCallback((token: string, action: 'approved' | 'rejected' | 'approved_all') => {
    // Optimistically update the card state. "Approve all" reads as an approval
    // on the card itself; it is the session that also changes, so the toggle
    // reflects auto right away.
    const cardStatus = action === 'approved_all' ? 'approved' : action;
    if (action === 'approved_all') setApprovalModeState('auto');
    setMessages(prev =>
      prev.map(msg =>
        msg.confirmation && msg.confirmation.token === token
          ? { ...msg, confirmation: { ...msg.confirmation, status: cardStatus } }
          : msg
      )
    );

    const assistantId = nanoid();
    setMessages(prev => [
      ...prev,
      { id: assistantId, role: 'assistant', content: '', isStreaming: true },
    ]);
    setStreaming(true);

    cleanupStream();

    const processor = new StreamProcessor(assistantId);
    processorRef.current = processor;

    const flushLoop = () => {
      const cmd = processor.flush();
      if (cmd) applyCommand(cmd);
      rafRef.current = requestAnimationFrame(flushLoop);
    };
    rafRef.current = requestAnimationFrame(flushLoop);

    clientRef.current!.start(
      '', // no new user prompt on resume
      (delta: string, isAgent?: boolean) => {
        processor.processDelta(delta, isAgent);
      },
      sessionKey,
      (frame: SSEFrame) => {
        try {
          // The server can REFUSE a resume: the card was already answered, or
          // it expired. That means the card we were holding is not the one the
          // conversation is waiting on, so the optimistic "Approved" we just
          // painted is a lie, and the queue behind it has not moved either
          // (the server only releases the next card on a claim that succeeded).
          //
          // Both are fixed the same way, by admitting the view is stale and
          // reading the conversation again: the answered card drops out (a card
          // lives in the park, never in the transcript) and the one actually
          // waiting comes back in its place. Which is what somebody clicking
          // approve wanted to see in the first place.
          if (frame.type === 'error' && (frame.code === 'expired' || frame.code === 'invalid_token')) {
            setMessages(prev =>
              prev.map(msg =>
                msg.confirmation && msg.confirmation.token === token
                  ? { ...msg, confirmation: { ...msg.confirmation, status: 'timeout' as const } }
                  : msg
              )
            );
            // Re-read the conversation, and put the refusal BACK afterwards.
            //
            // loadHistory replaces the message list wholesale, which would wipe
            // the error we are in the middle of rendering: the person would see
            // their card silently swapped for a different one with no word about
            // why, which is the same silence this whole path exists to end.
            //
            // Pinned to the chat this answer belongs to (`chat`, read now rather
            // than off the ref when the response lands), so switching
            // conversations mid-flight cannot paint one chat's transcript over
            // another's.
            const chat = chatIdRef.current || undefined;
            const why = frame.message;
            void loadHistoryRef.current?.(chat)
              .then(() => {
                if (chatIdRef.current !== (chat ?? null) && chatIdRef.current !== chat) return;
                setMessages(prev => [
                  ...prev,
                  { id: nanoid(), role: 'assistant', content: '', error: why },
                ]);
              })
              .catch(() => {
                /* the view stays as it is; processFrame below still renders the
                   refusal onto the assistant segment, so nothing is lost. */
              });
          }
          applyCommands(processor.processFrame(frame));
        } catch (err) {
          console.error('[useChatStream] Resume frame error:', err);
          setActivity({ kind: 'idle' });
        }
      },
      () => {
        cleanupStream();
        // finalize frees the lane through setStreaming, which drains any completion
        // nudge that arrived during the resume (a background task can finish while
        // the approve/reject stream is open). No explicit poke needed.
        applyCommands(processor.finalize());
      },
      0,
      undefined,
      token,
      action,
    );
  }, [applyCommand, applyCommands, cleanupStream, sessionKey, setStreaming]);

  /**
   * Fire a host-registered client tool. Pure fire-and-forget — the SSE stream
   * stays open, the AI already received its synthetic OK on the server side
   * and is continuing its narration. The handler runs in parallel; its return
   * value is intentionally discarded (the stream is one-way; sending a result
   * back to the model is out of scope for this design).
   *
   * Triggered by the `invoke_client_tool` StateCommand emitted on a
   * `client_tool_call` SSE frame.
   */
  const runClientTool = useCallback((payload: {
    name: string;
    friendly_name?: string;
    args: Record<string, unknown>;
    requires_confirm?: boolean;
  }) => {
    invokeLocalClientTool(payload, registryRef.current);
  }, []);

  // Bind the latest runClientTool to the ref so applyCommand can dispatch to it.
  runClientToolRef.current = runClientTool;

  return { messages, isStreaming, sendMessage, stop, reset, activity, respondToConfirmation, rejoin, approvalMode, setApprovalMode, delegations, cancelDelegation, accepts, hasOlder: older.more, loadingOlder, loadOlder }
}
