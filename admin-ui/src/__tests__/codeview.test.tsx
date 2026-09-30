import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { render, waitFor } from "@/test-utils";
import { CodeView } from "@/components/CodeView";

/**
 * Reading a file: that it is tokenised, and that the colours it asks for exist.
 *
 * Two tests because it broke in two places at once, and either one alone looks
 * like the other. The screen showed no colours; the highlighter was working
 * perfectly and every token carried `color: var(--code-keyword)`, which this
 * console defined nowhere. Black text, correctly tokenised.
 */
describe("reading a file", () => {
  it("tokenises a script once the highlighter arrives", async () => {
    const { container } = render(
      <CodeView
        path="scripts/extract.py"
        text={"import sys\n\ndef main():\n    print('hello')\n"}
        highlight={null}
        maxLines={500}
      />,
    );
    // The file arrives coloured, not plain-then-coloured: there is no frame
    // with the text on screen in the wrong colour. So this waits for the text
    // and reads it off the box, because a coloured file has no element holding
    // a whole line: every keyword and string is its own span.
    await waitFor(() =>
      expect(container.textContent).toContain("import sys"),
    );

    // Tokenised once the chunk AND the grammar are here.
    //
    // Found by the COLOUR it was given, not by a class. react-syntax-highlighter
    // does not put the token's type in the class name: every one of them is
    // `token token`, and which kind it is arrives as an inline style off the
    // theme. A test looking for `.token.keyword` finds nothing and reads as
    // "the grammar never loaded", which is a diagnosis this cost an hour of.
    await waitFor(
      () => {
        const painted = Array.from(container.querySelectorAll("span")).filter(
          (span) => span.style.color.includes("--code-"),
        );
        expect(
          painted.length,
          "nothing was tokenised: the grammar never registered",
        ).toBeGreaterThan(0);
      },
      { timeout: 5000 },
    );
    // And a keyword got the keyword colour rather than everything getting one.
    const keyword = Array.from(container.querySelectorAll("span")).find(
      (span) => span.textContent === "import",
    );
    expect(keyword?.style.color).toContain("--code-keyword");
  });

  /**
   * Every colour the theme asks for is defined, in BOTH themes.
   *
   * A static check, because the rendering test above cannot see this: jsdom
   * keeps `color: var(--code-keyword)` as written whether or not anything
   * defines it, so the one thing that was actually broken is invisible to a
   * test that renders. This reads the two files and compares them.
   */
  it("defines every colour the theme asks for, light and dark", () => {
    const theme = readFileSync("src/lib/code-theme.ts", "utf8");
    const css = readFileSync("src/index.css", "utf8");
    const wanted = [
      ...new Set(
        [...theme.matchAll(/var\((--code-[a-z]+)\)/g)].map((m) => m[1]),
      ),
    ];
    expect(wanted.length).toBeGreaterThan(0);

    // The two blocks that carry the console's palette. Split rather than
    // searched whole: a variable defined only for the light theme is a file
    // that goes black on a dark background, which is the same symptom as
    // defining none at all.
    const dark = css.slice(css.indexOf(".dark {"));
    const light = css.slice(css.indexOf(":root {"), css.indexOf(".dark {"));
    for (const name of wanted) {
      expect(light, `${name} is not defined for the light theme`).toContain(
        `${name}:`,
      );
      expect(dark, `${name} is not defined for the dark theme`).toContain(
        `${name}:`,
      );
    }
  });
});

/**
 * The two lists of languages agree.
 *
 * There are two because there have to be. The grammars are loaded by
 * `import.meta.glob`, whose pattern must be a string literal, and the dev
 * server needs the same set named in `optimizeDeps.include`, which cannot read
 * a glob. A language in one and not the other colours in a build and throws in
 * dev, which is the worst way round to find out:
 *
 *   SyntaxError: ... does not provide an export named 'default'
 */
describe("the languages a file can be coloured in", () => {
  it("are the same set in the glob and in the dev server's list", async () => {
    const { SUPPORTED_LANGUAGES } = await import("@/lib/code-language");
    const { CODE_LANGUAGES } = await import("@/lib/code-languages");
    expect([...SUPPORTED_LANGUAGES].sort()).toEqual([...CODE_LANGUAGES].sort());
  });
});
