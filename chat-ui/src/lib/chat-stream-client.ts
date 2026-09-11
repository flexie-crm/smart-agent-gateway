import type { SSEFrame } from './chat-types';
import { resolveDynamic } from './utils';
import { apiFetch } from './api';
import { traceCompletion } from './completion-trace';
import { chosenFolder, machineDeviceId } from './machine-link';

export type ChatStreamHandler = (chunk: string, isAgent?: boolean) => void;
export type ChatStreamFrameHandler = (frame: SSEFrame) => void;
export type ChatStreamEndHandler = () => void;

/**
 * Snapshot of host-registered client tools posted with every turn — names,
 * descriptions, parameter schemas, requires_confirm. Functions / handlers
 * are NEVER included; they live in the host registry and are invoked locally.
 */
export interface ClientToolDef {
  name: string;
  description: string;
  parameters: Record<string, unknown>;
  friendly_name?: string;
  requires_confirm?: boolean;
}

// 'approved_all' approves this action AND switches the whole conversation to
// auto-approve for the rest of the session (the "allow the rest this session"
// choice). The client maps it to resume_action=approved + approve_all=true.
export type ResumeAction = 'approved' | 'rejected' | 'approved_all';

/** A token may be a literal string or a (sync/async) factory called per request. */
export type TokenInput = string | (() => string | Promise<string>) | null | undefined;

export type AttachOutcome = 'attached' | 'none' | 'interrupted';

export class ChatStreamClient {
  private streamEndpoint: string;
  // Where to rejoin a turn already in flight, and where to stop one. A turn no
  // longer belongs to the request that asked for it: hanging up detaches, and
  // stopping is something the user has to actually say.
  private attachEndpoint: string | null = null;
  private cancelEndpoint: string | null = null;
  // The last frame index this client has seen. It is what "I got this far" is
  // said with, and it is how a reconnect resumes mid-sentence.
  private lastIndex = -1;
  private token: TokenInput;
  private extraData: any | null = null;
  private chatId: string | null = null;
  // When true and no chatId is set yet, the next turn asks the server to create a
  // fresh chat (draft "New chat" -> first prompt). The server streams chat_created
  // back with the new uid.
  private pendingNewChat = false;
  // The uid the client picks for a draft's first turn. Sent with new_chat so the
  // server creates the chat idempotently — a retry reuses the same uid instead of
  // creating a duplicate. Generated once per draft, cleared once the chat exists.
  private pendingNewChatUid: string | null = null;
  private clientToolDefs: ClientToolDef[] | null = null;
  private abortController: AbortController | null = null;
  private retryTimeout: number | null = null;
  private maxRetries = 3;
  private retryDelay = 2000; // ms

  constructor(
    streamEndpoint: string,
    token?: TokenInput,
    extraData?: any,
    clientToolDefs?: ClientToolDef[],
    endpoints?: { attach?: string; cancel?: string }
  ) {
    this.streamEndpoint = streamEndpoint;
    this.token = token;
    this.extraData = extraData;
    this.clientToolDefs = clientToolDefs && clientToolDefs.length > 0 ? clientToolDefs : null;
    this.attachEndpoint = endpoints?.attach ?? null;
    this.cancelEndpoint = endpoints?.cancel ?? null;
  }

  /** Update the live client-tool snapshot without recreating the client. */
  setClientToolDefs(defs: ClientToolDef[] | null | undefined): void {
    this.clientToolDefs = defs && defs.length > 0 ? defs : null;
  }

  /** Opaque DB-backed chat id (uid) sent as `chat_id` every turn (multi-thread
   * history). Null keeps the legacy per-user Redis behaviour server-side. */
  setChatId(chatId: string | null | undefined): void {
    this.chatId = (typeof chatId === 'string' && chatId.length > 0) ? chatId : null;
    if (this.chatId) { this.pendingNewChatUid = null; }
  }

  /** Attaching to a DIFFERENT run than the one this client last streamed (a
   * server-initiated completion turn, Mode C) starts from the beginning: the
   * frame index is per-run, so a stale one from an earlier turn would skip the
   * new run's opening frames. Reset it before such an attach. */
  resetIndex(): void {
    this.lastIndex = -1;
  }

  /** Draft mode: ask the server to create a fresh chat on the next turn (only
   * used when there is no chatId yet). */
  setPendingNewChat(v: boolean): void {
    this.pendingNewChat = !!v;
    if (!this.pendingNewChat) { this.pendingNewChatUid = null; }
  }

  async start(
    prompt: string,
    onDelta: ChatStreamHandler,
    sessionKey: string,
    onFrame?: ChatStreamFrameHandler,
    onEnd?: ChatStreamEndHandler,
    retry = 0,
    fileIds?: string[],
    resumeToken?: string,
    resumeAction?: ResumeAction
  ) {
    this.stop();
    this.abortController = new AbortController();
    // A new turn is a new stream of frames, numbered from the start.
    this.lastIndex = -1;

    try {
      // SAG: the request says what it means. The CRM used one-letter keys
      // (q, k, e, t) that a reader had to look up; a prompt is a prompt.
      const body: Record<string, any> = { prompt };
      // Which of the person's computers this was typed on, when it was typed
      // on one. A tool that reaches this machine's own network has to reach
      // THIS machine, and the server cannot work that out: somebody may be
      // signed in on a laptop and a desktop at once. Empty in a browser, where
      // there is no machine to reach.
      const deviceId = await machineDeviceId();
      if (deviceId) body.device_id = deviceId;
      // And which folder on it they gave the assistant to work in. Sent for the
      // same reason as the device and read every turn rather than once: it is
      // chosen in the application, changed there whenever they like, and the
      // server has no way to ask. Without it an assistant holding the file
      // tools answers "I cannot see your project" for a folder it could have
      // read from the start.
      const folder = await chosenFolder();
      if (folder) body.working_folder = folder;
      // Where the person is, by name ("Europe/Tirane"), which the server cannot
      // work out either: it runs in UTC and would otherwise tell somebody it is
      // nine in the morning while their own screen says eleven. The NAME rather
      // than the offset, because the name is what survives a daylight-saving
      // change. Sent every turn and cheap; the server keeps it against the
      // person so a background task finishing at midnight is in their evening
      // too.
      const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
      if (zone) body.timezone = zone;
      if (this.chatId != null) {
        // The id is opaque: the server assigns it and the client only ever
        // repeats it back. Nothing here knows or cares what it is made of.
        body.chat_id = this.chatId;
      }
      const resolvedExtra = await resolveDynamic(this.extraData);
      if (resolvedExtra && typeof resolvedExtra === 'object') {
        // Host context. model_id lives here until workflows choose the model
        // server-side, which is where that decision belongs.
        Object.assign(body, resolvedExtra);
      }
      if (fileIds && fileIds.length > 0) body.files = fileIds;
      // Live host-page client-tool registry — posted every turn so the server
      // can materialise the same tools the model sees.
      if (this.clientToolDefs && this.clientToolDefs.length > 0) {
        body.ct = this.clientToolDefs;
      }
      // Confirmation resume: backend branches on resume_token. resume_action
      // selects approve (replay tool) vs reject (synthesize rejection).
      if (resumeToken) {
        body.resume_token = resumeToken;
        if (resumeAction === 'approved_all') {
          // Approve this one and stop asking for the rest of the session.
          body.resume_action = 'approved';
          body.approve_all = true;
        } else {
          body.resume_action = resumeAction || 'approved';
        }
      }

      // SAG: bearer auth, and a token that refreshes itself, so an access
      // token expiring mid-conversation is invisible rather than a lost turn.
      const res = await apiFetch(this.streamEndpoint, {
        method: "POST",
        body: JSON.stringify(body),
        signal: this.abortController.signal,
      });

      if (!res.ok || !res.body) {
        const err: Error & { statusCode?: number; body?: unknown } = new Error(`Stream failed (${res.status})`);
        err.statusCode = res.status;
        // The refusal itself, kept. A 4xx here is the server ANSWERING (this
        // approval expired, this token is no longer awaiting a decision), and
        // throwing away what it said leaves the caller unable to tell a refusal
        // from a network that went quiet.
        err.body = await res.json().catch(() => null);
        throw err;
      }

      await this.consume(res, onDelta, onFrame, onEnd);
    } catch (err: any) {
      if (err?.name === 'AbortError') {
        return;
      }

      // Permanent error → stop. Retrying a 4xx (expired token, missing tool,
      // unauthenticated) just spam-fires the same failing request.
      const status: number | undefined = err?.statusCode;
      if (status && status >= 400 && status < 500) {
        console.error('[ChatStreamClient] Permanent error, not retrying:', status, err?.message);
        // SAID, not swallowed. This used to log to the console and call onEnd,
        // so a person who clicked Approve on a card the server had already
        // resolved watched an empty assistant bubble appear and nothing else
        // happen at all. The refusal goes through the ordinary frame pipeline
        // as an error frame, which every caller already renders.
        const body = err?.body as { error?: string; message?: string } | null;
        onFrame?.({
          type: 'error',
          final: true,
          message: body?.message || 'This could not be completed.',
          code: body?.error,
        });
        onEnd?.();
        this.stop();
        return;
      }

      console.warn('Stream connection error:', err);

      // The turn is still running on the server: it stopped needing us the
      // moment it started. So a broken connection is REJOINED, never re-asked.
      // Asking again would send the prompt a second time and produce a second
      // answer to a question that is already being answered.
      if (retry < this.maxRetries) {
        this.retryTimeout = window.setTimeout(() => {
          void this.attach(onDelta, onFrame, onEnd, retry + 1);
        }, this.retryDelay);
      } else {
        console.error('[ChatStreamClient] Max retries exhausted, ending stream');
        onEnd?.();
        this.stop();
      }
    }
  }

  /**
   * Rejoin a turn already in flight.
   *
   * The answer kept being written while nobody was listening, so this asks for
   * everything after the last frame we saw and then follows the rest live. A
   * page that has just loaded has seen nothing, and gets the turn whole.
   *
   * Returns false when there is nothing to rejoin, which is the ordinary case:
   * most conversations are not mid-answer when you open them.
   */
  /**
   * What came of an attach.
   *
   * Three answers rather than a boolean, because two of the three used to be
   * the same `false` and they mean opposite things. `none` is the server saying
   * there is nothing in this conversation to hear. `interrupted` is this client
   * stopping its own request, which says nothing at all about the run: it is
   * still there and its answer is still saved, so the caller must try again
   * rather than conclude there was nothing to hear.
   */
  async attach(
    onDelta: ChatStreamHandler,
    onFrame?: ChatStreamFrameHandler,
    onEnd?: ChatStreamEndHandler,
    retry = 0,
    // replay asks the server for a run's frames even if it has just finished:
    // used when attaching to a nudged background completion, whose fast narration
    // can end before this request lands. Without it the person had to reload.
    replay = false,
    // run names a specific run to attach to (a completion nudge carries its run),
    // so several completions finishing at once are each heard on their own run
    // rather than all resolving to the latest and losing the earlier ones.
    run = ''
  ): Promise<AttachOutcome> {
    if (!this.attachEndpoint || !this.chatId) return 'none';

    this.stop();
    this.abortController = new AbortController();

    try {
      const res = await apiFetch(this.attachEndpoint, {
        method: 'POST',
        body: JSON.stringify({ chat_id: this.chatId, after: this.lastIndex, replay, run }),
        signal: this.abortController.signal,
      });

      // 204: nothing is happening in this conversation.
      if (res.status === 204) {
        if (run) traceCompletion('attach got 204 (server had no run)', { run, after: this.lastIndex, replay });
        this.stop();
        return 'none';
      }
      if (!res.ok || !res.body) {
        const err: Error & { statusCode?: number } = new Error(`Attach failed (${res.status})`);
        err.statusCode = res.status;
        throw err;
      }

      await this.consume(res, onDelta, onFrame, onEnd);
      return 'attached';
    } catch (err: any) {
      if (err?.name === 'AbortError') {
        // INTERRUPTED, not empty. Somebody stopped this request; the run it was
        // going to read is still there and still has its answer. Saying "none"
        // here is how a saved result became a result nobody ever saw.
        if (run) traceCompletion('attach aborted', { run, retry });
        return 'interrupted';
      }

      const status: number | undefined = err?.statusCode;
      if ((status && status >= 400 && status < 500) || retry >= this.maxRetries) {
        if (run) traceCompletion('attach failed, giving up', { run, status, retry });
        onEnd?.();
        this.stop();
        return 'none';
      }
      if (run) traceCompletion('attach errored, retrying', { run, status, retry });
      this.retryTimeout = window.setTimeout(() => {
        void this.attach(onDelta, onFrame, onEnd, retry + 1, replay, run);
      }, this.retryDelay);
      return 'attached';
    }
  }

  /**
   * Stop the turn.
   *
   * Detaching does not stop anything any more, which is the point: a network
   * blip is not a decision. So stopping has to be said, and this says it.
   */
  async cancel(): Promise<void> {
    this.stop();
    if (!this.cancelEndpoint || !this.chatId) return;
    try {
      await apiFetch(this.cancelEndpoint, {
        method: 'POST',
        body: JSON.stringify({ chat_id: this.chatId }),
      });
    } catch (err) {
      console.warn('[ChatStreamClient] Could not stop the turn:', err);
    }
  }

  /** Read an SSE response until the turn ends or we are detached. */
  private async consume(
    res: Response,
    onDelta: ChatStreamHandler,
    onFrame?: ChatStreamFrameHandler,
    onEnd?: ChatStreamEndHandler
  ): Promise<void> {
    const reader = res.body!.getReader();
    const decoder = new TextDecoder();

    let buffer = '';

    while (true) {
      const { value, done } = await reader.read();
      if (done) break;

      buffer += decoder.decode(value, { stream: true });

      let parts = buffer.split(/\n\n/);
      buffer = parts.pop() || ''; // keep incomplete fragment

      for (const part of parts) {
        const match = part.match(/^data:\s*(.*)$/m);
        if (!match) continue;

        const jsonStr = match[1].trim();
        if (!jsonStr) continue;

        try {
          const data = JSON.parse(jsonStr);

          // How far we have got. A reconnect says this number, and is given
          // exactly what came after it: no gap, and no word twice.
          if (typeof data.index === 'number') this.lastIndex = data.index;

          if (onFrame) onFrame(data);

          // Adopt a freshly-created chat IMMEDIATELY, so a reconnect rejoins
          // this conversation instead of asking for another one.
          if (data.type === 'chat_created' && typeof data.chat_id === 'string' && data.chat_id) {
            this.chatId = data.chat_id;
            this.pendingNewChat = false;
            this.pendingNewChatUid = null;
          }

          if (data.type === 'delta') onDelta(data.message, !!data.is_agent);

          if (data.final || data.type === 'result') {
            onEnd?.();
            this.stop();
            return;
          }
        } catch {
          // Unparseable data line: the server only emits JSON on SSE data
          // lines, so anything else is not ours to render.
        }
      }
    }

    onEnd?.();
  }

  stop() {
    if (this.abortController) {
      this.abortController.abort();
      this.abortController = null;
    }
    if (this.retryTimeout) {
      clearTimeout(this.retryTimeout);
      this.retryTimeout = null;
    }
  }
}