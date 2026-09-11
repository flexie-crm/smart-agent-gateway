import { useCallback, useEffect, useRef, useState } from "react";
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
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { brains, documentCount } from "@/lib/brains";
import { describeError } from "@/lib/form";
import { useNotify } from "@/lib/notify";
import type { Brain, Category, Document, Selection, View } from "@/lib/brains";
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
  const asked = useRef(0);
  const show = useCallback(async (want: Selection = {}) => {
    const question = ++asked.current;
    const next = await brains.view(want);
    if (question === asked.current) setView(next);
  }, []);

  useEffect(() => {
    void show({});
  }, [show]);

  // The view we hold IS the selection: the ids come back resolved, so the panes
  // never highlight a row the server did not open.
  const brainID = view?.brain_id ?? 0;
  const categoryID = view?.category_id ?? 0;
  const documentID = view?.document_id ?? 0;

  const expanded = expandedDocument !== 0 && expandedDocument === documentID;

  const allBrains = view?.brains ?? [];
  const shownCategories = view?.categories ?? [];
  const shownDocuments = view?.documents ?? [];
  const shownDocument = view?.document ?? null;
  const selectedBrain = allBrains.find((brain) => brain.id === brainID);

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
      <div className="grid h-full grid-cols-[minmax(0,17rem)_minmax(0,19rem)_minmax(0,1fr)]">
        {/* Brains */}
        <Pane title="Brains" onAdd={() => setEditing({ kind: "brain" })}>
          {loading ? null : allBrains.length === 0 ? (
            <Empty>
              No brain yet. An agent with none knows nothing but its
              instructions.
            </Empty>
          ) : (
            allBrains.map((brain) => (
              <Row
                key={brain.id}
                selected={brain.id === brainID}
                onClick={() => void show({ brain: brain.id })}
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
                meta={documentCount(brain.documents)}
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
            <Empty>No category yet.</Empty>
          ) : (
            shownCategories.map((category) => (
              <Row
                key={category.id}
                selected={category.id === categoryID}
                onClick={() =>
                  void show({ brain: brainID, category: category.id })
                }
                icon={
                  <Folder className="size-4 shrink-0 text-muted-foreground" />
                }
                title={category.name}
                subtitle={category.description}
                meta={documentCount(category.documents)}
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
                    expanded ? "flex-1" : "max-h-[50%] border-b border-border",
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
                    <Empty>No document in this category.</Empty>
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
                            doc.id === documentID ? "bg-accent" : "hover:bg-accent",
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
          categories={shownCategories}
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

function Pane({
  title,
  onAdd,
  children,
}: {
  title: string;
  onAdd?: () => void;
  children: React.ReactNode;
}) {
  return (
    <div className="flex min-h-0 flex-col border-r border-border">
      <header className="flex h-11 shrink-0 items-center justify-between border-b border-border px-4">
        <h2 className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
          {title}
        </h2>
        {onAdd && (
          <button
            type="button"
            onClick={onAdd}
            className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
            aria-label={`New ${title.toLowerCase().replace(/s$/, "")}`}
          >
            <Plus className="size-4" />
          </button>
        )}
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto">{children}</div>
    </div>
  );
}

/**
 * Row is one item in a pane. The edit and delete actions appear on hover: they
 * are always reachable but never in the way of the thing you came here to read.
 */
function Row({
  selected,
  onClick,
  icon,
  title,
  subtitle,
  meta,
  onEdit,
  onDelete,
  confirm,
  deleted,
}: {
  selected: boolean;
  onClick: () => void;
  icon: React.ReactNode;
  title: React.ReactNode;
  subtitle?: string;
  meta?: string;
  onEdit: () => void;
  onDelete: () => Promise<void>;
  /** The question asked before deleting. Say what will be lost. */
  confirm: string;
  /** What is said afterwards. A row vanishing is not a confirmation. */
  deleted: string;
}) {
  const notify = useNotify();
  return (
    <div
      className={cn(
        "group relative flex cursor-pointer gap-2.5 border-b border-border/60 px-4 py-3 transition-colors",
        // The selected row carries an accent bar on its right edge, the way the
        // CRM marks the open item: the tint alone reads as hover, the bar reads
        // as "you are here".
        selected
          ? "bg-accent after:absolute after:inset-y-0 after:right-0 after:w-[3px] after:bg-primary"
          : "hover:bg-accent",
      )}
      onClick={onClick}
    >
      {/* The icon centers on the TITLE'S line, not on the row: next to a
          multi-line row it would float between the lines, which reads as
          misaligned next to every one of them. The slot is one title line
          tall (h-5 matches the text-sm leading) and the icon centers in it. */}
      <span className="flex h-5 shrink-0 items-center">{icon}</span>
      <div className="min-w-0 flex-1">
        <div className="truncate text-sm font-medium">{title}</div>
        {subtitle && (
          <p className="mt-0.5 line-clamp-2 text-xs leading-snug text-muted-foreground">
            {subtitle}
          </p>
        )}
        {meta && <p className="mt-1 text-xs text-muted-foreground">{meta}</p>}
      </div>

      <div className="absolute right-2 top-2 hidden gap-1 group-hover:flex">
        <IconButton
          label="Edit"
          onClick={(e) => {
            e.stopPropagation();
            onEdit();
          }}
        >
          <Pencil className="size-3.5" />
        </IconButton>
        <IconButton
          label="Delete"
          destructive
          onClick={async (e) => {
            e.stopPropagation();
            // Deleting a brain takes its categories and every document in them.
            // That is worth one question.
            if (
              !(await notify.confirm({
                title: confirm,
                confirmLabel: "Delete",
                destructive: true,
              }))
            )
              return;
            try {
              await onDelete();
              notify.success(deleted);
            } catch (failure) {
              notify.error(describeError(failure));
            }
          }}
        >
          <Trash2 className="size-3.5" />
        </IconButton>
      </div>
    </div>
  );
}

function IconButton({
  label,
  destructive,
  onClick,
  children,
}: {
  label: string;
  destructive?: boolean;
  onClick: (event: React.MouseEvent) => void | Promise<void>;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      onClick={onClick}
      className={cn(
        "rounded-md bg-card p-1.5 text-muted-foreground shadow-sm transition-colors",
        destructive
          ? "hover:bg-destructive hover:text-white"
          : "hover:bg-accent hover:text-foreground",
      )}
    >
      {children}
    </button>
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

function Empty({ children }: { children: React.ReactNode }) {
  return (
    <p className="px-4 py-8 text-center text-xs text-muted-foreground">
      {children}
    </p>
  );
}
