/**
 * The languages a code file can be coloured in.
 *
 * Here, as data, for ONE reason: the dev server needs it.
 *
 * The grammars are loaded through `import.meta.glob` (code-language.ts), whose
 * pattern has to be a string literal because it is read out of the source at
 * build time. That works for a build, where Rollup wraps each grammar's
 * CommonJS dependency for it. It does NOT work in dev, where Vite serves those
 * deep paths raw and the browser refuses them:
 *
 *   SyntaxError: The requested module '/node_modules/refractor/lang/
 *   javascript.js' does not provide an export named 'default'
 *
 * `refractor/lang/*.js` is CommonJS. Vite pre-bundles dependencies to fix
 * exactly that, but only the ones it knows about, and a path reached through a
 * glob is not one of them. Naming them in `optimizeDeps.include` is what tells
 * it (vite.config.ts), and that list is this one.
 *
 * So the literal in the glob and this array have to agree, and a test asserts
 * they do: a language added to one and not the other is a file that colours in
 * a build and throws in dev, which is the worst way round to find out.
 */
export const CODE_LANGUAGES = [
  "bash",
  "c",
  "cpp",
  "csharp",
  "css",
  "diff",
  "docker",
  "go",
  "graphql",
  "java",
  "javascript",
  "json",
  "jsx",
  "markdown",
  "markup",
  "php",
  "powershell",
  "python",
  "ruby",
  "rust",
  "sql",
  "tsx",
  "typescript",
  "yaml",
] as const;
