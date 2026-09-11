import { describe, expect, it } from 'vitest'
import { grammarNameFor, SUPPORTED_LANGUAGES } from '@/components/ui/ai/code-language'

/**
 * What somebody types on a fence, against the grammar that reads it.
 *
 * The fence is written by a model, so it is whatever the model felt like: the
 * canonical name, a common short form, an upper-case one, or a dialect of
 * something we do have. Every one of those should end at a grammar we ship, and
 * anything else should end at plain text rather than a broken block.
 */
describe('the grammar a fence asks for', () => {
  it('takes the canonical name as it is', () => {
    expect(grammarNameFor('typescript')).toBe('typescript')
    expect(grammarNameFor('python')).toBe('python')
  })

  it('takes what people actually type', () => {
    expect(grammarNameFor('ts')).toBe('typescript')
    expect(grammarNameFor('js')).toBe('javascript')
    expect(grammarNameFor('py')).toBe('python')
    expect(grammarNameFor('sh')).toBe('bash')
    expect(grammarNameFor('rb')).toBe('ruby')
    expect(grammarNameFor('c#')).toBe('csharp')
    expect(grammarNameFor('yml')).toBe('yaml')
  })

  it('is not fussy about case or stray space', () => {
    expect(grammarNameFor('  SQL ')).toBe('sql')
    expect(grammarNameFor('TypeScript')).toBe('typescript')
  })

  it('reads a dialect as the language it is one of', () => {
    // The database tools answer in a dialect, and the model labels its fence
    // with the dialect it was speaking. There is one SQL grammar.
    expect(grammarNameFor('postgres')).toBe('sql')
    expect(grammarNameFor('postgresql')).toBe('sql')
    expect(grammarNameFor('mariadb')).toBe('sql')
    expect(grammarNameFor('mysql')).toBe('sql')
  })

  it('reads html and its family as markup', () => {
    expect(grammarNameFor('html')).toBe('markup')
    expect(grammarNameFor('xml')).toBe('markup')
    expect(grammarNameFor('svg')).toBe('markup')
  })

  it('asks for nothing when the fence says plain text', () => {
    // Naming no grammar renders the code as-is, which is what was asked for.
    for (const plain of ['', 'text', 'txt', 'plaintext']) {
      expect(grammarNameFor(plain)).toBe('')
    }
  })

  it('asks for nothing rather than failing on a language we do not ship', () => {
    // A model writes ```pseudocode, or a language that went when the set was
    // trimmed. Plain text is the honest answer; a block that fails to render
    // because of the word after the backticks is not.
    expect(grammarNameFor('pseudocode')).toBe('')
    expect(grammarNameFor('kotlin')).toBe('')
    expect(grammarNameFor('brainfuck')).toBe('')
  })

  it('ships every language its aliases point at', () => {
    // An alias for a grammar that was trimmed out of the set is a fence that
    // silently renders plain while claiming to be handled. This is the check
    // that keeps the two lists honest with each other.
    for (const name of Object.values(ALIAS_TARGETS)) {
      if (name === '') continue
      expect(SUPPORTED_LANGUAGES).toContain(name)
    }
  })
})

// The alias targets, read back through the resolver so the test cannot drift
// from the table it is checking.
const ALIAS_TARGETS = Object.fromEntries(
  ['js', 'ts', 'py', 'sh', 'shell', 'zsh', 'html', 'xml', 'svg', 'rb', 'golang', 'rs',
   'c++', 'cs', 'c#', 'yml', 'md', 'dockerfile', 'gql', 'postgres', 'postgresql',
   'mysql', 'mariadb'].map((alias) => [alias, grammarNameFor(alias)]),
)
