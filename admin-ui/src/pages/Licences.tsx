import { useEffect, useMemo, useState } from "react";
import { ChevronRight, ExternalLink, Search } from "lucide-react";
import { Page } from "@/components/AppShell";
import { apiFetch } from "@/lib/api";
import { cn } from "@/lib/utils";

/**
 * Open source: what this product carries that somebody else wrote.
 *
 * The screen exists because attribution has to ARRIVE somewhere. MIT and BSD
 * both ask only that their notice travels with the software, and a notice in a
 * repository nobody installing the product will read has not travelled anywhere.
 * This is where it lands, in both editions and under both licences: the same
 * obligation applies whether somebody is running the AGPL build or has bought a
 * commercial licence.
 *
 * One request, one answer, and no state on the server. The list is compiled into
 * the binary (internal/licences), so it cannot disagree with what is actually
 * running, and it is generated from the manifests rather than kept by hand.
 */

type Component = {
  name: string;
  version?: string;
  part: string;
  licence?: string;
  text?: string;
  url?: string;
  note?: string;
};

async function fetchLicences(): Promise<Component[]> {
  const res = await apiFetch("/v1/licences");
  if (!res.ok) throw new Error("licences");
  const body = (await res.json()) as { components: Component[] };
  return body.components ?? [];
}

export function Licences() {
  const [components, setComponents] = useState<Component[] | null>(null);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState<string>("");

  useEffect(() => {
    let current = true;
    fetchLicences()
      .then((list) => {
        if (!current) return;
        setComponents(list);
        setError("");
      })
      .catch(() => {
        if (current) setError("The list of components could not be read.");
      });
    return () => {
      current = false;
    };
  }, []);

  // Filtering is done here rather than by asking again: the whole list is a few
  // hundred rows and it is already in hand.
  const shown = useMemo(() => {
    if (!components) return [];
    const needle = query.trim().toLowerCase();
    if (!needle) return components;
    return components.filter(
      (c) =>
        c.name.toLowerCase().includes(needle) ||
        (c.licence ?? "").toLowerCase().includes(needle) ||
        c.part.toLowerCase().includes(needle),
    );
  }, [components, query]);

  // Grouped by where it is used, because that is the question a reader has:
  // what is inside the thing I installed.
  const groups = useMemo(() => {
    const by = new Map<string, Component[]>();
    for (const c of shown) {
      const list = by.get(c.part) ?? [];
      list.push(c);
      by.set(c.part, list);
    }
    return [...by.entries()].sort(([a], [b]) => a.localeCompare(b));
  }, [shown]);

  return (
    <Page
      title="Open source"
      description="The open source this product is built on, and the licence each part travels under."
    >
      {error && <p className="text-sm text-destructive">{error}</p>}

      {!components && !error && (
        <p className="text-sm text-muted-foreground">Reading the list…</p>
      )}

      {components && (
        <>
          <div className="flex items-center gap-4 border-b border-border pb-4">
            <div className="relative flex-1 max-w-sm">
              <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              <input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="Search by name or licence"
                aria-label="Search the components"
                className="h-9 w-full rounded-md border border-input bg-background pl-9 pr-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
              />
            </div>
            <p className="text-sm text-muted-foreground">
              {shown.length} of {components.length}
            </p>
          </div>

          {groups.map(([part, list]) => (
            // The rule separates one group from the next, so the last one has
            // nothing to separate itself from and does not draw one.
            <section key={part} className="border-b border-border py-6 last:border-b-0">
              <h2 className="mb-1 text-sm font-medium">{part}</h2>
              <p className="mb-4 text-sm text-muted-foreground">
                {list.length} component{list.length === 1 ? "" : "s"}
              </p>

              <ul>
                {list.map((c) => {
                  const id = `${c.part}/${c.name}`;
                  const isOpen = open === id;
                  return (
                    <li key={id} className="border-t border-border first:border-t-0">
                      <button
                        type="button"
                        onClick={() => setOpen(isOpen ? "" : id)}
                        aria-expanded={isOpen}
                        className="flex w-full cursor-pointer items-center gap-3 py-2.5 text-left"
                      >
                        <ChevronRight
                          className={cn(
                            "size-4 shrink-0 text-muted-foreground transition-transform",
                            isOpen && "rotate-90",
                          )}
                        />
                        <span className="min-w-0 flex-1 truncate text-sm">{c.name}</span>
                        {c.version && (
                          <span className="shrink-0 font-mono text-xs text-muted-foreground">
                            {c.version}
                          </span>
                        )}
                        <span className="w-40 shrink-0 text-right text-xs text-muted-foreground">
                          {c.licence || "see the text"}
                        </span>
                      </button>

                      {isOpen && (
                        <div className="pb-5 pl-7">
                          {c.note && (
                            <p className="mb-3 max-w-3xl text-sm text-muted-foreground">
                              {c.note}
                            </p>
                          )}
                          {c.url && (
                            <a
                              href={c.url}
                              target="_blank"
                              rel="noreferrer noopener"
                              className="mb-3 inline-flex cursor-pointer items-center gap-1.5 text-sm underline underline-offset-4"
                            >
                              {c.url}
                              <ExternalLink className="size-3.5" />
                            </a>
                          )}
                          {c.text ? (
                            // The licence in full, as its author wrote it. It is
                            // pre-formatted because these texts are laid out with
                            // spaces and reflowing one changes what it looks like
                            // it says.
                            <pre className="max-w-3xl overflow-x-auto whitespace-pre-wrap break-words rounded-md border border-border bg-muted/40 p-4 font-mono text-xs leading-relaxed">
                              {c.text}
                            </pre>
                          ) : (
                            <p className="text-sm text-muted-foreground">
                              This package ships no licence file. Its terms are
                              published where the project is.
                            </p>
                          )}
                        </div>
                      )}
                    </li>
                  );
                })}
              </ul>
            </section>
          ))}

          {groups.length === 0 && (
            <p className="py-6 text-sm text-muted-foreground">
              Nothing matches “{query}”.
            </p>
          )}
        </>
      )}
    </Page>
  );
}
