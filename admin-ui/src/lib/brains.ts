import { json, nothing, send } from './api'

/**
 * The knowledge base, as the console sees it.
 *
 * The shapes here mirror the API exactly, and the API mirrors the tree: a brain
 * has categories, a category has documents, a document relates to documents.
 */

/**
 * How a count of documents is written, everywhere it is written. A brain and a
 * category both carry one, and a bare "2" under a category told the reader
 * nothing: two of what?
 */
export function documentCount(n: number): string {
  return `${n} ${n === 1 ? 'document' : 'documents'}`
}

export interface Brain {
  id: number
  name: string
  slug: string
  description: string
  /** Locked: the agent may read this brain and never write to it. */
  locked: boolean
  categories: number
  documents: number
}

export interface Category {
  id: number
  brain_id: number
  name: string
  description: string
  weight: number
  documents: number
}

export interface DocumentLink {
  id: number
  title: string
  /**
   * Where the linked document lives: a name to print, and an id to select with.
   *
   * The id is what stops the edit form fetching every category's documents, one
   * request per category, purely to find out where a link it already held
   * actually lived.
   */
  category_id: number
  category: string
}

export interface Document {
  id: number
  brain_id: number
  category_id: number
  title: string
  /** Markdown, stored as written. The server never turns it into HTML. */
  content: string
  weight: number
  related: DocumentLink[]
}

export interface Hit {
  document_id: number
  brain_id: number
  brain: string
  category: string
  title: string
  /** The sentence that matched, so you can see WHY it came back. */
  snippet: string
  score: number
}

/**
 * The whole screen, in one answer.
 *
 * The three panes used to be three requests, each waiting on the one before it
 * to know what to ask for: which brains exist, then which categories the first
 * has, then which documents, then the document itself. Four round trips to paint
 * one page, and the person watching them saw three empty columns fill in one at
 * a time. The server resolves the defaults now, because it already has the rows.
 */
export interface View {
  brains: Brain[]
  brain_id: number
  categories: Category[]
  category_id: number
  documents: Document[]
  document_id: number
  /** The open document, with its content and its links. */
  document: Document | null
}

/** What the screen wants to look at. Every id is a hint, never a demand. */
export interface Selection {
  brain?: number
  category?: number
  document?: number
}

export const brains = {
  /** The whole screen for a selection: a stale id opens the first of everything. */
  view: (want: Selection = {}) => {
    const params = new URLSearchParams()
    if (want.brain) params.set('brain', String(want.brain))
    if (want.category) params.set('category', String(want.category))
    if (want.document) params.set('document', String(want.document))
    const query = params.toString()
    return json<View>(`/v1/brains/view${query ? `?${query}` : ''}`)
  },

  list: () => json<Brain[]>('/v1/brains'),
  create: (b: { name: string; description: string; locked: boolean }) =>
    json<Brain>('/v1/brains', send('POST', b)),
  update: (id: number, b: { name: string; description: string; locked: boolean }) =>
    json<Brain>(`/v1/brains/${id}`, send('PUT', b)),
  remove: (id: number) => nothing(`/v1/brains/${id}`, { method: 'DELETE' }),

  categories: (brainID: number) => json<Category[]>(`/v1/brains/${brainID}/categories`),
  createCategory: (brainID: number, c: { name: string; description: string }) =>
    json<Category>(`/v1/brains/${brainID}/categories`, send('POST', c)),
  updateCategory: (id: number, c: { name: string; description: string }) =>
    json<Category>(`/v1/brain-categories/${id}`, send('PUT', c)),
  removeCategory: (id: number) => nothing(`/v1/brain-categories/${id}`, { method: 'DELETE' }),

  documents: (categoryID: number) =>
    json<Document[]>(`/v1/brain-categories/${categoryID}/documents`),
  document: (id: number) => json<Document>(`/v1/brain-documents/${id}`),
  createDocument: (
    categoryID: number,
    d: { title: string; content: string; related: number[] },
  ) => json<Document>(`/v1/brain-categories/${categoryID}/documents`, send('POST', d)),
  updateDocument: (
    id: number,
    d: { title: string; content: string; category_id: number; related: number[] },
  ) => json<Document>(`/v1/brain-documents/${id}`, send('PUT', d)),
  removeDocument: (id: number) => nothing(`/v1/brain-documents/${id}`, { method: 'DELETE' }),

  search: (query: string, brainID?: number) => {
    const params = new URLSearchParams({ q: query })
    if (brainID) params.set('brain_id', String(brainID))
    return json<Hit[]>(`/v1/brains/search?${params}`)
  },
}
