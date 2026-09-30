import { useState } from 'react'
import { ApiError, Offline, Unreadable } from './api'

/**
 * The two levels a form can fail at.
 *
 * `form` is about the attempt as a whole; each entry of `fields` belongs to
 * one input, keyed by the API name of the field it is about. Local checks and
 * the gateway's refusals land in the same two places, so a form cannot tell,
 * and need not care, who refused it.
 */
export interface FormErrors {
  form: string
  fields: Record<string, string>
  /** Wipe both levels, ahead of a new attempt. */
  clear(): void
  /** Refuse the submit locally: these fields are wrong, nothing was sent. */
  reject(fields: Record<string, string>): void
  /** Read a failed request into the two levels. */
  fail(failure: unknown): void
}

/** What the form-level line says when the specifics sit on the fields. */
const MARKED_FIELDS = 'Check the marked fields.'

export function useFormErrors(): FormErrors {
  const [form, setForm] = useState('')
  const [fields, setFields] = useState<Record<string, string>>({})

  return {
    form,
    fields,
    clear() {
      setForm('')
      setFields({})
    },
    reject(problems: Record<string, string>) {
      setForm(MARKED_FIELDS)
      setFields(problems)
    },
    fail(failure: unknown) {
      if (!(failure instanceof ApiError)) {
        // Not a refusal, so there is no field to blame. Which of the other
        // three it is decides what is said, and describeError is the one place
        // that decides it.
        setForm(describeError(failure))
        setFields({})
        return
      }
      // The gateway states field problems as lowercase clauses; on a form
      // they stand alone, so they are shown as sentences.
      setFields(
        Object.fromEntries(
          Object.entries(failure.fields).map(([field, message]) => [field, sentence(message)]),
        ),
      )
      if (Object.keys(failure.fields).length > 0) {
        setForm(MARKED_FIELDS)
      } else if (failure.code === 'server_error') {
        setForm('Something went wrong on the gateway. Try again.')
      } else {
        setForm(sentence(failure.description))
      }
    },
  }
}

/** The gateway speaks in lowercase clauses; shown alone, a clause becomes a sentence. */
export function sentence(text: string): string {
  if (!text) return 'It could not be saved.'
  const capped = text[0].toUpperCase() + text.slice(1)
  return /[.!?]$/.test(capped) ? capped : `${capped}.`
}

/**
 * describeError turns a caught failure into one line a person can read.
 *
 * FOUR things can go wrong and they are genuinely different, so they are told
 * apart here rather than collapsed into one sentence:
 *
 *   - the gateway REFUSED, and said why, in words we wrote (ApiError). Its own
 *     reason is what the person reads;
 *   - the gateway BROKE, which is a refusal with a 5xx on it. Handled on the
 *     form level by fail(), because there is no field to blame;
 *   - the gateway SAID NOTHING (Offline): down, restarting, or the network went
 *     away. It says nothing about the request either, so trying again is the
 *     honest advice;
 *   - the ANSWER could not be read (Unreadable): something arrived and was not
 *     what it claimed to be;
 *   - and anything else is OURS.
 *
 * That last one is why this function is not a ternary any more. A fault in the
 * console used to be reported as the gateway not answering, which is a claim
 * about a server that is working perfectly, and it hid our own defects: nobody
 * reports "the console threw", they report "the gateway is down".
 */
export function describeError(failure: unknown): string {
  if (failure instanceof ApiError) return sentence(failure.description)
  if (failure instanceof Offline || failure instanceof Unreadable) return failure.message

  // Ours. The real error goes to the browser's console, because it is the only
  // copy of what happened and swallowing it is how a bug becomes unfindable.
  //
  // Reloading is the advice because it is the actual fix for the common cause:
  // a page left open across an upgrade, where the gateway has moved on to a
  // newer answer shape and this script is still the old one, reading a field
  // that is no longer there.
  console.error('the console failed while handling an answer', failure)
  return 'Something went wrong in the console. Reload the page and try again.'
}

