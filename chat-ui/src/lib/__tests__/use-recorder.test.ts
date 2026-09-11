import { describe, expect, it } from 'vitest';
import { clock } from '../use-recorder';

// The elapsed time as somebody reads it. Small, but it is the only thing on the
// screen telling them it is still listening, so an hour of recording must not
// suddenly read as one minute.
describe('the recording clock', () => {
  it('reads as minutes and seconds', () => {
    expect(clock(0)).toBe('0:00');
    expect(clock(7)).toBe('0:07');
    expect(clock(59)).toBe('0:59');
    expect(clock(60)).toBe('1:00');
    expect(clock(61)).toBe('1:01');
    expect(clock(3599)).toBe('59:59');
  });

  it('keeps counting past an hour rather than wrapping', () => {
    // Nobody should record for an hour, but if they do, the number has to keep
    // meaning what it says.
    expect(clock(3600)).toBe('60:00');
    expect(clock(3661)).toBe('61:01');
  });
});
