import { describe, expect, it } from 'vitest';
import { toolStatusIsError } from '../tool-timeline';

// A finished tool the server marks failed or rejected is an error (rendered red);
// a completed or still-running one is not. These are the exact statuses the
// server sends, so the timeline must key off them, not off values it never sees.
describe('toolStatusIsError', () => {
  it('treats failed and rejected as errors', () => {
    expect(toolStatusIsError('failed')).toBe(true);
    expect(toolStatusIsError('rejected')).toBe(true);
  });

  it('does not treat completed, running, or unknown as errors', () => {
    expect(toolStatusIsError('completed')).toBe(false);
    expect(toolStatusIsError('running')).toBe(false);
    expect(toolStatusIsError(null)).toBe(false);
    expect(toolStatusIsError(undefined)).toBe(false);
    // Guard the old bug: the server never sends these, so they must not be errors.
    expect(toolStatusIsError('error')).toBe(false);
    expect(toolStatusIsError('timeout')).toBe(false);
  });
});
