import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Brain as BrainIcon,
  ChevronDown,
  FileText,
  Folder,
  Link2,
  Lock,
  Pencil,
  Plus,
  Trash2,
} from "lucide-react";
import { Markdown } from "@/components/Markdown";
import { Page } from "@/components/AppShell";
import { Empty, Pane, Row, SearchBand } from "@/components/Pane";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { attribution, brains, documentCount } from "@/lib/brains";
import { matchCount, summarise } from "@/lib/search";
import { describeError } from "@/lib/form";
import { useNotify } from "@/lib/notify";
import type {
  Brain,
  Category,
  Document,
  Hit,
  Selection,
  View,
} from "@/lib/brains";
import { BrainForm, CategoryForm, DocumentForm } from "./BrainForms";

/**
 * Brains: three panes from one answer.
 *
 * The screen lives at /brains, full stop. Every click asks the server for the
 * view it wants (a brain, a category, a document, all optional hints), the
 * server resolves the whole picture in one round trip, and the screen paints
 * it. Selection is screen state, the same model the CRM's screens use: one
 * address, one request, one paint.
 */
export function Brains() {
  // The whole screen in one answer. The panes used to be four requests, each
  // waiting on the one before it to know what to ask for, so the page painted
  // its columns empty and filled them in one at a time. Now it paints once,
  // complete: brains, the categories of the open brain, the documents of the
  // open category, and the document itself, content and all.
  const [view, setView] = useState<View | null>(null);
  const notify = useNotify();
  // Expansion belongs to the document it was asked for. Keeping it as a flag and
  // resetting it when the document changes would be state chasing state; here a
  // different document is simply not the one that was expanded.
  const [expandedDocument, setExpandedDocument] = useState(0);

  // The search. `query` is what has been typed and `hits` is the last answer
  // the server gave; null means there is no answer to narrow by, which is a
  // different thing from an answer that found nothing.
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<Hit[] | null>(null);
  // Whether a search is on is the BOX, not the answer: an empty box is the
  // whole knowledge base, whatever the last answer happened to say.
  const searching = query.trim() !== "";

  const [editing, setEditing] = useState<
    | { kind: "brain"; brain?: Brain }
    | { kind: "category"; category?: Category }
    | { kind: "document"; document?: Document }
    | null
  >(null);

  // show asks the server for a selection and paints the answer. Only the
  // latest question may paint: click twice quickly and the slower answer must
  // not overwrite the faster one. The previous answer stays on screen until
  // the new one lands, so the panes never blank between clicks.
  //
  // A failure is said, as the search's is, and only by the latest question.
  // Every caller throws this promise away, so a failure left uncaught here had
  // nowhere to go: the page stayed as it was, or blank on the first load, and
  // nothing said why. In state rather than a toast, because notify changes
  // whenever a confirmation opens, and a show that changed with it would reload
  // the panes every time one did.
  const asked = useRef(0);
  const [trouble, setTrouble] = useState("");
  const show = useCallback((want: Selection = {}) => {
    const question = ++asked.current;
    return brains.view(want).then(
      (next) => {
        if (question !== asked.current) return;
        setView(next);
        setTrouble("");
      },
      (failure: unknown) => {
        if (question === asked.current) setTrouble(describeError(failure));
      },
    );
  }, []);

  useEffect(() => {
    void show({});
  }, [show]);

  // Search as it is typed, debounced: a keystroke is not a question, a pause
  // is. Only the latest answer may land, the same rule the view follows, since
  // a slower answer to a shorter query would narrow the panes by a word the
  // person has already finished typing.
  //
  // An empty box asks nothing and CLEARS nothing here. What is held is the last
  // answer, and whether a search is running is read off the box below, so
  // clearing it needs no state to be put back: state chasing state is how a
  // screen ends up filtered by a word nobody can see.
  const searched = useRef(0);
  useEffect(() => {
    const text = query.trim();
    const question = ++searched.current;
    if (text === "") return;
    const timer = window.setTimeout(() => {
      void (async () => {
        try {
          const found = await brains.search(text);
          if (question === searched.current) setHits(found);
        } catch (failure) {
          // A search that failed narrows NOTHING: the answer is thrown away, so
          // the panes hold the whole knowledge base and the toast says why.
          // Keeping the previous answer would filter them by a word that is no
          // longer in the box, and an empty one would claim there are no
          // matches, which is the thing we just failed to find out.
          if (question === searched.current) setHits(null);
          notify.error(describeError(failure));
        }
      })();
    }, 250);
    return () => window.clearTimeout(timer);
  }, [query, notify]);

  // The view we hold IS the selection: the ids come back resolved, so the panes
  // never highlight a row the server did not open.
  const brainID = view?.brain_id ?? 0;
  const categoryID = view?.category_id ?? 0;
  const documentID = view?.document_id ?? 0;

  const allBrains = view?.brains ?? [];
  const allCategories = view?.categories ?? [];
  const allDocuments = view?.documents ?? [];
  const selectedBrain = allBrains.find((brain) => brain.id === brainID);

  // What the search narrowed the screen to: how many matches each brain and
  // each category holds, and which documents matched. null means no search,
  // and then every pane lists everything it has.
  const matched = useMemo(() => {
    if (!searching || hits === null) return null;
    const inBrain = new Map<number, number>();
    const inCategory = new Map<number, number>();
    const documents = new Set<number>();
    for (const hit of hits) {
      inBrain.set(hit.brain_id, (inBrain.get(hit.brain_id) ?? 0) + 1);
      inCategory.set(
        hit.category_id,
        (inCategory.get(hit.category_id) ?? 0) + 1,
      );
      documents.add(hit.document_id);
    }
    return { brains: inBrain, categories: inCategory, documents };
  }, [searching, hits]);

  // Each pane lists what matched; the full lists stay, because a form that
  // moves a document between categories has to offer all of them, not the two
  // a search happens to be pointing at.
  const shownBrains = matched
    ? allBrains.filter((brain) => matched.brains.has(brain.id))
    : allBrains;
  const shownCategories = matched
    ? allCategories.filter((category) => matched.categories.has(category.id))
    : allCategories;
  const shownDocuments = matched
    ? allDocuments.filter((document) => matched.documents.has(document.id))
    : allDocuments;

  // A search opens its best match when what is on screen is not one of them.
  //
  // The panes narrow to what matched, so a selection that is not among the hits
  // would leave all three highlighting a row they no longer show. Once per
  // answer, by identity: a view that comes back pointing somewhere else (the
  // document was deleted between the search and this) must not become the same
  // question asked forever.
  const opened = useRef<Hit[] | null>(null);
  useEffect(() => {
    if (!searching || hits === null || hits.length === 0) return;
    if (opened.current === hits) return;
    opened.current = hits;
    if (hits.some((hit) => hit.document_id === documentID)) return;
    const best = hits[0];
    void show({
      brain: best.brain_id,
      category: best.category_id,
      document: best.document_id,
    });
  }, [searching, hits, documentID, show]);

  // The reader shows the open document only while it is one of the ANSWERS.
  //
  // A search that matched nothing used to leave the last document standing
  // there in full, with all three panes around it saying they had no match: the
  // screen showed a document and denied having one at the same time.
  const shownDocument =
    matched && !matched.documents.has(documentID)
      ? null
      : (view?.document ?? null);

  // Expansion is the reader's, so it cannot outlive it: expanded with no reader
  // hides the list as well, and the column would hold nothing at all.
  const expanded =
    shownDocument !== null &&
    expandedDocument !== 0 &&
    expandedDocument === documentID;

  // Where a click lands while a search is on.
  //
  // Clicking a brain or a category that matched has to open something that
  // MATCHED inside it. Left to the server, the click opens the first document
  // the brain holds, which the narrowed list may not be showing at all, and a
  // pane that lists three rows while highlighting none of them is a pane that
  // has stopped saying where you are.
  const bestIn = (of: "brain_id" | "category_id", id: number) =>
    matched ? hits?.find((hit) => hit[of] === id) : undefined;

  // What the band says about the answer, and nothing until one lands: a count
  // is a claim about the data, and the data has not spoken yet.
  const summary = searching ? summarise(hits) : "";

  // After a write, the screen is asked again rather than patched. The relations
  // here are symmetric (saving one document changes another), and a console that
  // edits its own copy eventually shows something the server does not agree with.
  const reload = useCallback(
    () =>
      show({
        brain: brainID || undefined,
        category: categoryID || undefined,
        document: documentID || undefined,
      }),
    [show, brainID, categoryID, documentID],
  );

  // The frame never waits for the answer. The three panes and their titles are
  // the screen's chrome, known before the server says anything; swapping them
  // for a loading state and back is a layout shift on every remount (a
  // workspace switch remounts the whole tree). Only rows are data: while the
  // answer is on its way the panes are simply empty, and an empty-state
  // message is a claim about the data, so it waits until the data has spoken.
  const loading = view === null;

  return (
    <Page
      flush
      title="Brains"
      description="What the agent knows, and what it may write back to."
      actions={
        <Button size="sm" onClick={() => setEditing({ kind: "brain" })}>
          <Plus className="size-4" />
          New brain
        </Button>
      }
    >
      <div className="flex h-full min-h-0 flex-col">
        {/* Full text search over every document of every brain. Typing narrows
            all three panes to what matched and opens the best match, so the
            search answers WHERE a document lives as well as whether it
            exists. */}
        <SearchBand
          value={query}
          onChange={setQuery}
          placeholder="Search every document in every brain"
          label="Search the knowledge base"
          summary={summary}
        />

        <div className="grid min-h-0 flex-1 grid-cols-[minmax(0,17rem)_minmax(0,19rem)_minmax(0,1fr)]">
          {/* Brains */}
          <Pane title="Brains" onAdd={() => setEditing({ kind: "brain" })}>
            {/* Above the list rather than instead of it: the rows are what
                somebody clicks to try again. */}
            {trouble !== "" && <Empty>{trouble}</Empty>}
            {loading ? null : shownBrains.length === 0 ? (
              <Empty>
                {matched ? (
                  // Nothing in the workspace matched, so this is the pane that
                  // says so: the two to the right are empty as a consequence.
                  <>No document matches &ldquo;{query.trim()}&rdquo;.</>
                ) : (
                  <>
                    No brain yet. An agent with none knows nothing but its
                    instructions.
                  </>
                )}
              </Empty>
            ) : (
              shownBrains.map((brain) => (
                <Row
                  key={brain.id}
                  selected={brain.id === brainID}
                  onClick={() => {
                    const hit = bestIn("brain_id", brain.id);
                    void show(
                      hit
                        ? {
                            brain: hit.brain_id,
                            category: hit.category_id,
                            document: hit.document_id,
                          }
                        : { brain: brain.id },
                    );
                  }}
                  icon={<BrainIcon className="size-4 shrink-0 text-primary" />}
                  title={
                    <span className="flex items-center gap-1.5">
                      {brain.name}
                      {brain.locked && (
                        <Lock
                          className="size-3 text-muted-foreground"
                          aria-label="Read-only to agents"
                        />
                      )}
                    </span>
                  }
                  subtitle={brain.description}
                  meta={
                    matched
                      ? matchCount(matched.brains.get(brain.id) ?? 0)
                      : documentCount(brain.documents)
                  }
                  onEdit={() => setEditing({ kind: "brain", brain })}
                  onDelete={async () => {
                    await brains.remove(brain.id);
                    // Deleting the open brain leaves nothing to point at, so the
                    // server picks the first of everything again.
                    await (brain.id === brainID ? show({}) : reload());
                  }}
                  confirm={`Delete "${brain.name}" and everything in it?`}
                  deleted={`${brain.name} was deleted, and everything in it.`}
                />
              ))
            )}
          </Pane>

          {/* Categories */}
          <Pane
            title="Categories"
            onAdd={
              selectedBrain ? () => setEditing({ kind: "category" }) : undefined
            }
          >
            {loading ? null : !selectedBrain ? (
              <Empty>Choose a brain.</Empty>
            ) : shownCategories.length === 0 ? (
              <Empty>
                {matched ? "No match in this brain." : "No category yet."}
              </Empty>
            ) : (
              shownCategories.map((category) => (
                <Row
                  key={category.id}
                  selected={category.id === categoryID}
                  onClick={() => {
                    const hit = bestIn("category_id", category.id);
                    void show({
                      brain: brainID,
                      category: category.id,
                      document: hit?.document_id,
                    });
                  }}
                  icon={
                    <Folder className="size-4 shrink-0 text-muted-foreground" />
                  }
                  title={category.name}
                  subtitle={category.description}
                  meta={
                    matched
                      ? matchCount(matched.categories.get(category.id) ?? 0)
                      : documentCount(category.documents)
                  }
                  onEdit={() => setEditing({ kind: "category", category })}
                  onDelete={async () => {
                    await brains.removeCategory(category.id);
                    await (category.id === categoryID
                      ? show({ brain: brainID })
                      : reload());
                  }}
                  confirm={`Delete "${category.name}" and its documents?`}
                  deleted={`${category.name} was deleted, and its documents.`}
                />
              ))
            )}
          </Pane>

          {/* Documents: the open one, rendered, then the rest of the category */}
          <div className="flex min-h-0 flex-col">
            <header className="flex h-11 shrink-0 items-center justify-between border-b border-border px-4">
              <h2 className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
                Documents
              </h2>
              {categoryID !== 0 && (
                <button
                  type="button"
                  onClick={() => setEditing({ kind: "document" })}
                  className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
                  aria-label="New document"
                >
                  <Plus className="size-4" />
                </button>
              )}
            </header>

            {/* The reader sits over the list, the way the CRM lays this out: the
                open document takes up to half the room and scrolls inside itself,
                the bar under it carries what you do TO the document, and the rest
                of the category fills the bottom. Expanded, the reader takes the
                whole room, the list hides, and the bar rests on the page bottom. */}
            {loading ? (
              <div className="min-h-0 flex-1" />
            ) : !categoryID ? (
              <div className="min-h-0 flex-1 overflow-y-auto">
                <Empty>Choose a category.</Empty>
              </div>
            ) : (
              <div className="flex min-h-0 flex-1 flex-col">
                {shownDocument && (
                  <div
                    className={cn(
                      "flex min-h-0 flex-col",
                      expanded
                        ? "flex-1"
                        : "max-h-[50%] border-b border-border",
                    )}
                  >
                    <article className="min-h-0 flex-1 overflow-y-auto px-6 py-5">
                      <h3 className="mb-3 text-base font-semibold">
                        {shownDocument.title}
                      </h3>
                      {shownDocument.content ? (
                        <Markdown>{shownDocument.content}</Markdown>
                      ) : (
                        <p className="text-sm text-muted-foreground">
                          This document is empty.
                        </p>
                      )}
                      {/* Who wrote it, under it rather than in the header: it
                          is the provenance of what you have just read, and an
                          agent writes here as well as a person. Absent for
                          anything written before this was recorded, which is
                          honest: a blank says so and a guess would not. */}
                      {attribution(shownDocument) !== "" && (
                        <p className="mt-6 border-t border-border pt-3 text-xs text-muted-foreground">
                          {attribution(shownDocument)}
                        </p>
                      )}
                    </article>

                    <div className="flex shrink-0 items-center justify-between gap-3 bg-primary px-3 py-1.5 text-primary-foreground">
                      <div className="flex items-center gap-1">
                        <BarButton
                          label="Edit"
                          onClick={() =>
                            setEditing({
                              kind: "document",
                              document: shownDocument,
                            })
                          }
                        >
                          <Pencil className="size-4" />
                        </BarButton>
                        <BarButton
                          label="Delete"
                          onClick={async () => {
                            if (
                              !(await notify.confirm({
                                title: `Delete "${shownDocument.title}"?`,
                                confirmLabel: "Delete",
                                destructive: true,
                              }))
                            )
                              return;
                            const gone = shownDocument.title;
                            await brains.removeDocument(shownDocument.id);
                            await show({
                              brain: brainID,
                              category: categoryID,
                            });
                            notify.success(`${gone} was deleted.`);
                          }}
                        >
                          <Trash2 className="size-4" />
                        </BarButton>

                        {/* The relations live in the edit modal; here they are
                            only a count, so the reader stays a reader. */}
                        <span
                          className="ml-1 flex items-center gap-1 rounded-full bg-white/20 px-2 py-0.5 text-xs font-medium"
                          title={
                            shownDocument.related.length === 1
                              ? "1 related document"
                              : `${shownDocument.related.length} related documents`
                          }
                        >
                          <Link2 className="size-3" />
                          {shownDocument.related.length}
                        </span>
                      </div>

                      <button
                        type="button"
                        onClick={() =>
                          setExpandedDocument(expanded ? 0 : documentID)
                        }
                        className="flex items-center gap-1 rounded px-2 py-0.5 text-sm font-medium transition-colors hover:bg-white/15"
                      >
                        <ChevronDown
                          className={cn(
                            "size-4 transition-transform",
                            expanded && "rotate-180",
                          )}
                        />
                        {expanded ? "Show document list" : "Show full document"}
                      </button>
                    </div>
                  </div>
                )}

                {!expanded && (
                  <ul className="min-h-0 flex-1 overflow-y-auto p-2">
                    {shownDocuments.length === 0 ? (
                      <Empty>
                        {matched
                          ? "No match in this category."
                          : "No document in this category."}
                      </Empty>
                    ) : (
                      shownDocuments.map((doc) => (
                        <li key={doc.id}>
                          <button
                            type="button"
                            onClick={() =>
                              void show({
                                brain: brainID,
                                category: categoryID,
                                document: doc.id,
                              })
                            }
                            className={cn(
                              "flex w-full items-center gap-2 rounded-md px-3 py-1.5 text-left text-sm transition-colors",
                              doc.id === documentID
                                ? "bg-accent"
                                : "hover:bg-accent",
                            )}
                          >
                            <FileText className="size-3.5 shrink-0 text-muted-foreground" />
                            <span className="min-w-0 flex-1 truncate">
                              {doc.title}
                            </span>
                            {doc.related.length > 0 && (
                              <span className="shrink-0 text-xs text-muted-foreground">
                                {doc.related.length}
                              </span>
                            )}
                          </button>
                        </li>
                      ))
                    )}
                  </ul>
                )}
              </div>
            )}
          </div>
        </div>
      </div>

      {editing?.kind === "brain" && (
        <BrainForm
          brain={editing.brain}
          onClose={() => setEditing(null)}
          onSaved={async () => {
            setEditing(null);
            await reload();
          }}
        />
      )}
      {editing?.kind === "category" && selectedBrain && (
        <CategoryForm
          brain={selectedBrain}
          brains={allBrains}
          category={editing.category}
          onClose={() => setEditing(null)}
          onSaved={async () => {
            setEditing(null);
            await reload();
          }}
        />
      )}
      {editing?.kind === "document" && selectedBrain && categoryID !== 0 && (
        <DocumentForm
          brain={selectedBrain}
          categories={allCategories}
          categoryID={categoryID}
          document={editing.document}
          onClose={() => setEditing(null)}
          onSaved={async (saved) => {
            setEditing(null);
            // The saved document may have moved category; open it where it
            // now lives.
            await show({
              brain: brainID,
              category: saved.category_id,
              document: saved.id,
            });
          }}
        />
      )}
    </Page>
  );
}

/** BarButton is an action on the open document, in the bar under it. */
function BarButton({
  label,
  onClick,
  children,
}: {
  label: string;
  onClick: () => void | Promise<void>;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      onClick={() => void onClick()}
      className="rounded p-1.5 transition-colors hover:bg-white/20"
    >
      {children}
    </button>
  );
}
