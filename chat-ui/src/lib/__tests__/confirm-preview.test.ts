import { describe, expect, it } from 'vitest';
import { principalOf, secondaryOf, lookOf } from '../confirm-proposal';

// The card has to pick which argument IS the proposal, and clamp it.
//
// Both were the bug in the old card: it showed every argument equally, in
// break-all monospace, so the URL you needed to read arrived shattered under a
// headers blob you did not.

describe('which argument is the proposal', () => {
  it('prefers the thing being done over the settings around it', () => {
    // The real case from the screenshot: a headers blob is four times longer
    // than the URL, so picking the longest value picks the wrong one.
    const details = {
      headers: '{"User-Agent":"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36","Accept":"application/json"}',
      url: 'https://api.nasdaq.com/api/quote/SPX/summary?assetclass=indices',
      method: 'GET',
    };
    expect(principalOf(details)?.[0]).toBe('url');
  });

  it('picks the statement for a database tool', () => {
    expect(principalOf({ sql: 'SELECT 1', database: 'shop' })?.[0]).toBe('sql');
  });

  it('picks the command for a server tool', () => {
    expect(principalOf({ command: 'systemctl restart nginx', wait: '5' })?.[0]).toBe('command');
  });

  it('has no opinion when nothing is a proposal', () => {
    expect(principalOf({ id: '4', mode: 'auto' })).toBeNull();
    expect(principalOf(undefined)).toBeNull();
    // An empty value is not a proposal either.
    expect(principalOf({ url: '   ' })).toBeNull();
  });
});

describe('what falls behind the fold', () => {
  // The line this replaces sat open under the URL and read
  // `method=GET headers={"User-Agent":"Mozilla/5.0"…}`, which is a debug dump
  // where a decision should be. It is folded now, never dropped: what is about
  // to run must still be readable in full by whoever authorises it.
  const request = {
    url: 'https://api.example.com/quote',
    method: 'GET',
    headers: '{"Accept":"application/json"}',
    body: '   ',
  };

  it('folds everything except the proposal itself', () => {
    const keys = secondaryOf(request, principalOf(request)?.[0]).map(([k]) => k);
    expect(keys).toEqual(['method', 'headers']);
  });

  it('folds an empty value away rather than showing an empty row', () => {
    // `body=` teaches somebody less than no row at all.
    expect(secondaryOf(request).map(([k]) => k)).not.toContain('body');
  });

  it('has nothing to fold when the call is only its proposal', () => {
    expect(secondaryOf({ sql: 'SELECT 1' }, 'sql')).toEqual([]);
    expect(secondaryOf(undefined)).toEqual([]);
  });
});

describe('how far the card says an action reaches', () => {
  // The bug this replaces: the card switched on `info | warning | danger`,
  // three words the server has never sent. Every comparison missed, so a card
  // whose own description said it reaches OUTSIDE the workspace was labelled
  // "Changes something in your workspace". The vocabulary is the server's.

  it('reads the risk levels the server actually sends', () => {
    expect(lookOf('external_communication').scope).toMatch(/out of your workspace/);
    expect(lookOf('destructive_action').scope).toMatch(/cannot be undone/i);
    expect(lookOf('financial_action').scope).toMatch(/money/);
    expect(lookOf('admin_action').scope).toMatch(/who can do what/);
    expect(lookOf('internal_write').scope).toMatch(/in your workspace/);
    expect(lookOf('read_only').scope).toMatch(/Nothing changes/);
  });

  it('lets colour say one thing: whether to stop', () => {
    // Six levels, three colours, and that is deliberate. WHICH level it is, the
    // words say exactly. Colour answers the cruder question first.
    expect(lookOf('read_only').accent).toBe('neutral');
    expect(lookOf('internal_write').accent).toBe('blue');
    expect(lookOf('external_communication').accent).toBe('blue');
    expect(lookOf('admin_action').accent).toBe('blue');
    // The two you cannot take back, and only those two, are red.
    expect(lookOf('financial_action').accent).toBe('rose');
    expect(lookOf('destructive_action').accent).toBe('rose');
    expect(new Set(ALL.map((r) => lookOf(r).accent)).size).toBe(3);
  });

  it('treats a level it cannot read as a write, never as a read', () => {
    // A level we do not recognise is not grounds for saying "nothing changes".
    expect(lookOf(undefined).scope).toBe(lookOf('internal_write').scope);
    expect(lookOf('danger').scope).toBe(lookOf('internal_write').scope);
    expect(lookOf('').accent).not.toBe('neutral');
  });
});

const ALL = [
  'read_only',
  'internal_write',
  'external_communication',
  'financial_action',
  'destructive_action',
  'admin_action',
] as const;
