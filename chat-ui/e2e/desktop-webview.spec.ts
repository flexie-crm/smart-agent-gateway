import { expect, test } from '@playwright/test'

import { signInFresh, openConversation } from './helpers'

// The environment a desktop application's webview reports, which is not the one
// a browser reports, and the difference was a whole day.
//
// `navigator.maxTouchPoints` is 0 in Safari on a Mac and greater than zero
// inside the webview an application embeds on the same machine. The standard way
// to tell an iPhone from a Mac is that exact pair (iPads call themselves
// "MacIntel" and are told apart by touch points), so our virtual list's library
// concluded it was on iOS and turned on the behaviour iOS needs: scroll writes
// deferred until a flick has finished, or momentum dies mid-gesture.
//
// On a Mac that is a false positive, and the deferral is visible. Opening a
// conversation should place the view at its end once; with writes deferred,
// every row that measures itself nudges it instead, and the conversation scrolls
// down through its own history while you watch. The same bundle, byte for byte,
// landed instantly in a browser and travelled in the application, which is why
// we spent a day hunting a version difference that did not exist.

const CONVERSATION = 'A conversation worth scrolling'

/** Pretends to be a webview embedded in a Mac application. */
async function likeTheApplication(page: import('@playwright/test').Page, inShell: boolean) {
  await page.addInitScript(
    ({ shell }) => {
      Object.defineProperty(navigator, 'maxTouchPoints', { configurable: true, get: () => 5 })
      Object.defineProperty(navigator, 'platform', { configurable: true, get: () => 'MacIntel' })
      if (shell) (window as unknown as { __TAURI__: unknown }).__TAURI__ = {}
    },
    { shell: inShell },
  )
}

async function landing(page: import('@playwright/test').Page) {
  await page.evaluate(() => {
    const seen: Array<{ h: number; mid: string }> = []
    ;(window as unknown as { __t: typeof seen }).__t = seen
    const tick = () => {
      const v = document.querySelector('.fx-scroll') as HTMLElement | null
      if (v) {
        const b = v.getBoundingClientRect()
        const at = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2)
        seen.push({ h: Math.round(v.scrollHeight), mid: (at?.textContent ?? '').slice(0, 50) })
      }
      requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  })
  await openConversation(page, CONVERSATION)
  await page.waitForTimeout(5000)
  const trace = await page.evaluate(
    () => (window as unknown as { __t: Array<{ h: number; mid: string }> }).__t,
  )
  const rows = trace.filter((f) => f.h > 1000 && f.mid !== '')
  let changes = 0
  for (let i = 1; i < rows.length; i++) if (rows[i].mid !== rows[i - 1].mid) changes++
  // Distance from the newest message. A reversed column counts upward from the
  // bottom as a negative scrollTop, so nought is being at it.
  const gap = await page.evaluate(() => {
    const v = document.querySelector('.fx-scroll') as HTMLElement
    return Math.round(v.scrollHeight - v.scrollTop - v.clientHeight)
  })
  return { frames: rows.length, changes, gap }
}

test('inside the application, a conversation lands rather than travels', async ({ page }) => {
  // 1180x820 is the window the enterprise shell opens.
  await page.setViewportSize({ width: 1180, height: 820 })
  await likeTheApplication(page, true)
  await signInFresh(page)

  // The correction itself: inside our shell, on a Mac, the touch count is put
  // back to the truth. It is no longer what makes the conversation land (the
  // layout does that, and this test passes without it), but it is still right:
  // a library that concludes it is on iOS defers its scroll writes to protect
  // momentum, and none of that belongs on a desktop.
  expect(await page.evaluate(() => navigator.maxTouchPoints)).toBe(0)

  const result = await landing(page)
  console.log(`in-app landing: ${result.frames} frames, content changed ${result.changes}x, gap ${result.gap}px`)
  expect(result.frames).toBeGreaterThan(10)
  expect(result.gap).toBeLessThan(8)
  expect(result.changes).toBeLessThanOrEqual(2)
})

test('a touch device in a browser keeps its own answer', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await likeTheApplication(page, false) // touch reported, but no shell around it
  await signInFresh(page)

  // Not our shell, so nothing is corrected: a phone reporting touch points is
  // telling the truth, and the deferral it turns on is the behaviour it needs.
  expect(await page.evaluate(() => navigator.maxTouchPoints)).toBe(5)
})
