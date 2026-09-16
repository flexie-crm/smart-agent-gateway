import { describe, expect, it, vi, afterEach } from "vitest";
import userEvent from "@testing-library/user-event";
import { render, screen } from "@/test-utils";
import { Licences } from "@/pages/Licences";
import { NAVIGATION } from "@/components/AppShell";

// The screen exists so that a notice several licences require actually reaches
// somebody. What is worth asserting is therefore not that it renders: it is that
// the licence TEXT is reachable, that the entry is not hidden behind a
// permission, and that a component with an unusual shape still appears.

function respondWith(components: unknown[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(JSON.stringify({ components }), { status: 200 }),
    ),
  );
}

const MIT = {
  name: "react",
  version: "19.0.0",
  part: "Console",
  licence: "MIT",
  text: "MIT License\n\nPermission is hereby granted, free of charge...",
  url: "https://www.npmjs.com/package/react",
};

const GPL = {
  name: "MariaDB Server",
  part: "Desktop",
  licence: "GPL-2.0",
  text: "GNU GENERAL PUBLIC LICENSE\nVersion 2, June 1991",
  note: "Its source is published at https://github.com/MariaDB/server and can also be obtained from Flexie on request.",
};

afterEach(() => vi.unstubAllGlobals());

describe("the open source screen", () => {
  it("lists what the product carries, grouped by where it is used", async () => {
    respondWith([MIT, GPL]);
    render(<Licences />);

    expect(await screen.findByText("react")).toBeInTheDocument();
    expect(screen.getByText("MariaDB Server")).toBeInTheDocument();
    // Grouped by part, because the question a reader has is "what is inside the
    // thing I installed".
    expect(screen.getByRole("heading", { name: "Console" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Desktop" })).toBeInTheDocument();
  });

  // The whole point. A list of names with no licence text attached would satisfy
  // nobody's licence, so this opens one and looks for the words.
  it("shows a licence in full when its component is opened", async () => {
    respondWith([MIT]);
    render(<Licences />);

    const row = await screen.findByRole("button", { name: /react/ });
    expect(
      screen.queryByText(/Permission is hereby granted/),
    ).not.toBeInTheDocument();

    await userEvent.click(row);
    expect(
      screen.getByText(/Permission is hereby granted/),
    ).toBeInTheDocument();
  });

  // GPLv2 asks for more than a notice, and the offer of source is the part that
  // would be easy to drop without anything looking wrong.
  it("shows the source offer that a copyleft component carries", async () => {
    respondWith([GPL]);
    render(<Licences />);

    await userEvent.click(await screen.findByRole("button", { name: /MariaDB/ }));
    expect(screen.getByText(/obtained from Flexie on request/)).toBeInTheDocument();
    expect(screen.getByText(/GNU GENERAL PUBLIC LICENSE/)).toBeInTheDocument();
  });

  it("filters by name and by licence", async () => {
    respondWith([MIT, GPL]);
    render(<Licences />);
    await screen.findByText("react");

    const search = screen.getByRole("textbox", { name: /search/i });
    await userEvent.type(search, "GPL");
    expect(screen.getByText("MariaDB Server")).toBeInTheDocument();
    expect(screen.queryByText("react")).not.toBeInTheDocument();
  });

  it("says so when the list cannot be read, rather than showing an empty page", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("nope", { status: 500 })),
    );
    render(<Licences />);
    expect(
      await screen.findByText(/could not be read/),
    ).toBeInTheDocument();
  });
});

describe("the menu entry", () => {
  // Attribution is owed to the people whose work is in here, not granted to the
  // people reading it. Behind a permission, the notice would travel as far as
  // the administrators and no further.
  it("is reachable by anyone signed in, on every edition", () => {
    const about = NAVIGATION.find((group) => group.heading === "About");
    expect(about).toBeDefined();
    const entry = about?.items.find((item) => item.to === "/open-source");
    expect(entry).toBeDefined();
    expect(entry?.permission).toBeUndefined();
    // personalHides would take it off the personal edition, which carries
    // MariaDB and therefore needs this screen most.
    expect(about?.personalHides).toBeFalsy();
  });
});
