// lib/stream-processor.ts
// ─── Stateful SSE frame processor ───────────────────────────────────────────
// Extracts all mutable state from use-chat-stream's closure into a testable
// class. Produces declarative state updates that the React hook applies.

import { nanoid } from 'nanoid';
import type { SSEFrame, ChatMessage, ConfirmationRequest, AgentActivity, ToolChip, MessagePart } from './chat-types';

// ─── State Update Commands ──────────────────────────────────────────────────
// The processor never calls React setState directly.
// Instead, it returns commands that the hook interprets.

export type StateCommand =
  | { kind: 'set_streaming'; value: boolean }
  | { kind: 'set_activity'; value: AgentActivity }
  | { kind: 'update_messages'; updater: (prev: ChatMessage[]) => ChatMessage[] }
  | {
      kind: 'invoke_client_tool';
      payload: {
        name: string;
        friendly_name?: string;
        args: Record<string, unknown>;
        requires_confirm?: boolean;
      };
    };

// ─── Processor ──────────────────────────────────────────────────────────────

export class StreamProcessor {
  // Current assistant segment ID
  private segmentId: string;

  // Segment ID before a confirm_request parked it (for orphan clearing)
  private preConfirmSegmentId: string | null = null;

  // Ordered timeline parts (reasoning / text / tool) for the active segment, flushed
  // on RAF. Text + reasoning extend the last matching run; a finished tool is appended
  // as its own part, in position. `content` is kept in sync (concatenated text) for
  // search + the empty-content fallback.
  private parts: MessagePart[] = [];
  private partsDirty = false;

  // The agent the Gateway is currently delegating to, set between a
  // agent_start and its agent_end, so an agent's deltas can be
  // labelled with who produced them.
  private agentName: string | undefined = undefined;

  // Reasoning mode flag
  private isReasoningMode = false;

  // Reasoning source (captured from agent_start)
  private reasoningSourceModel: string | undefined;

  // Fallback: result content in case deltas were lost
  private lastResultContent = '';

  // ─── Activity state machine ─────────────────────────────────────────────
  private currentActivity: AgentActivity = { kind: 'running' };

  constructor(initialSegmentId: string) {
    this.segmentId = initialSegmentId;
  }

  // ─── Public API ─────────────────────────────────────────────────────────

  /** Get the current active segment ID */
  getSegmentId(): string {
    return this.segmentId;
  }

  /** Get the current activity state */
  getActivity(): AgentActivity {
    return this.currentActivity;
  }

  /** Process a content delta (from the onDelta callback) */
  processDelta(delta: string, isAgent = false): void {
    this.endReasoningMode();
    this.appendText(delta, isAgent);
  }

  /**
   * Append text to the current run. An agent's text (a continue-mode
   * delegation) becomes its own part, kept out of the Gateway's answer, so the
   * two are never one run of text.
   */
  private appendText(text: string, isAgent = false): void {
    if (!text) return;
    const last = this.parts[this.parts.length - 1];
    if (isAgent) {
      if (last && last.kind === 'agent' && last.agent === this.agentName) last.text += text;
      else this.parts.push({ kind: 'agent', text, agent: this.agentName });
    } else if (last && last.kind === 'text') {
      last.text += text;
    } else {
      this.parts.push({ kind: 'text', text });
    }
    this.partsDirty = true;
  }

  /** Append reasoning to the current run (extends the last reasoning part, or starts one). */
  private appendReasoning(text: string): void {
    if (!text) return;
    const last = this.parts[this.parts.length - 1];
    if (last && last.kind === 'reasoning') last.text += text;
    else this.parts.push({ kind: 'reasoning', text, model: this.reasoningSourceModel });
    this.partsDirty = true;
  }

  /** Append a finished tool as its own part, in position. */
  private appendTool(tool: ToolChip): void {
    this.parts.push({ kind: 'tool', tool });
    this.partsDirty = true;
  }

  /** Concatenated text of all text parts (search + empty-content fallback). */
  private textOfParts(): string {
    return this.parts.filter((p): p is { kind: 'text'; text: string } => p.kind === 'text').map((p) => p.text).join('');
  }

  /**
   * Process an SSE frame. Returns an array of state commands to apply.
   * The hook should iterate and apply each command.
   */
  processFrame(frame: SSEFrame): StateCommand[] {
    const cmds: StateCommand[] = [];

    switch (frame.type) {
      case 'heartbeat':
      case 'agent_start':
        cmds.push({ kind: 'set_streaming', value: true });
        if (frame.type === 'agent_start' && frame.message) {
          // Who the Gateway is delegating to, so the agent's deltas can be
          // labelled as its work rather than the Gateway's.
          //
          // Which MODEL it thinks with is not on this frame. It was read from
          // one here for as long as the frame was typed `any`, and no version
          // of the server has ever sent it: the attribution it fed was empty
          // every time. Whoever wants it back adds it to AgentMessage first.
          this.agentName = frame.message.name || frame.message.agent || undefined;
        }
        break;

      case 'agent_end':
        this.endReasoningMode();
        this.agentName = undefined;
        this.reasoningSourceModel = undefined;
        this.pushActivity(cmds, { kind: 'running' });
        break;

      case 'tool_preparing':
        this.endReasoningMode();
        cmds.push({ kind: 'set_streaming', value: true });
        // SAG sends the tool id and the words a person reads as separate
        // fields, so the indicator can show the label without guessing.
        this.pushActivity(cmds, {
          kind: 'tool',
          name: frame.message.friendly_name || frame.message.tool_name,
        });
        break;

      case 'reasoning_delta':
        if (frame.message && typeof frame.message === 'string') {
          if (!this.isReasoningMode) {
            this.isReasoningMode = true;
            this.pushActivity(cmds, { kind: 'reasoning' });
          }
          this.appendReasoning(frame.message);
        }
        break;

      case 'tool':
        this.endReasoningMode();
        cmds.push({ kind: 'set_streaming', value: true });
        // SAG sends a finished tool on this frame and announces the upcoming one
        // on tool_preparing, so there is no done flag to interpret: a `tool`
        // frame means the tool RAN, with its real duration and outcome.
        this.appendTool({
          name: frame.message.tool_name,
          friendly_name: frame.message.friendly_name || frame.message.tool_name,
          status: frame.message.status ?? null,
          duration_ms: frame.message.duration_ms ?? null,
          requested_approval: !!frame.message.requested_approval,
          // The stored call, when there is one to open. It arrives with the
          // frame that says the tool finished, so a row can be opened where it
          // happened; without it the row had to wait for a reload before the
          // arrow appeared, which is a reload to see what you just watched run.
          id: frame.message.id || undefined,
        });
        // The chip now shows it ran, so drop the "using X…" indicator.
        this.pushActivity(cmds, { kind: 'running' });
        break;

      case 'confirm_request':
        cmds.push(...this.handleConfirmRequest(frame));
        break;

      case 'said':
        return this.handleSaid(frame)

      case 'confirm_resolved':
        cmds.push(...this.handleConfirmResolved(frame));
        break;

      case 'client_tool_call':
        // Fire-and-forget — the AI helper already returned a synthetic OK
        // to the model and will continue narrating in the same stream. We
        // just surface a brief tool indicator and emit the invoke command
        // so the hook can run the registered handler in parallel.
        cmds.push({ kind: 'set_streaming', value: true });
        this.pushActivity(cmds, { kind: 'tool', name: frame.message.friendly_name || frame.message.name });
        // Local/client tools run fire-and-forget in the browser (no server post-frame),
        // so append the chip here in position (no duration — they complete instantly).
        this.appendTool({
          name: frame.message.name,
          friendly_name: frame.message.friendly_name || frame.message.name,
        });
        cmds.push({
          kind: 'invoke_client_tool',
          payload: {
            name: frame.message.name,
            friendly_name: frame.message.friendly_name,
            args: frame.message.args || {},
            requires_confirm: frame.message.requires_confirm,
          },
        });
        break;

      case 'result':
        this.endReasoningMode();
        if (frame.message && typeof frame.message === 'string') {
          this.lastResultContent = frame.message;
        }
        this.pushActivity(cmds, { kind: 'idle' });
        break;

      case 'delta':
        // Content deltas from the Gateway indicate it's actively responding.
        // Agent deltas should not flip the activity state — the agent's
        // work is tracked via its own tool_preparing / tool / reasoning events.
        if (!frame.is_agent && this.currentActivity.kind !== 'tool') {
          this.pushActivity(cmds, { kind: 'responding' });
        }
        break;

      case 'error': {
        this.endReasoningMode();
        const errorMsg = typeof frame.message === 'string' ? frame.message : 'An unexpected error occurred';
        const errorTargetId = this.segmentId;
        cmds.push({
          kind: 'update_messages',
          updater: (prev) =>
            prev.map((msg) =>
              msg.id === errorTargetId
                ? { ...msg, error: errorMsg }
                : msg
            ),
        });
        this.pushActivity(cmds, { kind: 'idle' });
        break;
      }

      default:
        // Unknown frame types — no activity change
        break;
    }

    return cmds;
  }

  /**
   * Flush buffered content + reasoning into the active segment.
   * Called on each requestAnimationFrame tick.
   * Returns a StateCommand if there's data to flush, or null.
   */
  flush(): StateCommand | null {
    if (!this.partsDirty) {
      return null;
    }

    // Snapshot the parts (clone the mutable text/reasoning runs so a later append to
    // the last part doesn't retro-change an already-rendered message object).
    const parts: MessagePart[] = this.parts.map((p) =>
      p.kind === 'tool' ? { kind: 'tool', tool: p.tool }
      : p.kind === 'reasoning' ? { kind: 'reasoning', text: p.text, model: p.model }
      : p.kind === 'agent' ? { kind: 'agent', text: p.text, agent: p.agent }
      : { kind: 'text', text: p.text }
    );
    const content = this.textOfParts();
    const targetId = this.segmentId;
    this.partsDirty = false;

    return {
      kind: 'update_messages',
      updater: (prev) =>
        prev.map((msg) =>
          msg.id === targetId ? { ...msg, content, parts } : msg
        ),
    };
  }

  /**
   * Finalize the stream. Flushes remaining buffers, applies result fallback,
   * removes empty trailing segments, marks all streaming messages as complete.
   * Returns all commands to apply.
   */
  finalize(): StateCommand[] {
    this.endReasoningMode();
    const cmds: StateCommand[] = [];

    // Flush remaining buffers
    const flushCmd = this.flush();
    if (flushCmd) cmds.push(flushCmd);

    // Finalize all assistant segments
    const activeId = this.segmentId;
    const fallback = this.lastResultContent;

    cmds.push({
      kind: 'update_messages',
      updater: (prev) => {
        const updated = prev.map((msg) => {
          if (msg.role !== 'assistant' || !msg.isStreaming) return msg;
          const hasContent = msg.content.trim().length > 0;
          // Apply fallback to the active segment if it has no text yet — also as a text
          // part so the timeline renderer shows it (after any tool parts).
          if (!hasContent && msg.id === activeId && fallback) {
            const parts: MessagePart[] = [...(msg.parts || []), { kind: 'text', text: fallback }];
            return { ...msg, content: fallback, parts, isStreaming: false };
          }
          return { ...msg, isStreaming: false };
        });

        // Remove trailing empty assistant messages (keep ones with errors, progress, or
        // any timeline parts — e.g. a tool-only step)
        while (updated.length > 0) {
          const last = updated[updated.length - 1];
          if (last.role === 'assistant' && !last.content.trim() && !last.reasoning && !last.error && !(last.parts && last.parts.length)) {
            updated.pop();
          } else {
            break;
          }
        }
        return updated;
      },
    });

    cmds.push({ kind: 'set_streaming', value: false });
    this.currentActivity = { kind: 'idle' };
    cmds.push({ kind: 'set_activity', value: { kind: 'idle' } });

    return cmds;
  }

  // ─── Private Helpers ────────────────────────────────────────────────────

  private endReasoningMode(): void {
    if (this.isReasoningMode) {
      this.isReasoningMode = false;
    }
  }

  /**
   * Push an activity change command only if activity actually changed.
   * Deduplicates: same kind (and same tool name / upload detail) → no-op.
   */
  private pushActivity(cmds: StateCommand[], next: AgentActivity): void {
    if (
      this.currentActivity.kind === next.kind &&
      (next.kind !== 'tool' || (this.currentActivity as { kind: 'tool'; name: string }).name === next.name) &&
      (next.kind !== 'uploading' || (this.currentActivity as { kind: 'uploading'; detail: string }).detail === next.detail)
    ) {
      return; // no change
    }
    this.currentActivity = next;
    cmds.push({ kind: 'set_activity', value: next });
  }

  private handleConfirmRequest(frame: SSEFrame & { type: 'confirm_request' }): StateCommand[] {
    this.endReasoningMode();
    const cmds: StateCommand[] = [];

    // Flush pending buffers to the current segment first
    const flushCmd = this.flush();
    if (flushCmd) cmds.push(flushCmd);

    const confirmId = 'confirm_' + frame.message.token;
    const confirmMsg: ChatMessage = {
      id: confirmId,
      content: '',
      role: 'confirm',
      confirmation: {
        token: frame.message.token,
        title: frame.message.title,
        description: frame.message.description,
        // No level declared is treated as a WRITE by the card, not as a read:
        // silence is not grounds for telling somebody it is harmless.
        severity: frame.message.severity,
        details: frame.message.details || {},
        status: 'pending',
      },
    };

    cmds.push({
      kind: 'update_messages',
      updater: (prev) => {
        if (prev.some((m) => m.id === confirmId)) return prev;
        return [...prev, confirmMsg];
      },
    });

    // Park the segment ID — stray deltas during confirmation wait won't
    // attach to the pre-confirm segment. The pre-confirm parts were already flushed;
    // reset so nothing carries onto the parked/next segment.
    this.preConfirmSegmentId = this.segmentId;
    this.segmentId = 'confirm_pending_' + nanoid();
    this.parts = [];
    this.agentName = undefined;
    this.partsDirty = false;
    this.pushActivity(cmds, { kind: 'confirming' });

    return cmds;
  }

  /**
   * Something the person said while this turn was running, at the point the turn
   * folded it in.
   *
   * The transcript turns a corner here and the screen has to turn with it. The
   * server closed the answer it had given, wrote these words after it, and
   * opened a new answer; a client that keeps appending to one assistant message
   * puts the words at the end instead, and the conversation then rearranges
   * itself on the next reload. Which is the transcript telling somebody they had
   * a different conversation from the one they remember.
   *
   * The message is usually already on screen: the composer put it there the
   * moment it was said, because it HAD been said. So this moves that one into
   * place rather than adding a second copy of it, and only adds one when there
   * is nothing to move (another tab, or a reattached stream).
   */
  private handleSaid(frame: SSEFrame & { type: 'said' }): StateCommand[] {
    const cmds: StateCommand[] = []
    const text = String(frame.message ?? '')
    const flush = this.flush()
    if (flush) cmds.push(flush)

    const newSegmentId = nanoid()
    const finished = this.segmentId
    cmds.push({
      kind: 'update_messages',
      updater: (prev) => {
        // The one the composer put up, if it is still waiting to be placed.
        const mine = prev.findIndex(
          (m) => m.role === 'user' && m.waiting && m.content === text,
        )
        const rest = mine === -1 ? [...prev] : prev.filter((_, i) => i !== mine)
        const said =
          mine === -1
            ? { id: nanoid(), role: 'user' as const, content: text }
            : { ...prev[mine], waiting: false }
        // The answer given so far is finished; the words come after it; what the
        // model says next is a new answer.
        return [
          ...rest.map((m) => (m.id === finished ? { ...m, isStreaming: false } : m)),
          said,
          { id: newSegmentId, role: 'assistant' as const, content: '', isStreaming: true },
        ]
      },
    })

    this.segmentId = newSegmentId
    this.parts = []
    this.agentName = undefined
    this.partsDirty = false
    return cmds
  }

  private handleConfirmResolved(frame: SSEFrame & { type: 'confirm_resolved' }): StateCommand[] {
    const cmds: StateCommand[] = [];
    const { token, status } = frame.message;
    this.preConfirmSegmentId = null;

    const newSegmentId = nanoid();

    cmds.push({
      kind: 'update_messages',
      updater: (prev) => {
        const updated = prev.map((msg) =>
          msg.confirmation && msg.confirmation.token === token
            ? { ...msg, confirmation: { ...msg.confirmation, status } as ConfirmationRequest }
            : msg
        );
        // Fresh assistant segment for the resumed continuation — its own timeline. The
        // pre-confirm segment keeps its parts (reasoning etc.) where they streamed.
        return [
          ...updated,
          { id: newSegmentId, role: 'assistant' as const, content: '', isStreaming: true },
        ];
      },
    });

    this.segmentId = newSegmentId;
    this.parts = [];
    this.agentName = undefined;
    this.partsDirty = false;
    this.pushActivity(cmds, { kind: 'running' });
    return cmds;
  }
}
