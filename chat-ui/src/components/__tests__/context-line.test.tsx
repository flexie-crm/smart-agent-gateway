import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { ContextLine } from '../ContextLine'
import { NO_METER, type ContextMeter } from '@lib/context-meter'

// The line above the message box. No jest-dom here (no setup file), so the
// assertions are on the DOM render() returns.

afterEach(cleanup)

const meter = (over: Partial<ContextMeter>): ContextMeter => ({ ...NO_METER, warnAt: 50, ...over })

function line(container: HTMLElement) {
  return container.querySelector('[data-context-line]') as HTMLElement | null
}

function filledTo(container: HTMLElement) {
  const bar = container.querySelector('[role="progressbar"] > div') as HTMLElement
  return bar.style.width
}

describe('ContextLine', () => {
  it('is not there below the threshold, or when nothing is known', () => {
    for (const m of [meter({ percent: 49 }), meter({ percent: null })]) {
      const { container } = render(<ContextLine meter={m} busy={false} onCompact={() => {}} />)
      expect(line(container)).toBeNull()
      cleanup()
    }
  })

  it('shows how full the conversation is from the threshold up, with the way to make room', () => {
    const onCompact = vi.fn()
    const { container } = render(<ContextLine meter={meter({ percent: 64 })} busy={false} onCompact={onCompact} />)
    expect(line(container)?.dataset.contextLine).toBe('shown')
    expect(filledTo(container)).toBe('64%')
    expect(screen.getByText('64% of context used')).toBeTruthy()

    const button = screen.getByRole('button', { name: /compact/i }) as HTMLButtonElement
    expect(button.disabled).toBe(false)
    fireEvent.click(button)
    expect(onCompact).toHaveBeenCalledOnce()
  })

  // Past full the oldest messages are being dropped: the line is full, and says
  // why, rather than a number over a hundred on a bar that cannot go further.
  it('says so when the conversation is past full', () => {
    const { container } = render(<ContextLine meter={meter({ percent: 130 })} busy={false} onCompact={() => {}} />)
    expect(line(container)?.dataset.contextLine).toBe('full')
    expect(filledTo(container)).toBe('100%')
    expect(screen.getByText(/oldest messages dropped/i)).toBeTruthy()
  })

  it('is frozen while compacting, whatever the number', () => {
    const onCompact = vi.fn()
    const { container } = render(<ContextLine meter={meter({ percent: 10, compacting: true })} busy={false} onCompact={onCompact} />)
    expect(line(container)?.dataset.contextLine).toBe('compacting')
    expect(screen.getByText('Compacting…')).toBeTruthy()
    const button = screen.getByRole('button', { name: /compact/i }) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    fireEvent.click(button)
    expect(onCompact).not.toHaveBeenCalled()
  })

  // A compaction cannot run under a turn being answered, so it is not offered.
  it('does not offer to compact while the assistant is answering', () => {
    render(<ContextLine meter={meter({ percent: 90 })} busy={true} onCompact={() => {}} />)
    expect((screen.getByRole('button', { name: /compact/i }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('says in place why a compaction did not happen', () => {
    render(<ContextLine meter={meter({ percent: 20, failed: 'Nothing new has been said.' })} busy={false} onCompact={() => {}} />)
    expect(screen.getByText('Nothing new has been said.')).toBeTruthy()
  })
})
