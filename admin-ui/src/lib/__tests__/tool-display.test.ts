import { describe, expect, it } from 'vitest'

import { bySource } from '@/lib/tool-display'

// The catalogue is read by where a tool came from. It used to be one flat list
// where a connection's tools were told apart by a prefix on their names, which
// asks somebody to decode `nli_` before they can find anything.

describe('grouping a tool catalogue', () => {
  it('reads ours first, then each connected service by name', () => {
    const groups = bySource([
      { name: 'nli_brain', source: 'NLI' },
      { name: 'current_time', source: 'Built-in' },
      { name: 'query_demo24_db', source: 'Custom' },
      { name: 'crm_deal', source: 'CRM' },
      { name: 'nli_browser', source: 'NLI' },
    ])

    // What the product ships, then what somebody here built, then the
    // connections alphabetically: ours is the same on every installation and so
    // the part people learn once, and no connection's order is anybody's call.
    expect(groups.map((g) => g.source)).toEqual(['Built-in', 'Custom', 'CRM', 'NLI'])
    expect(groups[3].items.map((t) => t.name)).toEqual(['nli_brain', 'nli_browser'])
  })

  it('files a tool with no source under the built-ins rather than a blank heading', () => {
    // A server too old to say. An unnamed group reads as a bug, and this is not
    // one.
    const groups = bySource([{ name: 'current_time' } as { name: string; source?: string }])
    expect(groups.map((g) => g.source)).toEqual(['Built-in'])
  })

  it('keeps every tool exactly once', () => {
    const tools = [
      { name: 'a', source: 'NLI' },
      { name: 'b', source: 'Built-in' },
      { name: 'c', source: 'NLI' },
    ]
    const kept = bySource(tools).flatMap((g) => g.items.map((t) => t.name))
    expect(kept.sort()).toEqual(['a', 'b', 'c'])
  })

  it('is empty for an empty catalogue, rather than one empty heading', () => {
    expect(bySource([])).toEqual([])
  })
})
