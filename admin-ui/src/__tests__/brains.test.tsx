import { StrictMode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@/test-utils";
import { Brains } from "@/pages/Brains";
import { SEARCH_LIMIT } from "@/lib/search";
import type { Hit, View } from "@/lib/brains";

// The brains screen is one address and one answer: the server resolves what to
// show, the screen paints it, and every click is exactly one question. These
// tests pin the "exactly one": development mounts the screen twice on purpose,
// and that must never reach the server as two requests.

const BRAINS = [
  {
    id: 1,
    name: "Platform",
    slug: "platform",
    description: "",
    locked: false,
    categories: 2,
    documents: 3,
    created_by: 0,
    created_by_name: "",
    updated_by: 0,
    updated_by_name: "",
  },
  {
    id: 2,
    name: "Sales",
    slug: "sales",
    description: "",
    locked: false,
    categories: 0,
    documents: 0,
    created_by: 0,
    created_by_name: "",
    updated_by: 0,
    updated_by_name: "",
  },
];

const CATEGORIES = [
  {
    id: 10,
    brain_id: 1,
    name: "Migrations",
    description: "",
    weight: 0,
    documents: 2,
    created_by: 0,
    created_by_name: "",
    updated_by: 0,
    updated_by_name: "",
  },
  {
    id: 11,
    brain_id: 1,
    name: "Jobs",
    description: "",
    weight: 0,
    documents: 1,
    created_by: 0,
    created_by_name: "",
    updated_by: 0,
    updated_by_name: "",
  },
];

const DOCUMENTS: Record<
  number,
  { id: number; title: string; content: string }[]
> = {
  10: [
    { id: 100, title: "How the schema changes", content: "Alpha body" },
    { id: 101, title: "The destructive guard", content: "Gamma body" },
  ],
  11: [{ id: 110, title: "Job ownership", content: "Beta body" }],
};

/** Answers /v1/brains/view the way the server does: hints in, whole screen out. */
function answer(url: string): View {
  const params = new URL(url, "http://sag.test").searchParams;
  const brainID = Number(params.get("brain")) || 1;
  const categoryID = Number(params.get("category")) || CATEGORIES[0].id;
  const docs = DOCUMENTS[categoryID] ?? [];
  const documentID = Number(params.get("document")) || docs[0]?.id || 0;
  const open = docs.find((d) => d.id === documentID) ?? null;
  return {
    brains: BRAINS,
    brain_id: brainID,
    categories: CATEGORIES,
    category_id: categoryID,
    documents: docs.map((d) => ({
      ...d,
      brain_id: 1,
      category_id: categoryID,
      weight: 0,
      related: [],
      created_by: 3,
      created_by_name: "A Curator",
      updated_by: 0,
      updated_by_name: "Research agent",
    })),
    document_id: documentID,
    document: open
      ? {
          ...open,
          brain_id: 1,
          category_id: categoryID,
          weight: 0,
          related: [],
          created_by: 3,
          created_by_name: "A Curator",
          updated_by: 0,
          updated_by_name: "Research agent",
        }
      : null,
  };
}

function serveViews() {
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (!url.includes("/v1/brains/view"))
      throw new Error(`unexpected request: ${url}`);
    return new Response(JSON.stringify(answer(url)), { status: 200 });
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

/** One hit: the "Job ownership" document, which is not the one that opens. */
const JOBS_HIT: Hit = {
  document_id: 110,
  brain_id: 1,
  brain: "Platform",
  category_id: 11,
  category: "Jobs",
  title: "Job ownership",
  snippet: "Beta body",
  score: 1.5,
};

/**
 * Views, and searches answered with `hits`.
 *
 * The two are counted apart, because what matters about this screen is how many
 * questions a keystroke costs: a search per pause, and a view only when the
 * answer points somewhere the panes are not already looking.
 */
function serveSearch(hits: Hit[]) {
  const searches: URL[] = [];
  const views: URL[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), "http://sag.test");
    if (url.pathname === "/v1/brains/search") {
      searches.push(url);
      return new Response(JSON.stringify(hits), { status: 200 });
    }
    if (url.pathname === "/v1/brains/view") {
      views.push(url);
      return new Response(JSON.stringify(answer(String(input))), {
        status: 200,
      });
    }
    throw new Error(`unexpected request: ${url}`);
  });
  vi.stubGlobal("fetch", fetch);
  return { fetch, searches, views };
}

/** The search box, addressed the way a person reaches it: by what it is for. */
const searchBox = () => screen.getByLabelText("Search the knowledge base");

describe("the brains screen", () => {
  afterEach(() => vi.unstubAllGlobals());

  // A view that fails is said, not dropped: every caller of show throws its
  // promise away, so a failure used to leave the page as it was, or blank on
  // the first load, with nothing to say why.
  it("says why when the gateway does not answer", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => {
      throw new TypeError("Failed to fetch");
    }));
    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    expect(await screen.findByText("The gateway did not answer.")).toBeInTheDocument();
  });

  // And a click that fails leaves the panes usable, so the next click can put
  // things right, and that answer takes the message away.
  it("keeps the panes after a failed click and clears the failure on the next answer", async () => {
    serveViews();
    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Alpha body");

    vi.stubGlobal("fetch", vi.fn(async () => {
      throw new TypeError("Failed to fetch");
    }));
    fireEvent.click(screen.getByText("Platform"));
    expect(await screen.findByText("The gateway did not answer.")).toBeInTheDocument();
    expect(screen.getByText("Platform")).toBeInTheDocument();

    serveViews();
    fireEvent.click(screen.getByText("Platform"));
    await waitFor(() =>
      expect(screen.queryByText("The gateway did not answer.")).not.toBeInTheDocument(),
    );
  });

  it("draws its frame before the server answers, so nothing shifts when it does", async () => {
    // An answer held back, so the test can look at the screen mid-flight.
    let respond!: (r: Response) => void;
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>((r) => (respond = r))),
    );

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );

    // The panes and their titles are chrome, not data. A workspace switch
    // remounts the screen, and chrome that waits for data is a layout shift.
    expect(
      screen.getByRole("heading", { name: "Categories" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "Documents" }),
    ).toBeInTheDocument();
    // An empty-state message is a claim about the data, and the data has not
    // spoken: saying "no brain yet" mid-flight would be a lie.
    expect(screen.queryByText(/No brain yet/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Choose a brain/)).not.toBeInTheDocument();

    // Let the request land: the single door shares one in-flight slot per
    // path across the module, and a request left hanging here would block
    // the same ask in whatever test runs next.
    respond(
      new Response(JSON.stringify(answer("/v1/brains/view")), { status: 200 }),
    );
    expect(await screen.findByText("Platform")).toBeInTheDocument();
  });

  it("paints all three panes from one request, even when development mounts it twice", async () => {
    const fetch = serveViews();

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );

    expect(await screen.findByText("Platform")).toBeInTheDocument();
    expect(screen.getByText("Migrations")).toBeInTheDocument();
    expect(screen.getByText("Alpha body")).toBeInTheDocument();
    // The double mount is deliberate and the server must not feel it.
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  // A category used to show a bare "2", which answers a question nobody asked:
  // two of what? Both panes count the same thing and both must say what it is.
  it("says what the number under a brain and a category counts", async () => {
    serveViews();

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );

    // The brain holds three, the open category two, and one is singular.
    expect(await screen.findByText("3 documents")).toBeInTheDocument();
    expect(screen.getByText("2 documents")).toBeInTheDocument();
    expect(screen.getByText("1 document")).toBeInTheDocument();
  });

  // A brain is written by people AND by agents (brain_write is a real tool), so
  // the reader has to say which, and an edit by one over the other's text is
  // the case that matters.
  it("says who wrote the open document and who last changed it", async () => {
    serveViews();

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );

    expect(
      await screen.findByText(
        "Written by A Curator, last changed by Research agent",
      ),
    ).toBeInTheDocument();
  });

  it("asks exactly one question per click and repaints from the answer", async () => {
    const fetch = serveViews();

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Alpha body");

    fireEvent.click(screen.getByText("Jobs"));

    expect(await screen.findByText("Beta body")).toBeInTheDocument();
    expect(fetch).toHaveBeenCalledTimes(2);
    const asked = new URL(String(fetch.mock.calls[1][0]), "http://sag.test");
    expect(asked.pathname).toBe("/v1/brains/view");
    expect(asked.searchParams.get("brain")).toBe("1");
    expect(asked.searchParams.get("category")).toBe("11");
  });

  // The search sits above the three panes and narrows all of them. What follows
  // pins the behaviour that cannot be seen by reading the markup: what is asked
  // of the server, when, and what the panes then claim about the data.

  it("puts the cursor in the search box on arrival", async () => {
    serveViews();

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );

    await screen.findByText("Platform");
    // This screen is where you come to find something. A click that only puts
    // the cursor where it was always going to go is a click nobody should pay.
    expect(searchBox()).toHaveFocus();
  });

  it("narrows all three panes to what matched, and opens the best match", async () => {
    const { searches, views } = serveSearch([JOBS_HIT]);

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    // It opens on the first of everything: Platform / Migrations / the first
    // document of that category.
    await screen.findByText("Alpha body");

    fireEvent.change(searchBox(), { target: { value: "ownership" } });

    // The hit is in another category of the same brain, so the screen moves to
    // it: a pane that filtered itself down to rows it was not pointing at would
    // highlight nothing at all.
    expect(await screen.findByText("Beta body")).toBeInTheDocument();
    expect(searches).toHaveLength(1);
    expect(searches[0].searchParams.get("q")).toBe("ownership");
    expect(searches[0].searchParams.get("limit")).toBe(String(SEARCH_LIMIT));
    expect(views[views.length - 1].searchParams.get("brain")).toBe("1");
    expect(views[views.length - 1].searchParams.get("category")).toBe("11");
    expect(views[views.length - 1].searchParams.get("document")).toBe("110");

    // Brains: the one with a match, and not the one without.
    expect(screen.getByText("Platform")).toBeInTheDocument();
    expect(screen.queryByText("Sales")).not.toBeInTheDocument();
    // Categories: the one the match lives in, and not its sibling.
    expect(screen.getByText("Jobs")).toBeInTheDocument();
    expect(screen.queryByText("Migrations")).not.toBeInTheDocument();
    // And the count under a narrowed row counts MATCHES: "3 documents" beside a
    // single row reads as a screen that has lost two of them. Three of them say
    // "1 match": the band about the whole answer, the brain, and the category.
    expect(screen.getAllByText("1 match")).toHaveLength(3);
    expect(screen.queryByText("3 documents")).not.toBeInTheDocument();
    expect(screen.queryByText("1 document")).not.toBeInTheDocument();
  });

  it("filters the document list to the matches, not the category", async () => {
    // Both documents of the open category exist; one of them matched.
    const { searches } = serveSearch([
      {
        ...JOBS_HIT,
        document_id: 100,
        category_id: 10,
        category: "Migrations",
        title: "How the schema changes",
      },
    ]);

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Alpha body");
    expect(screen.getByText("The destructive guard")).toBeInTheDocument();

    fireEvent.change(searchBox(), { target: { value: "schema" } });

    await waitFor(() => expect(searches).toHaveLength(1));
    // The list holds what matched. The other document of the same category is
    // still there, and still not an answer to what was asked.
    await waitFor(() =>
      expect(
        screen.queryByText("The destructive guard"),
      ).not.toBeInTheDocument(),
    );
    // The match is open, so its title is in the reader; the row for it is the
    // one the list keeps.
    expect(screen.getByText("Alpha body")).toBeInTheDocument();
  });

  it("asks once per pause, not once per keystroke", async () => {
    const { searches } = serveSearch([JOBS_HIT]);

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Alpha body");

    const box = searchBox();
    fireEvent.change(box, { target: { value: "o" } });
    fireEvent.change(box, { target: { value: "ow" } });
    fireEvent.change(box, { target: { value: "own" } });
    fireEvent.change(box, { target: { value: "owner" } });

    await waitFor(() => expect(searches).toHaveLength(1));
    expect(searches[0].searchParams.get("q")).toBe("owner");
    // Give a stray debounce every chance to fire before believing it did not.
    await new Promise((done) => setTimeout(done, 400));
    expect(searches).toHaveLength(1);
  });

  it("says nothing MATCHED, rather than that nothing exists", async () => {
    serveSearch([]);

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Platform");

    fireEvent.change(searchBox(), { target: { value: "nothinghere" } });

    // "No brain yet" would be a lie about the workspace; the brains are there
    // and none of them holds what was asked for.
    expect(await screen.findByText(/No document matches/)).toBeInTheDocument();
    expect(screen.queryByText(/No brain yet/)).not.toBeInTheDocument();
    expect(screen.getByText("0 matches")).toBeInTheDocument();
  });

  // Found by using it: the panes all said "no match" and the reader sat there
  // showing a document in full, so the screen was denying and displaying the
  // same thing at once.
  it("shows no document at all when nothing matched", async () => {
    serveSearch([]);

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    // The reader is open on a document, content and all.
    await screen.findByText("Alpha body");
    expect(screen.getByText("Show full document")).toBeInTheDocument();

    fireEvent.change(searchBox(), { target: { value: "nothinghere" } });

    await screen.findByText(/No document matches/);
    // The document is not an answer to what was asked, so it is not on screen:
    // neither its body nor the bar of things to do to it.
    expect(screen.queryByText("Alpha body")).not.toBeInTheDocument();
    expect(
      screen.queryByText("How the schema changes"),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("Show full document")).not.toBeInTheDocument();
    expect(screen.getByText(/No match in this category/)).toBeInTheDocument();
  });

  it("does not empty the documents column when an expanded reader stops matching", async () => {
    serveSearch([]);

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Alpha body");
    // Expanded, the reader owns the whole column and the list is hidden.
    fireEvent.click(screen.getByText("Show full document"));
    expect(screen.queryByText("The destructive guard")).not.toBeInTheDocument();

    fireEvent.change(searchBox(), { target: { value: "nothinghere" } });

    // The reader goes, so the expansion has to go with it, or the column holds
    // nothing whatsoever: no reader, and a list still hidden behind it.
    await screen.findByText(/No document matches/);
    expect(screen.getByText(/No match in this category/)).toBeInTheDocument();
  });

  it("says a full answer was cut off instead of implying it found everything", async () => {
    // Every hit is a real document except the padding, which only has to make
    // the answer as long as the limit.
    const many: Hit[] = [
      JOBS_HIT,
      ...Array.from({ length: SEARCH_LIMIT - 1 }, (_unused, index) => ({
        ...JOBS_HIT,
        document_id: 1000 + index,
      })),
    ];
    serveSearch(many);

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Alpha body");

    fireEvent.change(searchBox(), { target: { value: "the" } });

    // An answer exactly as long as the limit was cut off there, and saying "50
    // matches" would claim the panes are narrowed to all of them.
    expect(
      await screen.findByText(`first ${SEARCH_LIMIT} matches`),
    ).toBeInTheDocument();
  });

  it("lands a click inside a narrowed pane on something that matched", async () => {
    const { views } = serveSearch([JOBS_HIT]);

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Alpha body");

    fireEvent.change(searchBox(), { target: { value: "ownership" } });
    await screen.findByText("Beta body");

    // Clicking the brain again, with the search still on. Asked for the brain
    // alone, the server opens the first document it holds, which is in the
    // category the narrowed list is no longer showing: three panes listing
    // rows and highlighting none of them.
    fireEvent.click(screen.getByText("Platform"));

    await waitFor(() => {
      const asked = views[views.length - 1].searchParams;
      expect(asked.get("brain")).toBe("1");
      expect(asked.get("category")).toBe("11");
      expect(asked.get("document")).toBe("110");
    });
    // Still on the match, and the panes still narrowed to it.
    expect(screen.getByText("Beta body")).toBeInTheDocument();
    expect(screen.queryByText("Migrations")).not.toBeInTheDocument();
  });

  it("puts the whole knowledge base back when the search is cleared", async () => {
    const { searches, views } = serveSearch([JOBS_HIT]);

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Alpha body");

    fireEvent.change(searchBox(), { target: { value: "ownership" } });
    await screen.findByText("Beta body");
    expect(screen.queryByText("Sales")).not.toBeInTheDocument();
    const asked = views.length;

    fireEvent.click(screen.getByLabelText("Clear search"));

    // Everything is listed again, and the server was asked nothing: what the
    // panes hold was never thrown away, only filtered.
    expect(screen.getByText("Sales")).toBeInTheDocument();
    expect(screen.getByText("Migrations")).toBeInTheDocument();
    expect(screen.getByText("3 documents")).toBeInTheDocument();
    expect(views).toHaveLength(asked);
    expect(searches).toHaveLength(1);
    // And the cursor is back in the box, because somebody who wants the whole
    // thing again is usually about to look for something else.
    expect(searchBox()).toHaveFocus();
  });

  it("expands the reader over the list and puts it back, without asking again", async () => {
    const fetch = serveViews();

    render(
      <StrictMode>
        <Brains />
      </StrictMode>,
    );
    await screen.findByText("Alpha body");
    // Collapsed: the reader shows the open document, the list the whole category.
    expect(screen.getByText("The destructive guard")).toBeInTheDocument();

    fireEvent.click(screen.getByText("Show full document"));
    // Expanded: the reader has the room, the list is gone.
    expect(screen.queryByText("The destructive guard")).not.toBeInTheDocument();
    expect(screen.getByText("Alpha body")).toBeInTheDocument();

    fireEvent.click(screen.getByText("Show document list"));
    expect(screen.getByText("The destructive guard")).toBeInTheDocument();

    // Expansion is the screen's own state; the server was asked nothing new.
    expect(fetch).toHaveBeenCalledTimes(1);
  });
});
