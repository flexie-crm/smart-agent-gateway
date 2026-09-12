// Giving the desktop application a way into this computer's network.
//
// The gateway runs on a server, and a customer's older systems often do not: an
// accounts database in an office, a server on an isolated network. This
// computer can see those, so the desktop application carries the connection for
// a tool configured to be reached that way.
//
// The page's part in that is small and is the only part that can be done here:
// it is the half that is signed in. Rust cannot sign in, so it is handed a
// token that opens one socket and nothing else, and asks for another when that
// one is nearly out. Everything else happens underneath.
//
// In a browser this does nothing at all. There is no shell to hand anything to,
// and a browser tab cannot open a TCP connection to anything.

import { apiFetch } from './api';

/** The shell's bridge, when the page is running inside the application. */
interface Shell {
  core: { invoke: (command: string, args?: Record<string, unknown>) => Promise<unknown> };
  event: {
    listen: (event: string, handler: (event: { payload: unknown }) => void) => Promise<() => void>;
  };
}

/** The event the Rust half emits when its credential is nearly out. */
const NEEDS_TOKEN = 'link://needs-token';

/** And the one it emits when the route comes up or goes down. */
const STATE_CHANGED = 'link://state';

function shell(): Shell | null {
  // Both editions, because both are desktop applications with a shell
  // underneath. The personal one was excluded once, on the grounds that its
  // gateway is on the same computer so a route through the application would be
  // a route from here to here. That is true of a CONNECTION and false of a
  // CALL, whose far end is this application itself: the terminal and the file
  // tools are calls, and leaving the link out took them away from the edition
  // that is always on the person's own machine.
  const found = (window as unknown as { __TAURI__?: Shell }).__TAURI__;
  return found?.core?.invoke ? found : null;
}

/** Whether this page is running inside the desktop application. */
export function insideTheApp(): boolean {
  return shell() !== null;
}

/**
 * Which installation this is, or an empty string in a browser.
 *
 * Sent with every message, so a tool that reaches this computer reaches THIS
 * one. A person may be signed in on a laptop and a desktop, and only the
 * request knows which of them somebody is typing on.
 *
 * Asked once and remembered: it does not change while the application is open,
 * and a message must not wait on a round trip to Rust to be sent.
 */
let device: string | null = null;

export async function machineDeviceId(): Promise<string> {
  if (device !== null) return device;
  const app = shell();
  if (!app) {
    device = '';
    return device;
  }
  try {
    const answer = await app.core.invoke('link_device');
    device = typeof answer === 'string' ? answer : '';
  } catch {
    device = '';
  }
  return device;
}

/**
 * What computer this is: the system, the shell, how paths are written, and
 * which of a short list of programs are installed.
 *
 * Sent with every message, and NOT remembered, which is the one difference
 * from the device id above. The device cannot change while the application is
 * open; this can, because somebody installs Node in the middle of a
 * conversation and the next thing they ask is to run it. The cost is a look
 * along PATH on the Rust side, which is file lookups and no processes.
 *
 * Empty in a browser, where there is no computer the assistant can reach and
 * so nothing worth telling it about one.
 */
export async function machineEnvironment(): Promise<unknown | null> {
  const app = shell();
  if (!app) return null;
  try {
    return await app.core.invoke('machine_environment');
  } catch {
    // An application older than this page has never heard of the command. It
    // still works, it just says nothing about itself, and the gateway leaves
    // the paragraph out rather than guessing.
    return null;
  }
}

/** What was learned already, without waiting. Empty until the first ask. */
export function knownDeviceId(): string {
  return device ?? '';
}

/** Forget it, for a test that changes what is under the page. */
export function resetDeviceForTest(): void {
  device = null;
}

/**
 * Mint a link token and hand it to the application.
 *
 * Called once after signing in, and again whenever the application says it
 * needs another. A failure is not worth interrupting anybody over: the link is
 * how a tool reaches a local network, and a tool that then cannot reach one
 * says so itself, in words about that tool.
 */
export async function startMachineLink(): Promise<void> {
  const app = shell();
  if (!app) return;
  try {
    const deviceId = await machineDeviceId();
    if (!deviceId) return; // nothing to link: no application under this page
    const answer = await apiFetch('/v1/link/token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ device_id: deviceId }),
    });
    if (!answer.ok) return;
    const { token } = (await answer.json()) as { token: string };
    await app.core.invoke('link_start', { token });
  } catch {
    // Nothing to say here. The application keeps asking, and a tool that needs
    // the link reports its own absence.
  }
}

/** Stop it, and forget the credential. Signing out has to reach Rust too. */
export async function stopMachineLink(): Promise<void> {
  const app = shell();
  if (!app) return;
  try {
    await app.core.invoke('link_stop');
  } catch {
    // Signing out must not fail because of this.
  }
}

/**
 * Listen for the application asking for a fresh credential, for as long as this
 * page is open. Returns the way to stop listening.
 */
export async function renewMachineLinkOnRequest(): Promise<() => void> {
  const app = shell();
  if (!app?.event?.listen) return () => {};
  try {
    return await app.event.listen(NEEDS_TOKEN, () => {
      void startMachineLink();
    });
  } catch {
    return () => {};
  }
}

/**
 * Whether the route into this computer's network is up right now.
 *
 * False everywhere there is no route to be up: a browser, and the personal
 * edition, where the gateway is already on this machine. Both are "no link",
 * and neither is a fault, so nothing is shown for either.
 */
export async function machineLinkIsUp(): Promise<boolean> {
  const app = shell();
  if (!app) return false;
  try {
    return (await app.core.invoke('link_up')) === true;
  } catch {
    return false;
  }
}

/**
 * Watch it. The handler is called with the state when it changes, and once at
 * the start with what it is now: a window that has just opened missed whatever
 * was announced before it existed.
 */
export async function watchMachineLink(onChange: (up: boolean) => void): Promise<() => void> {
  const app = shell();
  if (!app?.event?.listen) return () => {};
  void machineLinkIsUp().then(onChange);
  try {
    return await app.event.listen(STATE_CHANGED, (event) => onChange(event.payload === true));
  } catch {
    return () => {};
  }
}

/**
 * The folder the assistant may work in on this computer, or null when nobody
 * has chosen one.
 *
 * There is one, not a list. It is the shape people already know from every
 * editor and every terminal — you are working in a project — and it is the
 * whole of the scope: a command runs there, a path is relative to it, and
 * anything resolving outside it is refused on the computer itself.
 */
export async function chosenFolder(): Promise<string | null> {
  const app = shell();
  if (!app) return null;
  try {
    const answer = await app.core.invoke('chosen_folder');
    return typeof answer === 'string' && answer ? answer : null;
  } catch {
    return null;
  }
}

/**
 * Ask for one, with the system's own folder picker.
 *
 * Returns the folder chosen, or null if the person closed the dialog, which is
 * an answer rather than a failure.
 */
export async function chooseFolder(): Promise<string | null> {
  const app = shell();
  if (!app) return null;
  const answer = await app.core.invoke('choose_folder');
  return typeof answer === 'string' && answer ? answer : null;
}

/** Take it away. The assistant then reaches nothing on this disk. */
export async function forgetFolder(): Promise<void> {
  const app = shell();
  if (!app) return;
  await app.core.invoke('forget_folder');
}
