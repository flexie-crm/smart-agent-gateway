import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { TOPIC_EVENT } from '@lib/agent-watch'
import type { Delegation } from '@lib/delegations'

// An agent opened from its chip: read through the API, kept current by the
// socket, and read-only. The network and the socket are stand-ins that record
// what was asked of them; everything between them and the page is the code
// that ships.

const asked: { path: string; body: Record<string, unknown> }[] = []
let answers: Record<string, unknown> = {}

vi.mock('@lib/api', async () => {
  const actual = await vi.importActual<Record<string, unknown>>('@lib/api')
  return {
    ...actual,
    apiFetch: vi.fn(async (path: string, init?: RequestInit) => {
      const body = JSON.parse(String(init?.body ?? '{}')) as Record<string, unknown>
      asked.push({ path, body })
      const key = path.endsWith('/fleet') ? `fleet:${body.fleet_id}` : `agent:${body.delegation_id}`
      if (!(key in answers)) return new Response('{}', { status: 404 })
      return new Response(JSON.stringify(answers[key]), { status: 200 })
    }),
  }
})

const joined: string[] = []
const left: string[] = []
vi.mock('@lib/ws', () => ({
  watch: (topic: string) => joined.push(topic),
  leave: (topic: string) => left.push(topic),
}))

beforeEach(() => {
  asked.length = 0
  joined.length = 0
  left.length = 0
  answers = {}
  // Not in jsdom; the page uses it to keep up with new work.
  globalThis.ResizeObserver = class {
    observe() {}
    disconnect() {}
    unobserve() {}
  } as unknown as typeof ResizeObserver
})
afterEach(cleanup)

const now = Math.floor(Date.now() / 1000)

function push(topic: string, payload: unknown) {
  act(() => {
    window.dispatchEvent(new CustomEvent(TOPIC_EVENT, { detail: { topic, payload } }))
  })
}

async function open(chip: Delegation, onClose = () => {}) {
  const { AgentView } = await import('../AgentView')
  const view = (c: Delegation) => (
    <AgentView chip={c} chatId="c1" historyEndpoint="/v1/chat/history" showReasoning showTools onClose={onClose} />
  )
  const rendered = render(view(chip))
  return { ...rendered, update: (c: Delegation) => rendered.rerender(view(c)) }
}

const panel = () => document.querySelector('[data-agent-view]') as HTMLElement

describe('an agent opened from its chip', () => {
  it('shows its task and its work, and follows it as it moves', async () => {
    const chip: Delegation = {
      id: '5', name: 'Ops', status: 'running', created_at: now - 10,
      progress: { activity: 'Fetching external data', tokens: 1200 },
    }
    answers['agent:5'] = {
      run: { id: '5', agent: 'ops', name: 'Ops', status: 'running', created_at: now - 10, task: 'take the spare model offline' },
      messages: [{ id: '11', role: 'assistant', content: '', tools: [{ name: 'set_model_status', friendly_name: 'Change model status', status: 'running' }] }],
    }
    const onClose = vi.fn()
    const { update, unmount } = await open(chip, onClose)

    await screen.findByText('take the spare model offline')
    expect(asked[0]).toEqual({ path: '/v1/chat/delegation', body: { chat_id: 'c1', delegation_id: 5 } })
    expect(joined).toEqual(['agent:5'])
    expect(screen.getAllByText('Fetching external data').length).toBeGreaterThan(0)
    expect(screen.getByText(/1\.2k tok/)).toBeTruthy()
    expect(screen.getByText('Change model status')).toBeTruthy()

    // A step pushed for this agent appears; one for another agent does not.
    push('agent:5', { kind: 'step', message: { id: '12', role: 'assistant', content: 'The spare model is offline.' } })
    push('agent:6', { kind: 'step', message: { id: '99', role: 'assistant', content: 'Somebody else entirely.' } })
    expect(screen.getByText('The spare model is offline.')).toBeTruthy()
    expect(screen.queryByText('Somebody else entirely.')).toBeNull()

    // To read, not to talk to: nowhere to type and nothing to answer.
    expect(panel().querySelector('textarea')).toBeNull()
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull()

    // Finishing is read once more, and what it handed back is marked.
    answers['agent:5'] = {
      run: { id: '5', agent: 'ops', name: 'Ops', status: 'done', created_at: now - 10, completed_at: now, task: 'take the spare model offline' },
      messages: [
        { id: '11', role: 'assistant', content: '', tools: [{ name: 'set_model_status', friendly_name: 'Change model status', status: 'completed' }] },
        { id: '12', role: 'assistant', content: 'The spare model is offline.' },
      ],
      result_id: '12',
    }
    update({ ...chip, status: 'done', completed_at: now })
    await screen.findByText('Result')
    expect(asked.filter((a) => a.path === '/v1/chat/delegation')).toHaveLength(2)
    // What it handed back is its own section, and not also the last of its
    // work: the same words appear once.
    const result = panel().querySelector('[data-agent-result]') as HTMLElement
    expect(result.textContent).toContain('Result')
    expect(result.textContent).toContain('The spare model is offline.')
    expect(panel().textContent?.split('The spare model is offline.').length).toBe(2)

    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(onClose).toHaveBeenCalled()
    unmount()
    expect(left).toEqual(['agent:5'])
  })

  // Opening it moves nothing: it opens at the top, and its work arriving does
  // not scroll the window to the end. (Asked for after the installed modal
  // jumped to its bottom the moment the work loaded.) jsdom lays nothing out,
  // so the window is given a height to scroll and every move of it is kept.
  it('opens at the top and stays there when its work arrives', async () => {
    const moved: number[] = []
    const height = vi.spyOn(Element.prototype, 'scrollHeight', 'get').mockReturnValue(2000)
    const shown = vi.spyOn(Element.prototype, 'clientHeight', 'get').mockReturnValue(800)
    const top = vi.spyOn(Element.prototype, 'scrollTop', 'set').mockImplementation((value: number) => {
      moved.push(value)
    })
    try {
      answers['agent:9'] = {
        run: { id: '9', agent: 'ops', name: 'Ops', status: 'done', created_at: now - 9, completed_at: now, task: 'a long job' },
        messages: [{ id: '91', role: 'assistant', content: 'Step one.' }, { id: '92', role: 'assistant', content: 'All done.' }],
        result_id: '92',
      }
      await open({ id: '9', name: 'Ops', status: 'done', created_at: now - 9, completed_at: now })
      await screen.findByText('All done.')
      expect(moved).toEqual([])
    } finally {
      height.mockRestore()
      shown.mockRestore()
      top.mockRestore()
    }
  })

  // Stopped to ask is not working: no spinner anywhere, and it says where to
  // go to let it carry on.
  it('shows an agent waiting for approval as paused', async () => {
    answers['agent:8'] = {
      run: { id: '8', agent: 'ops', name: 'Ops', status: 'waiting_approval', created_at: now - 5, task: 'x' },
      messages: [],
    }
    await open({ id: '8', name: 'Ops', status: 'waiting_approval', created_at: now - 5 })
    await screen.findByText('Waiting for your approval in the chat')
    const header = panel().querySelector('[data-agent-header]') as HTMLElement
    expect(header.textContent).toContain('Waiting for your approval in the chat')
    expect(panel().querySelectorAll('.animate-spin')).toHaveLength(0)
  })

  it('says why it stopped when it failed', async () => {
    answers['agent:7'] = {
      run: { id: '7', agent: 'ops', name: 'Ops', status: 'failed', created_at: now - 5, completed_at: now, task: 'x',
        error: 'the background task ran past its time limit' },
      messages: [],
    }
    await open({ id: '7', name: 'Ops', status: 'failed', created_at: now - 5, completed_at: now })
    await screen.findByText('the background task ran past its time limit')
  })
})

describe('a batch opened from its chip', () => {
  it('shows a block per agent, opens one, and comes back', async () => {
    const chip: Delegation = { id: 'fleet-9', kind: 'fleet', name: '2 × Ops', status: 'running', agents: 2, done: 1, created_at: now - 30 }
    answers['fleet:9'] = {
      runs: [
        // What each has spent, as the server answers it off the member's row.
        { id: '21', agent: 'ops', name: 'Ops', status: 'done', created_at: now - 30, completed_at: now - 5, task: 'price gold in London', tokens: 1234, cost: 0.01 },
        { id: '22', agent: 'ops', name: 'Ops', status: 'running', created_at: now - 30, task: 'price gold in New York', tokens: 766, cost: 0.02 },
      ],
    }
    answers['agent:21'] = {
      run: { id: '21', agent: 'ops', name: 'Ops', status: 'done', created_at: now - 30, completed_at: now - 5, task: 'price gold in London' },
      messages: [{ id: '31', role: 'assistant', content: 'London: 2,391.20' }],
      result_id: '31',
    }
    const { update } = await open(chip)

    await screen.findByText('price gold in London')
    expect(asked[0]).toEqual({ path: '/v1/chat/fleet', body: { chat_id: 'c1', fleet_id: 9 } })
    expect(joined).toEqual(['fleet:9'])
    const header = () => panel().querySelector('[data-agent-header]') as HTMLElement
    expect(header().textContent).toContain('1 of 2 finished')
    // What the batch has spent between them, and what each agent has.
    expect(header().textContent).toContain('2.0k tokens · $0.030')
    expect(panel().querySelector('[data-agent-block="21"]')?.textContent).toContain('1.2k tokens')
    expect(panel().querySelector('[data-agent-block="22"]')?.textContent).toContain('766 tokens')
    // The batch's progress is drawn on the header's own line.
    expect(header().querySelector('[role="progressbar"]')?.getAttribute('aria-valuenow')).toBe('50')
    const second = () => panel().querySelector('[data-agent-block="22"]') as HTMLElement
    expect(second().querySelector('.animate-spin')).not.toBeNull()

    // The batch moving is pushed whole.
    // As the server pushes it: the whole batch, spend included (app.FleetRuns).
    push('fleet:9', {
      kind: 'fleet',
      runs: [
        { id: '21', agent: 'ops', name: 'Ops', status: 'done', created_at: now - 30, completed_at: now - 5, task: 'price gold in London', tokens: 1234, cost: 0.01 },
        { id: '22', agent: 'ops', name: 'Ops', status: 'done', created_at: now - 30, completed_at: now, task: 'price gold in New York', tokens: 766, cost: 0.02 },
      ],
    })
    expect(second().querySelector('.animate-spin')).toBeNull()

    // One agent, opened: its own work, and the way back.
    fireEvent.click(panel().querySelector('[data-agent-block="21"]') as HTMLElement)
    await screen.findByText('London: 2,391.20')
    expect(joined).toEqual(['fleet:9', 'agent:21'])
    // Which of the batch it is, in the header that did not change shape.
    expect(header().textContent).toContain('Agent 1 of 2')
    expect(header().textContent).toContain('1.2k tokens · $0.010')
    expect(header().querySelector('[role="progressbar"]')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Back to all agents' }))
    await waitFor(() => expect(panel().querySelector('[data-agent-block="21"]')).not.toBeNull())
    expect(left).toEqual(['agent:21'])

    // Finished, its bottom line is the same border as every other: a full bar
    // would only be a darker one.
    update({ ...chip, status: 'done', done: 2, completed_at: now })
    await waitFor(() => expect(header().textContent).toContain('2 of 2 finished'))
    expect(header().querySelector('[role="progressbar"]')).toBeNull()
  })
})
