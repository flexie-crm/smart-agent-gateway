import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { ChatStreamClient } from '../chat-stream-client';
import { setSession, refresh, bootstrap, Offline } from '../api';
import { resetDeviceForTest } from '../machine-link';

// SAG: these tests pin the request contract. It changed from the CRM's, on
// purpose: the body says what it means (`prompt`, not `q`), the server assigns
// session ids so the client never invents one, and auth is a bearer token
// rather than an opaque field in the body.

function mockFetchCapture() {
  const captured: Array<{ url: any; body: any; headers: any }> = [];
  const fn = vi.fn(async (url: any, opts: any) => {
    captured.push({ url, body: JSON.parse(opts.body), headers: opts.headers });
    return {
      ok: true,
      status: 200,
      body: { getReader: () => ({ read: async () => ({ done: true, value: undefined }) }) },
    } as any;
  });
  vi.stubGlobal('fetch', fn);
  return captured;
}

async function runStart(client: ChatStreamClient, prompt = 'hi', files?: string[]) {
  await client.start(prompt, () => {}, 'sess-key', undefined, undefined, 0, files);
}

// Emit SSE frames on the FIRST call only; later calls end immediately.
function mockFetchSSE(frames: any[]) {
  const captured: Array<{ url: any; body: any }> = [];
  let call = 0;
  const fn = vi.fn(async (url: any, opts: any) => {
    captured.push({ url, body: JSON.parse(opts.body) });
    const emit = call === 0 ? frames.slice() : [];
    call++;
    const enc = new TextEncoder();
    let i = 0;
    return {
      ok: true,
      status: 200,
      body: { getReader: () => ({ read: async () => {
        if (i < emit.length) { return { done: false, value: enc.encode('data: ' + JSON.stringify(emit[i++]) + '\n\n') }; }
        return { done: true, value: undefined };
      } }) },
    } as any;
  });
  vi.stubGlobal('fetch', fn);
  return captured;
}

describe('the request the client sends', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('sends what kind of computer this is, and nothing when there is none', async () => {
    // The half of the seam that lives here: the page has to ASK the application
    // and put the answer in the body. Everything below it is proved on the Go
    // side; if this line goes missing, that all still passes and the assistant
    // silently goes back to guessing.
    const cap = mockFetchCapture();
    const described = { os: 'windows', arch: 'x86_64', shell: 'cmd.exe', has: ['git'], missing: ['bash'] };
    (window as any).__TAURI__ = { core: { invoke: async (name: string) => (name === 'machine_environment' ? described : '') } };
    await runStart(new ChatStreamClient('/v1/chat/stream'), 'build it');
    expect(cap[0].body.machine).toEqual(described);

    // In a browser there is no application to ask, so the field is absent
    // rather than present and empty: the server treats "said nothing" and
    // "described nothing" differently, and only one of them is true here.
    delete (window as any).__TAURI__;
    resetDeviceForTest();
    const browser = mockFetchCapture();
    await runStart(new ChatStreamClient('/v1/chat/stream'), 'build it');
    expect(browser[0].body.machine).toBeUndefined();
  });

  it('names its fields, so a reader does not have to decode them', async () => {
    const cap = mockFetchCapture();
    await runStart(new ChatStreamClient('/v1/chat/stream'), 'what day is it?');

    expect(cap[0].body.prompt).toBe('what day is it?');
    // The CRM's one-letter keys are gone.
    expect(cap[0].body.q).toBeUndefined();
    expect(cap[0].body.k).toBeUndefined();
  });

  // A new conversation carries no session id at all: the server assigns one and
  // announces it, so the client never invents an id the server has to honour.
  it('omits the session on a new conversation', async () => {
    const cap = mockFetchCapture();
    await runStart(new ChatStreamClient('/v1/chat/stream'));

    expect(cap[0].body.chat_id).toBeUndefined();
    expect(cap[0].body.new_chat).toBeUndefined();
    expect(cap[0].body.new_chat_uid).toBeUndefined();
  });

  it('sends the chat id once it has one, opaquely', async () => {
    const cap = mockFetchCapture();
    const client = new ChatStreamClient('/v1/chat/stream');
    client.setChatId('42');
    await runStart(client);

    expect(cap[0].body.chat_id).toBe('42');
  });

  it('drops the chat id when the conversation is reset', async () => {
    const cap = mockFetchCapture();
    const client = new ChatStreamClient('/v1/chat/stream');
    client.setChatId('42');
    client.setChatId(null);
    await runStart(client);

    expect(cap[0].body.chat_id).toBeUndefined();
  });

  // The server announces the id it assigned. A retry must continue that
  // conversation rather than open a second one.
  it('adopts the session the server created, so a retry does not start another', async () => {
    const cap = mockFetchSSE([{ type: 'chat_created', chat_id: '7' }]);
    const client = new ChatStreamClient('/v1/chat/stream');

    await runStart(client, 'first');
    expect(cap[0].body.chat_id).toBeUndefined();

    await runStart(client, 'second');
    expect(cap[1].body.chat_id).toBe('7');
  });

  it('carries the host context, which is where the model is named for now', async () => {
    const cap = mockFetchCapture();
    const client = new ChatStreamClient('/v1/chat/stream', undefined, { model_id: 3 });
    await runStart(client);

    expect(cap[0].body.model_id).toBe(3);
  });

  it('sends a confirmation decision as its own turn', async () => {
    const cap = mockFetchCapture();
    const client = new ChatStreamClient('/v1/chat/stream');
    client.setChatId('9');
    await client.start('', () => {}, 'sess-key', undefined, undefined, 0, undefined, 'tok-1', 'rejected');

    expect(cap[0].body.resume_token).toBe('tok-1');
    expect(cap[0].body.resume_action).toBe('rejected');
    expect(cap[0].body.chat_id).toBe('9');
  });

  it('turns "approve all" into an approval plus a session flag', async () => {
    const cap = mockFetchCapture();
    const client = new ChatStreamClient('/v1/chat/stream');
    await client.start('', () => {}, 'sess-key', undefined, undefined, 0, undefined, 'tok-2', 'approved_all');

    // The server never sees "approved_all": it is a normal approval that also
    // switches the conversation to auto-approve for the rest of the session.
    expect(cap[0].body.resume_action).toBe('approved');
    expect(cap[0].body.approve_all).toBe(true);
  });

  it('authenticates with a bearer token rather than a field in the body', async () => {
    // The session lives in memory now, not localStorage: seed it the way the app
    // does, through setSession, and the client reads the bearer from there.
    setSession({ accessToken: 'tok-abc', user: { id: 1, email: 'e', name: 'n' }, workspace: null, workspaces: [] });
    const cap = mockFetchCapture();
    await runStart(new ChatStreamClient('/v1/chat/stream'));

    const headers = cap[0].headers as Headers;
    expect(headers.get('Authorization')).toBe('Bearer tok-abc');
    expect(cap[0].body.t).toBeUndefined();
    setSession(null);
  });
});

// A turn outlives the request that asked for it. That changes what a broken
// connection means: the answer is still being written, so the client rejoins it
// rather than asking the question a second time.
describe('rejoining a turn', () => {
  afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

  it('asks for what it missed, not for the answer again', async () => {
    const calls: Array<{ url: string; body: any }> = [];
    let first = true;
    vi.stubGlobal('fetch', vi.fn(async (url: any, opts: any) => {
      calls.push({ url: String(url), body: JSON.parse(opts.body) });
      if (first) {
        first = false;
        // The connection drops mid-answer, after two frames.
        const enc = new TextEncoder();
        const frames = [
          { type: 'chat_created', chat_id: 'ch_x', index: 0 },
          { type: 'delta', message: 'half an ', index: 1 },
        ];
        let i = 0;
        return {
          ok: true, status: 200,
          body: { getReader: () => ({ read: async () => {
            if (i < frames.length) {
              return { done: false, value: enc.encode('data: ' + JSON.stringify(frames[i++]) + '\n\n') };
            }
            throw new Error('network died');
          } }) },
        } as any;
      }
      // The rejoin: the rest of the answer.
      const enc = new TextEncoder();
      const frames = [
        { type: 'delta', message: 'answer.', index: 2 },
        { type: 'result', final: true, message: 'half an answer.', index: 3 },
      ];
      let i = 0;
      return {
        ok: true, status: 200,
        body: { getReader: () => ({ read: async () => {
          if (i < frames.length) {
            return { done: false, value: enc.encode('data: ' + JSON.stringify(frames[i++]) + '\n\n') };
          }
          return { done: true, value: undefined };
        } }) },
      } as any;
    }));

    const client = new ChatStreamClient('/v1/chat/stream', 'tok', undefined, undefined, {
      attach: '/v1/chat/attach', cancel: '/v1/chat/cancel',
    });

    let text = '';
    const ended = new Promise<void>(resolve => {
      void client.start('tell me', (d: string) => { text += d; }, 'sess', undefined, () => resolve());
    });

    // The retry is on a timer; let it fire.
    await vi.waitFor(() => expect(calls.length).toBe(2), { timeout: 5000 });
    await ended;

    // The second request is a rejoin, not a second question.
    expect(calls[1].url).toContain('/v1/chat/attach');
    expect(calls[1].body.prompt).toBeUndefined();
    expect(calls[1].body.chat_id).toBe('ch_x');
    // And it says how far it got, so it is given exactly what came next.
    expect(calls[1].body.after).toBe(1);
    // No word arrives twice, and none is missing.
    expect(text).toBe('half an answer.');
  }, 10000);

  it('does nothing when there is nothing to rejoin', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 204, body: null }) as any));

    const client = new ChatStreamClient('/v1/chat/stream', 'tok', undefined, undefined, {
      attach: '/v1/chat/attach',
    });
    client.setChatId('ch_quiet');

    const outcome = await client.attach(() => {});
    expect(outcome).toBe('none');
  });

  it('stops the turn by saying so, because hanging up no longer does', async () => {
    const calls: Array<{ url: string; body: any }> = [];
    vi.stubGlobal('fetch', vi.fn(async (url: any, opts: any) => {
      calls.push({ url: String(url), body: JSON.parse(opts.body) });
      return { ok: true, status: 204, body: null } as any;
    }));

    const client = new ChatStreamClient('/v1/chat/stream', 'tok', undefined, undefined, {
      cancel: '/v1/chat/cancel',
    });
    client.setChatId('ch_x');

    await client.cancel();

    expect(calls).toHaveLength(1);
    expect(calls[0].url).toContain('/v1/chat/cancel');
    expect(calls[0].body.chat_id).toBe('ch_x');
  });
});

// A REFUSAL is the server answering, and it has to reach the person.
//
// The bug: approving a card the server had already resolved sent a dead token,
// the server said 410 with a reason, and the client logged it to the console and
// called onEnd. The card had already been painted "Approved" optimistically, so
// the UI claimed the action went through while nothing at all had happened, and
// the queued cards behind it never moved (the server only releases the next one
// on a claim that succeeds). What a person saw was an empty assistant bubble.
function mockFetchStatus(status: number, body: unknown) {
  const fn = vi.fn(async () => ({
    ok: false,
    status,
    body: null,
    json: async () => body,
  }) as any);
  vi.stubGlobal('fetch', fn);
  return fn;
}

describe('a refused resume', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('reaches the caller as an error frame carrying the reason', async () => {
    mockFetchStatus(410, {
      error: 'invalid_token',
      message: 'this request is no longer awaiting a decision',
    });
    const frames: any[] = [];
    let ended = false;

    await new ChatStreamClient('/v1/chat/stream').start(
      '', () => {}, 'sess-key',
      (f) => frames.push(f),
      () => { ended = true },
      0, undefined, 'sag_cf_dead', 'approved',
    );

    expect(frames).toHaveLength(1);
    expect(frames[0].type).toBe('error');
    // The CODE, not just the sentence: a caller that resyncs on a stale card has
    // to recognise this refusal, and matching on prose breaks the first time
    // somebody improves the wording.
    expect(frames[0].code).toBe('invalid_token');
    expect(frames[0].message).toBe('this request is no longer awaiting a decision');
    expect(ended).toBe(true);
  });

  it('says so for an expired approval too', async () => {
    mockFetchStatus(410, {
      error: 'expired',
      message: 'this request expired and can no longer be approved',
    });
    const frames: any[] = [];
    await new ChatStreamClient('/v1/chat/stream').start(
      '', () => {}, 'sess-key', (f) => frames.push(f), undefined,
      0, undefined, 'sag_cf_old', 'approved',
    );
    expect(frames[0].code).toBe('expired');
  });

  it('does not retry a refusal', async () => {
    // Retrying a dead token just spam-fires the same failing request.
    const fetchFn = mockFetchStatus(410, { error: 'invalid_token', message: 'no' });
    await new ChatStreamClient('/v1/chat/stream').start(
      '', () => {}, 'sess-key', () => {}, undefined, 0, undefined, 'sag_cf_dead', 'approved',
    );
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it('still says something when the server sends no body it can read', async () => {
    const fn = vi.fn(async () => ({
      ok: false, status: 403, body: null,
      json: async () => { throw new Error('not json') },
    }) as any);
    vi.stubGlobal('fetch', fn);
    const frames: any[] = [];
    await new ChatStreamClient('/v1/chat/stream').start(
      '', () => {}, 'sess-key', (f) => frames.push(f), undefined, 0, undefined, 'tok', 'approved',
    );
    expect(frames[0].type).toBe('error');
    expect(frames[0].message).toBeTruthy();
    expect(frames[0].code).toBeUndefined();
  });
});

// A SERVER ERROR IS NOT A REFUSAL.
//
// The session refresh treated every non-2xx as "your session is over". Only a
// thrown fetch (a refused connection) became Offline. But a restart behind a dev
// proxy does not refuse the connection: the proxy answers 500 or 502 on the
// backend's behalf, which is a real HTTP response, so the session was thrown
// away and the person was signed out mid-sentence. Same bug as treating silence
// as a refusal, and harder to see, because an answer really did come back.
describe('what ends a session and what does not', () => {
  afterEach(() => { vi.unstubAllGlobals(); setSession(null); });

  const refreshReturning = (status: number) => {
    vi.stubGlobal('fetch', vi.fn(async (url: any) => {
      if (String(url).includes('/v1/auth/refresh')) {
        return { ok: status >= 200 && status < 300, status, json: async () => ({}) } as any;
      }
      return { ok: false, status: 401, json: async () => ({}) } as any;
    }));
  };

  it('does not end a session because the gateway is restarting', async () => {
    for (const status of [500, 502, 503, 504]) {
      refreshReturning(status);
      await expect(refresh()).rejects.toThrow(Offline);
    }
  });

  it('does end a session when the server actually says no', async () => {
    refreshReturning(401);
    await expect(refresh()).resolves.toBeNull();
  });
});

// Reloading the page during a restart must not sign anybody out.
//
// bootstrap used to catch Offline and return null, with a comment saying that
// is not "please sign in" while returning the one value the caller reads as
// exactly that. A restart is seconds; it waits for one.
describe('reloading while the gateway is restarting', () => {
  afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); setSession(null); });

  it('waits for the gateway rather than dropping to sign-in', async () => {
    let calls = 0;
    vi.stubGlobal('fetch', vi.fn(async () => {
      calls++;
      // Down for the first two attempts, then back with the session.
      if (calls <= 2) throw new TypeError('Failed to fetch');
      return {
        ok: true, status: 200,
        json: async () => ({
          access_token: 'fresh', user: { id: 1, email: 'a@b.c' }, workspace: null,
        }),
      } as any;
    }));

    const recovered = await bootstrap();
    expect(recovered).not.toBeNull();
    expect(recovered!.accessToken).toBe('fresh');
    expect(calls).toBe(3);
  });

  // Longer than the sum of the waits, on purpose: this asserts they are BOUNDED.
  // It is a slow test because the thing it pins is a duration.
  it('gives up eventually, because a page that never resolves is broken too', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }));
    const started = Date.now();
    await expect(bootstrap()).resolves.toBeNull();
    // It waited a while (it is not giving up on the first failure)...
    expect(Date.now() - started).toBeGreaterThan(20_000);
    // ...but it did give up.
  }, 60_000);

  it('still signs out when the gateway answers that the cookie is dead', async () => {
    // A refusal is an answer. One attempt, no waiting, straight to sign-in.
    const fn = vi.fn(async () => ({ ok: false, status: 401, json: async () => ({}) }) as any);
    vi.stubGlobal('fetch', fn);
    await expect(bootstrap()).resolves.toBeNull();
    expect(fn).toHaveBeenCalledTimes(1);
  });
});

// Two refreshes of one cookie can be in the air at once: a page load and a
// socket reconnect, two tabs, an effect invoked twice. Rotation is single-use,
// so exactly one wins and the server refuses the other. That loser is a
// duplicate of a request that just worked, and treating it as a dead session
// signed people out for succeeding.
describe('a refresh that loses a race', () => {
  afterEach(() => { vi.unstubAllGlobals(); setSession(null); });

  it('does not end a session another refresh has just renewed', async () => {
    setSession(null);
    vi.stubGlobal('fetch', vi.fn(async () => {
      // While this attempt is in flight, the winner lands its session.
      setSession({ accessToken: 'won-by-the-other-one' } as any);
      return { ok: false, status: 401, json: async () => ({}) } as any;
    }));
    const recovered = await refresh();
    expect(recovered).not.toBeNull();
    expect(recovered!.accessToken).toBe('won-by-the-other-one');
  });

  it('still ends it when nothing arrived while it was in flight', async () => {
    // Precise, not forgiving: a genuinely dead cookie is still an answer.
    setSession(null);
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 401, json: async () => ({}) }) as any));
    await expect(refresh()).resolves.toBeNull();
  });
});


// Which computer the message was typed on.
//
// A tool that reaches this machine's own network has to reach THIS machine, and
// the server cannot work that out: the same person may be signed in on a laptop
// and a desktop. The request is the only thing that knows, so it has to say —
// and if it silently stopped saying, every such tool would answer "the chat
// application is not connected on this computer" and look like a broken link
// rather than a missing field.
describe('the computer a message came from', () => {
  // Before AND after: the answer is remembered for the life of the page (a
  // message must not wait on a round trip to Rust), so a test that ran earlier
  // in this file has already cached "there is no application here".
  beforeEach(resetDeviceForTest);
  afterEach(() => {
    vi.unstubAllGlobals();
    resetDeviceForTest();
  });

  it('travels with the message when there is an application under the page', async () => {
    const cap = mockFetchCapture();
    vi.stubGlobal('window', {
      ...globalThis.window,
      __TAURI__: {
        core: { invoke: vi.fn(async (command: string) => (command === 'link_device' ? 'this-laptop' : null)) },
        event: { listen: vi.fn() },
      },
    });

    await runStart(new ChatStreamClient('/v1/chat/stream'), 'read my file');
    expect(cap[0].body.device_id).toBe('this-laptop');
  });

  // The folder is chosen in the application and kept on that computer, so the
  // server has no way to ask. Unsent, an assistant holding the file tools tells
  // somebody it cannot see their project while able to read every file in it.
  it('carries the working folder, so the assistant knows where it is', async () => {
    const cap = mockFetchCapture();
    vi.stubGlobal('window', {
      ...globalThis.window,
      __TAURI__: {
        core: {
          invoke: vi.fn(async (command: string) => {
            if (command === 'link_device') return 'this-laptop';
            if (command === 'chosen_folder') return '/Users/someone/sia';
            return null;
          }),
        },
        event: { listen: vi.fn() },
      },
    });

    await runStart(new ChatStreamClient('/v1/chat/stream'), 'what is in here?');
    expect(cap[0].body.working_folder).toBe('/Users/someone/sia');
  });

  // Read every turn rather than cached with the device: somebody changes it,
  // or takes it away, in the middle of a conversation.
  it('leaves the folder out when none has been chosen', async () => {
    const cap = mockFetchCapture();
    vi.stubGlobal('window', {
      ...globalThis.window,
      __TAURI__: {
        core: { invoke: vi.fn(async (command: string) => (command === 'link_device' ? 'this-laptop' : null)) },
        event: { listen: vi.fn() },
      },
    });

    await runStart(new ChatStreamClient('/v1/chat/stream'), 'hello');
    expect('working_folder' in cap[0].body).toBe(false);
  });

  it('is absent in a browser, rather than empty or invented', async () => {
    const cap = mockFetchCapture();
    await runStart(new ChatStreamClient('/v1/chat/stream'), 'read my file');
    // Not present at all: a browser has no machine, and an empty string would
    // be a device the gateway then has to decide is not a device.
    expect('device_id' in cap[0].body).toBe(false);
  });
});
