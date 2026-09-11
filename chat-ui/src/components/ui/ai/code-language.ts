// By path, for the reason given in code-block.tsx: the package index brings
// the full build, and with it every grammar there is.
import SyntaxHighlighter from 'react-syntax-highlighter/dist/esm/prism-light'
import { useEffect, useState } from 'react'

/**
 * A grammar is fetched when a code block needs it, and never before.
 *
 * Twenty-six languages used to be imported at the top of the code block, and so
 * landed in the main bundle: every person waited for Kotlin, nginx and GraphQL
 * on first paint, whether or not they ever saw a line of code. Nothing here is
 * eager. The tokeniser for `sql` arrives when an assistant first answers with
 * SQL, and Kotlin only ever arrives for somebody who asked about Kotlin.
 *
 * `import.meta.glob` is what makes it honest. A bare dynamic import built from a
 * variable is a path a bundler cannot see through, so it either bundles every
 * candidate anyway or resolves nothing at run time; the glob states the set at
 * build time and emits one chunk per language, which is exactly the shape
 * wanted here.
 *
 * The set is named rather than a `*`, for two reasons. It is the list of
 * languages we say we support, in one place, which is worth having; and a bare
 * star makes the bundler resolve all three hundred, several of which pull
 * `refractor` grammars that are not installed and fail the build outright.
 * Swift, Kotlin, nginx and the assembler dialects went with it: this is a
 * business gateway, and a language nobody here will paste is a chunk, a name to
 * keep working, and a build-time dependency for nothing.
 */
// One string literal, not a concatenation. The pattern is read by looking at
// the source at build time, so it has to BE a literal there.
const GRAMMARS = import.meta.glob<{ default: unknown }>(
  '../../../../node_modules/react-syntax-highlighter/dist/esm/languages/prism/{bash,c,cpp,csharp,css,diff,docker,go,graphql,java,javascript,json,jsx,markdown,markup,php,python,ruby,rust,sql,tsx,typescript,yaml}.js',
)

/**
 * What people write on a fence, against the grammar that reads it.
 *
 * Only the aliases: a name that already IS the grammar's own file name needs no
 * entry, because the lookup falls through to it. So this table is the list of
 * things people type that are not the canonical name, which is the only part
 * worth maintaining by hand.
 */
const ALIASES: Record<string, string> = {
  js: 'javascript',
  ts: 'typescript',
  py: 'python',
  sh: 'bash',
  shell: 'bash',
  zsh: 'bash',
  html: 'markup',
  xml: 'markup',
  svg: 'markup',
  rb: 'ruby',
  golang: 'go',
  rs: 'rust',
  'c++': 'cpp',
  cs: 'csharp',
  'c#': 'csharp',
  yml: 'yaml',
  md: 'markdown',
  dockerfile: 'docker',
  gql: 'graphql',
  postgres: 'sql',
  postgresql: 'sql',
  mysql: 'sql',
  mariadb: 'sql',
  plaintext: '',
  text: '',
  txt: '',
}

const PREFIX = '../../../../node_modules/react-syntax-highlighter/dist/esm/languages/prism/'

/**
 * The languages this build ships, read back off the glob rather than written
 * out again. A second hand-kept list is a list that drifts from the first.
 */
export const SUPPORTED_LANGUAGES: string[] = Object.keys(GRAMMARS)
  .map((path) => path.slice(PREFIX.length, -'.js'.length))
  .sort()

/**
 * The grammar a fence asks for, or the empty string for none.
 *
 * Empty is a real answer and not a failure: a fence naming a language we do not
 * ship renders as plain text, which is what it would have done anyway and is a
 * great deal better than a block that fails to render because a model wrote
 * ```pseudocode.
 */
export function grammarNameFor(language: string): string {
  const typed = language.trim().toLowerCase()
  const name = typed in ALIASES ? ALIASES[typed] : typed
  return SUPPORTED_LANGUAGES.includes(name) ? name : ''
}

/** Grammars already registered, so a second block of the same language is free. */
const loaded = new Set<string>()
/** In flight, so ten blocks of one language in one answer make one request. */
const loading = new Map<string, Promise<void>>()

async function register(name: string): Promise<void> {
  const load = GRAMMARS[`${PREFIX}${name}.js`]
  if (!load) return
  const grammar = await load()
  SyntaxHighlighter.registerLanguage(name, grammar.default)
  loaded.add(name)
}

/**
 * The grammar for a fence, once it is here.
 *
 * Returns the empty string until then, which renders the code as plain text.
 * That is deliberate: the code is READABLE from the first frame and gains its
 * colours a moment later. Holding the block back until a grammar arrives would
 * hide the very thing somebody is waiting to read, to no end.
 */
export function useGrammar(language: string): string {
  const name = grammarNameFor(language)
  const [ready, setReady] = useState(() => name === '' || loaded.has(name))

  useEffect(() => {
    if (name === '' || loaded.has(name)) {
      setReady(true)
      return
    }
    setReady(false)
    let live = true
    let pending = loading.get(name)
    if (!pending) {
      pending = register(name).finally(() => loading.delete(name))
      loading.set(name, pending)
    }
    void pending.then(() => {
      if (live) setReady(true)
    })
    return () => {
      live = false
    }
  }, [name])

  return ready && loaded.has(name) ? name : ''
}
