import { useEffect, useState } from 'react'
import { ArrowUp } from 'lucide-react'

/**
 * The one thing said about an update: it is already here, reopen when you like.
 *
 * The shell has by then downloaded and installed it, because replacing a bundle
 * on macOS leaves the process running from it untouched. So there is nothing to
 * ask permission for and nothing to wait on: the whole of the interaction is
 * telling somebody, once, that the next launch is a newer version.
 *
 * Which is why it is not a dialog. A dialog interrupts to demand a decision
 * about something already done, and the only decision left is when to quit an
 * application, which is not ours to make.
 *
 * It IS a chip rather than a line of quiet text, because a line in the muted
 * colour every other label uses is a line nobody reads. This is good news that
 * expires: a version sitting on the disk unused is the one thing here worth
 * noticing once.
 *
 * SOLID, not a tint, and that is a measurement rather than a preference. This
 * palette is deliberately monochrome: --primary is oklch(0.25 0.008 255), a
 * near-black with a chroma of 0.008, which is to say no colour at all. So
 * bg-primary/10 is ten percent of near-black on a near-white sidebar, a wash
 * too faint to read as anything, and the first version of this chip was exactly
 * that. There is no accent in this product to tint with, so the only thing here
 * that can carry a notification is the palette's own highest-contrast pair, used
 * whole. Which stays flat: one filled shape, no border, no second container.
 *
 * There is no "restart now". The gateway is holding a database and possibly a
 * conversation being written, and ending that to save somebody a double click is
 * a bad trade.
 */
/** The part of the shell's global this chip uses, or nothing in a browser. */
function shell(): Shell | undefined {
  return (window as unknown as { __TAURI__?: Shell }).__TAURI__
}

export function UpdateReady() {
  const [version, setVersion] = useState<string | null>(null)

  useEffect(() => {
    // Only inside the application. In a browser there is no shell to listen to,
    // and no bundle that could have been replaced, so a browser shows nothing
    // whatever its storage happens to hold.
    const tauri = shell()
    const listen = tauri?.event?.listen
    if (!listen) return

    let stop: (() => void) | undefined
    let gone = false

    // What an earlier page was told. The update stays pending until somebody
    // reopens, so the notice is kept in storage to survive a reload, and it is
    // only still true while the version saved is later than the one RUNNING.
    //
    // Storage cannot tell those apart by itself. Every launch takes the same
    // port (desktop/personal/shell/src/gateway.rs:534) and so the same origin,
    // which means a reopened application reads what the previous one saved, and
    // without this check it went on saying "reopen" to the version it already
    // was. getVersion answers with the version compiled into this process,
    // never the bundle on disk, which is exactly the question.
    //
    // Read again when the answer arrives rather than captured now, so a notice
    // that lands in between is judged instead of overwritten.
    if (remembered() !== null) {
      void tauri?.app?.getVersion?.().then((running) => {
        if (gone) return
        const saved = remembered()
        if (saved && isLater(saved, running)) setVersion(saved)
        else forget()
      })
    }

    void listen('sag://update-ready', (event) => {
      const v = event.payload?.version ?? ''
      remember(v)
      setVersion(v)
    }).then((unlisten) => {
      // The component may have gone before the subscription was ready, and
      // unsubscribing then is what stops a listener outliving what it updates.
      if (gone) unlisten()
      else stop = unlisten
    })
    return () => {
      gone = true
      stop?.()
    }
  }, [])

  if (version === null) return null
  return (
    <div className="px-3 pb-2">
      <div className="flex items-start gap-2.5 rounded-md bg-primary px-3 py-2.5 text-xs text-primary-foreground">
        <span className="mt-px flex size-4 shrink-0 items-center justify-center rounded-full bg-primary-foreground/20">
          <ArrowUp className="size-3" />
        </span>
        <span className="leading-relaxed">
          <span className="block">
            {version ? `Version ${version} is installed.` : 'A new version is installed.'}
          </span>
          <span className="block opacity-70">Reopen SAG to use it.</span>
        </span>
      </div>
    </div>
  )
}

type Listen = (
  event: string,
  handler: (event: { payload?: { version?: string } }) => void,
) => Promise<() => void>

type Shell = {
  event?: { listen?: Listen }
  app?: { getVersion?: () => Promise<string> }
}

/**
 * Whether one version is later than another, a number at a time, so 0.1.10 is
 * later than 0.1.9 (as text it sorts first). Both sides are plain dotted
 * numbers, taken from the same tauri.conf.json. A part that is not a number
 * reads as not later, so a notice nothing can compare is dropped rather than
 * kept for ever.
 */
function isLater(a: string, b: string): boolean {
  const x = a.split('.').map(Number)
  const y = b.split('.').map(Number)
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (x[i] ?? 0) - (y[i] ?? 0)
    if (d !== 0) return d > 0
  }
  return false
}

/** Where the pending version is kept, for as long as it is pending. */
const KEY = 'sag.update-ready'

// Storage throws outright in some contexts rather than returning nothing, so
// both directions are guarded: a chip is not worth a blank screen.
function remembered(): string | null {
  try {
    return window.localStorage.getItem(KEY)
  } catch {
    return null
  }
}

function remember(version: string) {
  try {
    window.localStorage.setItem(KEY, version)
  } catch {
    /* nothing to do: the chip simply will not survive a reload */
  }
}

function forget() {
  try {
    window.localStorage.removeItem(KEY)
  } catch {
    /* nothing to do: the next launch asks again */
  }
}
