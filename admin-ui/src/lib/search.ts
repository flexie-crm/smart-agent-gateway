/**
 * What every full-text search on the console has in common.
 *
 * Two screens search (brains, skills) and both hit an endpoint with the same
 * ceiling on it, so the ceiling and the sentence that reports it are one thing
 * here rather than one thing each. Two copies of `SEARCH_LIMIT` is how one
 * screen quietly starts asking for more than the server will give.
 */

/**
 * The most matches one search asks for. It is the server's own ceiling
 * (`searchMaxLimit` in the API), so a search that comes back with exactly this
 * many was cut off, and the screen says so instead of implying it found
 * everything.
 */
export const SEARCH_LIMIT = 50;

/**
 * How a count of MATCHES is written.
 *
 * A pane narrowed by a search must not show a total: "3 documents" next to a
 * single row reads as a screen that has lost two of them.
 */
export function matchCount(n: number): string {
  return `${n} ${n === 1 ? "match" : "matches"}`;
}

/**
 * What the search band says about what came back.
 *
 * Nothing at all when nothing has been asked, because a box somebody has not
 * typed in yet has no answer to report. And "first 50 matches" rather than "50
 * matches" when the answer is exactly the ceiling: at that point what we have
 * is a page of the matches, and saying the count plainly would claim it was all
 * of them.
 */
export function summarise(hits: { length: number } | null): string {
  if (hits === null) return "";
  if (hits.length === SEARCH_LIMIT) return `first ${SEARCH_LIMIT} matches`;
  return matchCount(hits.length);
}
