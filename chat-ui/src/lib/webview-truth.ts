/**
 * Correcting one thing a Mac application's webview says about itself.
 *
 * `navigator.maxTouchPoints` is how a page asks whether it is being touched
 * rather than pointed at. In Safari on a Mac it is 0, which is the truth: a
 * trackpad is not a touchscreen. Inside the webview an application embeds, on
 * the same machine, it comes back greater than zero.
 *
 * That is not a curiosity, because the standard way to tell an iPhone from a Mac
 * is exactly this pair: iPads report themselves as "MacIntel" and are told apart
 * by having touch points. Our virtual list's library uses that test, and it is
 * right to: iOS needs its scroll writes deferred until a flick has finished, or
 * the momentum is killed mid-gesture.
 *
 * On a Mac the test is a false positive, and the deferral it turns on is the
 * whole of a bug people could see. Opening a conversation should place the view
 * at the end once; with writes deferred, each row that measures itself nudges it
 * instead, and the conversation visibly scrolls down through its own history.
 * The same bundle, byte for byte, lands instantly in a browser and travels in
 * the application. It cost a day, mostly spent looking for a version difference
 * that was never there.
 *
 * So this says the true thing, and only where it is true: inside our own desktop
 * shell, on a Mac. In a browser nothing is touched, which matters because a
 * phone reporting touch points is not lying. It runs before anything reads the
 * value, since the library asks once and remembers.
 */
export function tellTheTruthAboutTouch(): void {
  const inOurShell = '__TAURI__' in window || '__TAURI_INTERNALS__' in window
  if (!inOurShell) return
  // A Mac, by the same name iOS borrows. Anything else is left alone.
  if (!/Mac/i.test(navigator.platform)) return
  if (navigator.maxTouchPoints === 0) return
  try {
    Object.defineProperty(navigator, 'maxTouchPoints', {
      configurable: true,
      get: () => 0,
    })
  } catch {
    // A webview that will not allow it keeps its own answer; the list is then
    // no worse than it was.
  }
}
