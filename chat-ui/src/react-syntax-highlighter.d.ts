/**
 * The light Prism build, typed for its own path.
 *
 * `react-syntax-highlighter` ships types for the package entry only, and the
 * entry re-exports the FULL build too: importing `{ PrismLight }` from it puts
 * `prism.js` in the module graph, whose `refractor` import registers all 277
 * grammars, and CommonJS defeats the tree-shaking that would otherwise drop
 * them. Importing the light build by path is what keeps them out, so the path
 * is what needs a type.
 *
 * Narrow on purpose: this declares what we call on it and nothing else, so it
 * says what the module is FOR here rather than pretending to describe a library
 * we did not write. `any` on the styles is honest — a Prism theme is a free-form
 * map from token class to inline styles, which is exactly what codeTheme is.
 */
declare module 'react-syntax-highlighter/dist/esm/prism-light' {
  import type { ComponentType, CSSProperties, ReactNode } from 'react'

  interface PrismLightProps {
    /** The grammar to tokenise with. Empty renders the code as plain text. */
    language?: string
    style?: Record<string, CSSProperties>
    customStyle?: CSSProperties
    codeTagProps?: { className?: string }
    lineNumberStyle?: CSSProperties
    showLineNumbers?: boolean
    className?: string
    children?: ReactNode
  }

  // A component that also carries a method, which is what the library exports:
  // an intersection says that, where extending the interface only produced
  // something React declines to render.
  type PrismLight = ComponentType<PrismLightProps> & {
    registerLanguage(name: string, grammar: unknown): void
  }

  const SyntaxHighlighter: PrismLight
  export default SyntaxHighlighter
}
