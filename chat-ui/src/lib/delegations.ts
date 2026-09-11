// lib/delegations.ts
// ─── Background-delegation chips (Mode C) ────────────────────────────────────
//
// The Gateway can hand a task to an agent that runs in the background. While
// it runs, a chip shows the agent and what state it is in. The chip is fed
// two ways: the still-running set arrives with a conversation's history, and
// live changes arrive over the socket. This module is the pure core both use, so
// the folding of an event into the chip list is tested without React.

/** Live progress the card shows beyond a spinner. */
export interface DelegationProgress {
  activity?: string
  /** Running token total (input + output), updated once per model call. */
  tokens?: number
  tokens_in?: number
  tokens_out?: number
  /** Running cost in dollars, derived from the model's pricing. */
  cost?: number
  step?: number
}

/** One background delegation, as a card renders it. */
export interface Delegation {
  id: string
  /**
   * What this chip stands for. Absent means one agent, which is what every chip
   * was before fleets existed; 'fleet' means a whole batch started together.
   *
   * A fleet is ONE chip however many agents it has: twenty chips saying almost
   * the same thing is a wall, and what somebody wants to know about a batch is
   * how many there are and how many are back.
   */
  kind?: string
  agent?: string
  name: string
  status: string // running | waiting_approval | done | failed | cancelled
  /** Unix seconds it started, for an elapsed clock. */
  created_at?: number
  /** Unix seconds it stopped. Present once it has, so the clock can stop. */
  completed_at?: number
  progress?: DelegationProgress
  /** A fleet's numbers: how many were started, and how many are back. */
  agents?: number
  done?: number
}

/** The window events the socket dispatches for the chat to react to. */
export const DELEGATION_EVENT = 'sag:delegation'
export const TURN_EVENT = 'sag:turn'
// A background agent's approval card, pushed whole over the socket (KB/27),
// so the client renders it with no API round-trip.
export const CARD_EVENT = 'sag:card'
// The socket is connected and authenticated, first time or after a drop. What
// was pushed while it was down was pushed to nobody, so a listener holding
// state that arrives by push re-reads it here.
export const SOCKET_READY_EVENT = 'sag:socket-ready'

const TERMINAL = new Set(['done', 'failed', 'cancelled'])

/** isTerminalDelegation reports whether a status ends the delegation. */
export function isTerminalDelegation(status: string): boolean {
  return TERMINAL.has(status)
}

/**
 * applyDelegationEvent folds one delegation state change into the chip list.
 *
 * A terminal state UPDATES the chip; it does not remove it. The column is a
 * record of what has been done here, and work that disappears the moment it
 * finishes leaves the person with no idea what happened for them: "what has it
 * done" is most of what they wanted the column for. A finished chip is quieter,
 * not absent.
 *
 * A new chip goes on TOP, so the thing that just started is where the eye
 * already is and everything older moves down.
 */
export function applyDelegationEvent(list: Delegation[], evt: Delegation): Delegation[] {
  const chip: Delegation = {
    id: evt.id, kind: evt.kind, agent: evt.agent, name: evt.name, status: evt.status,
    created_at: evt.created_at, completed_at: evt.completed_at,
    progress: evt.progress, agents: evt.agents, done: evt.done,
  }
  const idx = list.findIndex(d => d.id === evt.id)
  if (idx === -1) return [chip, ...list]
  const next = list.slice()
  next[idx] = chip
  return next
}
