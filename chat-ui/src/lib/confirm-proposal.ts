/**
 * Reading an approval request: which argument IS the proposal, and how far it
 * reaches.
 *
 * Its own module rather than living in the card, so the card, the parked-card
 * renderer, and anything else that ever has to show one all answer these two
 * questions the same way. Two surfaces deciding separately what a call is about
 * is how two surfaces come to describe one call differently.
 */

/**
 * The arguments that could BE the thing about to happen, in preference order:
 * the statement, the command, the body being posted. Everything else is a
 * setting on it.
 *
 * Ordered rather than picked by length, because length picks wrong: a headers
 * blob is several times longer than the URL it belongs to, and the URL is what
 * somebody needs to read before they say yes.
 */
const PRINCIPAL = [
  'sql', 'statement', 'query',
  'command', 'script',
  'url', 'endpoint',
  'body', 'content', 'text', 'message',
  'title', 'path',
]

/** The proposal and the key it came from, or nothing when no argument is one. */
export function principalOf(details?: Record<string, unknown>): [string, string] | null {
  if (!details) return null
  for (const key of PRINCIPAL) {
    const value = details[key]
    if (typeof value === 'string' && value.trim() !== '') return [key, value]
  }
  return null
}

/**
 * Everything that is NOT the proposal: the method, the headers, the settings.
 *
 * Kept out of the component so the rule for what lands behind the fold is one
 * testable thing rather than a filter buried in some JSX. An empty value is not
 * a detail worth folding: it is nothing, and a row saying `body=` teaches
 * somebody less than no row at all.
 */
export function secondaryOf(
  details: Record<string, unknown> | undefined,
  skip?: string,
): [string, string][] {
  if (!details) return []
  return Object.entries(details)
    .filter(([k, v]) => k !== skip && text(v).trim() !== '')
    .map(([k, v]) => [k, text(v)])
}

/**
 * A detail's value as something readable. An argument is not always a string
 * on the wire (a number is a number, a headers blob is an object), and a card
 * that printed "[object Object]" beside a request somebody is authorising would
 * be worse than showing nothing.
 */
function text(v: unknown): string {
  if (typeof v === 'string') return v
  if (v === null || v === undefined) return ''
  if (typeof v === 'object') return JSON.stringify(v)
  return String(v)
}

/**
 * What kind of thing is about to happen, and how the card should say it.
 *
 * # The vocabulary is the SERVER's
 *
 * This card used to switch on `info | warning | danger`, three words inherited
 * from the reference CRM that the server has never once sent. What it sends is
 * the tool's own risk level, so every comparison here missed, every card fell
 * through to the same default, and the colour system was decoration: a card
 * whose own description said it reaches OUTSIDE your workspace was labelled
 * "Changes something in your workspace", in the one grey both branches shared.
 *
 * So the switch is on the six levels a tool can actually declare, and an
 * unknown one is treated as a WRITE rather than as a read: a level we do not
 * recognise is not a reason to say something reassuring.
 *
 * # What colour is for
 *
 * The accent is the colour of the CONSEQUENCE, and it runs all the way to the
 * button somebody presses. Not decoration and not a severity rainbow: three
 * weights, so the eye separates "reads things" from "sends something out" from
 * "cannot be undone" before any reading happens. The words still say it exactly;
 * the colour just says which sentence to read first.
 */
export type Risk =
  | 'read_only'
  | 'internal_write'
  | 'external_communication'
  | 'financial_action'
  | 'destructive_action'
  | 'admin_action'

/**
 * The three weights. More than three and nobody can tell them apart.
 *
 * Colour answers ONE question, and it is not "which of the six levels is this":
 * that is what the words are for, and they are exact. Colour says whether to
 * stop. So blue is everything ordinary enough to read and get on with, red is
 * what cannot be taken back, and neutral is the case where nothing happens at
 * all. An amber middle tier sat between blue and red saying neither.
 */
export type Accent = 'neutral' | 'blue' | 'rose'

export interface RiskLook {
  /** Which glyph, chosen by the component that owns the icon set. */
  icon: 'read' | 'write' | 'outside' | 'money' | 'delete' | 'access'
  accent: Accent
  /** The translation key for the scope line, and its English. */
  scopeKey: string
  scope: string
}

const LOOK: Record<Risk, RiskLook> = {
  read_only: {
    icon: 'read',
    accent: 'neutral',
    scopeKey: 'scope_read',
    scope: 'Only looks at things. Nothing changes.',
  },
  internal_write: {
    icon: 'write',
    accent: 'blue',
    scopeKey: 'scope_write',
    scope: 'Changes something saved in your workspace',
  },
  external_communication: {
    icon: 'outside',
    accent: 'blue',
    scopeKey: 'scope_outside',
    scope: 'Sends something out of your workspace',
  },
  admin_action: {
    icon: 'access',
    accent: 'blue',
    scopeKey: 'scope_access',
    scope: 'Changes settings, or who can do what',
  },
  financial_action: {
    icon: 'money',
    accent: 'rose',
    scopeKey: 'scope_money',
    scope: 'Can spend money or change what you are billed',
  },
  destructive_action: {
    icon: 'delete',
    accent: 'rose',
    scopeKey: 'scope_delete',
    scope: 'Removes something for good. It cannot be undone.',
  },
}

/**
 * How to present this approval. An unrecognised level lands on internal_write:
 * a level we cannot read is not grounds for telling somebody it is harmless.
 */
export function lookOf(severity: string | undefined): RiskLook {
  return LOOK[severity as Risk] ?? LOOK.internal_write
}
