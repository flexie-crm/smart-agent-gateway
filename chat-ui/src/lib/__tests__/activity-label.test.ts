import { describe, expect, it } from 'vitest';

import { activityLabel } from '../activity-label';
import type { AgentActivity } from '../chat-types';

// What the chat says the agent is doing.
//
// These exist because this decision was wrong twice, and both times the only
// way it was found was building the desktop application, installing it and
// watching a real turn. Both faults are one assertion each here.

describe('what the agent says it is doing', () => {
  it('speaks while the agent is working and nothing else is saying so', () => {
    expect(activityLabel({ kind: 'running' })).toBe('Running...');
    expect(activityLabel({ kind: 'reasoning' })).toBe('Reasoning...');
    expect(activityLabel({ kind: 'tool', name: 'current_time' })).toBe(
      'Agent is using current_time...',
    );
    expect(activityLabel({ kind: 'uploading', detail: 'Uploading report.pdf' })).toBe(
      'Uploading report.pdf',
    );
  });

  // The fault that shipped: `responding` fell through a default branch, so
  // "Running..." sat under an answer that was already being written and only
  // cleared when the whole turn ended.
  it('goes quiet the moment the answer starts arriving', () => {
    expect(activityLabel({ kind: 'responding' })).toBeNull();
  });

  it('goes quiet while an approval card is asking, and when the turn is over', () => {
    expect(activityLabel({ kind: 'confirming' })).toBeNull();
    expect(activityLabel({ kind: 'idle' })).toBeNull();
  });

  // The fault that started all of this. Running and reasoning must BOTH speak,
  // because that is what keeps one element mounted across the handover instead
  // of one unmounting and another mounting somewhere else.
  it('never falls silent between running and reasoning', () => {
    expect(activityLabel({ kind: 'running' })).not.toBeNull();
    expect(activityLabel({ kind: 'reasoning' })).not.toBeNull();
  });

  // Silence has to be reachable, or the assertions above pass on a function
  // that simply always speaks.
  it('can be silent at all', () => {
    const states: AgentActivity[] = [
      { kind: 'idle' },
      { kind: 'running' },
      { kind: 'reasoning' },
      { kind: 'responding' },
      { kind: 'confirming' },
      { kind: 'tool', name: 'x' },
      { kind: 'uploading', detail: 'y' },
    ];
    const spoken = states.filter((s) => activityLabel(s) !== null);
    expect(spoken.length).toBeGreaterThan(0);
    expect(spoken.length).toBeLessThan(states.length);
  });

  it('takes the words from the caller when they are given', () => {
    expect(activityLabel({ kind: 'running' }, { agent_is_running: 'Duke po punon...' })).toBe(
      'Duke po punon...',
    );
  });
});
