import { describe, expect, it } from 'vitest'
import {
  agentTopic,
  batchSpend,
  fleetOf,
  fleetTopic,
  isFinished,
  runsOf,
  stepOf,
  withEarlier,
  withStep,
} from '../agent-watch'
import type { ChatMessage } from '../chat-types'

const step = (id: string, content = '', status?: string): ChatMessage => ({
  id,
  role: 'assistant',
  content,
  tools: status ? [{ name: 'http_request', friendly_name: 'Fetch', status }] : undefined,
})

describe('watching an agent', () => {
  // A batch's total is its agents' spend added up; an agent that counted
  // nothing (it ran before agents counted) adds nothing rather than breaking it.
  it('adds up what a batch has spent', () => {
    const run = (tokens?: number, cost?: number) => ({ id: 'x', agent: 'ops', name: 'Ops', status: 'done', created_at: 1, task: 't', tokens, cost })
    expect(batchSpend([run(1234, 0.01), run(766, 0.02), run()])).toEqual({ tokens: 2000, cost: 0.03 })
    expect(batchSpend([])).toEqual({ tokens: 0, cost: 0 })
  })

  it('names the channel an agent and a batch are watched on', () => {
    expect(agentTopic('12')).toBe('agent:12')
    expect(fleetTopic('3')).toBe('fleet:3')
  })

  it('tells a batch chip from an agent chip by its kind, not by the shape of its id', () => {
    expect(fleetOf({ id: 'fleet-3', kind: 'fleet' })).toBe('3')
    expect(fleetOf({ id: '12' })).toBeNull()
    // An agent's id is never read as a batch, whatever it looks like.
    expect(fleetOf({ id: 'fleet-3' })).toBeNull()
  })

  it('knows when a run has stopped for good', () => {
    for (const s of ['done', 'failed', 'cancelled']) expect(isFinished(s)).toBe(true)
    for (const s of ['running', 'waiting_approval']) expect(isFinished(s)).toBe(false)
  })

  // A step is pushed each time it changes, as it now stands: written with its
  // tool running, then again when the tool has finished.
  it('replaces a step already shown, where it is, and adds a new one at the end', () => {
    const first = [step('1', 'thinking'), step('2', '', 'running')]
    const updated = withStep(first, step('2', '', 'completed'))
    expect(updated.map((m) => m.id)).toEqual(['1', '2'])
    expect(updated[1].tools?.[0].status).toBe('completed')
    expect(first[1].tools?.[0].status).toBe('running') // the list shown is not changed under React

    const added = withStep(updated, step('3', 'the answer'))
    expect(added.map((m) => m.id)).toEqual(['1', '2', '3'])
  })

  // What arrived while the first read was on its way: the read is the truth
  // for every step it knows, and a step it does not know was written after it.
  it('keeps the read for steps it knows and adds the ones it did not', () => {
    const read = [step('1', 'a'), step('2', '', 'completed')]
    const pushed = [step('2', '', 'running'), step('3', 'newer')]
    const merged = withEarlier(read, pushed)
    expect(merged.map((m) => m.id)).toEqual(['1', '2', '3'])
    expect(merged[1].tools?.[0].status).toBe('completed')
  })

  it('reads a step only off its own agent, and only a step', () => {
    const message = step('7', 'hi')
    expect(stepOf({ topic: 'agent:5', payload: { kind: 'step', message } }, '5')).toEqual(message)
    expect(stepOf({ topic: 'agent:6', payload: { kind: 'step', message } }, '5')).toBeNull()
    expect(stepOf({ topic: 'agent:5', payload: { kind: 'fleet', runs: [] } }, '5')).toBeNull()
    expect(stepOf({ topic: 'agent:5', payload: null }, '5')).toBeNull()
  })

  it('reads a batch only off its own channel', () => {
    const runs = [{ id: '1', agent: 'ops', name: 'Ops', status: 'done', created_at: 1, task: 't' }]
    expect(runsOf({ topic: 'fleet:3', payload: { kind: 'fleet', runs } }, '3')).toEqual(runs)
    expect(runsOf({ topic: 'fleet:4', payload: { kind: 'fleet', runs } }, '3')).toBeNull()
    expect(runsOf({ topic: 'fleet:3', payload: { kind: 'step' } }, '3')).toBeNull()
  })
})
