import { describe, expect, it } from "vitest";
import { configuredBy } from "@/lib/resources";
import { attribution } from "@/lib/brains";

// Who made a thing, in one line.
//
// The same shape twice, for the same reason: these rows are written by people
// AND by agents, so the line has to read for both, and for the rows that were
// written before any of it was recorded.

describe("the line that says who made something", () => {
  it("names one person when the same one made it and last changed it", () => {
    expect(
      configuredBy({ created_by_name: "Maren", updated_by_name: "Maren" }),
    ).toBe("Configured by Maren");
  });

  it("names both when they differ, which is the case that matters", () => {
    // A person sets something up and an agent changes it. If the line could
    // only say one, this is the information it would lose.
    expect(
      configuredBy({
        created_by_name: "Maren",
        updated_by_name: "Research agent",
      }),
    ).toBe("Configured by Maren, last changed by Research agent");
  });

  it("says nothing at all when nobody was recorded", () => {
    // Everything that existed before this was kept. A blank reads as "before
    // we recorded it"; anything invented would read as fact.
    expect(configuredBy({ created_by_name: "", updated_by_name: "" })).toBe("");
  });

  it("falls back to the change when only that was recorded", () => {
    expect(
      configuredBy({ created_by_name: "", updated_by_name: "Maren" }),
    ).toBe("Last changed by Maren");
  });

  it("reads the same way for a document, in its own words", () => {
    const document = {
      id: 1,
      brain_id: 1,
      category_id: 1,
      title: "t",
      content: "c",
      weight: 0,
      related: [],
      created_by: 3,
      created_by_name: "A Curator",
      updated_by: 0,
      updated_by_name: "Research agent",
    };
    expect(attribution(document)).toBe(
      "Written by A Curator, last changed by Research agent",
    );
    expect(attribution({ ...document, updated_by_name: "A Curator" })).toBe(
      "Written by A Curator",
    );
    expect(
      attribution({ ...document, created_by_name: "", updated_by_name: "" }),
    ).toBe("");
  });
});
