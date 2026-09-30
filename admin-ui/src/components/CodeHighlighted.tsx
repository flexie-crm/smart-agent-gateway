// The light build BY PATH, not the package index: the index also re-exports the
// full build, whose refractor import registers all 277 grammars, and CommonJS
// defeats the tree-shaking that would otherwise drop them.
import SyntaxHighlighter from "react-syntax-highlighter/dist/esm/prism-light";

import { grammarFor, useGrammar } from "@/lib/code-language";
import { codeTheme } from "@/lib/code-theme";

/**
 * The coloured view of a file.
 *
 * ITS OWN MODULE so it is its own chunk. There is no lazy loading anywhere in
 * this console, so every page is in the main bundle and anything imported from
 * a page is paid for on first paint by everybody. Measured, three ways:
 *
 *   main bundle, before any of this      724.90 kB   222.14 gzipped
 *   imported straight into CodeView      785.94 kB   244.34 gzipped
 *   as this separate chunk               729.83 kB   223.63 gzipped
 *                                      + 57.98 kB    20.54 gzipped, on demand
 *
 * So the highlighter costs nothing until somebody opens a file, and the five
 * kilobytes that remain are the new components themselves. Nothing on the
 * dashboard, the models screen or the agents screen has a code file on it.
 */
export default function CodeHighlighted({
  path,
  text,
  highlight,
  line,
  size,
}: {
  path: string;
  text: string;
  highlight: { from: number; to: number } | null;
  /** The shared line height and font size (CodeView). Passed in rather than
   *  chosen here, because the gutter beside this and the plain view behind it
   *  are drawn to the same numbers, and anything this disagrees about is a
   *  layout shift the moment this chunk lands. */
  line: number;
  size: number;
}) {
  // The grammar arrives separately again, one chunk per language, so opening a
  // Python script does not also fetch the reader for PowerShell.
  const wanted = grammarFor(path);
  const grammar = useGrammar(wanted);
  // NOTHING until it is here, when there is one coming.
  //
  // This used to render the text uncoloured and recolour it a moment later, on
  // the reasoning that readable-now beats pretty-later. On a screen where a
  // file fills the pane that is not a nicety, it is the text changing colour
  // under somebody who has started reading it, and it was the first thing
  // anybody said about this. A file with no grammar to wait for (a CSV, a
  // LICENSE) is not waiting for anything, so it renders as it is.
  if (wanted !== "" && grammar === "") return null;
  return (
    <SyntaxHighlighter
      language={grammar}
      style={codeTheme}
      // NO line numbers: the frame around this draws them, once, for all three
      // ways a file is shown (CodeView).
      //
      // One element per line, so a range can be lit. Without it the file is one
      // string and there is nothing to reach.
      wrapLines
      lineProps={(at) => {
        const lit =
          highlight !== null && at >= highlight.from && at <= highlight.to;
        // A class rather than an inline colour, so it follows the theme the
        // console is in rather than being painted once.
        // The height is set per line for the same reason the plain view sets
        // it: every line is exactly one row of the gutter beside it.
        return {
          className: lit ? "bg-primary/10" : undefined,
          style: { display: "block", height: `${line}px` },
        };
      }}
      codeTagProps={{ className: "font-mono" }}
      customStyle={{
        margin: 0,
        padding: 0,
        background: "transparent",
        color: "var(--foreground)",
        fontSize: `${size}px`,
        lineHeight: `${line}px`,
        overflow: "visible",
      }}
    >
      {text}
    </SyntaxHighlighter>
  );
}
