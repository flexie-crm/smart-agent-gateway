import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@/test-utils'
import { Brains } from '@/pages/Brains'
import type { View } from '@/lib/brains'

// The brains screen is one address and one answer: the server resolves what to
// show, the screen paints it, and every click is exactly one question. These
// tests pin the "exactly one": development mounts the screen twice on purpose,
// and that must never reach the server as two requests.

const BRAINS = [
  { id: 1, name: 'Platform', slug: 'platform', description: '', locked: false, categories: 2, documents: 3 },
  { id: 2, name: 'Sales', slug: 'sales', description: '', locked: false, categories: 0, documents: 0 },
]

const CATEGORIES = [
  { id: 10, brain_id: 1, name: 'Migrations', description: '', weight: 0, documents: 2 },
  { id: 11, brain_id: 1, name: 'Jobs', description: '', weight: 0, documents: 1 },
]

const DOCUMENTS: Record<number, { id: number; title: string; content: string }[]> = {
  10: [
    { id: 100, title: 'How the schema changes', content: 'Alpha body' },
    { id: 101, title: 'The destructive guard', content: 'Gamma body' },
  ],
  11: [{ id: 110, title: 'Job ownership', content: 'Beta body' }],
}

/** Answers /v1/brains/view the way the server does: hints in, whole screen out. */
function answer(url: string): View {
  const params = new URL(url, 'http://sag.test').searchParams
  const brainID = Number(params.get('brain')) || 1
  const categoryID = Number(params.get('category')) || CATEGORIES[0].id
  const docs = DOCUMENTS[categoryID] ?? []
  const documentID = Number(params.get('document')) || docs[0]?.id || 0
  const open = docs.find((d) => d.id === documentID) ?? null
  return {
    brains: BRAINS,
    brain_id: brainID,
    categories: CATEGORIES,
    category_id: categoryID,
    documents: docs.map((d) => ({ ...d, brain_id: 1, category_id: categoryID, weight: 0, related: [] })),
    document_id: documentID,
    document: open
      ? { ...open, brain_id: 1, category_id: categoryID, weight: 0, related: [] }
      : null,
  }
}

function serveViews() {
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (!url.includes('/v1/brains/view')) throw new Error(`unexpected request: ${url}`)
    return new Response(JSON.stringify(answer(url)), { status: 200 })
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

describe('the brains screen', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('draws its frame before the server answers, so nothing shifts when it does', async () => {
    // An answer held back, so the test can look at the screen mid-flight.
    let respond!: (r: Response) => void
    vi.stubGlobal(
      'fetch',
      vi.fn(() => new Promise<Response>((r) => (respond = r))),
    )

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    )

    // The panes and their titles are chrome, not data. A workspace switch
    // remounts the screen, and chrome that waits for data is a layout shift.
    expect(screen.getByRole('heading', { name: 'Categories' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Documents' })).toBeInTheDocument()
    // An empty-state message is a claim about the data, and the data has not
    // spoken: saying "no brain yet" mid-flight would be a lie.
    expect(screen.queryByText(/No brain yet/)).not.toBeInTheDocument()
    expect(screen.queryByText(/Choose a brain/)).not.toBeInTheDocument()

    // Let the request land: the single door shares one in-flight slot per
    // path across the module, and a request left hanging here would block
    // the same ask in whatever test runs next.
    respond(new Response(JSON.stringify(answer('/v1/brains/view')), { status: 200 }))
    expect(await screen.findByText('Platform')).toBeInTheDocument()
  })

  it('paints all three panes from one request, even when development mounts it twice', async () => {
    const fetch = serveViews()

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    )

    expect(await screen.findByText('Platform')).toBeInTheDocument()
    expect(screen.getByText('Migrations')).toBeInTheDocument()
    expect(screen.getByText('Alpha body')).toBeInTheDocument()
    // The double mount is deliberate and the server must not feel it.
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  // A category used to show a bare "2", which answers a question nobody asked:
  // two of what? Both panes count the same thing and both must say what it is.
  it('says what the number under a brain and a category counts', async () => {
    serveViews()

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    )

    // The brain holds three, the open category two, and one is singular.
    expect(await screen.findByText('3 documents')).toBeInTheDocument()
    expect(screen.getByText('2 documents')).toBeInTheDocument()
    expect(screen.getByText('1 document')).toBeInTheDocument()
  })

  it('asks exactly one question per click and repaints from the answer', async () => {
    const fetch = serveViews()

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    )
    await screen.findByText('Alpha body')

    fireEvent.click(screen.getByText('Jobs'))

    expect(await screen.findByText('Beta body')).toBeInTheDocument()
    expect(fetch).toHaveBeenCalledTimes(2)
    const asked = new URL(String(fetch.mock.calls[1][0]), 'http://sag.test')
    expect(asked.pathname).toBe('/v1/brains/view')
    expect(asked.searchParams.get('brain')).toBe('1')
    expect(asked.searchParams.get('category')).toBe('11')
  })

  it('expands the reader over the list and puts it back, without asking again', async () => {
    const fetch = serveViews()

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    )
    await screen.findByText('Alpha body')
    // Collapsed: the reader shows the open document, the list the whole category.
    expect(screen.getByText('The destructive guard')).toBeInTheDocument()

    fireEvent.click(screen.getByText('Show full document'))
    // Expanded: the reader has the room, the list is gone.
    expect(screen.queryByText('The destructive guard')).not.toBeInTheDocument()
    expect(screen.getByText('Alpha body')).toBeInTheDocument()

    fireEvent.click(screen.getByText('Show document list'))
    expect(screen.getByText('The destructive guard')).toBeInTheDocument()

    // Expansion is the screen's own state; the server was asked nothing new.
    expect(fetch).toHaveBeenCalledTimes(1)
  })
})
