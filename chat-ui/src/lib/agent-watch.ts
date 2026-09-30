// lib/agent-watch.ts
// ─── Watching an agent work ─────────────────────────────────────────────────
//
// A background agent or a whole fleet, opened from its chip. What it has done
// so far is read with one request; every step after that arrives on the socket,
// on a channel joined while it is open and left when it closes, so an agent
// nobody has open costs nothing on the wire. This is the pure core, so how a
// pushed step folds into what is on screen is tested without React.

import { apiFetch } from './api'
import type { ChatMessage } from './chat-types'
import type { Delegation } from './delegations'

/** The window event the socket dispatches for a message on a joined channel. */
export const TOPIC_EVENT = 'sag:topic'

/** One agent run, as the server answers it. */
export interface AgentRun {
  id: string
  agent: string
  name: string
  status: string // running | waiting_approval | done | failed | cancelled
  created_at: number
  completed_at?: number
  task: string
  error?: string
  // What it is doing and what it has spent so far, as the server answers them
  // (app.AgentRun: activity, tokens, cost, read off the delegation's progress).
  activity?: string
  tokens?: number
  cost?: number
}

/**
 * What a whole batch has spent: its agents' tokens and cost added up. Zero
 * where nothing was counted, which is every batch run before its agents
 * counted what they spent.
 */
export function batchSpend(runs: AgentRun[]): { tokens: number; cost: number } {
  return runs.reduce(
    (sum, run) => ({ tokens: sum.tokens + (run.tokens ?? 0), cost: sum.cost + (run.cost ?? 0) }),
    { tokens: 0, cost: 0 },
  )
}

/** One agent run opened: the run, and its steps in the shape the chat draws. */
export interface AgentWork {
  run: AgentRun
  messages: ChatMessage[]
  /** The message that is what the agent handed back, once it has. */
  result_id?: string
}

export interface TopicMessage {
  topic: string
  payload: unknown
}

/** The channel one agent run is watched on. */
export function agentTopic(runId: string): string {
  return `agent:${runId}`
}

/** The channel a whole batch is watched on. */
export function fleetTopic(fleetId: string): string {
  return `fleet:${fleetId}`
}

/**
 * The fleet a chip stands for, or null for one agent. A batch's chip id is
 * `fleet-<id>`, kept apart from a delegation's because both are row ids from
 * different tables and share one list.
 */
export function fleetOf(chip: Pick<Delegation, 'id' | 'kind'>): string | null {
  if (chip.kind !== 'fleet') return null
  return chip.id.replace(/^fleet-/, '')
}

const TERMINAL = new Set(['done', 'failed', 'cancelled'])

/** Whether a run has stopped for good. */
export function isFinished(status: string): boolean {
  return TERMINAL.has(status)
}

/**
 * A pushed step folded into what is on screen.
 *
 * A step is pushed each time it changes (written, then once per tool call that
 * finishes), and each push is the step as it NOW stands. So one already shown
 * is replaced where it is, and a new one goes at the end.
 */
export function withStep(messages: ChatMessage[], step: ChatMessage): ChatMessage[] {
  const at = messages.findIndex((m) => m.id === step.id)
  if (at === -1) return [...messages, step]
  const next = messages.slice()
  next[at] = step
  return next
}

/**
 * What arrived while the first read was on its way, folded into what that read
 * answered. The read is the truth for every step it knows, and a push it does
 * not know is newer than it: the step was written after the read was taken.
 */
export function withEarlier(read: ChatMessage[], pushed: ChatMessage[]): ChatMessage[] {
  const known = new Set(read.map((m) => m.id))
  return pushed.reduce((messages, step) => (known.has(step.id) ? messages : withStep(messages, step)), read)
}

/** The step a message on an agent's channel carries, or null. */
export function stepOf(message: TopicMessage, runId: string): ChatMessage | null {
  if (message.topic !== agentTopic(runId)) return null
  const payload = message.payload as { kind?: unknown; message?: unknown } | null
  if (!payload || payload.kind !== 'step' || typeof payload.message !== 'object' || !payload.message) return null
  return payload.message as ChatMessage
}

/** Where every agent of a batch stands, from a message on its channel, or null. */
export function runsOf(message: TopicMessage, fleetId: string): AgentRun[] | null {
  if (message.topic !== fleetTopic(fleetId)) return null
  const payload = message.payload as { kind?: unknown; runs?: unknown } | null
  if (!payload || payload.kind !== 'fleet' || !Array.isArray(payload.runs)) return null
  return payload.runs as AgentRun[]
}

/** The endpoints beside the conversation's history. */
function beside(historyEndpoint: string, name: string): string {
  return historyEndpoint.replace(/\/[^/]+$/, `/${name}`)
}

/** One agent run, as far as it has got. */
export async function readAgentWork(historyEndpoint: string, chatId: string, runId: string): Promise<AgentWork> {
  const res = await apiFetch(beside(historyEndpoint, 'delegation'), {
    method: 'POST',
    body: JSON.stringify({ chat_id: chatId, delegation_id: Number(runId) }),
  })
  if (!res.ok) throw new Error(`agent ${res.status}`)
  return (await res.json()) as AgentWork
}

/** Every agent of a batch, where each of them stands. */
export async function readFleetRuns(historyEndpoint: string, chatId: string, fleetId: string): Promise<AgentRun[]> {
  const res = await apiFetch(beside(historyEndpoint, 'fleet'), {
    method: 'POST',
    body: JSON.stringify({ chat_id: chatId, fleet_id: Number(fleetId) }),
  })
  if (!res.ok) throw new Error(`fleet ${res.status}`)
  const body = (await res.json()) as { runs?: AgentRun[] }
  return body.runs ?? []
}
