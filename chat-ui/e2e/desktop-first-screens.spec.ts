import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test } from '@playwright/test'

// The two screens a desktop application opens on, driven as FILES.
//
// It lives here because this is where the browser gate runs, and it needs
// nothing from the server it starts: no application, no build, just the two HTML
// files the shells embed.
//
// They are the only part of a desktop application that can be tested without
// building one, and every fault they have had was invisible from the outside: a
// window that navigated before the page painted, a rule that hid the one word
// that said what was happening, and a missing semicolon that made the whole
// script throw before any of it ran. None of those show up in a compiled binary,
// where the front end is embedded compressed and grepping it proves nothing.

// Found from this file, not typed: an absolute path here is one machine's
// checkout, and it breaks for everybody else and names its owner.
const SHELLS = path.resolve(fileURLToPath(new URL('../../desktop', import.meta.url)))

/** Stands in for the application, and records what the screen asks it to do. */
async function withAnApplication(page: import('@playwright/test').Page) {
  await page.addInitScript(() => {
    ;(window as unknown as { __TAURI__: unknown }).__TAURI__ = {
      core: {
        invoke: (cmd: string) => {
          const w = window as unknown as { __asked?: string[] }
          w.__asked = [...(w.__asked ?? []), cmd]
          return Promise.resolve('dark')
        },
      },
      // The screen listens for a server it cannot reach. Without this the page
      // throws on its first line and does nothing at all, which looks exactly
      // like a screen that was never written.
      event: {
        listen: () => Promise.resolve(() => undefined),
      },
    }
  })
}

const asked = (page: import('@playwright/test').Page) =>
  page.evaluate(() => (window as unknown as { __asked?: string[] }).__asked ?? [])

test('the enterprise screen shows itself, says what it is doing, then leaves', async ({ page }) => {
  const broken: string[] = []
  page.on('pageerror', (e) => broken.push(e.message))
  await withAnApplication(page)
  await page.goto(`file://${SHELLS}/enterprise/shell/connect/index.html?appearance=dark&going=1`)

  // Nothing may throw. A script that throws on its first line does nothing at
  // all and looks exactly like a screen that was never written.
  await page.waitForTimeout(300)
  expect(broken).toEqual([])

  // It says what is happening rather than offering the form.
  await expect(page.getByText(/Loading/)).toBeVisible()
  await expect(page.locator('#form')).toBeHidden()
  expect(await page.evaluate(() => document.documentElement.hasAttribute('data-night'))).toBe(true)

  // A real bar, filling. It measures the wait, which is a known length, so it
  // can honestly go from nothing to full rather than sliding about saying only
  // "something is happening".
  const width = () =>
    page.evaluate(() => {
      const bar = document.getElementById('bar') as HTMLElement
      const track = bar.parentElement as HTMLElement
      return Math.round((bar.getBoundingClientRect().width / track.getBoundingClientRect().width) * 100)
    })
  const early = await width()
  const shown = await page.textContent('#percent')
  await page.waitForTimeout(700)
  const later = await width()
  // Generous, because this is a clock. The bar fills over a known wait, so
  // where an early sample lands depends on how fast the machine got here: 43%
  // on a busy one against a threshold of 40 says nothing about the bar. What
  // the test is for is the SHAPE, which the next line asserts: it started well
  // short of full, and it moved.
  expect(early).toBeLessThan(70)
  expect(later).toBeGreaterThan(early + 15) // it is actually moving
  expect(shown).toMatch(/^\d+%$/)

  // And it is still there a second in. Leaving as soon as it was drawn showed it
  // for half a second, which reads as a glitch rather than as a start.
  expect(await asked(page)).not.toContain('waiting_finished')
  await expect(page.getByText(/Loading/)).toBeVisible()

  // Full, and then it leaves.
  // Nothing has said the gateway is ready yet, so the bar must NOT be full: it
  // approaches the end without arriving, because a bar that sits at 100% while
  // the wait continues is a bar that lied.
  expect(await width()).toBeLessThan(100)
  expect(await asked(page)).not.toContain('waiting_finished')

  // The application says it is ready, which is what the shell does when the
  // gateway answers. Only then is it full, and only then does the screen leave.
  await page.evaluate(() => (window as unknown as { __sagDone: () => void }).__sagDone())
  await expect(page.locator('#percent')).toHaveText('100%', { timeout: 5_000 })
  expect(await width()).toBeGreaterThan(95)
  await expect
    .poll(async () => await asked(page), { timeout: 5_000 })
    .toContain('waiting_finished')
})

test('the enterprise screen offers the form when there is no server', async ({ page }) => {
  await withAnApplication(page)
  await page.goto(`file://${SHELLS}/enterprise/shell/connect/index.html?appearance=light`)
  await page.waitForTimeout(300)
  await expect(page.locator('#form')).toBeVisible()
  expect(await asked(page)).not.toContain('waiting_finished')
  expect(await page.evaluate(() => document.documentElement.hasAttribute('data-night'))).toBe(false)
})

test('the personal screen draws in the chosen appearance', async ({ page }) => {
  const broken: string[] = []
  page.on('pageerror', (e) => broken.push(e.message))
  await withAnApplication(page)
  await page.goto(`file://${SHELLS}/personal/shell/splash/index.html?appearance=dark`)
  await page.waitForTimeout(300)

  expect(broken).toEqual([])
  // The chat's own dark. See the colour test below for why it is this and not
  // some other dark.
  expect(await page.evaluate(() => getComputedStyle(document.body).backgroundColor)).toBe(
    'rgb(28, 30, 34)',
  )
})

// The progress reporter writes into these by id, from Rust (shared/src/lib.rs).
// Renaming one here would break the waiting screen silently.
test('the personal screen keeps the handles the application writes to', async ({ page }) => {
  await withAnApplication(page)
  await page.goto(`file://${SHELLS}/personal/shell/splash/index.html`)
  // The handles the application actually writes to. `track` was one of these
  // and is not any more: it was removed with the dead rule that used it, and a
  // test asking for a handle nothing writes to is asking for the past.
  for (const id of ['status', 'bar', 'percent']) {
    expect(await page.locator(`#${id}`).count()).toBe(1)
  }
})

// The screen an application opens ON and the screen it opens INTO must be the
// same colour, or starting it is two shades of dark with a step between them.
//
// They cannot share a stylesheet: one is a page the application ships, the other
// is served by a gateway. So the values are written in both places, and this is
// what stops them drifting apart.
test('both first screens are the chat\'s own colours', async ({ page }) => {
  const CHAT_NIGHT = 'rgb(28, 30, 34)' // --background in chat-ui/src/index.css
  const CHAT_DAY = 'rgb(250, 251, 252)'

  for (const url of [
    `${SHELLS}/enterprise/shell/connect/index.html`,
    `${SHELLS}/personal/shell/splash/index.html`,
  ]) {
    await withAnApplication(page)
    await page.goto(`file://${url}?appearance=dark`)
    await page.waitForTimeout(200)
    expect(await page.evaluate(() => getComputedStyle(document.body).backgroundColor)).toBe(
      CHAT_NIGHT,
    )

    await page.goto(`file://${url}?appearance=light`)
    await page.waitForTimeout(200)
    expect(await page.evaluate(() => getComputedStyle(document.body).backgroundColor)).toBe(
      CHAT_DAY,
    )
  }
})
