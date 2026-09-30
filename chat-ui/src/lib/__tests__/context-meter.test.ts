import { describe, it, expect } from 'vitest'
import { NO_METER, filled, readMeter, showsMeter } from '../context-meter'

describe('readMeter', () => {
  it('reads what the server sends', () => {
    expect(readMeter({ percent: 72, warn_at: 80, compacting: true, failed: 'no' })).toEqual({
      percent: 72, warnAt: 80, compacting: true, failed: 'no',
    })
  })

  // A server a release behind sends no meter at all, and one with no window to
  // measure against sends no percent: both are "not known", never zero.
  it('reads anything missing or malformed as not known', () => {
    expect(readMeter(undefined)).toEqual(NO_METER)
    expect(readMeter({ warn_at: 50, compacting: false })).toEqual({ ...NO_METER, percent: null, warnAt: 50 })
    expect(readMeter({ percent: '64', warn_at: 'x', compacting: 'yes' })).toEqual(NO_METER)
  })
})

describe('showsMeter', () => {
  const at = (percent: number | null, warnAt = 50) => ({ ...NO_METER, percent, warnAt })

  it('shows from the threshold up, and not below it', () => {
    expect(showsMeter(at(49))).toBe(false)
    expect(showsMeter(at(50))).toBe(true)
    expect(showsMeter(at(130))).toBe(true)
    expect(showsMeter(at(79, 80))).toBe(false)
  })

  it('shows nothing when nothing is known', () => {
    expect(showsMeter(at(null))).toBe(false)
  })

  // Frozen while compacting, and a failure has to be said, whatever the number.
  it('shows while compacting and after a failed compaction, below the threshold too', () => {
    expect(showsMeter({ ...at(10), compacting: true })).toBe(true)
    expect(showsMeter({ ...at(10), failed: 'could not' })).toBe(true)
  })
})

describe('filled', () => {
  it('is the percent, and past full is still full', () => {
    expect(filled({ ...NO_METER, percent: 64 })).toBe(64)
    expect(filled({ ...NO_METER, percent: 140 })).toBe(100)
    expect(filled({ ...NO_METER, percent: null })).toBe(0)
  })
})
