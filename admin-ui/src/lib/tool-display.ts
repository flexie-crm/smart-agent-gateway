/**
 * How a tool is read: what it is called, and where it came from.
 *
 * It was one flat list where a connection's tools were told apart by a prefix on
 * their names, which asks somebody to decode `nli_` before they can find
 * anything. The server already knows which connection projected each tool, so
 * the grouping is its answer rather than a guess made here from a name.
 *
 * The order is deliberate: what the product ships, then what somebody here
 * built, then each connected service by name. Ours first because it is the same
 * on every installation and therefore the part people learn once; the
 * connections after it, alphabetically, because their order is nobody's
 * decision.
 */

const OURS = ['Built-in', 'Custom']

export interface HasSource {
  source?: string
}

/** Group items by source, in the order the headings should be read. */
export function bySource<T extends HasSource>(items: T[]): { source: string; items: T[] }[] {
  const groups = new Map<string, T[]>()
  for (const item of items) {
    // A tool from a server too old to say gets the generic heading rather than
    // a blank one: an unnamed group reads as a bug, and this is not one.
    const key = item.source || 'Built-in'
    const list = groups.get(key)
    if (list) list.push(item)
    else groups.set(key, [item])
  }
  return [...groups.entries()]
    .map(([source, items]) => ({ source, items }))
    .sort((a, b) => {
      const ai = OURS.indexOf(a.source)
      const bi = OURS.indexOf(b.source)
      if (ai !== -1 || bi !== -1) return (ai === -1 ? OURS.length : ai) - (bi === -1 ? OURS.length : bi)
      return a.source.localeCompare(b.source)
    })
}

export interface HasLabel {
  name: string
  friendly_name?: string
  short_name?: string
}

/**
 * What a tool is called, on any screen that shows one.
 *
 * Its own words first. A tool also has a name the model calls it by, and that
 * is an internal identifier: it carries the prefix that keeps two services'
 * `query` apart, it obeys an alphabet a vendor accepts rather than one a person
 * reads, and it told nobody anything the row above it was not already saying.
 * It used to sit under every tool on three screens.
 *
 * The fallbacks are what is left when a tool has no words of its own: the name
 * without the prefix, and only then the whole thing. Ordered so the internal
 * part is the last resort rather than the default it used to be.
 */
export function toolLabel(t: HasLabel): string {
  return t.friendly_name || t.short_name || t.name
}
