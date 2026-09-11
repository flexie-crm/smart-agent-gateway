import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// Vitest only tears the DOM down automatically when it injects globals, and we
// do not inject globals. Without this, one test's screen is still on the page
// while the next one looks at it, and "found multiple elements" is the kindest
// way that can fail.
afterEach(cleanup)

// jsdom has no ResizeObserver, and the checkbox primitive measures itself with
// one. Nothing in a test cares about the measurement, only that mounting does
// not throw.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
globalThis.ResizeObserver ??= ResizeObserverStub as unknown as typeof ResizeObserver
