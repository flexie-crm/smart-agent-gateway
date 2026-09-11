import { describe, it, expect } from 'vitest'
import { applyDelegationEvent, isTerminalDelegation, type Delegation } from '../delegations'

const running: Delegation = { id: '1', agent: 'research', name: 'Research', status: 'running' }

describe('applyDelegationEvent', () => {
  it('adds a delegation not seen before', () => {
    expect(applyDelegationEvent([], running)).toEqual([running])
  })

  it('updates a delegation in place, preserving order', () => {
    const list: Delegation[] = [
      { id: '1', agent: 'a', name: 'A', status: 'running' },
      { id: '2', agent: 'b', name: 'B', status: 'running' },
    ]
    const out = applyDelegationEvent(list, { id: '1', agent: 'a', name: 'A', status: 'waiting_approval' })
    expect(out.map(d => d.id)).toEqual(['1', '2'])
    expect(out[0].status).toBe('waiting_approval')
  })

  // The column is a record of what has been done here. Work that disappears
  // the moment it finishes leaves the person with no idea what happened for
  // them, which is most of what they wanted it for.
  it('keeps a delegation when it finishes, and says how it ended', () => {
    for (const status of ['done', 'failed', 'cancelled']) {
      const after = applyDelegationEvent([running], { ...running, status, completed_at: 200 })
      expect(after).toHaveLength(1)
      expect(after[0].status).toBe(status)
      expect(after[0].completed_at).toBe(200)
    }
  })

  // The agent's RESULT is never on a chip. It is technical fact written for
  // the Gateway, which reads it and tells the person what it means; a chip
  // carrying it shows raw JSON to somebody who asked a question in English.
  it('carries no result', () => {
    const after = applyDelegationEvent([running], { ...running, status: 'done', completed_at: 200 })
    expect(Object.keys(after[0])).not.toContain('outcome')
    expect(Object.keys(after[0])).not.toContain('result')
  })

  // Newest on top: the thing that just started belongs where the eye already
  // is, and everything older moves down.
  it('puts a new delegation on top', () => {
    const older = { ...running, id: '1' }
    const newer = { id: '2', agent: 'x', name: 'X', status: 'running' }
    expect(applyDelegationEvent([older], newer).map((d) => d.id)).toEqual(['2', '1'])
  })

  it('an event for an unknown id adds it rather than being dropped', () => {
    // A terminal event can be the FIRST thing seen about a delegation: a tab
    // opened after the work started still has to show that it happened.
    const after = applyDelegationEvent([running], { id: '9', agent: 'x', name: 'X', status: 'done' })
    expect(after.map((d) => d.id)).toEqual(['9', '1'])
  })
})

describe('isTerminalDelegation', () => {
  it('treats only done/failed/cancelled as terminal', () => {
    expect(isTerminalDelegation('running')).toBe(false)
    expect(isTerminalDelegation('waiting_approval')).toBe(false)
    expect(isTerminalDelegation('done')).toBe(true)
    expect(isTerminalDelegation('failed')).toBe(true)
    expect(isTerminalDelegation('cancelled')).toBe(true)
  })
})

// A fleet is ONE chip, and what it carries is a count rather than an activity
// line. The fold has to keep those numbers or the chip stops moving.
describe('a fleet chip', () => {
  const fleet = {
    id: 'fleet-7', kind: 'fleet', name: '5 × Research',
    status: 'running', created_at: 100, agents: 5, done: 2,
  }

  it('keeps its numbers when it is folded in', () => {
    const [chip] = applyDelegationEvent([], fleet)
    expect(chip.kind).toBe('fleet')
    expect(chip.agents).toBe(5)
    expect(chip.done).toBe(2)
  })

  it('updates in place as members report, rather than stacking up', () => {
    const first = applyDelegationEvent([], fleet)
    const second = applyDelegationEvent(first, { ...fleet, done: 5, status: 'done' })
    expect(second).toHaveLength(1)
    expect(second[0].done).toBe(5)
    expect(second[0].status).toBe('done')
  })

  it('does not collide with a delegation of the same row id', () => {
    const list = applyDelegationEvent([], { id: '7', agent: 'research', name: 'Research', status: 'running' })
    const both = applyDelegationEvent(list, fleet)
    expect(both).toHaveLength(2)
  })
})
