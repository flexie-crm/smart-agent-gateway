/**
 * What the composer may offer.
 *
 * The attach button and the microphone are not preferences and not props: they
 * are a reading of how the Gateway is configured. Offering to send a file
 * nothing can read is the interface making a promise the server will refuse,
 * and the person finds out only after choosing the file.
 *
 * It arrives with the conversation, on the history answer's `meta.accepts`, and
 * until it does nothing is offered: the honest state, rather than a button that
 * might turn out to do nothing.
 */
export interface ChatAccepts {
  /** The file types that can be attached, lower case, without dots. */
  fileTypes: string[]
  /** A rule catches everything, so the picker should not filter at all. */
  anyFile: boolean
  /** Somebody can talk instead of typing. */
  audio: boolean
  /** The ceiling on one file, from the server, so one number lives in one place. */
  maxBytes: number
  /**
   * Why files or audio cannot be used, when a vendor was set up for the job and
   * cannot do it. Empty means nothing is wrong.
   *
   * This is not a restriction on what an administrator may configure: they pick
   * the model, and whether it understands a spreadsheet is their call. It is the
   * narrower fact that the vendor cannot be asked at all, which is worth saying
   * on the screen rather than leaving as a button that fails.
   */
  filesUnsupported: string
  audioUnsupported: string
  /** The answer has arrived. Before this, offer nothing. */
  ready: boolean
}

/**
 * Nothing known yet, which is not the same as nothing allowed.
 *
 * `ready` is false, and every question below reads it: a button that appears
 * and then vanishes is worse than one that appears when it is known to work.
 */
export const NO_ACCEPTS: ChatAccepts = {
  fileTypes: [],
  anyFile: false,
  audio: false,
  maxBytes: 0,
  filesUnsupported: '',
  audioUnsupported: '',
  ready: false,
}

/**
 * The answer, read off the history response's `meta.accepts`.
 *
 * This was a request of its own (`GET /v1/chat/accepts`), shared through a
 * module-level promise so that mounting twice asked once. That machinery is
 * gone with the endpoint: what the composer may offer is needed exactly when a
 * conversation opens and at no other time, so it rides on the one request a
 * conversation already makes.
 */
export function readAccepts(body: unknown): ChatAccepts {
  if (!body || typeof body !== 'object') return NO_ACCEPTS
  const a = body as Record<string, unknown>
  return {
    fileTypes: Array.isArray(a.file_types) ? (a.file_types as string[]) : [],
    anyFile: Boolean(a.any_file),
    audio: Boolean(a.audio),
    maxBytes: Number(a.max_bytes) || 0,
    filesUnsupported: String(a.files_unsupported || ''),
    audioUnsupported: String(a.audio_unsupported || ''),
    ready: true,
  }
}

/** canAttach is whether anything at all may be sent as a file. */
export function canAttach(a: ChatAccepts): boolean {
  return a.ready && !a.filesUnsupported && (a.anyFile || a.fileTypes.length > 0)
}

/**
 * whyUnavailable is what to tell somebody when the Gateway was configured for
 * files or audio and the vendor behind it cannot do the job.
 *
 * It is shown rather than swallowed because the person looking at the chat is
 * often not the person who set it up, and "the button is missing" is not
 * something they can act on or report.
 */
export function whyUnavailable(a: ChatAccepts): string {
  return [a.filesUnsupported, a.audioUnsupported].filter(Boolean).join(' ')
}

/**
 * pickerFilter is the `accept` attribute for the file dialog: the types a rule
 * covers, so a file that would be refused is never offered in the first place.
 * A catch-all filters nothing.
 */
export function pickerFilter(a: ChatAccepts): string | undefined {
  if (a.anyFile || a.fileTypes.length === 0) return undefined
  return a.fileTypes.map((t) => `.${t}`).join(',')
}
