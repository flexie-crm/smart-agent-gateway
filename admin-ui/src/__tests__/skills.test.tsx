import { StrictMode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@/test-utils";
import { Skills } from "@/pages/Skills";
import type { SkillFile, SkillView } from "@/lib/skills";
import { SEARCH_LIMIT } from "@/lib/search";

// The skills screen shows what was STORED, which is the only way to tell an
// import that worked from one that quietly dropped a file or read a code block
// as four headings. These tests pin that: the files, the passages with their
// line ranges, and the two names a skill has.

const VERSIONS = [
  {
    id: 2,
    skill_id: 1,
    parent_version_id: 1,
    number: 2,
    source: "imported" as const,
    status: "active" as const,
    created_by: 0,
    created_by_name: "Research agent",
    change_summary: "",
    package_sha256: "bbb",
    validated_at: "2026-09-17T08:00:00Z",
    activated_at: "2026-09-17T08:00:00Z",
    created_at: "2026-09-17T08:00:00Z",
    files: 3,
    sections: 4,
  },
  {
    id: 1,
    skill_id: 1,
    parent_version_id: 0,
    number: 1,
    source: "imported" as const,
    status: "archived" as const,
    created_by: 7,
    created_by_name: "Maren Holt",
    change_summary: "",
    package_sha256: "aaa",
    validated_at: "2026-09-16T08:00:00Z",
    activated_at: null,
    created_at: "2026-09-16T08:00:00Z",
    files: 3,
    sections: 4,
  },
];

const SKILLS = [
  {
    id: 1,
    name: "pdf-processing",
    title: "PDF Toolkit",
    description: "Extract, inspect and transform PDF files.",
    created_by: 7,
    created_by_name: "Maren Holt",
    updated_by: 7,
    updated_by_name: "Maren Holt",
    active_version_id: 2,
    status: "active" as const,
    created_at: "2026-09-16T08:00:00Z",
    updated_at: "2026-09-17T08:00:00Z",
    version: VERSIONS[0],
    versions: 2,
    files: 3,
    sections: 4,
  },
  {
    id: 2,
    // A package that carried no title of its own. It is printed by its handle,
    // and the handle is not printed twice.
    name: "csv-tools",
    title: "",
    description: "Read and write CSV.",
    created_by: 7,
    created_by_name: "Maren Holt",
    updated_by: 7,
    updated_by_name: "Maren Holt",
    active_version_id: 9,
    status: "disabled" as const,
    created_at: "2026-09-16T08:00:00Z",
    updated_at: "2026-09-16T08:00:00Z",
    version: null,
    versions: 1,
    files: 1,
    sections: 1,
  },
];

const FILES: SkillFile[] = [
  {
    id: 11,
    version_id: 2,
    path: "SKILL.md",
    file_type: "skill",
    mime_type: "text/markdown; charset=utf-8",
    binary: false,
    size_bytes: 400,
    sha256: "f1",
    sections: 3,
  },
  {
    id: 12,
    version_id: 2,
    path: "assets/template.docx",
    file_type: "asset",
    mime_type:
      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    binary: true,
    size_bytes: 2048,
    sha256: "f2",
    sections: 0,
  },
  {
    id: 13,
    version_id: 2,
    path: "scripts/extract.py",
    file_type: "script",
    mime_type: "text/x-python",
    binary: false,
    size_bytes: 164,
    sha256: "f3",
    sections: 1,
  },
];

const SECTIONS = [
  {
    id: 101,
    file_id: 11,
    heading: "",
    path: "",
    line_start: 1,
    line_end: 6,
    sequence: 1,
    size_bytes: 120,
    file: "SKILL.md",
  },
  {
    id: 102,
    file_id: 11,
    heading: "Extracting text",
    path: "PDF processing > Extracting text",
    line_start: 14,
    line_end: 22,
    sequence: 2,
    size_bytes: 117,
    file: "SKILL.md",
  },
  {
    id: 103,
    file_id: 13,
    heading: "scripts/extract.py",
    path: "scripts/extract.py",
    line_start: 1,
    line_end: 8,
    sequence: 3,
    size_bytes: 163,
    file: "scripts/extract.py",
  },
];

/** Answers /v1/skills/view the way the server does: hints in, whole screen out. */
function answer(url: string): SkillView {
  const params = new URL(url, "http://sag.test").searchParams;
  const skillID = Number(params.get("skill")) || 1;
  const skill = SKILLS.find((s) => s.id === skillID) ?? null;
  const versions = skillID === 1 ? VERSIONS : [];
  const versionID =
    Number(params.get("version")) || skill?.active_version_id || 0;
  return {
    skills: SKILLS,
    skill_id: skillID,
    skill,
    versions,
    version_id: versionID,
    files: skillID === 1 && versionID === 2 ? FILES : [],
    sections: skillID === 1 && versionID === 2 ? SECTIONS : [],
    // The manifest rides on the screen's answer, with its text: it is what the
    // reader opens before anybody clicks anything.
    manifest:
      skillID === 1 && versionID === 2
        ? { ...FILES[0], text: MANIFEST_TEXT }
        : null,
  };
}

const MANIFEST_TEXT = [
  "---",
  "name: pdf-processing",
  "description: Extract, inspect and transform PDF files.",
  "---",
  "",
  "# PDF processing",
  "",
  "Read the document first.",
].join("\n");

/**
 * The server, as this screen uses it. The three kinds of request are counted
 * apart, because what matters is that a click costs exactly one of them.
 */
function serve(
  overrides: { file?: Partial<SkillFile>; upload?: () => Response } = {},
) {
  const views: URL[] = [];
  const fileReads: URL[] = [];
  const uploads: URL[] = [];
  const searches: URL[] = [];
  // Every save, with the body, so a test can assert that the whole form went in
  // ONE request rather than a request per field.
  const saves: { url: URL; body: Record<string, unknown> }[] = [];
  const deletes: URL[] = [];
  // Every part of every import, so a test can assert that several packages
  // went in ONE request rather than one request each.
  const sent: File[][] = [];
  // Every save of an edit, with its body, so a test can assert that two edited
  // files went in ONE request: they are one change, not two versions.
  const drafts: { url: URL; body: Record<string, unknown> }[] = [];
  const discards: URL[] = [];

  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://sag.test");
    if (url.pathname === "/v1/skills/view") {
      views.push(url);
      return new Response(JSON.stringify(answer(String(input))), {
        status: 200,
      });
    }
    // The new version an edit writes. Matched BEFORE the plain /v1/skills POST
    // below, which it would otherwise not reach.
    if (/^\/v1\/skills\/\d+\/versions$/.test(url.pathname) && init?.method === "POST") {
      drafts.push({ url, body: JSON.parse(String(init.body)) });
      return new Response(
        JSON.stringify({
          version: { ...VERSIONS[0], id: 3, number: 3, status: "draft" },
          changed: ["scripts/extract.py"],
        }),
        { status: 201 },
      );
    }
    if (
      /^\/v1\/skills\/\d+\/versions\/\d+$/.test(url.pathname) &&
      init?.method === "DELETE"
    ) {
      discards.push(url);
      return new Response(null, { status: 204 });
    }
    if (url.pathname === "/v1/skills" && init?.method === "POST") {
      uploads.push(url);
      const body = init.body;
      sent.push(
        body instanceof FormData ? (body.getAll("file") as File[]) : [],
      );
      return overrides.upload
        ? overrides.upload()
        : new Response(
            JSON.stringify({
              results: [
                {
                  file: "pdf-processing.zip",
                  status: "imported",
                  skill: { ...SKILLS[0], version: VERSIONS[0] },
                },
              ],
            }),
            { status: 200 },
          );
    }
    if (url.pathname === "/v1/skills/search") {
      searches.push(url);
      const query = (url.searchParams.get("q") ?? "").toLowerCase();
      // Matched the way the index does, near enough for a screen test: over the
      // handle, the name and the description of each skill.
      return new Response(
        JSON.stringify(
          SKILLS.filter((skill) =>
            [skill.name, skill.title, skill.description]
              .join(" ")
              .toLowerCase()
              .includes(query),
          ),
        ),
        { status: 200 },
      );
    }
    if (/^\/v1\/skills\/\d+$/.test(url.pathname) && init?.method === "PUT") {
      const body = JSON.parse(String(init.body)) as Record<string, unknown>;
      saves.push({ url, body });
      const id = Number(url.pathname.split("/")[3]);
      const skill = SKILLS.find((s) => s.id === id) ?? SKILLS[0];
      const chosen =
        VERSIONS.find((v) => v.id === body.version_id) ?? VERSIONS[0];
      return new Response(
        JSON.stringify({
          ...skill,
          title: body.title,
          description: body.description,
          status: body.status,
          active_version_id: chosen.id,
          version: chosen,
        }),
        { status: 200 },
      );
    }
    if (/^\/v1\/skills\/\d+$/.test(url.pathname) && init?.method === "DELETE") {
      deletes.push(url);
      return new Response(null, { status: 204 });
    }
    if (url.pathname.startsWith("/v1/skill-files/")) {
      fileReads.push(url);
      const id = Number(url.pathname.split("/")[3]);
      const file = FILES.find((f) => f.id === id);
      return new Response(
        JSON.stringify({
          ...file,
          text: file?.binary ? undefined : MANIFEST_TEXT,
          ...overrides.file,
        }),
        { status: 200 },
      );
    }
    throw new Error(`unexpected request: ${url.pathname}`);
  });
  vi.stubGlobal("fetch", fetch);
  return {
    fetch, views, fileReads, uploads, sent, searches, saves, deletes,
    drafts, discards,
  };
}

/**
 * A chosen file, as a browser hands one over.
 *
 * The modification time is FIXED, because that is what a real one does: picking
 * the same file twice gives the same name, size and time, which is exactly how
 * the dialog recognises it as the same file. Leaving it to default would stamp
 * each fabrication with the current millisecond and make two picks of one file
 * look like two files.
 */
function zip(name: string): File {
  return new File(["zip bytes"], name, {
    type: "application/zip",
    lastModified: 1_700_000_000_000,
  });
}

/** The search box, addressed the way a person reaches it: by what it is for. */
const searchBox = () => screen.getByLabelText("Search skills");

/**
 * One skill's row, found by the name on it.
 *
 * The row is a frame holding a button for the body and buttons for the actions,
 * so reaching "the Edit of THIS row" means going up from the body and back
 * down. Addressing the actions by index instead would pass while pointing at
 * another skill's bin.
 */
function row(name: RegExp | string) {
  const body = screen.getByRole("button", { name });
  const frame = body.parentElement;
  if (!frame) throw new Error("a row body with no row around it");
  return within(frame);
}

/**
 * The reader's own box: the one holding the line-number gutter.
 *
 * Named this way because there is more than one <code> on the screen. A
 * rendered SKILL.md draws its fenced blocks as code too, and a probe that took
 * the first one it found was reading the manifest while believing it was
 * reading the file somebody opened. That cost an hour of chasing a flash that
 * was not there.
 */
function reader(): HTMLElement {
  const gutter = document.querySelector('[aria-hidden="true"].sticky');
  const frame = gutter?.parentElement?.parentElement;
  if (!frame) throw new Error("the reader is not on screen");
  return frame as HTMLElement;
}

/**
 * What the reader is showing, as one string.
 *
 * Read off the whole box rather than by finding an element with the text on it,
 * because a coloured file HAS no such element: every keyword, string and
 * operator is its own span, so `import sys` is two spans and a line is a dozen.
 * The four tests that broke when colouring stopped waiting for a plain pass
 * were all looking for a whole line in one element.
 */
function codeText(): string {
  return reader().textContent ?? "";
}

/**
 * Whether the gutter's row for a line is lit up.
 *
 * The gutter rather than the code, because the gutter is one element per line
 * whichever way the file is drawn, and it is the part that makes a cited range
 * findable at a glance.
 */
function litLine(number: number): boolean {
  const gutter = reader().querySelector('[aria-hidden="true"].sticky');
  const row = Array.from(gutter?.children ?? []).find(
    (child) => child.textContent === String(number),
  );
  if (!row) throw new Error(`line ${number} is not in the gutter`);
  return row.className.includes("bg-primary");
}

function paint() {
  render(
    <StrictMode>
      <Skills />
    </StrictMode>,
  );
}

describe("the skills screen", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("paints the whole screen from one request, even when development mounts it twice", async () => {
    const { views } = serve();
    paint();

    expect(
      await screen.findByRole("button", { name: /PDF Toolkit/ }),
    ).toBeInTheDocument();
    // The files of the live version, and the passages parsed out of them. A
    // file is a button; the same path also appears in the passages table, and
    // that is a cell rather than a way to open a file.
    expect(
      screen.getByRole("button", { name: /SKILL\.md/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /extract\.py/ }),
    ).toBeInTheDocument();
    // The passages are counted in the pane, and are one click away: the reader
    // is holding the manifest, which is what a skill IS.
    expect(
      screen.getByRole("button", { name: /^Passages/ }),
    ).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    // The double mount is deliberate and the server must not feel it.
    expect(views).toHaveLength(1);
  });

  // The format's `name` is an identifier: lowercase, hyphenated, equal to the
  // package's directory name. It is not a name for a person, and the screen
  // shows the one the package carried instead, with the handle still in sight.
  it("calls a skill what the package calls it, once", async () => {
    serve();
    paint();

    // ONCE, in its row. The screen used to repeat it in a band above the panes
    // summarising whichever skill was open, which said nothing the row did not
    // and took the space the search now has.
    expect(await screen.findAllByText("PDF Toolkit")).toHaveLength(1);

    // And only the title. The handle is an identifier, so printing it beside
    // the title says the same thing twice in two shapes; it is in SKILL.md, one
    // click away, for anybody who needs it.
    expect(screen.queryByText("pdf-processing")).not.toBeInTheDocument();

    // A package that carried no title is called by its handle, which is the
    // whole reason the fallback exists.
    expect(screen.getAllByText("csv-tools")).toHaveLength(1);
  });

  // A skill IS its instructions, so that is what the screen opens on. Landing on
  // a list of files means everybody's first click is the same one.
  it("opens on the manifest, rendered, with no request of its own", async () => {
    const { views, fileReads } = serve();
    paint();

    // Rendered: the heading is a heading, not a line of text with a hash in it.
    expect(
      await screen.findByRole("heading", { name: "PDF processing" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Read the document first.")).toBeInTheDocument();

    // The frontmatter is machine-readable metadata, and a rendered view that
    // keeps it opens on a block of YAML instead of on the instructions.
    //
    // Asserted over the whole screen rather than over the reader's code box,
    // because in this state there is no code box: the manifest is rendered, and
    // a helper that goes looking for the line-number gutter would throw.
    expect(document.body.textContent).not.toContain("name: pdf-processing");
    expect(screen.queryByText(/^---$/)).not.toBeInTheDocument();

    // And it cost nothing: the manifest came with the screen.
    expect(views).toHaveLength(1);
    expect(fileReads).toHaveLength(0);
  });

  it("shows the file exactly as stored when asked for the source", async () => {
    serve();
    paint();

    fireEvent.click(await screen.findByRole("button", { name: "Source" }));

    // Nothing is hidden here, which is the whole point of the toggle: the
    // frontmatter is part of the file, numbered like every other line.
    await waitFor(() => expect(codeText()).toContain("name: pdf-processing"));
    expect(codeText()).toContain("# PDF processing");
    // Numbered like every other line: the gutter has a row for line 6.
    expect(() => litLine(6)).not.toThrow();
  });

  it("goes back to the manifest when another skill is chosen", async () => {
    serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    // Somewhere other than the manifest: this skill's passages, found by a row
    // only this skill has (the table prints the heading PATH, not the heading).
    fireEvent.click(screen.getByRole("button", { name: /^Passages/ }));
    expect(await screen.findByText("PDF processing > Extracting text")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /csv-tools/ }));

    // What was on screen belonged to one skill and must not survive being taken
    // to another. Asserted on the ROW, not on the "Passages" heading: a skill
    // with no manifest has nothing else the reader can show, so that heading is
    // there either way and an earlier version of this test was passing on the
    // flicker while the old view was still up.
    await waitFor(() =>
      expect(screen.queryByText("PDF processing > Extracting text")).not.toBeInTheDocument(),
    );
    expect(screen.getAllByText(/This skill has no versions/).length).toBeGreaterThan(0);

    // And coming back opens the manifest rather than returning to the passages:
    // arriving at a skill starts the reader again, every time, not only the
    // first time.
    fireEvent.click(screen.getByRole("button", { name: /PDF Toolkit/ }));
    expect(
      await screen.findByText(/Read the document first/),
    ).toBeInTheDocument();
    expect(screen.queryByText("PDF processing > Extracting text")).not.toBeInTheDocument();
  });

  it("opens a file that is clicked, and asks for it once", async () => {
    const { fileReads } = serve();
    paint();

    fireEvent.click(await screen.findByRole("button", { name: /extract\.py/ }));

    await waitFor(() => expect(codeText()).toContain("name: pdf-processing"));
    // One click, one request. A script has no rendered form, so it is source.
    expect(fileReads).toHaveLength(1);
  });

  it("opens the file a passage came from, at the lines it came from", async () => {
    serve({
      file: {
        text: Array.from({ length: 24 }, (_, i) => `line ${i + 1}`).join("\n"),
      },
    });
    paint();

    await screen.findByRole("button", { name: /PDF Toolkit/ });
    fireEvent.click(screen.getByRole("button", { name: /^Passages/ }));
    fireEvent.click(
      await screen.findByText("PDF processing > Extracting text"),
    );

    // The passage says lines 14 to 22, so those are the lines marked, in the
    // SOURCE: a citation to a line number cannot be followed in a rendered
    // document, which has no lines to count.
    await waitFor(() => expect(codeText()).toContain("line 14"));
    expect(litLine(14)).toBe(true);
    expect(litLine(13)).toBe(false);
  });

  // What this can and cannot see: it pins the CLASS, not the rendering. jsdom
  // paints nothing, so it cannot tell that a see-through header had rows
  // scrolling through it; what it can do is refuse the semi-transparent tint
  // that caused it from coming back.
  it("keeps the sticky passages header opaque and above the rows", async () => {
    serve();
    paint();
    // The screen first, then the click. The reader's own header says "Passages"
    // while the screen is still loading, and it is a heading rather than
    // something to press: clicking it does nothing, and a test that raced the
    // answer was clicking it instead of the row.
    await screen.findByRole("button", { name: /PDF Toolkit/ });
    fireEvent.click(screen.getByRole("button", { name: /^Passages/ }));

    const header = (await screen.findByRole("heading", { name: "Passages" }))
      .closest("div")
      ?.parentElement?.querySelector("thead");
    expect(header).not.toBeNull();
    expect(header!.className).toContain("bg-muted");
    // A header that moves cannot be translucent: the rows go under it.
    expect(header!.className).not.toContain("bg-muted/");
    expect(header!.className).toContain("z-10");
  });

  it("offers a binary file as a download rather than as text", async () => {
    serve();
    paint();

    fireEvent.click(
      await screen.findByRole("button", { name: /template\.docx/ }),
    );

    expect(
      await screen.findByText(/stored exactly as it arrived/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Download" }),
    ).toBeInTheDocument();
    // And the pane said so before it was opened, from the listing alone.
    expect(screen.getByText(/not indexed/)).toBeInTheDocument();
  });

  // Rolling back is an EDIT, so it is in the skill's form rather than on the
  // screen: the screen shows what is running, and changing what runs is a save.
  it("rolls back from the form, in the same request as everything else", async () => {
    const { saves } = serve();
    paint();

    await screen.findByRole("button", { name: /PDF Toolkit/ });
    fireEvent.click(row(/PDF Toolkit/).getByRole("button", { name: "Edit" }));

    // It opens on the version that IS live, so opening the form and saving it
    // changes nothing about which version runs.
    const picker = await screen.findByLabelText("Live version");
    expect(picker).toHaveValue("2");

    fireEvent.change(picker, { target: { value: "1" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    // ONE request, carrying the rollback and the three fields beside it. Two
    // requests is how a dialog half saves: the rename lands, the rollback does
    // not, and the person is told it worked.
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].body).toMatchObject({
      version_id: 1,
      title: "PDF Toolkit",
      description: "Extract, inspect and transform PDF files.",
      status: "active",
    });
  });

  it("offers no version picker when there is only one version", async () => {
    serve();
    paint();

    // csv-tools has no version history in the fixture, and a picker with one
    // option is a control that cannot do anything.
    fireEvent.click(
      (await screen.findAllByRole("button", { name: /csv-tools/ }))[0],
    );
    fireEvent.click(row(/csv-tools/).getByRole("button", { name: "Edit" }));

    await screen.findByRole("heading", { name: "Edit skill" });
    expect(screen.queryByLabelText("Live version")).not.toBeInTheDocument();
  });

  // A person imports a skill and an agent improves it, so "who made this" has
  // two answers, and the one that matters is whoever made what is RUNNING.
  it("says in the package header which version it is showing and who made it", async () => {
    serve();
    paint();

    await screen.findByRole("button", { name: /PDF Toolkit/ });
    // On the column whose files they are, not in a band about the whole screen:
    // the version and its author describe this package and nothing else.
    expect(
      await screen.findByText(/v2 · by Research agent/),
    ).toBeInTheDocument();
    // Version 1 was imported by a person and is not what is running.
    expect(screen.queryByText(/Maren Holt/)).not.toBeInTheDocument();
  });

  it("says a package was imported, and says when it was not", async () => {
    serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(screen.getByRole("button", { name: /Import skill/ }));
    const chooser = screen.getByLabelText("Skill packages");
    fireEvent.change(chooser, {
      target: {
        files: [
          new File(["zip bytes"], "pdf-processing.zip", {
            type: "application/zip",
          }),
        ],
      },
    });
    fireEvent.click(screen.getByRole("button", { name: "Import" }));

    expect(
      await screen.findByText(/was imported as version 2/),
    ).toBeInTheDocument();
  });

  it("does not claim an import happened when the package was already here", async () => {
    // The server's honest answer to the same bytes twice.
    serve({
      upload: () =>
        new Response(
          JSON.stringify({
            results: [
              {
                file: "again.zip",
                status: "unchanged",
                skill: { ...SKILLS[0], version: VERSIONS[0] },
              },
            ],
          }),
          { status: 200 },
        ),
    });
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(screen.getByRole("button", { name: /Import skill/ }));
    const chooser = screen.getByLabelText("Skill packages");
    fireEvent.change(chooser, {
      target: {
        files: [
          new File(["zip bytes"], "again.zip", { type: "application/zip" }),
        ],
      },
    });
    fireEvent.click(screen.getByRole("button", { name: "Import" }));

    expect(
      await screen.findByText(/is unchanged: that is already version 2/),
    ).toBeInTheDocument();
  });

  // A package can be wrong in a dozen ways and the server names which file is
  // wrong and why. That answer belongs on the field, not flattened into
  // "import failed".
  it("puts the server's reason for refusing a package on the field", async () => {
    serve({
      upload: () =>
        new Response(
          JSON.stringify({
            results: [
              {
                file: "bad.zip",
                status: "refused",
                reason:
                  "pdf-processing/leak.txt is a symbolic link, and a package may only contain real files",
              },
            ],
          }),
          { status: 200 },
        ),
    });
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(screen.getByRole("button", { name: /Import skill/ }));
    const chooser = screen.getByLabelText("Skill packages");
    fireEvent.change(chooser, {
      target: {
        files: [
          new File(["zip bytes"], "bad.zip", { type: "application/zip" }),
        ],
      },
    });
    fireEvent.click(screen.getByRole("button", { name: "Import" }));

    expect(await screen.findByText(/is a symbolic link/)).toBeInTheDocument();
    // The dialog stays open, because there is something to correct.
    expect(screen.getByRole("button", { name: "Import" })).toBeInTheDocument();
  });

  // Several packages at once: one request, and a summary at the end.
  it("sends every chosen package in one request and says what happened to each", async () => {
    const server = serve({
      upload: () =>
        new Response(
          JSON.stringify({
            results: [
              {
                file: "one.zip",
                status: "imported",
                skill: { ...SKILLS[0], version: VERSIONS[0] },
              },
              {
                file: "two.zip",
                status: "imported",
                skill: { ...SKILLS[1], version: VERSIONS[0] },
              },
              {
                file: "three.zip",
                status: "unchanged",
                skill: { ...SKILLS[0], version: VERSIONS[0] },
              },
            ],
          }),
          { status: 200 },
        ),
    });
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(screen.getByRole("button", { name: /Import skill/ }));
    fireEvent.change(screen.getByLabelText("Skill packages"), {
      target: { files: [zip("one.zip"), zip("two.zip"), zip("three.zip")] },
    });

    // The button counts them, so it is clear before the click that three are
    // going and not the last one chosen.
    fireEvent.click(screen.getByRole("button", { name: "Import 3 packages" }));

    expect(
      await screen.findByText("2 skills imported, 1 already here."),
    ).toBeInTheDocument();

    // ONE request, carrying three parts under the one field name.
    expect(server.uploads).toHaveLength(1);
    expect(server.sent[0].map((file) => file.name)).toEqual([
      "one.zip",
      "two.zip",
      "three.zip",
    ]);
  });

  it("keeps the ones it skipped on screen, with the reason, and drops the rest", async () => {
    serve({
      upload: () =>
        new Response(
          JSON.stringify({
            results: [
              {
                file: "good.zip",
                status: "imported",
                skill: { ...SKILLS[0], version: VERSIONS[0] },
              },
              {
                file: "junk.zip",
                status: "refused",
                reason: "that file is not a zip archive",
              },
            ],
          }),
          { status: 200 },
        ),
    });
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(screen.getByRole("button", { name: /Import skill/ }));
    fireEvent.change(screen.getByLabelText("Skill packages"), {
      target: { files: [zip("good.zip"), zip("junk.zip")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Import 2 packages" }));

    const summary = await screen.findByText("1 skill imported, 1 skipped.");
    expect(summary).toBeInTheDocument();
    // Something landed, so the toast is a status.
    expect(summary.closest('[role="status"]')).not.toBeNull();
    // The reason is on the row it is about, not flattened into the summary.
    expect(
      await screen.findByText(/that file is not a zip archive/),
    ).toBeInTheDocument();

    // The dialog is still open, holding the one that was skipped and not the
    // one that landed: what is left is what there is left to do.
    expect(screen.getByRole("button", { name: "Import" })).toBeInTheDocument();
    expect(screen.getByText("junk.zip")).toBeInTheDocument();
    expect(screen.queryByText("good.zip")).not.toBeInTheDocument();
  });

  it("closes when everything landed, and stays when nothing did", async () => {
    serve({
      upload: () =>
        new Response(
          JSON.stringify({
            results: [
              {
                file: "junk.zip",
                status: "refused",
                reason: "that file is not a zip archive",
              },
              {
                file: "other.zip",
                status: "refused",
                reason: "that package has no SKILL.md",
              },
            ],
          }),
          { status: 200 },
        ),
    });
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(screen.getByRole("button", { name: /Import skill/ }));
    fireEvent.change(screen.getByLabelText("Skill packages"), {
      target: { files: [zip("junk.zip"), zip("other.zip")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Import 2 packages" }));

    // Nothing landed, so this is not reported as a success: the toast carries
    // the error tone, and both reasons are on their rows.
    const nothing = await screen.findByText("2 skipped.");
    // An import where everything was skipped is not a success, and a tick
    // beside it would say it was. The tone is decided from the outcomes, so
    // this toast is an alert and not a status.
    expect(nothing.closest('[role="alert"]')).not.toBeNull();
    expect(nothing.closest('[role="status"]')).toBeNull();
    expect(await screen.findByText(/no SKILL.md/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Import 2 packages" })).toBeInTheDocument();
  });

  it("adds to the selection rather than replacing it, and takes one back out", async () => {
    serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(screen.getByRole("button", { name: /Import skill/ }));
    const chooser = screen.getByLabelText("Skill packages");

    fireEvent.change(chooser, { target: { files: [zip("one.zip")] } });
    // A second go at the picker: somebody collecting a dozen packages does not
    // do it in one selection, and losing the first eleven is the annoyance.
    fireEvent.change(chooser, {
      target: { files: [zip("two.zip"), zip("one.zip")] },
    });

    expect(screen.getByText("one.zip")).toBeInTheDocument();
    expect(screen.getByText("two.zip")).toBeInTheDocument();
    // The same file twice is one entry.
    expect(
      screen.getByRole("button", { name: "Import 2 packages" }),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Remove one.zip" }));
    expect(screen.queryByText("one.zip")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Import" })).toBeInTheDocument();
  });

  // A new console asking an OLD gateway, which really happened: the answer was
  // the previous contract ({skill, added}), the client reads `results`, and
  // reading a property off undefined throws in our own code. That used to be
  // reported as "the gateway did not answer" -- which sent somebody to look at
  // a server that was answering perfectly well, every time.
  it("blames itself, not the gateway, when it cannot read the answer", async () => {
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    serve({
      upload: () =>
        new Response(
          JSON.stringify({ skill: { ...SKILLS[0] }, added: true }),
          { status: 200 },
        ),
    });
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(screen.getByRole("button", { name: /Import skill/ }));
    fireEvent.change(screen.getByLabelText("Skill packages"), {
      target: { files: [zip("one.zip")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Import" }));

    expect(
      await screen.findByText(
        "Something went wrong in the console. Reload the page and try again.",
      ),
    ).toBeInTheDocument();
    // The control: the message the gateway's silence produces must not be the
    // message its perfectly good answer produces.
    expect(
      screen.queryByText("The gateway did not answer."),
    ).not.toBeInTheDocument();
    // And what actually happened is in the browser's console, not lost.
    expect(logged).toHaveBeenCalled();
    logged.mockRestore();
  });

  it("says nothing about the data until the server has spoken", async () => {
    let respond!: (r: Response) => void;
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>((r) => (respond = r))),
    );

    paint();

    // The panes are chrome. An empty-state message mid-flight is a claim about
    // data that has not arrived.
    // The page's own header and the first pane's, both of them chrome: a
    // workspace switch remounts this screen, and chrome that waits for data is
    // a layout shift somebody watches happen.
    expect(screen.getAllByRole("heading", { name: "Skills" })).toHaveLength(2);
    expect(
      screen.getByRole("heading", { name: "Package" }),
    ).toBeInTheDocument();
    expect(screen.queryByText(/No skills yet/)).not.toBeInTheDocument();

    respond(
      new Response(JSON.stringify(answer("/v1/skills/view")), { status: 200 }),
    );
    expect(
      await screen.findByRole("button", { name: /PDF Toolkit/ }),
    ).toBeInTheDocument();
  });
});

// The search sits above the panes and narrows the list, which is what the band
// across the top is FOR now that it no longer summarises whatever was open.
describe("searching for a skill", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("puts the cursor in the box on arrival", async () => {
    serve();
    paint();

    // This is the screen you come to to find something. A click that only puts
    // the cursor where it was always going to go is a click for nothing.
    await screen.findByRole("button", { name: /PDF Toolkit/ });
    expect(searchBox()).toHaveFocus();
  });

  it("narrows the list to what matched, and opens the best match", async () => {
    const { searches, views } = serve();
    paint();

    // It opens on the first skill, which is not the one about to be searched
    // for: without that, "it opened the match" would be true by accident.
    await screen.findByRole("button", { name: /PDF Toolkit/ });
    expect(screen.getByText("csv-tools")).toBeInTheDocument();

    fireEvent.change(searchBox(), { target: { value: "csv" } });

    // The match is the other skill, so the screen moves to it: a pane narrowed
    // to rows it is not pointing at would highlight nothing at all.
    await waitFor(() => expect(searches).toHaveLength(1));
    expect(searches[0].searchParams.get("q")).toBe("csv");
    expect(searches[0].searchParams.get("limit")).toBe(String(SEARCH_LIMIT));

    await waitFor(() =>
      expect(views[views.length - 1].searchParams.get("skill")).toBe("2"),
    );
    await waitFor(() =>
      expect(screen.queryByText("PDF Toolkit")).not.toBeInTheDocument(),
    );
    expect(screen.getByText("csv-tools")).toBeInTheDocument();
    expect(screen.getByText("1 match")).toBeInTheDocument();
  });

  it("asks once for a pause, not once per keystroke", async () => {
    const { searches } = serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    // Typed with real gaps between the letters, each SHORTER than the debounce
    // and adding up to longer than it. Firing them in one tick would prove
    // nothing: the effect cancels the previous timer on every change, so four
    // instant keystrokes cost one search whatever the interval is, and the test
    // passed with the debounce set to zero. That is why it reads like this.
    for (const typed of ["c", "cs", "csv"]) {
      fireEvent.change(searchBox(), { target: { value: typed } });
      await new Promise((wake) => setTimeout(wake, 90));
    }
    expect(searches).toHaveLength(0);

    await waitFor(() => expect(searches).toHaveLength(1));
    expect(searches[0].searchParams.get("q")).toBe("csv");
  });

  it("says nothing matched rather than that there are no skills", async () => {
    serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.change(searchBox(), { target: { value: "helicopter" } });

    // The difference matters: "No skills yet" would tell somebody their
    // packages had gone.
    expect(
      await screen.findByText(/No skill matches .helicopter./),
    ).toBeInTheDocument();
    expect(screen.queryByText(/No skills yet/)).not.toBeInTheDocument();
  });

  // Found by use, and it was the whole right-hand side of the screen: the panes
  // went on showing the last skill's package and its SKILL.md while the pane
  // beside them said nothing matched.
  it("empties the package and the reader too, not just the list", async () => {
    serve();
    paint();

    // A skill is open, with its files and its manifest on screen.
    await screen.findByRole("button", { name: /PDF Toolkit/ });
    expect(screen.getByRole("button", { name: /SKILL\.md/ })).toBeInTheDocument();
    expect(screen.getByText(/Read the document first/)).toBeInTheDocument();

    fireEvent.change(searchBox(), { target: { value: "helicopter" } });
    await screen.findByText(/No skill matches/);

    // The package column, the passages count and the document all go. A screen
    // that shows a skill and denies having one is worse than an empty one.
    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: /SKILL\.md/ }),
      ).not.toBeInTheDocument(),
    );
    expect(screen.queryByText(/Read the document first/)).not.toBeInTheDocument();
    expect(screen.queryByText(/v2 · by Research agent/)).not.toBeInTheDocument();
    // And the column says what to do instead.
    expect(screen.getAllByText(/Choose a skill/).length).toBeGreaterThan(0);

    // Clearing the box brings all of it back: the skill was never closed, only
    // hidden by a question that did not match it.
    fireEvent.click(screen.getByLabelText("Clear search"));
    expect(
      await screen.findByRole("button", { name: /SKILL\.md/ }),
    ).toBeInTheDocument();
    expect(screen.getByText(/Read the document first/)).toBeInTheDocument();
  });

  it("puts the whole list back when the box is cleared", async () => {
    serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.change(searchBox(), { target: { value: "csv" } });
    await waitFor(() =>
      expect(screen.queryByText("PDF Toolkit")).not.toBeInTheDocument(),
    );

    // Whether a search is ON is the box, not the last answer it got. Clearing
    // it needs no state to be put back, and a list still filtered by a word
    // nobody can see is the bug this pins.
    fireEvent.click(screen.getByLabelText("Clear search"));
    expect(await screen.findByText("PDF Toolkit")).toBeInTheDocument();
    expect(screen.getByText("csv-tools")).toBeInTheDocument();
  });
});

// Editing and deleting are on the ROW, on hover, the way a brain's are. They
// used to be in a band above the panes, which meant selecting a skill before
// you could do anything to it.
describe("editing a skill from its row", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("saves the name, the description and the switch in one request", async () => {
    const { saves } = serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(row(/PDF Toolkit/).getByRole("button", { name: "Edit" }));
    await screen.findByRole("heading", { name: "Edit skill" });

    fireEvent.change(screen.getByDisplayValue("PDF Toolkit"), {
      target: { value: "Invoice tooling" },
    });
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].body).toMatchObject({
      title: "Invoice tooling",
      status: "disabled",
      // The version nobody touched is sent as it was, which is what tells the
      // server this save is not a rollback.
      version_id: 2,
    });
    // And it says so. A dialog that closes is not a confirmation.
    expect(
      await screen.findByText(/Invoice tooling was saved/),
    ).toBeInTheDocument();
  });

  it("can be opened on a skill that is not the one selected", async () => {
    const { saves } = serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    // The whole point of putting these on the row: the band made you select a
    // skill before you could do anything to it.
    fireEvent.click(row(/csv-tools/).getByRole("button", { name: "Edit" }));
    await screen.findByRole("heading", { name: "Edit skill" });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].url.pathname).toBe("/v1/skills/2");
  });

  it("asks before deleting, says what goes, and reports that it did", async () => {
    const { deletes } = serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(row(/PDF Toolkit/).getByRole("button", { name: "Delete" }));

    // The question names what will be lost. A row that vanishes on one click is
    // a row somebody deleted by accident.
    expect(
      await screen.findByText(/Delete "PDF Toolkit"\?/),
    ).toBeInTheDocument();
    expect(screen.getByText(/All 2 of its versions go too/)).toBeInTheDocument();
    expect(deletes).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(deletes).toHaveLength(1));
    expect(deletes[0].pathname).toBe("/v1/skills/1");
    expect(
      await screen.findByText(/PDF Toolkit was deleted, with every version/),
    ).toBeInTheDocument();
  });

  it("does not delete when the question is answered no", async () => {
    const { deletes } = serve();
    paint();
    await screen.findByRole("button", { name: /PDF Toolkit/ });

    fireEvent.click(row(/PDF Toolkit/).getByRole("button", { name: "Delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));

    // The control for the test above: without it, a confirm that did nothing at
    // all would pass that one and this one would be the only thing to notice.
    await waitFor(() =>
      expect(screen.queryByText(/Delete "PDF Toolkit"\?/)).not.toBeInTheDocument(),
    );
    expect(deletes).toHaveLength(0);
  });
});

/**
 * Editing a package's files.
 *
 * The rule the whole thing rests on: a version cannot be changed, so editing
 * writes a NEW one, and it is not live until somebody says so. These pin the
 * parts of that a person can see.
 */
/**
 * The same server, with a draft already waiting.
 *
 * What somebody arriving at the screen after a colleague's edit sees: a version
 * that exists, is not live, and has to be decided about.
 */
function servingADraft() {
  const served = serve({ file: { text: "print(1)\n" } });
  const withDraft = vi.fn(
    async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://sag.test");
      if (url.pathname === "/v1/skills/view") {
        served.views.push(url);
        const view = answer(String(input));
        const reading = Number(url.searchParams.get("version")) || view.version_id;
        return new Response(
          JSON.stringify({
            ...view,
            version_id: reading,
            // The draft holds the same files as the version it came from, which
            // is what a real one does: every file is carried forward and the
            // edited ones differ.
            files: FILES.map((f) => ({ ...f, version_id: reading })),
            manifest: { ...FILES[0], version_id: reading, text: MANIFEST_TEXT },
            versions: [
              {
                ...VERSIONS[0],
                id: 3,
                number: 3,
                status: "draft",
                parent_version_id: 2,
                created_by_name: "Maren Holt",
                change_summary: "edited scripts/extract.py",
                activated_at: null,
              },
              ...VERSIONS,
            ],
          }),
          { status: 200 },
        );
      }
      return served.fetch(input, init);
    },
  );
  vi.stubGlobal("fetch", withDraft);
  return served;
}

describe("editing a skill's files", () => {
  afterEach(() => vi.unstubAllGlobals());

  /** Opens the skill, then the script, and hands back nothing. */
  async function openScript() {
    paint();
    await screen.findByText("PDF Toolkit");
    fireEvent.click(screen.getByRole("button", { name: /extract\.py/ }));
    await screen.findByRole("button", { name: "Edit this file" });
  }

  it("shows a box to type in, and says the file is unsaved", async () => {
    serve({ file: { text: "print(1)\n" } });
    await openScript();

    // Nothing is claimed to be unsaved before anything is typed.
    expect(screen.queryByText(/not saved/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Edit this file" }));
    const box = await screen.findByLabelText(
      "The contents of scripts/extract.py",
    );
    // What it opens with is what the file says, not an empty box.
    expect((box as HTMLTextAreaElement).value).toBe("print(1)\n");
    // OPEN is not EDITED. Clicking Edit and typing nothing has changed
    // nothing, so nothing is announced as unsaved and there is nothing to
    // save yet.
    expect(
      await screen.findByText("Editing scripts/extract.py."),
    ).toBeTruthy();
    expect(screen.queryByText(/not saved/)).toBeNull();
    expect(
      screen.getByRole("button", { name: "Save as a new version" }),
    ).toBeDisabled();

    fireEvent.change(box, { target: { value: "print(2)\n" } });
    expect(
      await screen.findByText("scripts/extract.py is edited and not saved."),
    ).toBeTruthy();
  });

  it("saves every edited file in one request, against the version it was read from", async () => {
    const { drafts } = serve({ file: { text: "print(1)\n" } });
    await openScript();

    fireEvent.click(screen.getByRole("button", { name: "Edit this file" }));
    fireEvent.change(
      await screen.findByLabelText("The contents of scripts/extract.py"),
      { target: { value: "print(2)\n" } },
    );
    // A second file, edited in the same sitting.
    fireEvent.click(screen.getByRole("button", { name: /SKILL\.md/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Edit this file" }));
    fireEvent.change(await screen.findByLabelText("The contents of SKILL.md"), {
      target: { value: `${MANIFEST_TEXT}\n\nAnd then stop.\n` },
    });
    expect(
      await screen.findByText("2 files are edited and not saved."),
    ).toBeTruthy();

    // And a THIRD file opened and not touched is not part of the change: it is
    // neither counted here nor sent below.
    fireEvent.click(screen.getByRole("button", { name: /template\.docx/ }));
    await waitFor(() =>
      expect(
        screen.getByText("2 files are edited and not saved."),
      ).toBeTruthy(),
    );

    fireEvent.click(
      screen.getByRole("button", { name: "Save as a new version" }),
    );
    await waitFor(() => expect(drafts.length).toBe(1));
    // ONE request, carrying both, written against the version on screen.
    const sent = drafts[0].body as {
      from: number;
      edits: { path: string; text: string }[];
    };
    expect(sent.from).toBe(2);
    expect(sent.edits.map((e) => e.path).sort()).toEqual([
      "SKILL.md",
      "scripts/extract.py",
    ]);
    // And what was typed is gone from the bar, because it is stored now.
    await waitFor(() => expect(screen.queryByText(/not saved/)).toBeNull());
  });

  it("keeps what was typed when the save fails", async () => {
    const { drafts } = serve({ file: { text: "print(1)\n" } });
    await openScript();
    fireEvent.click(screen.getByRole("button", { name: "Edit this file" }));
    fireEvent.change(
      await screen.findByLabelText("The contents of scripts/extract.py"),
      { target: { value: "print(2)\n" } },
    );

    // The server refuses this one.
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            error: "invalid_request",
            error_description: "some fields need a change",
            fields: { edits: "scripts/extract.py would no longer be text" },
          }),
          { status: 400 },
        ),
      ),
    );
    fireEvent.click(
      screen.getByRole("button", { name: "Save as a new version" }),
    );

    // Still unsaved, still in the box: a refused save that threw away what
    // somebody typed would be the worst possible answer.
    expect(
      await screen.findByText("scripts/extract.py is edited and not saved."),
    ).toBeTruthy();
    expect(drafts.length).toBe(0);
  });

  it("leaves the editor through Cancel, for every open file at once", async () => {
    serve({ file: { text: "print(1)\n" } });
    await openScript();
    fireEvent.click(screen.getByRole("button", { name: "Edit this file" }));
    fireEvent.change(
      await screen.findByLabelText("The contents of scripts/extract.py"),
      { target: { value: "print(2)\n" } },
    );
    await screen.findByText("scripts/extract.py is edited and not saved.");

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    // The bar is gone, the box is gone, and the file is being read again.
    await waitFor(() =>
      expect(
        screen.queryByLabelText("The contents of scripts/extract.py"),
      ).toBeNull(),
    );
    expect(screen.queryByText(/is edited and not saved/)).toBeNull();
    expect(
      screen.getByRole("button", { name: "Edit this file" }),
    ).toBeTruthy();
  });

  it("offers a draft for publishing, and says the agent is not using it yet", async () => {
    // A draft already waiting, which is what somebody arriving at the screen
    // after a colleague's edit sees.
    const { saves } = servingADraft();
    paint();
    await screen.findByText("PDF Toolkit");

    expect(await screen.findByText(/Version 3 is a draft/)).toBeTruthy();
    expect(screen.getByText(/still using version 2/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Publish" }));
    fireEvent.click(await screen.findByRole("button", { name: "Publish" }));
    await waitFor(() => expect(saves.length).toBe(1));
    // Published through the one route that makes a version live, naming the
    // draft, and sending the skill's own words back unchanged so nothing is
    // renamed by the act of publishing.
    expect(saves[0].body.version_id).toBe(3);
    expect(saves[0].body.title).toBe("PDF Toolkit");
  });

  it("discards a draft after asking", async () => {
    const { discards } = servingADraft();
    paint();
    await screen.findByText("PDF Toolkit");
    await screen.findByText(/Version 3 is a draft/);

    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));
    await waitFor(() => expect(discards.length).toBe(1));
    expect(discards[0].pathname).toBe("/v1/skills/1/versions/3");
  });

  it("opens the draft for review, and a draft can be edited further", async () => {
    servingADraft();
    paint();
    await screen.findByText("PDF Toolkit");
    await screen.findByText(/Version 3 is a draft/);

    fireEvent.click(screen.getByRole("button", { name: "Review" }));
    // Now reading the draft, and it is still editable: a draft is not live, so
    // editing it again is an edit of something nobody is using.
    fireEvent.click(await screen.findByRole("button", { name: /extract\.py/ }));
    expect(
      await screen.findByRole("button", { name: "Edit this file" }),
    ).toBeTruthy();
  });
});
