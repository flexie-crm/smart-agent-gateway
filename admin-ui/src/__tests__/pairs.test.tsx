import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { fireEvent, render, screen } from '@/test-utils'
import { PairRows } from '@/components/ui/pairs'

/**
 * A settings field that is a LIST of name/value pairs.
 *
 * It exists because a fixed pair of inputs ("second name", "second value")
 * could only ever carry one, and a service wanting a key AND a secret AND an
 * account id is ordinary.
 *
 * The rows are the interface and the stored value is one string, "name: value"
 * per line. That is not a shortcut for the renderer: a secret is sealed at rest
 * by path and only a string leaf is sealed, so a list stored there would be
 * skipped and every value written in plaintext. These tests are therefore about
 * BOTH halves: what a person does, and what comes out.
 */
function Harness({ start = '' }: { start?: string }) {
  const [value, setValue] = useState(start)
  return (
    <div>
      <PairRows value={value} onChange={setValue} />
      <pre data-testid="stored">{value}</pre>
    </div>
  )
}

const stored = () => screen.getByTestId('stored').textContent

describe('name and value rows', () => {
  it('starts with one row, carrying the plus and nothing to remove', () => {
    render(<Harness />)
    expect(screen.getAllByLabelText('Name')).toHaveLength(1)
    expect(screen.getAllByLabelText('Add')).toHaveLength(1)
    // Nothing to remove when it is the only row.
    expect(screen.queryByLabelText('Remove')).toBeNull()
  })

  // The button is ON the row, and which button depends on where the row is:
  // the last one adds, the ones above remove. So the control is always where
  // the eye already is.
  it('puts the plus on the last row and a remove on every row above it', () => {
    render(<Harness start={'A: 1\nB: 2\nC: 3'} />)
    expect(screen.getAllByLabelText('Add')).toHaveLength(1)
    expect(screen.getAllByLabelText('Remove')).toHaveLength(2)

    // And the plus is on the LAST row, not merely somewhere.
    const rowsShown = screen.getAllByLabelText('Name').map((i) => (i as HTMLInputElement).value)
    expect(rowsShown).toEqual(['A', 'B', 'C'])
    const lastRow = screen.getAllByLabelText('Name')[2].closest('div')!
    expect(lastRow.querySelector('[aria-label="Add"]')).not.toBeNull()
    expect(lastRow.querySelector('[aria-label="Remove"]')).toBeNull()
  })

  it('adds a row when Add is pressed, and keeps what was already typed', () => {
    render(<Harness />)
    fireEvent.change(screen.getAllByLabelText('Name')[0], { target: { value: 'X-Account' } })
    fireEvent.change(screen.getAllByLabelText('Value')[0], { target: { value: 'acct_1' } })
    fireEvent.click(screen.getByLabelText('Add'))

    expect(screen.getAllByLabelText('Name')).toHaveLength(2)
    expect((screen.getAllByLabelText('Name')[0] as HTMLInputElement).value).toBe('X-Account')

    fireEvent.change(screen.getAllByLabelText('Name')[1], { target: { value: 'X-Version' } })
    fireEvent.change(screen.getAllByLabelText('Value')[1], { target: { value: '2026-01-01' } })
    expect(stored()).toBe('X-Account: acct_1\nX-Version: 2026-01-01')
  })

  it('reads a stored value back into rows', () => {
    render(<Harness start={'A: 1\nB: 2\nC: 3'} />)
    expect(screen.getAllByLabelText('Name')).toHaveLength(3)
    expect((screen.getAllByLabelText('Value')[2] as HTMLInputElement).value).toBe('3')
  })

  it('takes a row away', () => {
    render(<Harness start={'A: 1\nB: 2'} />)
    // Only the first row has a remove here: the second is the last row and
    // carries the plus.
    fireEvent.click(screen.getAllByLabelText('Remove')[0])
    expect(stored()).toBe('B: 2')
    expect(screen.getAllByLabelText('Name')).toHaveLength(1)
  })

  // The separator is the FIRST colon, both ways: a URL with a port or a
  // timestamp is an ordinary header value, and splitting on every colon would
  // silently truncate it. The Go reader (tool.ReadPairs) follows the same rule,
  // and this is the half of it a person can see.
  it('keeps a colon inside a value', () => {
    render(<Harness start={'X-Callback: https://example.com:8443/hook'} />)
    expect((screen.getAllByLabelText('Value')[0] as HTMLInputElement).value).toBe(
      'https://example.com:8443/hook',
    )
    expect(stored()).toBe('X-Callback: https://example.com:8443/hook')
  })

  // An empty row is not a pair, so it contributes nothing: otherwise pressing
  // Add and saving would store a blank line the server has to refuse.
  it('does not store an empty row', () => {
    render(<Harness start={'A: 1'} />)
    fireEvent.click(screen.getByLabelText('Add'))
    expect(screen.getAllByLabelText('Name')).toHaveLength(2)
    expect(stored()).toBe('A: 1')
  })
})
