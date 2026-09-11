import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@/test-utils'
import { Agents } from '@/pages/Agents'

// The screen is the pipeline, in the order a turn goes through it: what a
// person sends is read first, the Gateway thinks with it, and work is handed on
// to an agent only if the Gateway decides to. Reading it downwards is reading
// what actually happens, so the order is the thing worth asserting.

const AGENTS = [
  { id: 1, key: 'default', name: 'Gateway', model_id: null, reasoning: true, tools: [], status: 'active' },
  { id: 2, key: 'finance', name: 'Finance', model_id: null, reasoning: false, tools: ['crm.invoice'], status: 'active' },
]

/**
 * The screen's own answer: four sections, in the shape the screen has.
 *
 * The fixture used to be a flat list of agents that the page picked apart —
 * find the one keyed `default`, call it the Gateway, call the rest the agents,
 * reach inside it for the file rules and the audio model. The server sends the
 * sections now, so the fixture states them, and a test can say what the FILES
 * section holds without building an agent to hide it inside.
 */
// The writes a dialog makes, so a test can say what was SENT rather than only
// what was asked for.
function serve(
  gateway: Record<string, unknown>,
  agents: unknown[] = [AGENTS[1]],
  wrote: { url: string; body: unknown }[] = [],
  agentForm: Record<string, unknown> | null = null,
) {
  const rules = (gateway.file_rules ?? []) as { types: string[]; model_id: number }[]
  const audioID = (gateway.audio_model_id ?? null) as number | null
  const body = {
    files: { rules: rules.map((r) => ({ ...r, model_name: 'a-model' })) },
    audio: { model_id: audioID, model_name: audioID == null ? undefined : 'a-model' },
    gateway,
    agents,
  }
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      if (method !== 'GET') {
        wrote.push({ url, body: init?.body ? JSON.parse(String(init.body)) : null })
        return new Response(null, { status: 204 })
      }
      // Each dialog's own answer, asked for when it opens.
      if (url.includes('/v1/gateway/files/form')) {
        return new Response(
          JSON.stringify({ rules, models: [{ id: 4, label: 'Anthropic / claude-opus-4-8' }] }),
          { status: 200 },
        )
      }
      if (url.includes('/v1/gateway/audio/form')) {
        return new Response(
          JSON.stringify({ model_id: audioID, models: [{ id: 9, label: 'OpenAI / whisper-1' }] }),
          { status: 200 },
        )
      }
      if (url.includes('/form')) {
        return new Response(
          JSON.stringify(
            agentForm ?? { agent: null, models: [], tools: [], brains: [] },
          ),
          { status: 200 },
        )
      }
      if (url.includes('/v1/gateway')) return new Response(JSON.stringify(body), { status: 200 })
      return new Response('[]', { status: 200 })
    }),
  )
}

describe('the gateway screen', () => {
  afterEach(() => vi.unstubAllGlobals())

  // The order IS the explanation. If these three ever come out in another
  // order the page stops describing what happens and starts being decoration.
  it('reads downwards in the order things happen', async () => {
    serve(AGENTS[0])
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )

    const headings = (await screen.findAllByRole('heading', { level: 2 })).map((h) => h.textContent)
    expect(headings).toEqual(['What people send', 'The Gateway', 'Agents'])

    // And each step says what it hands to the next, so the arrow means
    // something rather than merely pointing.
    expect(screen.getByText(/the message the person wrote/)).toBeInTheDocument()
    expect(screen.getByText(/an agent should take it/)).toBeInTheDocument()
  })

  it('keeps the agents out of the Gateway step', async () => {
    serve(AGENTS[0])
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    const gatewayStep = (await screen.findByText('The Gateway')).closest('section') as HTMLElement
    expect(within(gatewayStep).getByText('Gateway')).toBeInTheDocument()
    expect(within(gatewayStep).queryByText('Finance')).not.toBeInTheDocument()
    expect(screen.getByText('Finance')).toBeInTheDocument()
  })

  // What can be attached is decided before the Gateway ever sees it, so it
  // belongs in the first step and not among the Gateway's own settings.
  // Each rule against the model that reads it. A COUNT of rules is a number
  // somebody has to open a form to understand; this says what will happen to
  // the next file that arrives.
  it('lists what reads what, in the step before the Gateway', async () => {
    serve({ ...AGENTS[0], file_rules: [{ types: ['pdf', 'docx'], model_id: 7 }], audio_model_id: null })
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    const step = (await screen.findByText('What people send')).closest('section') as HTMLElement
    expect(within(step).getByText('PDF, Word')).toBeInTheDocument()
    // Audio with nothing chosen says so plainly, rather than leaving a blank.
    expect(within(step).getByText(/Nobody can talk instead of typing/)).toBeInTheDocument()
  })

  it('calls a catch-all rule anything else', async () => {
    serve({ ...AGENTS[0], file_rules: [{ types: [], model_id: 7 }] })
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    const step = (await screen.findByText('What people send')).closest('section') as HTMLElement
    expect(within(step).getByText('Anything else')).toBeInTheDocument()
  })
})

// Each dialog asks ONE question when it opens, and none before.
//
// This screen used to hold five resources — the models, the vendors that only
// labelled them, the tools, the brains and the agent — and every one of them
// was fetched the moment ANY of the three dialogs opened. The dialog that sets
// a single audio model was waiting on the tool catalogue to arrive.
describe('the gateway screen and its dialogs', () => {
  afterEach(() => vi.unstubAllGlobals())

  function asked() {
    const fetch = globalThis.fetch as unknown as { mock: { calls: [RequestInfo | URL][] } }
    return fetch.mock.calls.map(([input]) => String(input))
  }

  it('asks one question for the screen, and nothing for a dialog nobody opened', async () => {
    serve({ ...AGENTS[0], file_rules: [{ types: ['pdf'], model_id: 4 }] })
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    await screen.findByText('Finance')

    expect(asked()).toEqual(['/v1/gateway'])
  })

  it('asks exactly one more when the Files dialog opens, and it is the Files dialog', async () => {
    serve({ ...AGENTS[0], file_rules: [{ types: ['pdf'], model_id: 4 }] })
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    await screen.findByText('Finance')

    fireEvent.click(screen.getAllByRole('button', { name: 'Change' })[0])
    // The choices arrive already labelled, so nothing here joins a model to a
    // vendor to print a name.
    expect(await screen.findByText('Anthropic / claude-opus-4-8')).toBeInTheDocument()

    // Not the tools. Not the brains. Not the vendors. Not the whole agent.
    expect(asked()).toEqual(['/v1/gateway', '/v1/gateway/files/form'])
  })

  it('asks the Audio dialog only for audio', async () => {
    serve({ ...AGENTS[0], audio_model_id: 9 })
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    await screen.findByText('Finance')

    fireEvent.click(screen.getAllByRole('button', { name: 'Change' })[1])
    expect(await screen.findByText('OpenAI / whisper-1')).toBeInTheDocument()

    await waitFor(() => expect(asked()).toEqual(['/v1/gateway', '/v1/gateway/audio/form']))
  })
})

/**
 * What a dialog SENDS, which is the half a request count cannot see.
 *
 * All three of these changed where they read from, so the thing worth pinning
 * down is that saving still writes the one section the dialog edits, in the
 * shape the server takes, and nothing else. A form that fetches beautifully and
 * saves the wrong body is worse than the five requests it replaced.
 */
describe('what the gateway dialogs save', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('writes only the file rules, to the files section', async () => {
    const wrote: { url: string; body: unknown }[] = []
    serve({ ...AGENTS[0], file_rules: [{ types: ['pdf'], model_id: 4 }] }, [AGENTS[1]], wrote)
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    await screen.findByText('Finance')
    fireEvent.click(screen.getAllByRole('button', { name: 'Change' })[0])
    await screen.findByText('Anthropic / claude-opus-4-8')

    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(wrote).toHaveLength(1))
    // The SECTION, not the agent. Sending the whole agent back is what made
    // this form fetch one first, so that it had something to echo.
    expect(wrote[0].url).toBe('/v1/gateway/files')
    expect(wrote[0].body).toEqual({ rules: [{ types: ['pdf'], model_id: 4 }] })
  })

  it('writes one model id, to the audio section', async () => {
    const wrote: { url: string; body: unknown }[] = []
    serve({ ...AGENTS[0], audio_model_id: 9 }, [AGENTS[1]], wrote)
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    await screen.findByText('Finance')
    fireEvent.click(screen.getAllByRole('button', { name: 'Change' })[1])
    await screen.findByText('OpenAI / whisper-1')

    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(wrote).toHaveLength(1))
    expect(wrote[0].url).toBe('/v1/gateway/audio')
    expect(wrote[0].body).toEqual({ model_id: 9 })
  })

  it('says what changed, rather than closing and leaving you to look', async () => {
    const wrote: { url: string; body: unknown }[] = []
    serve({ ...AGENTS[0], audio_model_id: 9 }, [AGENTS[1]], wrote)
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    await screen.findByText('Finance')
    fireEvent.click(screen.getAllByRole('button', { name: 'Change' })[1])
    await screen.findByText('OpenAI / whisper-1')
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText(/how speech is read has changed/i)).toBeInTheDocument()
  })

  it('prefills an agent from its own form, and saves the whole agent', async () => {
    const wrote: { url: string; body: unknown }[] = []
    serve(AGENTS[0], [AGENTS[1]], wrote, {
      agent: {
        id: 2,
        key: 'finance',
        name: 'Finance',
        instructions: 'answer in euros',
        model_id: 4,
        reasoning: false,
        status: 'active',
        delegation_mode: 'auto',
        tools: ['current_time'],
        confirm_tools: [],
        brains: [],
        file_rules: [],
        audio_model_id: null,
        memory_brain_id: null,
        approval_ttl_seconds: null,
        max_iterations: null,
        background_timeout_seconds: null,
      },
      models: [{ id: 4, label: 'Anthropic / claude-opus-4-8' }],
      tools: [{ id: 1, name: 'current_time', friendly_name: 'Current time', approval_locked: false }],
      brains: [],
    })
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    await screen.findByText('Finance')
    fireEvent.click(screen.getByLabelText('Edit'))

    // The values came from the form's own answer, not from the row the table
    // drew: a row carries a name and a count, and this is an agent.
    expect(await screen.findByDisplayValue('answer in euros')).toBeInTheDocument()
    // And the tool it already holds is ticked, from the same answer.
    expect(screen.getByRole('checkbox', { name: /current time/i })).toBeChecked()

    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(wrote).toHaveLength(1))
    expect(wrote[0].url).toBe('/v1/agents/2')
    const sent = wrote[0].body as Record<string, unknown>
    expect(sent.name).toBe('Finance')
    expect(sent.instructions).toBe('answer in euros')
    expect(sent.tools).toEqual(['current_time'])
  })

  // The name a tool is called by the model is an internal identifier: it
  // carries the prefix that keeps two services' `query` apart in one registry,
  // and it used to be printed beside every checkbox and under every option in
  // the confirmation list. What identifies a tool to a person is its own words,
  // under the heading that says where it came from.
  it('picks tools by their words, with no identifier anywhere in the dialog', async () => {
    serve(AGENTS[0], [AGENTS[1]], [], {
      agent: {
        id: 2,
        key: 'finance',
        name: 'Finance',
        instructions: 'answer in euros',
        model_id: 4,
        reasoning: false,
        status: 'active',
        delegation_mode: 'auto',
        tools: ['nli_update_entity'],
        confirm_tools: [],
        brains: [],
        file_rules: [],
        audio_model_id: null,
        memory_brain_id: null,
        approval_ttl_seconds: null,
        max_iterations: null,
        background_timeout_seconds: null,
      },
      models: [{ id: 4, label: 'Anthropic / claude-opus-4-8' }],
      tools: [
        {
          id: 1,
          name: 'nli_update_entity',
          short_name: 'update_entity',
          friendly_name: 'Update Entities',
          approval_locked: false,
          source: 'NLI',
        },
      ],
      brains: [],
    })
    render(
      <StrictMode>
        <Agents />
      </StrictMode>,
    )
    await screen.findByText('Finance')
    fireEvent.click(screen.getByLabelText('Edit'))

    // The checkbox: its words, under its service.
    expect(await screen.findByRole('checkbox', { name: 'Update Entities' })).toBeChecked()
    expect(screen.getByText('NLI')).toBeInTheDocument()

    // The confirmation list offers the same tool the same way. Opening it is
    // what renders the options at all.
    fireEvent.focus(screen.getByPlaceholderText('Add a tool to confirm…'))
    expect(await screen.findByRole('button', { name: 'Update Entities' })).toBeInTheDocument()

    // Neither form of the identifier reaches the dialog.
    expect(screen.queryByText('nli_update_entity')).not.toBeInTheDocument()
    expect(screen.queryByText('update_entity')).not.toBeInTheDocument()
  })
})
