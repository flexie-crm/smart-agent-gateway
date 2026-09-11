import { describe, it, expect, vi, beforeEach } from 'vitest';
import { StreamProcessor, type StateCommand } from '../stream-processor';
import type { ChatMessage, SSEFrame, AgentActivity } from '../chat-types';

// ─── Test Helpers ───────────────────────────────────────────────────────────

/** Apply an array of state commands to a message array, simulating React state */
function applyUpdates(messages: ChatMessage[], cmds: StateCommand[]): ChatMessage[] {
  let result = messages;
  for (const cmd of cmds) {
    if (cmd.kind === 'update_messages') {
      result = cmd.updater(result);
    }
  }
  return result;
}

/** Extract streaming/activity commands from a command list */
function extractFlags(cmds: StateCommand[]) {
  let streaming: boolean | undefined;
  let activity: AgentActivity | undefined;
  for (const cmd of cmds) {
    if (cmd.kind === 'set_streaming') streaming = cmd.value;
    if (cmd.kind === 'set_activity') activity = cmd.value;
  }
  return { streaming, activity };
}

/** Create a bare assistant message array for testing */
function makeMessages(segId: string, content = '', reasoning?: string): ChatMessage[] {
  return [{ id: segId, role: 'assistant', content, isStreaming: true, reasoning }];
}

/** Reasoning text from the parts timeline (parts model). */
function reasoningOf(msg: ChatMessage): string {
  return (msg.parts || []).filter((p) => p.kind === 'reasoning').map((p) => (p as { text: string }).text).join('');
}
/** Ordered part kinds of a message. */
function partKinds(msg: ChatMessage): string[] {
  return (msg.parts || []).map((p) => p.kind);
}
/** A finished-tool post-execution frame. */
// SAG: a `tool` frame means the tool FINISHED. Announcing one is a separate
// frame (tool_preparing), because announcing and reporting are different
// events and overloading one frame with a `done` flag hid that.
function toolDone(over: Record<string, unknown>): SSEFrame {
  return { type: 'tool', message: { ...over } } as SSEFrame;
}

// ═══════════════════════════════════════════════════════════════════════════
// Construction
// ═══════════════════════════════════════════════════════════════════════════

describe('StreamProcessor construction', () => {
  it('tracks the initial segment ID', () => {
    const p = new StreamProcessor('seg-1');
    expect(p.getSegmentId()).toBe('seg-1');
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// processDelta + flush
// ═══════════════════════════════════════════════════════════════════════════

describe('processDelta + flush', () => {
  it('buffers content deltas and flushes to the active segment', () => {
    const p = new StreamProcessor('seg-1');
    p.processDelta('Hello ');
    p.processDelta('World');

    const cmd = p.flush();
    expect(cmd).not.toBeNull();

    const msgs = applyUpdates(makeMessages('seg-1'), [cmd!]);
    expect(msgs[0].content).toBe('Hello World');
  });

  it('returns null when nothing is buffered', () => {
    const p = new StreamProcessor('seg-1');
    expect(p.flush()).toBeNull();
  });

  it('clears the buffer after flush', () => {
    const p = new StreamProcessor('seg-1');
    p.processDelta('data');
    p.flush();
    expect(p.flush()).toBeNull();
  });

  it('flush sets content from the processor parts (processor owns its segment)', () => {
    const p = new StreamProcessor('seg-1');
    const msgs = makeMessages('seg-1');

    p.processDelta('Hello ');
    p.processDelta('World');
    const result = applyUpdates(msgs, [p.flush()!]);
    expect(result[0].content).toBe('Hello World');
    expect(partKinds(result[0])).toEqual(['text']);
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// delegation: an agent's deltas are kept apart from the Gateway's answer
// ═══════════════════════════════════════════════════════════════════════════

describe('agent deltas', () => {
  it("keeps an agent's words out of the Gateway's answer", () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    p.processDelta('Let me check.'); // the Gateway
    p.processFrame({ type: 'agent_start', message: { agent: 'finance', name: 'Finance' } } as SSEFrame);
    p.processDelta('Acme is high risk.', true); // the agent
    p.processFrame({ type: 'agent_end' } as SSEFrame);
    p.processDelta(' Proceed with care.'); // the Gateway again

    msgs = applyUpdates(msgs, [p.flush()!]);

    // Three runs, the agent's between the Gateway's two.
    expect(partKinds(msgs[0])).toEqual(['text', 'agent', 'text']);
    // The agent's text is its own part, labelled, not the Gateway's answer.
    const sub = (msgs[0].parts || []).find((part) => part.kind === 'agent') as {
      text: string;
      agent?: string;
    };
    expect(sub.text).toBe('Acme is high risk.');
    expect(sub.agent).toBe('Finance');
    expect(msgs[0].content).toBe('Let me check. Proceed with care.');
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// processFrame: heartbeat / agent_start
// ═══════════════════════════════════════════════════════════════════════════

describe('processFrame: heartbeat / agent_start', () => {
  it('sets streaming=true on heartbeat', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.processFrame({ type: 'heartbeat', final: false } as SSEFrame);
    expect(extractFlags(cmds).streaming).toBe(true);
  });

  it('sets streaming=true on agent_start', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.processFrame({ type: 'agent_start', final: false } as SSEFrame);
    expect(extractFlags(cmds).streaming).toBe(true);
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// processFrame: tool_preparing
// ═══════════════════════════════════════════════════════════════════════════

describe('processFrame: tool_preparing', () => {
  // SAG: the frame carries the tool id AND the words a person reads, as
  // separate fields. The indicator shows the label, never the id.
  it('shows the friendly name while a tool runs', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.processFrame({
      type: 'tool_preparing',
      message: { tool_name: 'flexie_search', friendly_name: 'Flexie Search' },
    } as SSEFrame);
    expect(extractFlags(cmds).activity).toEqual({ kind: 'tool', name: 'Flexie Search' });
  });

  it('falls back to the tool id when a tool has no friendly name', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.processFrame({
      type: 'tool_preparing',
      message: { tool_name: 'flexie_search' },
    } as SSEFrame);
    expect(extractFlags(cmds).activity).toEqual({ kind: 'tool', name: 'flexie_search' });
  });

  it('sets streaming=true', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.processFrame({
      type: 'tool_preparing',
      message: { tool_name: 'Query' },
      final: false,
    } as SSEFrame);
    expect(extractFlags(cmds).streaming).toBe(true);
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// processFrame: file_processing
// ═══════════════════════════════════════════════════════════════════════════


// ═══════════════════════════════════════════════════════════════════════════
// processFrame: reasoning_delta
// ═══════════════════════════════════════════════════════════════════════════

describe('processFrame: reasoning_delta', () => {
  it('accumulates reasoning into a reasoning part', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame({ type: 'reasoning_delta', message: 'Think ' } as SSEFrame);
    p.processFrame({ type: 'reasoning_delta', message: 'more' } as SSEFrame);

    const cmd = p.flush()!;
    const msgs = applyUpdates(makeMessages('seg-1'), [cmd]);
    expect(reasoningOf(msgs[0])).toBe('Think more');
    expect(partKinds(msgs[0])).toEqual(['reasoning']);
  });

  it('emits reasoning activity on first reasoning delta', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.processFrame({ type: 'reasoning_delta', message: 'hmm' } as SSEFrame);
    expect(extractFlags(cmds).activity).toEqual({ kind: 'reasoning' });
  });

  it('does not emit activity on subsequent reasoning deltas', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame({ type: 'reasoning_delta', message: 'A' } as SSEFrame);
    const cmds2 = p.processFrame({ type: 'reasoning_delta', message: 'B' } as SSEFrame);
    // Second delta doesn't emit set_activity because already in reasoning mode
    const activityCmds = cmds2.filter((c) => c.kind === 'set_activity');
    expect(activityCmds).toHaveLength(0);
  });

  it('switches from reasoning to content when delta arrives (ordered parts)', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame({ type: 'reasoning_delta', message: 'think' } as SSEFrame);
    p.processDelta('Real content');

    const cmd = p.flush()!;
    const msgs = applyUpdates(makeMessages('seg-1'), [cmd]);
    expect(msgs[0].content).toBe('Real content');
    expect(reasoningOf(msgs[0])).toBe('think');
    expect(partKinds(msgs[0])).toEqual(['reasoning', 'text']);
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// processFrame: tool
// ═══════════════════════════════════════════════════════════════════════════

describe('processFrame: tool', () => {
  it('appends the finished tool where it happened, with its real cost', () => {
    const p = new StreamProcessor('seg-1');
    p.processDelta('before');
    p.processFrame(toolDone({
      tool_name: 'current_time',
      friendly_name: 'Current time',
      duration_ms: 42,
      status: 'completed',
    }));
    const msgs = applyUpdates(makeMessages('seg-1'), [p.flush()!]);

    expect(partKinds(msgs[0])).toEqual(['text', 'tool']);
    const toolPart = msgs[0].parts![1];
    if (toolPart.kind === 'tool') {
      expect(toolPart.tool.name).toBe('current_time');
      expect(toolPart.tool.friendly_name).toBe('Current time');
      expect(toolPart.tool.duration_ms).toBe(42);
      expect(toolPart.tool.status).toBe('completed');
    }
    expect(msgs[0].content).toBe('before'); // the tool is its own part, not text
  });

  // The audit is visible in the conversation, not only in a log.
  it('records that a tool went through an approval', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame(toolDone({
      tool_name: 'set_model_status',
      friendly_name: 'Enable or disable an AI model',
      status: 'completed',
      duration_ms: 6,
      requested_approval: true,
    }));
    const msgs = applyUpdates(makeMessages('seg-1'), [p.flush()!]);

    const toolPart = msgs[0].parts![0];
    if (toolPart.kind === 'tool') {
      expect(toolPart.tool.requested_approval).toBe(true);
    }
  });

  // Openable where it happened.
  //
  // The id used to arrive only with a conversation read back from the
  // database, so a tool you had just watched run had no arrow on it and the
  // way to see what it did was to reload the page.
  it('can be opened the moment it finishes', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame(toolDone({
      tool_name: 'terminal',
      friendly_name: 'Terminal',
      status: 'completed',
      duration_ms: 26,
      id: '4181',
    }));
    const msgs = applyUpdates(makeMessages('seg-1'), [p.flush()!]);

    const toolPart = msgs[0].parts![0];
    if (toolPart.kind === 'tool') {
      expect(toolPart.tool.id).toBe('4181');
    }
  });

  // And the assistant's own wiring sends none, so there is nothing to open
  // rather than a panel that refuses.
  it('has nothing to open when the server sent no id', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame(toolDone({ tool_name: 'delegate', friendly_name: 'Delegate to an agent', status: 'completed' }));
    const msgs = applyUpdates(makeMessages('seg-1'), [p.flush()!]);

    const toolPart = msgs[0].parts![0];
    if (toolPart.kind === 'tool') {
      expect(toolPart.tool.id).toBeUndefined();
    }
  });

  it('keeps a failed tool visible rather than hiding it', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame(toolDone({ tool_name: 'broken', status: 'failed' }));
    const msgs = applyUpdates(makeMessages('seg-1'), [p.flush()!]);

    const toolPart = msgs[0].parts![0];
    if (toolPart.kind === 'tool') {
      expect(toolPart.tool.status).toBe('failed');
    }
  });

  // Announcing a tool is not the same event as reporting it: the announcement
  // drives the indicator and adds nothing to the timeline.
  it('an announced tool adds no part, only the indicator', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame({ type: 'tool_preparing', message: { tool_name: 'x', friendly_name: 'X' } } as SSEFrame);
    expect(p.flush()).toBeNull();
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// processFrame: result
// ═══════════════════════════════════════════════════════════════════════════

describe('processFrame: result', () => {
  it('captures result content as fallback', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame({ type: 'result', message: 'Final answer', final: true } as SSEFrame);

    // If we finalize with empty content, fallback should apply
    const cmds = p.finalize();
    const msgs = applyUpdates(makeMessages('seg-1'), cmds);
    expect(msgs[0].content).toBe('Final answer');
  });

  it('sets activity to idle', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.processFrame({ type: 'result', message: 'done', final: true } as SSEFrame);
    expect(extractFlags(cmds).activity).toEqual({ kind: 'idle' });
  });

  it('does not overwrite content when deltas already arrived', () => {
    const p = new StreamProcessor('seg-1');
    p.processDelta('I have deltas');
    p.processFrame({ type: 'result', message: 'Fallback content', final: true } as SSEFrame);

    const cmds = p.finalize();
    const msgs = applyUpdates(makeMessages('seg-1'), cmds);
    // Content from deltas takes priority, fallback is NOT applied when content exists
    expect(msgs[0].content).toBe('I have deltas');
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// processFrame: confirm_request
// ═══════════════════════════════════════════════════════════════════════════

describe('processFrame: confirm_request', () => {
  it('creates a confirmation message', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.processFrame({
      type: 'confirm_request',
      message: {
        token: 'abc123',
        title: 'Delete Workflow',
        description: 'Are you sure?',
        severity: 'destructive_action',
        details: { ID: '42' },
      },
    } as SSEFrame);

    const msgs = applyUpdates(makeMessages('seg-1'), cmds);
    const confirm = msgs.find((m) => m.role === 'confirm');
    expect(confirm).toBeDefined();
    expect(confirm!.confirmation!.token).toBe('abc123');
    expect(confirm!.confirmation!.severity).toBe('destructive_action');
    expect(confirm!.confirmation!.status).toBe('pending');
  });

  it('does not duplicate confirmation for same token', () => {
    const p = new StreamProcessor('seg-1');
    const frame = {
      type: 'confirm_request',
      message: { token: 'abc123', title: 'X', description: 'Y' },
    } as SSEFrame;

    const cmds1 = p.processFrame(frame);
    let msgs = applyUpdates(makeMessages('seg-1'), cmds1);
    const cmds2 = p.processFrame(frame);
    msgs = applyUpdates(msgs, cmds2);

    const confirms = msgs.filter((m) => m.role === 'confirm');
    expect(confirms).toHaveLength(1);
  });

  it('parks segment ID to avoid stray deltas attaching to pre-confirm segment', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame({
      type: 'confirm_request',
      message: { token: 'tok', title: 'X', description: 'Y' },
    } as SSEFrame);

    expect(p.getSegmentId()).not.toBe('seg-1');
    expect(p.getSegmentId()).toMatch(/^confirm_pending_/);
  });

  it('flushes pending buffers before adding confirm card', () => {
    const p = new StreamProcessor('seg-1');
    p.processDelta('buffered text');

    const cmds = p.processFrame({
      type: 'confirm_request',
      message: { token: 'tok', title: 'X', description: 'Y' },
    } as SSEFrame);

    // Find the flush update_messages command (should be first)
    const updateCmds = cmds.filter((c) => c.kind === 'update_messages');
    const msgs = applyUpdates(makeMessages('seg-1'), cmds);
    expect(msgs[0].content).toBe('buffered text');
  });

  it('sets activity to confirming', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.processFrame({
      type: 'confirm_request',
      message: { token: 'tok', title: 'X', description: 'Y' },
    } as SSEFrame);
    expect(extractFlags(cmds).activity).toEqual({ kind: 'confirming' });
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// processFrame: confirm_resolved
// ═══════════════════════════════════════════════════════════════════════════

describe('processFrame: confirm_resolved', () => {
  it('updates confirmation status and creates new segment', () => {
    const p = new StreamProcessor('seg-1');

    // First: send confirm_request
    const reqCmds = p.processFrame({
      type: 'confirm_request',
      message: { token: 'tok1', title: 'X', description: 'Y' },
    } as SSEFrame);

    let msgs = applyUpdates(makeMessages('seg-1'), reqCmds);

    // Then: resolve it
    const resCmds = p.processFrame({
      type: 'confirm_resolved',
      message: { token: 'tok1', status: 'approved' },
    } as SSEFrame);

    msgs = applyUpdates(msgs, resCmds);

    // Confirmation should be approved
    const confirm = msgs.find((m) => m.role === 'confirm');
    expect(confirm!.confirmation!.status).toBe('approved');

    // A new assistant segment should exist
    const assistants = msgs.filter((m) => m.role === 'assistant');
    expect(assistants.length).toBeGreaterThanOrEqual(1);
    const newSeg = assistants[assistants.length - 1];
    expect(newSeg.isStreaming).toBe(true);
    expect(newSeg.content).toBe('');
  });

  it('starts a FRESH segment on resolve (reasoning is not carried across)', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    const reqCmds = p.processFrame({
      type: 'confirm_request',
      message: { token: 'tok1', title: 'X', description: 'Y' },
    } as SSEFrame);
    msgs = applyUpdates(msgs, reqCmds);

    // Reasoning arrives during the pending wait (parked segment — dropped on resolve).
    p.processFrame({ type: 'reasoning_delta', message: 'thinking...' } as SSEFrame);

    const resCmds = p.processFrame({
      type: 'confirm_resolved',
      message: { token: 'tok1', status: 'approved' },
    } as SSEFrame);
    msgs = applyUpdates(msgs, resCmds);

    const streamingSegs = msgs.filter((m) => m.role === 'assistant' && m.isStreaming);
    const newSeg = streamingSegs[streamingSegs.length - 1];
    expect(reasoningOf(newSeg)).toBe(''); // fresh
    expect(newSeg.content).toBe('');
  });

  it('keeps the pre-confirm segment\'s reasoning where it streamed (no clearing)', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    // Reasoning streamed into seg-1 before the confirm.
    p.processFrame({ type: 'reasoning_delta', message: 'orphan' } as SSEFrame);
    msgs = applyUpdates(msgs, [p.flush()!]);
    expect(reasoningOf(msgs[0])).toBe('orphan');

    const reqCmds = p.processFrame({
      type: 'confirm_request',
      message: { token: 'tok1', title: 'X', description: 'Y' },
    } as SSEFrame);
    msgs = applyUpdates(msgs, reqCmds);

    const resCmds = p.processFrame({
      type: 'confirm_resolved',
      message: { token: 'tok1', status: 'approved' },
    } as SSEFrame);
    msgs = applyUpdates(msgs, resCmds);

    // The reasoning stays on seg-1 (its own timeline) — not cleared, not duplicated.
    const oldSeg = msgs.find((m) => m.id === 'seg-1');
    expect(reasoningOf(oldSeg!)).toBe('orphan');
    const newSeg = msgs.filter((m) => m.role === 'assistant' && m.isStreaming).pop();
    expect(reasoningOf(newSeg!)).toBe('');
  });

  it('does not duplicate reasoning to the new segment when the old one has content', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    p.processFrame({ type: 'reasoning_delta', message: 'initial thoughts' } as SSEFrame);
    p.processDelta('some response content');
    msgs = applyUpdates(msgs, [p.flush()!]);
    expect(reasoningOf(msgs[0])).toBe('initial thoughts');
    expect(msgs[0].content).toBe('some response content');

    const reqCmds = p.processFrame({
      type: 'confirm_request',
      message: { token: 'tok1', title: 'X', description: 'Y' },
    } as SSEFrame);
    msgs = applyUpdates(msgs, reqCmds);

    const resCmds = p.processFrame({
      type: 'confirm_resolved',
      message: { token: 'tok1', status: 'approved' },
    } as SSEFrame);
    msgs = applyUpdates(msgs, resCmds);

    const oldSeg = msgs.find((m) => m.id === 'seg-1');
    expect(reasoningOf(oldSeg!)).toBe('initial thoughts');
    expect(oldSeg?.content).toBe('some response content');

    const newSeg = msgs.filter((m) => m.role === 'assistant').pop();
    expect(reasoningOf(newSeg!)).toBe('');
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// finalize
// ═══════════════════════════════════════════════════════════════════════════

describe('finalize', () => {
  it('marks streaming as false and sets activity to idle', () => {
    const p = new StreamProcessor('seg-1');
    const cmds = p.finalize();
    const flags = extractFlags(cmds);
    expect(flags.streaming).toBe(false);
    expect(flags.activity).toEqual({ kind: 'idle' });
  });

  it('flushes remaining buffers', () => {
    const p = new StreamProcessor('seg-1');
    p.processDelta('final text');
    const cmds = p.finalize();
    const msgs = applyUpdates(makeMessages('seg-1'), cmds);
    expect(msgs[0].content).toBe('final text');
    expect(msgs[0].isStreaming).toBe(false);
  });

  it('applies result fallback to empty active segment', () => {
    const p = new StreamProcessor('seg-1');
    p.processFrame({ type: 'result', message: 'fallback', final: true } as SSEFrame);
    const cmds = p.finalize();
    const msgs = applyUpdates(makeMessages('seg-1'), cmds);
    expect(msgs[0].content).toBe('fallback');
  });

  it('removes trailing empty assistant messages', () => {
    const p = new StreamProcessor('seg-2');
    const msgs: ChatMessage[] = [
      { id: 'seg-1', role: 'assistant', content: 'content', isStreaming: false },
      { id: 'seg-2', role: 'assistant', content: '', isStreaming: true },
    ];
    const cmds = p.finalize();
    const result = applyUpdates(msgs, cmds);
    expect(result).toHaveLength(1);
    expect(result[0].id).toBe('seg-1');
  });

  it('does not remove trailing segment if it has reasoning', () => {
    const p = new StreamProcessor('seg-2');
    const msgs: ChatMessage[] = [
      { id: 'seg-1', role: 'assistant', content: 'content', isStreaming: false },
      { id: 'seg-2', role: 'assistant', content: '', isStreaming: true, reasoning: 'I thought' },
    ];
    const cmds = p.finalize();
    const result = applyUpdates(msgs, cmds);
    expect(result).toHaveLength(2);
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// Full scenario: end-to-end streaming session
// ═══════════════════════════════════════════════════════════════════════════

describe('full scenario: simple Q&A', () => {
  it('handles delta → result → finalize', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    // Deltas arrive
    p.processDelta('Hello ');
    p.processDelta('World!');
    msgs = applyUpdates(msgs, [p.flush()!]);
    expect(msgs[0].content).toBe('Hello World!');

    // Result frame
    p.processFrame({ type: 'result', message: 'Hello World!', final: true } as SSEFrame);

    // Finalize
    msgs = applyUpdates(msgs, p.finalize());
    expect(msgs[0].content).toBe('Hello World!');
    expect(msgs[0].isStreaming).toBe(false);
  });
});

describe('full scenario: reasoning → tool → content → confirm → resolve → content', () => {
  it('processes a complex multi-step stream', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    // Step 1: Reasoning
    p.processFrame({ type: 'reasoning_delta', message: 'Let me think...' } as SSEFrame);
    msgs = applyUpdates(msgs, [p.flush()!]);
    expect(reasoningOf(msgs[0])).toBe('Let me think...');

    // Step 2: Tool finishes → a tool part
    const toolCmds = p.processFrame(toolDone({ tool: 'Flexie Search', duration: 20, status: 'success' }));
    msgs = applyUpdates(msgs, toolCmds);
    msgs = applyUpdates(msgs, [p.flush()!]);
    expect(msgs[0].parts!.some((x) => x.kind === 'tool')).toBe(true);

    // Step 3: Content deltas
    p.processDelta('Found 42 leads.');
    msgs = applyUpdates(msgs, [p.flush()!]);
    expect(msgs[0].content).toContain('Found 42 leads.');
    expect(partKinds(msgs[0])).toEqual(['reasoning', 'tool', 'text']);

    // Step 4: Confirm request
    const confirmCmds = p.processFrame({
      type: 'confirm_request',
      message: { token: 'tok1', title: 'Create Workflow', description: 'OK?' },
    } as SSEFrame);
    msgs = applyUpdates(msgs, confirmCmds);
    expect(msgs.some((m) => m.role === 'confirm')).toBe(true);

    // Step 5: Resolve
    const resCmds = p.processFrame({
      type: 'confirm_resolved',
      message: { token: 'tok1', status: 'approved' },
    } as SSEFrame);
    msgs = applyUpdates(msgs, resCmds);
    const newSegId = p.getSegmentId();

    // Step 6: More content after confirm
    p.processDelta('Workflow created!');
    msgs = applyUpdates(msgs, [p.flush()!]);
    const newSeg = msgs.find((m) => m.id === newSegId);
    expect(newSeg?.content).toBe('Workflow created!');

    // Finalize
    msgs = applyUpdates(msgs, p.finalize());
    const allStreaming = msgs.filter((m) => m.isStreaming);
    expect(allStreaming).toHaveLength(0);
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// Error frame handling
// ═══════════════════════════════════════════════════════════════════════════

describe('error frame handling', () => {
  it('sets error message on the active assistant segment', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    const cmds = p.processFrame({
      type: 'error',
      message: 'Rate limit exceeded',
      final: true,
    } as SSEFrame);

    msgs = applyUpdates(msgs, cmds);
    expect(msgs[0].error).toBe('Rate limit exceeded');

    const { activity } = extractFlags(cmds);
    expect(activity).toEqual({ kind: 'idle' });
  });

  it('preserves error segments during finalize (does not remove them)', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    // Set error (no content)
    msgs = applyUpdates(msgs, p.processFrame({
      type: 'error',
      message: 'Model refused to reply',
      final: true,
    } as SSEFrame));

    // Finalize should NOT remove the segment because it has an error
    msgs = applyUpdates(msgs, p.finalize());
    expect(msgs).toHaveLength(1);
    expect(msgs[0].error).toBe('Model refused to reply');
    expect(msgs[0].isStreaming).toBe(false);
  });

  it('keeps existing content alongside the error', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    // Some content was streamed before the error
    p.processDelta('Partial response...');
    msgs = applyUpdates(msgs, [p.flush()!]);
    expect(msgs[0].content).toBe('Partial response...');

    // Then error arrives
    msgs = applyUpdates(msgs, p.processFrame({
      type: 'error',
      message: 'Stream stalled - no data for 90 seconds',
      final: true,
    } as SSEFrame));

    msgs = applyUpdates(msgs, p.finalize());
    expect(msgs[0].content).toBe('Partial response...');
    expect(msgs[0].error).toBe('Stream stalled - no data for 90 seconds');
  });

  it('handles non-string error messages with a fallback', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    const cmds = p.processFrame({
      type: 'error',
      message: undefined as any,
      final: true,
    } as SSEFrame);

    msgs = applyUpdates(msgs, cmds);
    expect(msgs[0].error).toBe('An unexpected error occurred');
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// Agent frame filtering
// ═══════════════════════════════════════════════════════════════════════════

describe('agent frame filtering', () => {
  it('agent delta does not switch activity to responding', () => {
    const p = new StreamProcessor('seg-1');

    // Gateway delta DOES trigger 'responding'
    const mainCmds = p.processFrame({ type: 'delta', message: 'hello', final: false } as SSEFrame);
    const mainFlags = extractFlags(mainCmds);
    expect(mainFlags.activity).toEqual({ kind: 'responding' });

    // Agent delta does NOT change activity
    const subCmds = p.processFrame({
      type: 'delta',
      message: 'agent text',
      final: false,
      is_agent: true,
    } as SSEFrame);
    const subFlags = extractFlags(subCmds);
    expect(subFlags.activity).toBeUndefined(); // no activity change
  });

  it('finished tools add a part for both the Gateway and an agent', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    // Main-agent finished tool → a tool part.
    p.processFrame(toolDone({ tool: 'query', duration: 5, status: 'success' }));
    msgs = applyUpdates(msgs, [p.flush()!]);
    expect(msgs[0].parts!.some((x) => x.kind === 'tool')).toBe(true);

    // Agent's finished tool (is_agent-tagged) → a part too. The backend
    // persists it as a display-only row, so the reload timeline matches live.
    const p2 = new StreamProcessor('seg-2');
    let msgs2 = makeMessages('seg-2');
    p2.processFrame({ type: 'tool', final: false, is_agent: true, message: { done: true, tool_name: 'pdf_document', friendly_name: 'PDF Document', duration_ms: 5, status: 'completed' } } as SSEFrame);
    msgs2 = applyUpdates(msgs2, [p2.flush()!]);
    expect(msgs2[0].parts!.some((x) => x.kind === 'tool')).toBe(true);
  });

  // An agent's tools are shown too, so the user sees what was done on their
  // behalf even when an agent did it. Agents are not implemented on the
  // server yet, but the frames are reserved and the client already honours them.
  it('shows an agent\'s finished tool in the timeline', () => {
    const p = new StreamProcessor('seg-1');

    p.processFrame({
      type: 'tool',
      message: { tool_name: 'pdf_document', friendly_name: 'Build a PDF', status: 'completed', duration_ms: 12 },
      is_agent: true,
    } as SSEFrame);

    const msgs = applyUpdates(makeMessages('seg-1'), [p.flush()!]);
    const toolPart = msgs[0].parts![0];
    if (toolPart.kind === 'tool') {
      expect(toolPart.tool.friendly_name).toBe('Build a PDF');
      expect(toolPart.tool.status).toBe('completed');
    }
  });

  it('agent reasoning_delta still accumulates reasoning', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    // The Gateway hands over to an agent: what the frame carries is which
    // agent, not which model it thinks with.
    p.processFrame({
      type: 'agent_start',
      message: { agent: 'writer', name: 'Writer' },
    } as SSEFrame);

    // Agent reasoning should still flow through
    p.processFrame({
      type: 'reasoning_delta',
      message: 'thinking about PDF...',
    } as SSEFrame);

    const flushCmd = p.flush();
    expect(flushCmd).not.toBeNull();
    msgs = applyUpdates(msgs, [flushCmd!]);
    expect(reasoningOf(msgs[0])).toBe('thinking about PDF...');
  });

  it('full agent lifecycle does not pollute main content', () => {
    const p = new StreamProcessor('seg-1');
    let msgs = makeMessages('seg-1');

    // Gateway starts, streams some content
    p.processDelta('Here is ');
    msgs = applyUpdates(msgs, [p.flush()!]);

    // Agent starts
    p.processFrame({ type: 'agent_start', message: { agent: 'writer', name: 'Writer' } } as SSEFrame);

    // Agent reasoning (OK to accumulate)
    p.processFrame({ type: 'reasoning_delta', message: 'Let me create...' } as SSEFrame);

    // Agent tool use (should NOT add \n\n)
    p.processFrame({
      type: 'tool',
      message: { tool_name: 'pdf_document' },
      final: false,
      is_agent: true,
    } as SSEFrame);

    // Agent deltas (should NOT reach content via processDelta — client filters them,
    // but processFrame should also not change activity to 'responding')
    const deltaCmds = p.processFrame({
      type: 'delta',
      message: 'Created PDF template...',
      final: false,
      is_agent: true,
    } as SSEFrame);
    expect(extractFlags(deltaCmds).activity).toBeUndefined();

    // Agent ends
    p.processFrame({ type: 'agent_end', message: {} } as SSEFrame);

    // Gateway continues with its own response
    p.processDelta('the result.');

    // Flush everything
    msgs = applyUpdates(msgs, [p.flush()!]);

    // Content should be ONLY from Gateway
    expect(msgs[0].content).toBe('Here is the result.');
    // Reasoning from agent should be present (as a reasoning part)
    expect(reasoningOf(msgs[0])).toBe('Let me create...');
  });
});
