import { describe, expect, it, vi, afterEach } from 'vitest';

import { machineLinkIsUp, watchMachineLink } from '../machine-link';

// Whether this computer's network can be reached, as the window sees it.
//
// In a browser there is no route to be up and nothing to ask, and the answer
// must be a quiet "no" rather than an error: the mark simply stays its ordinary
// colour, which is also what it does when a route exists and is down.

describe('the machine link, as the window sees it', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('is down in a browser, and asks nothing', async () => {
    expect(await machineLinkIsUp()).toBe(false);
    const stop = await watchMachineLink(() => {
      throw new Error('a browser has no link to report');
    });
    stop();
  });

  it('reads the state the application reports', async () => {
    const invoke = vi.fn(async (command: string) => command === 'link_up');
    vi.stubGlobal('window', {
      ...globalThis.window,
      __TAURI__: { core: { invoke }, event: { listen: vi.fn() } },
    });
    expect(await machineLinkIsUp()).toBe(true);
    expect(invoke).toHaveBeenCalledWith('link_up');
  });

  it('says down rather than throwing when the application will not answer', async () => {
    vi.stubGlobal('window', {
      ...globalThis.window,
      __TAURI__: {
        core: {
          invoke: vi.fn(async () => {
            throw new Error('no such command');
          }),
        },
        event: { listen: vi.fn() },
      },
    });
    expect(await machineLinkIsUp()).toBe(false);
  });

  it('reports the state at once, then on every change', async () => {
    const announcer: { fire?: (event: { payload: unknown }) => void } = {};
    vi.stubGlobal('window', {
      ...globalThis.window,
      __TAURI__: {
        core: { invoke: vi.fn(async () => true) },
        event: {
          listen: vi.fn(async (_event: string, handler: (e: { payload: unknown }) => void) => {
            announcer.fire = handler;
            return () => {};
          }),
        },
      },
    });

    const seen: boolean[] = [];
    await watchMachineLink((up) => seen.push(up));
    // The window has just opened and missed whatever was announced before it
    // existed, so it is told what the state is now.
    await vi.waitFor(() => expect(seen).toEqual([true]));

    announcer.fire?.({ payload: false });
    expect(seen).toEqual([true, false]);
  });
});
