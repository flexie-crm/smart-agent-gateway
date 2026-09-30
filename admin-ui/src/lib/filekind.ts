/**
 * What a file IS, in the words somebody would use for it.
 *
 * The header used to show the mime type, which is what the store keeps and not
 * what anybody wants to read: "text/javascript; charset=utf-8 · 3.5 kB" says
 * the same thing as "JavaScript · 3.5 kB" and makes you parse it first.
 *
 * Read off the EXTENSION, with the mime type as the fallback. The extension is
 * what the product already decides by elsewhere (which runtime runs a script,
 * which grammar colours it), so a third opinion derived from the mime type
 * would be a third thing that can disagree.
 */
const BY_EXTENSION: Record<string, string> = {
  // The four a skill's scripts may be written in (KB/42).
  py: "Python",
  js: "JavaScript",
  mjs: "JavaScript",
  cjs: "JavaScript",
  sh: "Shell",
  ps1: "PowerShell",
  // What a package's instructions and references are.
  md: "Markdown",
  markdown: "Markdown",
  txt: "Text",
  json: "JSON",
  yaml: "YAML",
  yml: "YAML",
  csv: "CSV",
  tsv: "TSV",
  xml: "XML",
  html: "HTML",
  css: "CSS",
  sql: "SQL",
  ts: "TypeScript",
  toml: "TOML",
  ini: "Configuration",
  // And what it carries.
  docx: "Word document",
  xlsx: "Excel workbook",
  pptx: "PowerPoint deck",
  pdf: "PDF",
  png: "PNG image",
  jpg: "JPEG image",
  jpeg: "JPEG image",
  gif: "GIF image",
  svg: "SVG image",
  zip: "Zip archive",
};

/**
 * describeFile names a file's kind for a person.
 *
 * A name with no extension and a name with one nobody here knows both fall back
 * to the mime type, tidied: the parameters go (nobody needs to be told a text
 * file is utf-8) and a bare `application/octet-stream` becomes "Binary", which
 * is the honest answer for bytes nothing recognised.
 */
export function describeFile(path: string, mime: string): string {
  const name = path.slice(path.lastIndexOf("/") + 1);
  const dot = name.lastIndexOf(".");
  if (dot > 0) {
    const known = BY_EXTENSION[name.slice(dot + 1).toLowerCase()];
    if (known) return known;
  }
  const bare = mime.split(";")[0].trim();
  if (bare === "" || bare === "application/octet-stream") return "Binary";
  // Something real but unlisted: its subtype is closer to a name than the
  // whole header is ("text/x-rustsrc" -> "x-rustsrc").
  const slash = bare.indexOf("/");
  return slash < 0 ? bare : bare.slice(slash + 1);
}
