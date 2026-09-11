import { useState } from 'react'
import { ApiError } from './api'

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
        // The request never got an answer. There is no field to blame.
        setForm('The gateway did not answer.')
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
 * describeError turns a caught failure into one line a person can read: the
 * gateway's own reason when it gave one, and a plain fallback when the request
 * never reached it. It is what an error toast says.
 */
export function describeError(failure: unknown): string {
  return failure instanceof ApiError ? sentence(failure.description) : 'The gateway did not answer.'
}
