/**
 * Light, night, or whatever the computer is set to.
 *
 * Three states, not two. A switch with two positions cannot say "follow the
 * machine", and following the machine is what most people want: the screen goes
 * dark in the evening along with everything else they have open. So the stored
 * value is a CHOICE, and one of the choices is to have no opinion.
 *
 * What is stored is that choice and never the result of it. Storing "dark"
 * because it happened to be evening when somebody first opened the application
 * would freeze them there for good.
 */
export type ThemeChoice = 'system' | 'light' | 'dark'

const REMEMBERED = 'fx_theme'

/**
 * The one file both halves read and write.
 *
 * The page keeps the choice in browser storage as well, because reading it there
 * is instant and happens before anything is drawn. But browser storage is not
 * something the application can read, and in the personal edition it is not even
 * something the PAGE can rely on: that gateway binds to a free port, so the
 * address changes on every launch, and a different address has its own empty
 * store.
 *
 * So the settings file is what survives, and the application reads it before it
 * makes a window, which is how the very first frame is the right colour.
 */
const SETTINGS = 'settings.json'

/** What the computer is set to right now, if it will say. */
export function whatTheMachineWants(): 'light' | 'dark' {
  if (typeof window === 'undefined' || !window.matchMedia) return 'light'
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

/** The choice this person made last time, or to follow the machine. */
export function rememberedChoice(): ThemeChoice {
  // What the desktop application injected before this document was parsed, if
  // it did. It outranks storage because storage can be empty through no fault
  // of the person: the personal edition's gateway binds to a free port, so the
  // address changes on every launch and a different origin has its own empty
  // store. Reading storage first is what made the chat render light and then
  // turn dark in front of somebody who had chosen night.
  const told = (window as unknown as { __SAG_APPEARANCE__?: string }).__SAG_APPEARANCE__
  try {
    const mine = localStorage.getItem(REMEMBERED)
    // What this page last remembered wins: it is written the moment the choice
    // is made, while what the application told us was injected when the WINDOW
    // was built and never changes after. The other order meant somebody who
    // switched to night and then opened the console got the value from before
    // they switched, on every navigation, for the rest of the session.
    if (mine === 'light' || mine === 'dark' || mine === 'system') return mine
  } catch {
    // A webview with storage turned off still gets a working application.
  }
  if (told === 'light' || told === 'dark' || told === 'system') return told
  return 'system'
}

/** What a choice actually means on this machine at this moment. */
export function settledOn(choice: ThemeChoice): 'light' | 'dark' {
  return choice === 'system' ? whatTheMachineWants() : choice
}

/**
 * Paints the page.
 *
 * One class on the root element, which is what every colour in the stylesheet is
 * written against, plus `color-scheme` so that the things the page does NOT draw
 * follow too: scrollbars, form controls, the flash of background before the
 * first paint.
 */
export function paint(choice: ThemeChoice): void {
  const dark = settledOn(choice) === 'dark'
  const root = document.documentElement
  root.classList.toggle('dark', dark)
  root.style.colorScheme = dark ? 'dark' : 'light'
}

/**
 * Keeps the choice for next time, in both places that need it.
 *
 * Browser storage is the fast one: it is read before anything is drawn, without
 * waiting on anybody. The settings file is the durable one: the application
 * reads it before it makes a window, and it survives the gateway moving to a new
 * port. Neither is enough on its own, and writing only one of them is how two
 * records of the same thing drift apart.
 */
export function remember(choice: ThemeChoice): void {
  try {
    localStorage.setItem(REMEMBERED, choice)
  } catch {
    // Not being able to remember here is not a reason to refuse to change.
  }
  void writeToSettings(choice)
  tellTheApplication(choice)
}

/** Writes to the settings file the application reads at startup. */
async function writeToSettings(choice: ThemeChoice): Promise<void> {
  try {
    const { load } = await import('@tauri-apps/plugin-store')
    const store = await load(SETTINGS, { autoSave: false })
    await store.set(REMEMBERED, choice)
    await store.save()
  } catch {
    // A browser has no settings file, and an application that refuses still
    // works: the cost is one frame of the wrong colour on the next start.
  }
}

/**
 * Tells the desktop application, which has its own first screen to paint.
 *
 * That screen belongs to the application and this page belongs to a server:
 * different origins, different storage, so it cannot read what is stored above.
 * It has to be handed over, and this is the only moment anybody knows.
 *
 * Best effort by design. In a browser there is nothing to tell, and an
 * application that refuses the call still works: the worst it costs is one flash
 * of the wrong colour on the next start.
 */
function tellTheApplication(choice: ThemeChoice): void {
  const shell = (window as unknown as {
    __TAURI__?: { core?: { invoke?: (cmd: string, args: unknown) => Promise<unknown> } }
  }).__TAURI__
  try {
    void shell?.core?.invoke?.('remember_appearance', { theme: choice })?.catch?.(() => undefined)
  } catch {
    // Not a reason to fail to change the colours.
  }
}

/**
 * Follows the machine while the choice is to follow it.
 *
 * The listener stays attached whatever the choice, because somebody can switch
 * back to following at any moment and the machine may have changed in the
 * meantime. It reads the choice when it fires rather than when it is attached.
 */
export function followTheMachine(choiceNow: () => ThemeChoice): () => void {
  if (typeof window === 'undefined' || !window.matchMedia) return () => undefined
  const media = window.matchMedia('(prefers-color-scheme: dark)')
  const changed = () => {
    if (choiceNow() === 'system') paint('system')
  }
  media.addEventListener('change', changed)
  return () => media.removeEventListener('change', changed)
}

/**
 * Asks the desktop application what was chosen, and paints that.
 *
 * The page cannot rely on its own storage inside the personal edition, and the
 * reason is worth stating plainly: that gateway binds to a FREE PORT, so the
 * address it serves the chat from changes on every launch. A different port is a
 * different origin, a different origin is a different storage, and everything
 * this page remembered last time is gone. Somebody chose night, quit, opened it
 * again, and it was light.
 *
 * So the application remembers, because it is the half that survives the port,
 * and this asks it. In a browser there is nothing to ask and nothing is changed.
 * Asynchronous by nature, so the page paints from what it has first (the inline
 * script in the document head) and corrects here, within a frame or two, against
 * a window whose background is already the right colour.
 */
export function askTheApplication(): void {
  const shell = (window as unknown as {
    __TAURI__?: { core?: { invoke?: (cmd: string) => Promise<unknown> } }
  }).__TAURI__
  if (!shell?.core?.invoke) return
  void shell.core
    .invoke('chosen_appearance')
    .then((answer) => {
      const choice = String(answer)
      if (choice !== 'system' && choice !== 'light' && choice !== 'dark') return
      // Kept here too, so this page is right on its own until the port changes
      // again.
      try {
        localStorage.setItem(REMEMBERED, choice)
      } catch {
        // The application is the record; this is only a shortcut.
      }
      paint(choice)
    })
    .catch(() => undefined)
}
