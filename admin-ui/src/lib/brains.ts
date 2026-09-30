import { json, nothing, send } from "./api";
import { SEARCH_LIMIT } from "./search";

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
  return `${n} ${n === 1 ? "document" : "documents"}`;
}

export interface Brain {
  id: number;
  name: string;
  slug: string;
  description: string;
  /** Locked: the agent may read this brain and never write to it. */
  locked: boolean;
  /**
   * Who wrote it, and who last changed it.
   *
   * Both, because a brain is written by people AND by agents: an agent
   * correcting what somebody wrote has to be visible as exactly that. The ids
   * are 0 once that person is deleted; the names are frozen and survive them,
   * and for an agent there is no id at all.
   */
  created_by: number;
  created_by_name: string;
  updated_by: number;
  updated_by_name: string;

  categories: number;
  documents: number;
}

export interface Category {
  id: number;
  brain_id: number;
  name: string;
  description: string;
  weight: number;
  documents: number;
  /**
   * Who wrote it, and who last changed it.
   *
   * Both, because a brain is written by people AND by agents: an agent
   * correcting what somebody wrote has to be visible as exactly that. The ids
   * are 0 once that person is deleted; the names are frozen and survive them,
   * and for an agent there is no id at all.
   */
  created_by: number;
  created_by_name: string;
  updated_by: number;
  updated_by_name: string;
}

export interface DocumentLink {
  id: number;
  title: string;
  /**
   * Where the linked document lives: a name to print, and an id to select with.
   *
   * The id is what stops the edit form fetching every category's documents, one
   * request per category, purely to find out where a link it already held
   * actually lived.
   */
  category_id: number;
  category: string;
}

export interface Document {
  id: number;
  brain_id: number;
  category_id: number;
  title: string;
  /** Markdown, stored as written. The server never turns it into HTML. */
  content: string;
  weight: number;
  related: DocumentLink[];
  /**
   * Who wrote it, and who last changed it.
   *
   * Both, because a document is written by people AND by agents: an agent
   * correcting what somebody wrote has to be visible as exactly that. The ids
   * are 0 once that person is deleted; the names are frozen and survive them,
   * and for an agent there is no id at all.
   */
  created_by: number;
  created_by_name: string;
  updated_by: number;
  updated_by_name: string;
}

/**
 * Who wrote a document, in one line, or nothing when nobody was recorded.
 *
 * Nothing is the honest answer for everything written before this was kept: a
 * blank reads as "before we recorded it", and a guess would read as fact.
 * When the same name is on both halves it is said once, because "written by A,
 * last changed by A" tells a reader less than "written by A" does.
 */
export function attribution(document: Document): string {
  const wrote = document.created_by_name;
  const changed = document.updated_by_name;
  if (wrote === "" && changed === "") return "";
  if (wrote === "") return `Last changed by ${changed}`;
  if (changed === "" || changed === wrote) return `Written by ${wrote}`;
  return `Written by ${wrote}, last changed by ${changed}`;
}

export interface Hit {
  document_id: number;
  brain_id: number;
  brain: string;
  /**
   * Where the document lives, as an id and not only a name.
   *
   * A hit has to be OPENABLE: the screen selects a document by naming its
   * brain, its category and itself, and a category name selects nothing.
   */
  category_id: number;
  category: string;
  title: string;
  /** The sentence that matched, so you can see WHY it came back. */
  snippet: string;
  score: number;
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
  brains: Brain[];
  brain_id: number;
  categories: Category[];
  category_id: number;
  documents: Document[];
  document_id: number;
  /** The open document, with its content and its links. */
  document: Document | null;
}

/** What the screen wants to look at. Every id is a hint, never a demand. */
export interface Selection {
  brain?: number;
  category?: number;
  document?: number;
}

export const brains = {
  /** The whole screen for a selection: a stale id opens the first of everything. */
  view: (want: Selection = {}) => {
    const params = new URLSearchParams();
    if (want.brain) params.set("brain", String(want.brain));
    if (want.category) params.set("category", String(want.category));
    if (want.document) params.set("document", String(want.document));
    const query = params.toString();
    return json<View>(`/v1/brains/view${query ? `?${query}` : ""}`);
  },

  list: () => json<Brain[]>("/v1/brains"),
  create: (b: { name: string; description: string; locked: boolean }) =>
    json<Brain>("/v1/brains", send("POST", b)),
  update: (
    id: number,
    b: { name: string; description: string; locked: boolean },
  ) => json<Brain>(`/v1/brains/${id}`, send("PUT", b)),
  remove: (id: number) => nothing(`/v1/brains/${id}`, { method: "DELETE" }),

  categories: (brainID: number) =>
    json<Category[]>(`/v1/brains/${brainID}/categories`),
  createCategory: (brainID: number, c: { name: string; description: string }) =>
    json<Category>(`/v1/brains/${brainID}/categories`, send("POST", c)),
  updateCategory: (id: number, c: { name: string; description: string }) =>
    json<Category>(`/v1/brain-categories/${id}`, send("PUT", c)),
  removeCategory: (id: number) =>
    nothing(`/v1/brain-categories/${id}`, { method: "DELETE" }),

  documents: (categoryID: number) =>
    json<Document[]>(`/v1/brain-categories/${categoryID}/documents`),
  document: (id: number) => json<Document>(`/v1/brain-documents/${id}`),
  createDocument: (
    categoryID: number,
    d: { title: string; content: string; related: number[] },
  ) =>
    json<Document>(
      `/v1/brain-categories/${categoryID}/documents`,
      send("POST", d),
    ),
  updateDocument: (
    id: number,
    d: {
      title: string;
      content: string;
      category_id: number;
      related: number[];
    },
  ) => json<Document>(`/v1/brain-documents/${id}`, send("PUT", d)),
  removeDocument: (id: number) =>
    nothing(`/v1/brain-documents/${id}`, { method: "DELETE" }),

  /**
   * Full text search over every document of every brain in the workspace.
   *
   * No brain is named deliberately, though the endpoint takes one: not knowing
   * which brain a document is in is the reason to search at all, and the screen
   * narrows its panes to where the matches turned out to live.
   *
   * The limit is asked for rather than left to the server, because the screen
   * has to know whether it is looking at every match: it compares what came
   * back with what it asked for, and says so when the two are equal.
   */
  search: (query: string, limit: number = SEARCH_LIMIT) => {
    const params = new URLSearchParams({ q: query, limit: String(limit) });
    return json<Hit[]>(`/v1/brains/search?${params}`);
  },
};
