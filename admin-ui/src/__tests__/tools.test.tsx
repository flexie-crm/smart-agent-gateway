import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@/test-utils'
import { Tools } from '@/pages/Tools'

// The "Add tool" flow discloses step by step: pick a native template, pick its
// driver, and only then does the driver's form appear, built from what the
// server says that driver needs. These tests pin that the form is server-driven
// (not hard-coded), that a connection can be tested before saving, and that
// creating posts the settings the person filled in.

const TEMPLATES = [
  {
    name: 'query',
    title: 'Query Database',
    description: 'Run SQL against a database.',
    variants: [{ key: 'mysql', label: 'MySQL / MariaDB' }],
    params: [
      { key: 'sql', type: 'string', required: true, description: 'The SQL statement to run.' },
      { key: 'params', type: 'array', required: false, description: 'Positional values.' },
    ],
    default_guide: 'Run one statement per call.',
  },
]

const SECTIONS = [
  {
    title: 'Connection',
    fields: [
      { key: 'access', label: 'Access', type: 'select', options: ['read', 'write', 'both'], default: 'read', required: true },
      { key: 'host', label: 'Host', type: 'text', required: true },
      { key: 'database', label: 'Database', type: 'text', required: true },
      { key: 'username', label: 'Username', type: 'text', required: true },
      { key: 'password', label: 'Password', type: 'password', secret: true },
      // A field the template seeds. The stored tool below has no value for it,
      // which is what tells a new form apart from an edit.
      { key: 'refused', label: 'Commands to refuse', type: 'textarea', default: 'rm\nshutdown' },
    ],
  },
  {
    title: 'SSH tunnel',
    hint: 'Reach a database that is only accessible through a bastion.\n\n- The port is usually 22.\n- Leave a secret blank to keep the saved one.',
    fields: [
      { key: 'ssh.host', label: 'SSH host', type: 'text' },
    ],
  },
]

// What the server answers with for a NEW tool: the form, and the values it
// starts from. The console does not assemble those from field defaults; the
// decision of what an unfilled field starts as is the server's.
const NEW_FORM = {
  sections: SECTIONS,
  values: { access: 'read', host: '', database: '', username: '', password: '', 'ssh.host': '', refused: 'rm\nshutdown' },
}

function serve(overrides: { create?: () => Response; test?: () => Response } = {}) {
  const calls: { create?: unknown } = {}
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/v1/tools/templates/query/fields')) {
      return new Response(JSON.stringify(NEW_FORM), { status: 200 })
    }
    if (url.includes('/v1/tools/templates')) {
      return new Response(JSON.stringify(TEMPLATES), { status: 200 })
    }
    if (url.includes('/v1/tools/custom/test')) {
      return overrides.test?.() ?? new Response(JSON.stringify({ ok: true }), { status: 200 })
    }
    if (url.includes('/v1/tools/custom')) {
      calls.create = JSON.parse(String(init?.body))
      return (
        overrides.create?.() ??
        new Response(JSON.stringify({ id: 9, name: 'query_local', kind: 'custom', template: 'query', friendly_name: 'Query local', grants: [] }), {
          status: 201,
        })
      )
    }
    if (url.includes('/v1/groups')) return new Response(JSON.stringify([]), { status: 200 })
    return new Response(JSON.stringify([]), { status: 200 }) // /v1/tools list
  })
  vi.stubGlobal('fetch', fetch)
  return calls
}

async function openFormToDriver() {
  render(
    <StrictMode>
      <Tools />
    </StrictMode>,
  )
  fireEvent.click(await screen.findByRole('button', { name: /add tool/i }))
  // The first select is the template; choosing it reveals the driver select.
  fireEvent.change(await screen.findByRole('combobox'), { target: { value: 'query' } })
  const selects = await screen.findAllByRole('combobox')
  fireEvent.change(selects[1], { target: { value: 'mysql' } })
}

describe('the add-tool flow', () => {
  afterEach(() => vi.unstubAllGlobals())

  it("lays a section's explanation out instead of running it together", async () => {
    serve()
    await openFormToDriver()

    // A template writes what somebody needs to know before filling a form in,
    // and some of it is several thoughts. Run together into one block, nobody
    // reads any of it: a blank line is a paragraph and "- " is a point.
    fireEvent.click(await screen.findByRole('button', { name: /ssh tunnel/i }))
    const explanation = await screen.findByText(/only accessible through a bastion/)

    // Scoped to the explanation this is about, and awaited. An unscoped
    // getAllByRole collects every listitem on the PAGE, so it passed or failed
    // depending on what else happened to be mounted, which is how it came to
    // fail under a full parallel run and pass on its own.
    await waitFor(() => {
      const block = explanation.closest('div') as HTMLElement
      const points = within(block).getAllByRole('listitem').map((li) => li.textContent)
      expect(points).toEqual(['The port is usually 22.', 'Leave a secret blank to keep the saved one.'])
    })
  })

  it('builds the driver form from what the server declares', async () => {
    serve()
    await openFormToDriver()

    // The fields the server returned are rendered; a secret is a password input.
    await waitFor(() => expect(screen.getByPlaceholderText('••••••••')).toBeInTheDocument())
    expect(screen.getByText('Host')).toBeInTheDocument()
    expect(screen.getByText('Access')).toBeInTheDocument()

    // The sections are tabs; the second one (SSH tunnel) is a tab, and choosing
    // it reveals its own field. The console renders whatever sections it is given.
    fireEvent.click(screen.getByRole('button', { name: /ssh tunnel/i }))
    expect(await screen.findByText('SSH host')).toBeInTheDocument()
  })

  it("seeds a new tool's form from the template's defaults", async () => {
    serve()
    await openFormToDriver()
    await screen.findByPlaceholderText('••••••••')

    // A new tool starts from what the template suggests, which is the whole
    // point of a default: nobody should have to type a list of dangerous
    // commands from memory to get a sensible tool. Matched loosely because the
    // query normalizes whitespace, and what matters is that the suggestion is
    // in the box, not how it is wrapped.
    expect(await screen.findByDisplayValue(/rm\s+shutdown/)).toBeInTheDocument()
  })

  it('tests the connection before saving', async () => {
    serve()
    await openFormToDriver()
    await screen.findByPlaceholderText('••••••••')

    fireEvent.click(screen.getByRole('button', { name: /test connection/i }))
    expect(await screen.findByText(/connected/i)).toBeInTheDocument()
  })

  it('shows the reason when a connection cannot be made', async () => {
    serve({ test: () => new Response(JSON.stringify({ ok: false, error: 'connect: no route to host' }), { status: 200 }) })
    await openFormToDriver()
    await screen.findByPlaceholderText('••••••••')

    fireEvent.click(screen.getByRole('button', { name: /test connection/i }))
    expect(await screen.findByText(/no route to host/i)).toBeInTheDocument()
  })

  it('prefills the parameters and guide from the template, and posts edited descriptions', async () => {
    const calls = serve()
    render(
      <StrictMode>
        <Tools />
      </StrictMode>,
    )
    fireEvent.click(await screen.findByRole('button', { name: /add tool/i }))
    fireEvent.change(await screen.findByRole('combobox'), { target: { value: 'query' } })

    // The template's inputs show, with their identity and their prefilled,
    // editable description; the guide is prefilled too.
    expect(await screen.findByText('Parameters')).toBeInTheDocument()
    expect(screen.getByText('sql')).toBeInTheDocument()
    expect(screen.getByDisplayValue('Run one statement per call.')).toBeInTheDocument()

    // Only the description is editable; sharpen it and create.
    fireEvent.change(screen.getByDisplayValue('The SQL statement to run.'), {
      target: { value: 'SELECT only, against the orders schema.' },
    })
    fireEvent.change(screen.getByPlaceholderText('prod_orders'), { target: { value: 'orders' } })
    fireEvent.click(screen.getByRole('button', { name: /create tool/i }))

    await waitFor(() => expect(calls.create).toBeTruthy())
    expect(calls.create).toMatchObject({
      alias: 'orders',
      param_descriptions: { sql: 'SELECT only, against the orders schema.' },
    })
  })

  it('creates the tool with the alias and settings', async () => {
    const calls = serve()
    await openFormToDriver()
    await screen.findByPlaceholderText('••••••••')

    fireEvent.change(screen.getByPlaceholderText('prod_orders'), { target: { value: 'local' } })
    fireEvent.click(screen.getByRole('button', { name: /create tool/i }))

    await waitFor(() => expect(calls.create).toBeTruthy())
    expect(calls.create).toMatchObject({ template: 'query', variant: 'mysql', alias: 'local' })
  })
})

// Editing a custom tool opens its full form (not the limited built-in one),
// prefilled from the server with the secret blanked, and saves the settings and
// presentation over updateCustom plus the switches over update.
describe('the edit-custom-tool flow', () => {
  afterEach(() => vi.unstubAllGlobals())

  const CUSTOM = {
    id: 9,
    name: 'query_local',
    kind: 'custom',
    template: 'query',
    friendly_name: 'Query local',
    description: 'Reads the orders db.',
    risk: 'read_only',
    status: 'active',
    grants: [],
  }
  // One request answers the whole edit form: the tool, its driver's form, the
  // values that fill it, and the three things the template contributes. Note
  // "refused" comes back BLANK though the template seeds a default for it: this
  // tool has no value for it, and the server says so rather than leaving the
  // console to decide what an absent one means.
  const DETAIL = {
    ...CUSTOM,
    variant: 'mysql',
    variant_label: 'MySQL / MariaDB',
    about: 'Run SQL against a database.',
    params: TEMPLATES[0].params,
    sections: SECTIONS,
    guide: 'Existing guide.',
    settings: {
      access: 'read',
      host: 'db.internal',
      port: 3306,
      database: 'orders',
      username: 'app',
      password: '',
      'ssh.host': '',
      refused: '',
    },
    param_descriptions: { sql: 'The SQL to run.' },
  }

  it('opens with the form already there and the values still arriving', async () => {
    let answer: (r: Response) => void = () => {}
    const held = new Promise<Response>((resolve) => {
      answer = resolve
    })
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (/\/v1\/tools\/9$/.test(url)) return held
      if (url.includes('/v1/groups')) return new Response(JSON.stringify([]), { status: 200 })
      if (/\/v1\/tools$/.test(url)) return new Response(JSON.stringify([CUSTOM]), { status: 200 })
      return new Response(JSON.stringify([]), { status: 200 })
    })
    vi.stubGlobal('fetch', fetch)

    render(
      <StrictMode>
        <Tools />
      </StrictMode>,
    )
    fireEvent.click(await screen.findByRole('button', { name: /edit/i }))

    // The dialog is there immediately, saying its values are still coming. It is
    // the FORM that is there, not an empty box: a body that fills in later drops
    // the footer half a screen when it does, and that jump is the thing people
    // actually see. So the buttons are present from the start, and only waiting.
    expect(await screen.findByRole('dialog')).toBeInTheDocument()
    expect(screen.getByRole('progressbar')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /save changes/i })).toBeDisabled()
    expect(screen.queryByDisplayValue('db.internal')).toBeNull()

    answer(new Response(JSON.stringify(DETAIL), { status: 200 }))

    // Then the values land in it, and the bar goes.
    expect(await screen.findByDisplayValue('db.internal')).toBeInTheDocument()
    expect(screen.getByDisplayValue('Existing guide.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /save changes/i })).toBeEnabled()
    expect(screen.queryByRole('progressbar')).toBeNull()
  })

  it('opens a tool without a cascade of lookups', async () => {
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (/\/v1\/tools\/9$/.test(url)) return new Response(JSON.stringify(DETAIL), { status: 200 })
      if (url.includes('/v1/groups')) return new Response(JSON.stringify([]), { status: 200 })
      if (/\/v1\/tools$/.test(url)) return new Response(JSON.stringify([CUSTOM]), { status: 200 })
      return new Response(JSON.stringify([]), { status: 200 })
    })
    vi.stubGlobal('fetch', fetch)

    render(
      <StrictMode>
        <Tools />
      </StrictMode>,
    )
    const edit = await screen.findByRole('button', { name: /edit/i })

    // Everything the page itself loads is already done. From here, opening the
    // tool asks for the tool. ONE request, and it answers with everything the
    // form draws: the driver's own fields, the values that fill them, and the
    // groups it may be granted to.
    //
    // Nothing else. Not the list again, not the template catalogue to find out
    // about one template, not the driver's form as a second trip, and not the
    // workspace's groups as a question of their own (which the PAGE used to ask
    // on every visit, for a dialog nobody had opened). The set is asserted
    // exactly, so any new lookup added here has to be argued for.
    fetch.mockClear()
    fireEvent.click(edit)
    expect(await screen.findByDisplayValue('db.internal')).toBeInTheDocument()

    const asked = fetch.mock.calls.map(([input]) => String(input)).sort()
    expect(asked).toEqual(['/v1/tools/9'])
  })

  it('prefills the form and saves the config, presentation, and switches', async () => {
    const calls: { updateCustom?: Record<string, unknown>; update?: Record<string, unknown> } = {}
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = init?.method ?? 'GET'
      if (url.includes('/v1/tools/templates')) return new Response(JSON.stringify(TEMPLATES), { status: 200 })
      if (/\/v1\/tools\/custom\/9\/test$/.test(url) && method === 'POST') return new Response(JSON.stringify({ ok: true }), { status: 200 })
      if (/\/v1\/tools\/custom\/9$/.test(url) && method === 'PUT') {
        calls.updateCustom = JSON.parse(String(init?.body))
        return new Response(JSON.stringify(CUSTOM), { status: 200 })
      }
      if (/\/v1\/tools\/9$/.test(url) && method === 'PUT') {
        calls.update = JSON.parse(String(init?.body))
        return new Response(JSON.stringify(CUSTOM), { status: 200 })
      }
      if (/\/v1\/tools\/9$/.test(url)) return new Response(JSON.stringify(DETAIL), { status: 200 })
      if (url.includes('/v1/groups')) return new Response(JSON.stringify([]), { status: 200 })
      if (/\/v1\/tools$/.test(url)) return new Response(JSON.stringify([CUSTOM]), { status: 200 })
      return new Response(JSON.stringify([]), { status: 200 })
    })
    vi.stubGlobal('fetch', fetch)

    render(
      <StrictMode>
        <Tools />
      </StrictMode>,
    )

    // Open the custom tool's edit form.
    fireEvent.click(await screen.findByRole('button', { name: /edit/i }))

    // It is prefilled from the server: description, a connection setting, the
    // guide, and the parameter description all come back.
    expect(await screen.findByDisplayValue('Reads the orders db.')).toBeInTheDocument()
    expect(await screen.findByDisplayValue('db.internal')).toBeInTheDocument()
    expect(screen.getByDisplayValue('Existing guide.')).toBeInTheDocument()
    expect(screen.getByDisplayValue('The SQL to run.')).toBeInTheDocument()

    // And nothing else. The template seeds a default for "refused" and this tool
    // has no value for it, which means somebody cleared it: offering it back
    // would undo their decision, and saving would write it straight to the row.
    expect(screen.getByText('Commands to refuse')).toBeInTheDocument()
    expect(screen.queryByDisplayValue(/rm\s+shutdown/)).toBeNull()

    // Testing works with the secret left blank (the server carries it forward).
    fireEvent.click(screen.getByRole('button', { name: /test connection/i }))
    expect(await screen.findByText(/connected/i)).toBeInTheDocument()

    // Sharpen the description and save.
    fireEvent.change(screen.getByDisplayValue('Reads the orders db.'), { target: { value: 'Reads orders, carefully.' } })
    fireEvent.click(screen.getByRole('button', { name: /save changes/i }))

    await waitFor(() => expect(calls.updateCustom).toBeTruthy())
    expect(calls.updateCustom).toMatchObject({ description: 'Reads orders, carefully.' })


    expect((calls.updateCustom as { settings: Record<string, unknown> }).settings).toMatchObject({ host: 'db.internal' })
    await waitFor(() => expect(calls.update).toBeTruthy())
    // Whether it is on and who may reach it. Approval is not a field: where a
    // call pauses for a person is decided on the agent.
    expect(calls.update).toMatchObject({ status: 'active' })
    expect(calls.update).not.toHaveProperty('requires_approval')
  })
})

// The catalogue is read in groups by where a tool came from, and a name is read
// without the prefix that grouping stands in for. The prefix is ours: it keeps
// two services' `query` apart in the registry, and it was never something to
// hand a person and ask them to decode.
describe('the tool catalogue', () => {
  afterEach(() => vi.unstubAllGlobals())

  const CATALOGUE = [
    {
      id: 1,
      name: 'current_time',
      kind: 'builtin',
      source: 'Built-in',
      short_name: 'current_time',
      friendly_name: 'Current time',
      description: 'The clock.',
      risk: 'read_only',
      status: 'active',
      grants: [],
    },
    {
      id: 2,
      name: 'nli_update_entity',
      kind: 'mcp',
      source: 'NLI',
      short_name: 'update_entity',
      friendly_name: 'Update Entities',
      description:
        '**START HERE for any field task:** call `mode="topic"` first.\n\n- it returns a table of contents\n- then drill down',
      risk: 'external_communication',
      status: 'active',
      grants: [],
      mcp_server_id: 4,
    },
  ]

  it('groups by service and shows a tool by its own words, never by its identifier', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify(CATALOGUE), { status: 200 })),
    )
    render(
      <StrictMode>
        <Tools />
      </StrictMode>,
    )

    // The service is a heading, said once, rather than a prefix repeated on
    // every row.
    expect(await screen.findByRole('heading', { name: 'NLI' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Built-in' })).toBeInTheDocument()

    // The tool is its own words. The name the model calls it by is internal:
    // neither the prefixed form nor the bare one belongs on the screen.
    expect(screen.getByText('Update Entities')).toBeInTheDocument()
    expect(screen.getByText('Current time')).toBeInTheDocument()
    expect(screen.queryByText('nli_update_entity')).not.toBeInTheDocument()
    expect(screen.queryByText('update_entity')).not.toBeInTheDocument()
    expect(screen.queryByText('current_time')).not.toBeInTheDocument()
  })

  // A description is written for a model, and a connected service writes its
  // own: bold, backticks and bullets, all of it Markdown. It used to be split
  // on blank lines into paragraphs, which put the asterisks on the screen.
  it('reads a tool description as the Markdown it is', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (/\/v1\/tools\/2$/.test(url)) {
          return new Response(JSON.stringify({ ...CATALOGUE[1], groups: [] }), { status: 200 })
        }
        return new Response(JSON.stringify(CATALOGUE), { status: 200 })
      }),
    )
    render(
      <StrictMode>
        <Tools />
      </StrictMode>,
    )

    const edits = await screen.findAllByRole('button', { name: /edit/i })
    fireEvent.click(edits[edits.length - 1])
    const dialog = await screen.findByRole('dialog')

    expect(within(dialog).getByText('START HERE for any field task:').tagName).toBe('STRONG')
    expect(within(dialog).getByText('mode="topic"').tagName).toBe('CODE')
    expect(within(dialog).getAllByRole('listitem')).toHaveLength(2)
    expect(within(dialog).queryByText(/\*\*START HERE/)).not.toBeInTheDocument()
  })
})
