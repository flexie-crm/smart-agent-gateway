// lib/use-agent-watch.ts
// ─── The React half of watching an agent ────────────────────────────────────
//
// Join the channel, read what has happened so far, then fold in what the
// socket pushes (agent-watch.ts). Read again whenever what arrived by push
// might have been missed: the socket came back after a drop, or the run just
// finished and the last word on it is the server's.

import { useCallback, useEffect, useRef, useState } from 'react'
import { SOCKET_READY_EVENT } from './delegations'
import { leave, watch } from './ws'
import type { ChatMessage } from './chat-types'
import {
  TOPIC_EVENT,
  agentTopic,
  fleetTopic,
  isFinished,
  readAgentWork,
  readFleetRuns,
  runsOf,
  stepOf,
  withEarlier,
  withStep,
  type AgentRun,
  type AgentWork,
  type TopicMessage,
} from './agent-watch'

/**
 * One agent run, kept current while it is open. `status` is how the run stands
 * as the caller knows it (its chip, or its batch), and its turning final is
 * the moment to read the finished record once more.
 */
export function useAgentWork(historyEndpoint: string, chatId: string | null, runId: string | null, status: string | undefined) {
  const [work, setWork] = useState<AgentWork | null>(null)
  const [failed, setFailed] = useState(false)
  // Non-null while a read is on its way: a step pushed meanwhile waits here and
  // is folded into the answer, rather than landing on a list about to be
  // replaced.
  const waiting = useRef<ChatMessage[] | null>(null)
  const latest = useRef(0)

  const read = useCallback(async () => {
    if (!chatId || !runId) return
    const mine = ++latest.current
    waiting.current = []
    try {
      const answered = await readAgentWork(historyEndpoint, chatId, runId)
      if (mine !== latest.current) return
      const early = waiting.current ?? []
      waiting.current = null
      setWork({ ...answered, messages: withEarlier(answered.messages, early) })
      setFailed(false)
    } catch {
      if (mine !== latest.current) return
      waiting.current = null
      setFailed(true)
    }
  }, [historyEndpoint, chatId, runId])

  useEffect(() => {
    if (!runId) return
    setWork(null)
    setFailed(false)
    const topic = agentTopic(runId)
    const onTopic = (event: Event) => {
      const step = stepOf((event as CustomEvent<TopicMessage>).detail, runId)
      if (!step) return
      if (waiting.current) {
        waiting.current.push(step)
        return
      }
      setWork((current) => (current ? { ...current, messages: withStep(current.messages, step) } : current))
    }
    // What was pushed while the socket was down was pushed to nobody.
    const onReady = () => void read()
    window.addEventListener(TOPIC_EVENT, onTopic)
    window.addEventListener(SOCKET_READY_EVENT, onReady)
    watch(topic)
    void read()
    return () => {
      latest.current++
      waiting.current = null
      window.removeEventListener(TOPIC_EVENT, onTopic)
      window.removeEventListener(SOCKET_READY_EVENT, onReady)
      leave(topic)
    }
  }, [runId, read])

  // The run finishing is read once more: what it handed back, and why it
  // stopped if it failed, are the server's to say.
  const before = useRef(status)
  useEffect(() => {
    const was = before.current
    before.current = status
    if (status && was && !isFinished(was) && isFinished(status)) void read()
  }, [status, read])

  return { work, failed }
}

/** Every agent of a batch, kept current while the batch is open. */
export function useFleetRuns(historyEndpoint: string, chatId: string | null, fleetId: string | null) {
  const [runs, setRuns] = useState<AgentRun[] | null>(null)
  const [failed, setFailed] = useState(false)
  const latest = useRef(0)

  const read = useCallback(async () => {
    if (!chatId || !fleetId) return
    const mine = ++latest.current
    try {
      const answered = await readFleetRuns(historyEndpoint, chatId, fleetId)
      if (mine !== latest.current) return
      setRuns(answered)
      setFailed(false)
    } catch {
      if (mine === latest.current) setFailed(true)
    }
  }, [historyEndpoint, chatId, fleetId])

  useEffect(() => {
    if (!fleetId) return
    setRuns(null)
    setFailed(false)
    const topic = fleetTopic(fleetId)
    const onTopic = (event: Event) => {
      const pushed = runsOf((event as CustomEvent<TopicMessage>).detail, fleetId)
      if (!pushed) return
      // The whole batch as it now stands, so it replaces rather than merges;
      // and it is newer than any read still on its way.
      latest.current++
      setRuns(pushed)
    }
    const onReady = () => void read()
    window.addEventListener(TOPIC_EVENT, onTopic)
    window.addEventListener(SOCKET_READY_EVENT, onReady)
    watch(topic)
    void read()
    return () => {
      latest.current++
      window.removeEventListener(TOPIC_EVENT, onTopic)
      window.removeEventListener(SOCKET_READY_EVENT, onReady)
      leave(topic)
    }
  }, [fleetId, read])

  return { runs, failed }
}
