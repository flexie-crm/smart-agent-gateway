import { describe, expect, it, vi, afterEach } from "vitest";
import { render, screen } from "@/test-utils";
import { Dashboard } from "@/pages/Dashboard";

// The dashboard has to be true, and it has to be READABLE as true. A refusal
// filed under failures, or a parked turn counted as running, teaches people to
// distrust the whole screen.

function respondWith(stats: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(stats), { status: 200 })),
  );
}

const EMPTY = {
  live: { running: 0, waiting_approval: 0 },
  today: {
    runs: 0,
    input_tokens: 0,
    output_tokens: 0,
    tool_calls: 0,
    failed: 0,
    refused: 0,
  },
  total: {
    runs: 0,
    input_tokens: 0,
    output_tokens: 0,
    tool_calls: 0,
    failed: 0,
    refused: 0,
  },
  models: [],
  tools: [],
  configured: { vendors: 0, models: 0, agents: 0, tools: 3, workflows: 0 },
};

function show(stats: unknown) {
  respondWith(stats);
  render(<Dashboard />);
}

describe("the dashboard", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("has its heading before the numbers arrive, not after", async () => {
    // Arriving on this screen used to paint a header and then swap it for a
    // different one the moment the fetch landed: the loading state rendered a
    // Page with only a title, so the subtitle and the Refresh button appeared
    // out of nowhere a beat later, and everything under them moved.
    //
    // The title and the description are facts about the SCREEN, known before
    // anything is fetched. Only the numbers are loading.
    let release: (r: Response) => void = () => {};
    vi.stubGlobal(
      "fetch",
      vi.fn(
        () =>
          new Promise<Response>((resolve) => {
            release = resolve;
          }),
      ),
    );
    render(<Dashboard />);

    // Before anything has answered.
    expect(screen.getByText("Dashboard")).toBeInTheDocument();
    expect(
      screen.getByText(/what the gateway is doing right now/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /refresh/i }),
    ).toBeInTheDocument();

    release(new Response(JSON.stringify(EMPTY), { status: 200 }));

    // And still there afterwards, the same one: nothing was swapped out.
    expect(
      await screen.findByText(/cannot answer anything/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/what the gateway is doing right now/i),
    ).toBeInTheDocument();
  });

  it("says why it is empty, rather than just being empty", async () => {
    show(EMPTY);
    // A workspace with nothing connected cannot answer anything, and the screen
    // says so instead of leaving somebody to hunt through menus for the reason.
    expect(
      await screen.findByText(/cannot answer anything/i),
    ).toBeInTheDocument();
    // And says what to DO about it, naming the screen they would go to.
    expect(screen.getByText(/vendor/i)).toBeInTheDocument();
  });

  it("explains itself without using our words for things", async () => {
    show(EMPTY);
    await screen.findByText(/cannot answer anything/i);
    // "turn" is what WE call a unit of work. The copy said "no turn can run",
    // which is accurate and means nothing to the person reading it. This is the
    // house rule (CLAUDE.md) as a test, on the one screen somebody sees before
    // they have set anything up and are least able to guess at our vocabulary.
    expect(document.body.textContent ?? "").not.toMatch(/\bturns?\b/i);
  });

  it("shows what is happening now, apart from what has happened", async () => {
    show({
      ...EMPTY,
      live: { running: 2, waiting_approval: 3 },
      today: {
        runs: 9,
        input_tokens: 1000,
        output_tokens: 200,
        tool_calls: 4,
        failed: 0,
      },
      configured: { vendors: 1, models: 1, agents: 1, tools: 3, workflows: 0 },
    });

    expect(await screen.findByText("Answering now")).toBeInTheDocument();
    expect(screen.getByText("2")).toBeInTheDocument();
    // A turn waiting on a person is not running. It is its own number.
    expect(screen.getByText("Waiting on a person")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    // Tokens today, in and out.
    expect(screen.getByText("1,200")).toBeInTheDocument();
  });

  it("keeps a refusal apart from a failure", async () => {
    show({
      ...EMPTY,
      configured: { vendors: 1, models: 1, agents: 1, tools: 3, workflows: 0 },
      tools: [{ name: "set_model_status", calls: 5, failed: 0, refused: 2 }],
    });

    expect(await screen.findByText("set_model_status")).toBeInTheDocument();
    // Both columns exist, so a person who said no is never filed as an error.
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.getByText("Refused")).toBeInTheDocument();
    expect(
      screen.getByText(/a refusal is a person saying no/i),
    ).toBeInTheDocument();
  });

  it("says so when a turn failed today, because a busy dashboard can still be broken", async () => {
    show({
      ...EMPTY,
      configured: { vendors: 1, models: 1, agents: 1, tools: 3, workflows: 0 },
      today: {
        runs: 10,
        input_tokens: 5,
        output_tokens: 5,
        tool_calls: 0,
        failed: 1,
        refused: 0,
      },
    });

    // The failure is surfaced as its own tile (counted apart from a refusal), so
    // a busy day still shows plainly that something broke.
    expect(await screen.findByText("Failed today")).toBeInTheDocument();
    expect(screen.getByText("1")).toBeInTheDocument();
  });
});
