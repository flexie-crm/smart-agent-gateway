/**
 * Our syntax theme, in eight colours, every one of them a CSS variable.
 *
 * Prism themes ship as a map from token kind to inline styles, which is why the
 * code block used to render TWICE: one pass styled by `oneLight`, one by
 * `oneDark`, and CSS hiding whichever you were not looking at. Double the DOM
 * and double the tokenising on every block of every message, because the theme
 * is decided by a class on an ancestor this app does not own (`.dark`, set by
 * the host page) and inline colours cannot follow it.
 *
 * A variable can. One render, and `.dark` repaints it, which is what a custom
 * property is for. Both stock themes stop being imported at all, and the pair
 * of them was the larger part of what this file cost.
 *
 * Eight colours is a decision, not a shortcut. Highlighting exists to tell a
 * string from a keyword at a glance; past that it is decoration, and a palette
 * nobody can hold in their head is one nobody keeps consistent.
 */

type TokenStyle = Record<string, string>

const comment: TokenStyle = { color: 'var(--code-comment)', fontStyle: 'italic' }
const keyword: TokenStyle = { color: 'var(--code-keyword)' }
const string: TokenStyle = { color: 'var(--code-string)' }
const number: TokenStyle = { color: 'var(--code-number)' }
const fn: TokenStyle = { color: 'var(--code-function)' }
const name: TokenStyle = { color: 'var(--code-name)' }
const operator: TokenStyle = { color: 'var(--code-operator)' }

/**
 * The map Prism reads, keyed by token class.
 *
 * Kinds share a colour where they mean the same thing to a reader: a character
 * literal is a string, a boolean is a number, an attribute name is a name. The
 * grouping is the point, and it is why eight colours cover thirty kinds.
 */
export const codeTheme: Record<string, TokenStyle> = {
  'code[class*="language-"]': { color: 'var(--foreground)', background: 'none' },
  'pre[class*="language-"]': { color: 'var(--foreground)', background: 'none' },

  comment,
  prolog: comment,
  doctype: comment,
  cdata: comment,

  punctuation: operator,
  operator,
  entity: operator,
  url: operator,

  keyword,
  atrule: keyword,
  important: { ...keyword, fontWeight: 'bold' },
  'attr-value': string,

  string,
  char: string,
  regex: string,
  inserted: string,

  number,
  boolean: number,
  constant: number,
  symbol: number,

  function: fn,
  'class-name': fn,
  builtin: fn,

  tag: name,
  'attr-name': name,
  property: name,
  selector: name,
  variable: name,
  deleted: { color: 'var(--code-name)' },
}
