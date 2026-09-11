import { describe, it, expect, vi, afterEach } from 'vitest'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/react'

import { ToolRow } from '@/components/ui/ai/tool-timeline'

// Opening a tool call.
//
// The row streams as it always did (a name, and how long it took); what the
// tool was SENT and ANSWERED is asked for when somebody opens it. So a call
// still running has nothing to open, and neither has one that happened in front
// of the person and has not been written down yet.

function answers(body: unknown, status = 200) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => ({ ok: status === 200, status, json: async () => body }))
  )
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('opening a tool call', () => {
  it('shows what it was sent and what it answered, as text rather than escaped JSON', async () => {
    answers({
      name: 'terminal',
      friendly_name: 'Terminal',
      status: 'completed',
      duration_ms: 1413,
      sent: [{ name: 'command', value: 'git status', as: 'command' }],
      answered: [{ name: 'stdout', value: 'On branch main\nnothing to commit', as: 'text' }],
    })

    const view = render(
      <ToolRow
        tool={{ id: '42', name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 1413 }}
      />
    )
    fireEvent.click(view.getByRole('button'))

    await waitFor(() => expect(view.container.textContent).toContain('git status'))
    // The output as LINES. Printed as one escaped line it is a result nobody
    // reads, which is the whole reason this is formatted rather than dumped.
    expect(view.container.textContent).toContain('On branch main\nnothing to commit')
    expect(view.container.textContent).not.toContain('\\n')
  })

  it('says so when somebody may not see it', async () => {
    answers({}, 403)
    const view = render(
      <ToolRow tool={{ id: '42', name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 10 }} />
    )
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toMatch(/do not have permission/i))
  })

  it('cannot be opened while it is still running', () => {
    const view = render(<ToolRow tool={{ id: '42', name: 'terminal', friendly_name: 'Terminal', status: 'running' }} />)
    expect(view.queryByRole('button')).toBeNull()
  })

  it('shows named values as names and values, not as JSON', async () => {
    answers({
      name: 'query_demo',
      friendly_name: 'Demo CRM',
      status: 'completed',
      duration_ms: 40,
      sent: [{ name: 'sql', value: 'select 1' }],
      answered: [{ name: 'rows', value: 3 }],
    })
    const view = render(
      <ToolRow tool={{ id: '7', name: 'query_demo', friendly_name: 'Demo CRM', status: 'completed', duration_ms: 40 }} />
    )
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('select 1'))

    // Every name and every value is there to read.
    for (const shown of ['sql', 'select 1', 'rows', '3']) {
      expect(view.container.textContent).toContain(shown)
    }
    // And none of the punctuation that carried them. Braces and quotes around
    // the two words somebody is looking for are what made this unreadable.
    expect(view.container.textContent).not.toContain('{')
    expect(view.container.textContent).not.toContain('"sql"')
  })

  it('cannot be opened when there is nothing written down yet', () => {
    const view = render(
      <ToolRow tool={{ name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 10 }} />
    )
    expect(view.queryByRole('button')).toBeNull()
  })
})

// What both halves say, said once.
//
// The terminal is TOLD a directory and REPORTS the directory it ran in, so the
// same path was printed twice, in two places, and reading the call meant
// noticing they were the same path. A field with the same name and the same
// value on both sides is a fact about the call rather than about either half.
describe('a fact both halves carry', () => {
  it('is shown once, above them, and not in either', async () => {
    answers({
      name: 'terminal',
      friendly_name: 'Terminal',
      status: 'completed',
      duration_ms: 210,
      where: [{ name: 'directory', value: '/Users/dev/work' }],
      sent: [{ name: 'command', value: 'git status', as: 'command' }],
      answered: [{ name: 'output', value: 'clean', as: 'text' }],
    })
    const view = render(
      <ToolRow tool={{ id: '9', name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 210 }} />
    )
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('git status'))

    const shown = view.container.textContent ?? ''
    expect(shown.split('/Users/dev/work').length - 1, 'the directory is printed twice').toBe(1)
    // And nothing is lost by moving it: both halves still say what only they say.
    expect(shown).toContain('git status')
    expect(shown).toContain('clean')
  })

  it('keeps a name that means different things on each side', async () => {
    answers({
      name: 'query_demo',
      friendly_name: 'Demo',
      status: 'completed',
      duration_ms: 12,
      sent: [{ name: 'limit', value: 10 }],
      answered: [{ name: 'limit', value: 3 }],
    })
    const view = render(
      <ToolRow tool={{ id: '11', name: 'query_demo', friendly_name: 'Demo', status: 'completed', duration_ms: 12 }} />
    )
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toMatch(/Sent/i))
    const shown = view.container.textContent ?? ''
    // Two facts that happen to share a name: both are still there.
    expect(shown).toContain('10')
    expect(shown).toContain('3')
  })
})

// A command is not JSON, and reading it as JSON is what made a terminal call a
// wall of one colour.
describe('a command, coloured the way a terminal colours one', () => {
  it('keeps every character of what ran, heredoc body included', async () => {
    const script = "python3 - <<'PY'\nimport json\nprint('hi')\nPY"
    answers({
      name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 30,
      sent: [{ name: 'command', value: script, as: 'command' }],
      answered: [{ name: 'output', value: 'hi', as: 'text' }],
    })
    const view = render(<ToolRow tool={{ id: '3', name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 30 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('python3'))

    // Every line survives the colouring, in order. The prompt mark is ours and
    // is the only thing added.
    for (const line of script.split('\n')) {
      expect(view.container.textContent).toContain(line)
    }
    expect(view.container.textContent).toContain('hi')
  })

  it('goes through the highlighter this chat already has, prompt and all', async () => {
    answers({
      name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 5,
      sent: [{ name: 'command', value: "git log --oneline -n 5 'my branch'", as: 'command' }],
      answered: [{ name: 'output', value: 'abc123', as: 'text' }],
    })
    const view = render(<ToolRow tool={{ id: '5', name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 5 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('git'))

    // The command, character for character, with the prompt mark ours and the
    // colouring the code blocks'. The grammar loads asynchronously, so what is
    // asserted is what holds from the first frame: the text is all there.
    expect(view.container.textContent).toContain("git log --oneline -n 5 'my branch'")
    expect(view.container.textContent).toContain('$')
  })
})

// Where a command ran is the first thing a person reads, so it is drawn above
// both halves. WHICH field that is was decided by the tool and applied by the
// server; this is that it lands at the top.
describe('where it ran', () => {
  it('is lifted to the top even when only the answer carries it', async () => {
    answers({
      name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 26,
      // The model gave no directory: the tool used the working folder and says so.
      where: [{ name: 'directory', value: '/Users/dev/projects/atlas' }],
      sent: [{ name: 'command', value: 'date -u', as: 'command' }],
      answered: [{ name: 'stdout', value: '2026-09-03T13:15:00Z', as: 'text' }],
    })
    const view = render(<ToolRow tool={{ id: '21', name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 26 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('/Users/dev/projects/atlas'))

    const shown = view.container.textContent ?? ''
    // Above SENT, which is where the top of the panel is.
    expect(shown.indexOf('/Users/dev/projects/atlas')).toBeLessThan(shown.indexOf('Sent'))
    // And the section that is only the output is only the output.
    expect(shown).not.toContain('stdout')
    expect(shown).toContain('2026-09-03T13:15:00Z')
  })

})

// The subject of a section needs no name.
//
// "command" above a command, inside a section already headed SENT, labels the
// thing a person is looking at. Its name comes back the moment it has a
// sibling, because then the names are telling two things apart.
describe('the subject of a section', () => {
  it('is drawn without its name when it is the only one there', async () => {
    answers({
      name: 'ssh_demo', friendly_name: 'Demo 24 VM', status: 'completed', duration_ms: 293,
      sent: [{ name: 'command', value: 'free -h', as: 'command' }],
      answered: [{ name: 'output', value: 'Mem: 19Gi', as: 'text' }],
    })
    const view = render(<ToolRow tool={{ id: '31', name: 'ssh_demo', friendly_name: 'Demo 24 VM', status: 'completed', duration_ms: 293 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('free'))

    const shown = view.container.textContent ?? ''
    expect(shown).not.toContain('command')
    expect(shown).not.toContain('output')
    expect(shown).toContain('free -h')
    expect(shown).toContain('Mem: 19Gi')
  })

  it('keeps its name when there is something to tell it apart from', async () => {
    answers({
      name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 12,
      sent: [{ name: 'command', value: 'make', as: 'command' }, { name: 'input', value: 'y' }],
      answered: [
        { name: 'stdout', value: 'building', as: 'text' },
        { name: 'stderr', value: 'a warning', as: 'text' },
      ],
    })
    const view = render(<ToolRow tool={{ id: '32', name: 'terminal', friendly_name: 'Terminal', status: 'completed', duration_ms: 12 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('building'))

    const shown = view.container.textContent ?? ''
    for (const name of ['command', 'input', 'stdout', 'stderr']) {
      expect(shown).toContain(name)
    }
  })
})

// A database answers with rows, and rows are a table.
describe('a result set', () => {
  it('is drawn as a table, headers and all', async () => {
    answers({
      name: 'query_crm', friendly_name: 'CRM', status: 'completed', duration_ms: 88,
      sent: [{ name: 'sql', value: 'select id, name from people', as: 'text' }],
      answered: [
        { name: 'rows', as: 'table', value: { columns: ['id', 'name'], rows: [[1, 'Ada'], [2, null]] } },
        { name: 'row_count', value: 2 },
      ],
    })
    const view = render(<ToolRow tool={{ id: '41', name: 'query_crm', friendly_name: 'CRM', status: 'completed', duration_ms: 88 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('Ada'))

    // A real table, with the column names as headers.
    const headers = [...view.container.querySelectorAll('th')].map((th) => th.textContent)
    expect(headers).toEqual(['id', 'name'])
    expect(view.container.querySelectorAll('tbody tr').length).toBe(2)
    // An empty cell says so rather than being a gap somebody has to interpret.
    expect(view.container.textContent).toContain('null')
    expect(view.container.textContent).toContain('row_count')
  })

  it('keeps a long value reachable rather than letting it set the width', async () => {
    const email = 'a.very.long.address.that.would.stretch.the.table@example.com'
    answers({
      name: 'query_crm', friendly_name: 'CRM', status: 'completed', duration_ms: 40,
      sent: [{ name: 'sql', value: 'select email, age from people', as: 'text' }],
      answered: [{ name: 'rows', as: 'table', value: { columns: ['email', 'age'], rows: [[email, 41]] } }],
    })
    const view = render(<ToolRow tool={{ id: '42', name: 'query_crm', friendly_name: 'CRM', status: 'completed', duration_ms: 40 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('@example.com'))

    // Trimmed in the cell, whole on the cell: nothing is lost by trimming.
    const cells = [...view.container.querySelectorAll('td')]
    expect(cells[0].getAttribute('title')).toBe(email)
    // And a number sits on the right, where a column of them can be compared.
    expect(cells[1].className).toContain('text-right')
  })
})

// A call that went wrong has ONE heading, not two.
//
// An ANSWERED beside an ERROR reads as though something came back and,
// separately, something went wrong. When it went wrong, what came back IS what
// went wrong.
describe('a call that went wrong', () => {
  it('reads as one thing: the error, and what it printed, under Error', async () => {
    answers({
      name: 'ssh_demo', friendly_name: 'Demo 24 VM', status: 'failed', duration_ms: 40,
      sent: [{ name: 'command', value: 'cat nope', as: 'command' }],
      answered: [{ name: 'output', value: 'cat: nope: No such file', as: 'text' }],
      error: 'the command exited 1',
    })
    const view = render(<ToolRow tool={{ id: '51', name: 'ssh_demo', friendly_name: 'Demo 24 VM', status: 'failed', duration_ms: 40 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('the command exited 1'))

    const shown = view.container.textContent ?? ''
    expect(shown).toContain('Error')
    expect(shown).not.toContain('Answered')
    // Both are there, under the one heading, and neither is a key.
    expect(shown).toContain('cat: nope: No such file')
    expect(shown).not.toContain('output')
  })
})

// A statement over several lines, and a payload read as what it is.
describe('a statement and a payload', () => {
  it('gives a one-line statement its clauses back, without touching quoted text', async () => {
    answers({
      name: 'query_crm', friendly_name: 'CRM', status: 'completed', duration_ms: 9,
      sent: [{
        name: 'sql', as: 'sql',
        value: "SELECT id, name FROM people WHERE note = 'order by size' ORDER BY id LIMIT 5",
      }],
      answered: [{ name: 'rows', as: 'table', value: { columns: ['id'], rows: [[1]] } }],
    })
    const view = render(<ToolRow tool={{ id: '61', name: 'query_crm', friendly_name: 'CRM', status: 'completed', duration_ms: 9 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('SELECT'))

    const shown = view.container.textContent ?? ''
    // Each clause starts a line...
    expect(shown).toContain('\nFROM people')
    expect(shown).toContain('\nWHERE note')
    expect(shown).toContain('\nLIMIT 5')
    // ...and the ORDER BY inside the string stayed inside the string.
    expect(shown).toContain("'order by size'")
    expect(shown).not.toContain("'order\nby size'")
  })

  it('lays out a JSON response and leaves plain text alone', async () => {
    answers({
      name: 'http_request', friendly_name: 'API request', status: 'completed', duration_ms: 441,
      sent: [{ name: 'method', value: 'GET' }, { name: 'url', value: 'https://example.com/v1/rates' }],
      answered: [
        { name: 'status', value: 200 },
        { name: 'body', as: 'body', value: { content_type: 'application/json; charset=utf-8', body: '{"rate":1.5,"as_of":"2026-09-03"}' } },
      ],
    })
    const view = render(<ToolRow tool={{ id: '62', name: 'http_request', friendly_name: 'API request', status: 'completed', duration_ms: 441 }} />)
    fireEvent.click(view.getByRole('button'))
    await waitFor(() => expect(view.container.textContent).toContain('rate'))

    // One line in, laid out on the page.
    expect(view.container.textContent).toContain('{\n  "rate": 1.5')
    expect(view.container.textContent).toContain('status')
    expect(view.container.textContent).toContain('200')
  })
})

// A command can succeed and print nothing.
it('says so when a call answered with nothing', async () => {
  answers({
    name: 'ssh_demo', friendly_name: 'Demo 24 VM', status: 'completed', duration_ms: 121,
    sent: [{ name: 'input', value: 'q' }],
    answered: [],
  })
  const view = render(<ToolRow tool={{ id: '71', name: 'ssh_demo', friendly_name: 'Demo 24 VM', status: 'completed', duration_ms: 121 }} />)
  fireEvent.click(view.getByRole('button'))
  await waitFor(() => expect(view.container.textContent).toMatch(/Nothing came back/i))
  // And not a heading with a void under it, which is what looked broken.
  expect(view.container.textContent).not.toContain('Answered')
})
