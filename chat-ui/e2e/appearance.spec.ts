import { expect, test } from '@playwright/test'

import { signInFresh, openConversation } from './helpers'

// Light, night, or whatever the computer is set to.
//
// Driven through the real control, because what matters is not that a class can
// be toggled but that a person can find the switch, that their choice survives a
// reload, and that the page is ALREADY in the right state on the first paint
// rather than flashing white at somebody working at night.

const painted = (page: import('@playwright/test').Page) =>
  page.evaluate(() => ({
    dark: document.documentElement.classList.contains('dark'),
    scheme: document.documentElement.style.colorScheme,
    page: getComputedStyle(document.body).backgroundColor,
  }))

test('the appearance can be chosen, and is remembered', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await page.emulateMedia({ colorScheme: 'light' })
  await signInFresh(page)

  // Following the computer, which is set to light.
  const day = await painted(page)
  expect(day.dark).toBe(false)

  await page.getByRole('radio', { name: 'Night' }).click()
  const night = await painted(page)
  expect(night.dark).toBe(true)
  // The rest of the browser is told too, so scrollbars and form controls follow.
  expect(night.scheme).toBe('dark')
  expect(night.page).not.toBe(day.page)

  // It survives a reload, and is right on the FIRST paint: the class is applied
  // before React renders, or somebody working at night gets a white flash in the
  // face every time they open the application.
  await page.reload()
  await expect
    .poll(() => page.evaluate(() => document.documentElement.classList.contains('dark')))
    .toBe(true)

  // And back again.
  await page.getByRole('radio', { name: 'Light' }).click()
  expect((await painted(page)).dark).toBe(false)
})

test('following the computer means following it', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await page.emulateMedia({ colorScheme: 'dark' })
  await signInFresh(page)

  // Never touched: the default is to follow, and the computer says night.
  expect((await painted(page)).dark).toBe(true)

  // Morning.
  await page.emulateMedia({ colorScheme: 'light' })
  await expect
    .poll(() => page.evaluate(() => document.documentElement.classList.contains('dark')))
    .toBe(false)

  // Somebody who has chosen is NOT followed around by the computer.
  await page.getByRole('radio', { name: 'Night' }).click()
  await page.emulateMedia({ colorScheme: 'light' })
  await page.waitForTimeout(300)
  expect((await painted(page)).dark).toBe(true)
})

test('the conversation is legible at night', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await signInFresh(page)
  await page.getByRole('radio', { name: 'Night' }).click()
  await openConversation(page, 'A conversation worth scrolling')
  await expect(page.getByText(/answer \d+/).first()).toBeVisible({ timeout: 15_000 })

  // What the person said used to be a hardcoded pale slate: a white card every
  // few lines, which at night is a lamp pointed at the reader.
  const said = await page.evaluate(() => {
    const row = document.querySelector('.is-user [class*="bg-said"], .is-user') as HTMLElement
    const body = getComputedStyle(document.body).backgroundColor
    const bubble = getComputedStyle(row).backgroundColor
    const lum = (c: string) => {
      const [r, g, b] = c.match(/[\d.]+/g)!.map(Number)
      return 0.2126 * r + 0.7152 * g + 0.0722 * b
    }
    return { page: lum(body), bubble: lum(bubble) }
  })
  // Near the page, not a white slab on it.
  expect(said.bubble).toBeLessThan(said.page + 60)
})

// The theme has to be on BEFORE the page is drawn, not once the application has
// started.
//
// A module script runs after the browser has already painted, so somebody
// working at night got a white flash and a layout settling under it every time
// they moved between the chat and the console. The only place that can be fixed
// is an inline script in the head, ahead of the stylesheet and the bundle, and
// the only way to prove it is to look at the document before the application
// exists.
test('night is already on before the page is drawn', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await signInFresh(page)
  await page.evaluate(() => localStorage.setItem('fx_theme', 'dark'))

  const early: string[] = []
  page.on('domcontentloaded', async () => {
    try {
      early.push(
        await page.evaluate(() => {
          const html = document.documentElement
          return `${html.classList.contains('dark') ? 'dark' : 'LIGHT'}/${
            html.style.colorScheme || 'none'
          }/${(window as unknown as { React?: unknown }).React ? 'react' : 'no-react'}`
        }),
      )
    } catch {
      // navigating; the next frame will do
    }
  })
  await page.reload()
  await page.waitForTimeout(1200)

  // Dark, and the browser told too (which is what paints the canvas behind the
  // page before any stylesheet has loaded), at the moment the document is
  // parsed.
  expect(early.length).toBeGreaterThan(0)
  expect(early[0]).toContain('dark/dark')
  expect(early[0]).not.toContain('LIGHT')
})

// The whole chain, end to end, in the shape the personal edition actually has.
//
// That gateway binds to a free port, so the address it serves this page from
// changes on every launch: a different origin, its own empty storage, nothing
// remembered. The application is the half that survives it, and it hands the
// answer down before the document is parsed. This is that exact situation:
// storage empty, computer set to light, application says night.
test('with nothing remembered, the application decides before anything renders', async ({
  page,
}) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await page.emulateMedia({ colorScheme: 'light' })

  // What the shell injects, at the moment it injects it: before the document is
  // parsed and before any script in it runs.
  await page.addInitScript(() => {
    ;(window as unknown as { __SAG_APPEARANCE__: string }).__SAG_APPEARANCE__ = 'dark'
    try {
      localStorage.removeItem('fx_theme')
    } catch {
      // a store that refuses is the case being tested
    }
  })

  // The very first frames, before React exists.
  const early: string[] = []
  page.on('domcontentloaded', async () => {
    try {
      early.push(
        await page.evaluate(
          () =>
            `${document.documentElement.classList.contains('dark') ? 'dark' : 'LIGHT'}/${
              document.documentElement.style.colorScheme || 'none'
            }`,
        ),
      )
    } catch {
      // navigating
    }
  })

  await signInFresh(page)
  await page.waitForTimeout(800)

  // Dark from the parse, with nothing in storage and a computer set to light.
  // Without this the page renders light and turns dark once it hears back, which
  // is a flash with a correction after it.
  expect(early.length).toBeGreaterThan(0)
  expect(early[0]).toBe('dark/dark')
  expect(await page.evaluate(() => document.documentElement.classList.contains('dark'))).toBe(true)
})
