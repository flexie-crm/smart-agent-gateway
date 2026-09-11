import { describe, expect, it } from 'vitest';
import { canAttach, pickerFilter, whyUnavailable, type ChatAccepts } from '../use-chat-accepts';

// What the composer may offer is a reading of the Gateway's configuration, so
// these are the readings. The interesting one is the negative: a button that
// offers to send a file nothing can read is the interface promising something
// the server will refuse, and the person finds out only after choosing.

const nothing: ChatAccepts = {
  fileTypes: [],
  anyFile: false,
  audio: false,
  maxBytes: 0,
  filesUnsupported: '',
  audioUnsupported: '',
  ready: false,
};

describe('what the composer may offer', () => {
  it('offers nothing before the answer has arrived', () => {
    // Not "assume yes and hide it later": a button that appears and then
    // vanishes is worse than one that appears when it is known to work.
    expect(canAttach({ ...nothing, fileTypes: ['pdf'], ready: false })).toBe(false);
  });

  it('offers nothing when no rule can read anything', () => {
    expect(canAttach({ ...nothing, ready: true })).toBe(false);
  });

  it('offers attaching when a rule names types', () => {
    expect(canAttach({ ...nothing, ready: true, fileTypes: ['pdf', 'docx'] })).toBe(true);
  });

  it('offers attaching when a rule catches everything', () => {
    expect(canAttach({ ...nothing, ready: true, anyFile: true })).toBe(true);
  });
});

describe('what the file picker filters to', () => {
  it('offers only the types a rule covers', () => {
    // So a file that would be refused is never chosen in the first place.
    expect(pickerFilter({ ...nothing, ready: true, fileTypes: ['pdf', 'docx'] })).toBe('.pdf,.docx');
  });

  it('filters nothing when a rule catches everything', () => {
    expect(pickerFilter({ ...nothing, ready: true, anyFile: true, fileTypes: ['pdf'] })).toBeUndefined();
  });

  it('filters nothing when there is nothing to filter to', () => {
    expect(pickerFilter({ ...nothing, ready: true })).toBeUndefined();
  });
});

describe('a vendor that cannot do the job it was set up for', () => {
  it('does not offer attaching, whatever the rules say', () => {
    // The administrator is not stopped from configuring it: that is their call
    // and their responsibility. What is stopped is offering a button that could
    // never work.
    expect(
      canAttach({
        ...nothing,
        ready: true,
        anyFile: true,
        filesUnsupported: 'Anthropic cannot be sent files.',
      }),
    ).toBe(false);
  });

  it('says why, so somebody who did not configure it can report it', () => {
    // "The button is missing" is not something a person can act on.
    const why = whyUnavailable({
      ...nothing,
      ready: true,
      audioUnsupported: 'Anthropic does not transcribe audio.',
    });
    expect(why).toContain('does not transcribe');
  });

  it('says nothing when nothing is wrong', () => {
    expect(whyUnavailable({ ...nothing, ready: true, anyFile: true })).toBe('');
  });
});
