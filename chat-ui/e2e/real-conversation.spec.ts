import { expect, test } from '@playwright/test'

import { signInFresh } from './helpers'

// The same measurements, against a REAL transcript.
//
// Everything else here is driven over conversations built to resemble one, and
// resemblance is not the same claim. This one is a copy of an actual session,
// 284 steps of it: 108 messages, 207 blocks of reasoning, 400 tool calls across
// nine tools with results from ninety characters to five thousand. Its rows are
// whatever height they really are.
//
// It only runs when that conversation has been put into the scratch database
// (SAG_E2E_AFTER_SEED), so the gate stays self-contained; on any other machine
// it skips rather than failing for the wrong reason.

const REAL = 'Bug hunt fixed and committed'

const readScroll = (page: import('@playwright/test').Page) =>
  page.evaluate(() => {
    const v = document.querySelector('.fx-scroll') as HTMLElement | null
    if (!v) return null
    return {
      top: Math.round(v.scrollTop),
      height: Math.round(v.scrollHeight),
      client: Math.round(v.clientHeight),
      // Distance from the newest message: a reversed column counts upward from
      // the bottom as a negative scrollTop, so nought is being at it.
      gap: Math.round(v.scrollHeight - v.scrollTop - v.clientHeight),
    }
  })

test('a real conversation opens at its end, lands, and jumps back cleanly', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)

  const row = page.getByText(REAL).first()
  if ((await row.count()) === 0) {
    test.skip(true, 'the real conversation was not imported into this database')
  }

  await page.evaluate(() => {
    const seen: Array<{ top: number; h: number; mid: string }> = []
    ;(window as unknown as { __trace: typeof seen }).__trace = seen
    const tick = () => {
      const v = document.querySelector('.fx-scroll') as HTMLElement | null
      if (v) {
        const box = v.getBoundingClientRect()
        const at = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2)
        seen.push({ top: Math.round(v.scrollHeight - v.scrollTop - v.clientHeight), h: Math.round(v.scrollHeight), mid: (at?.textContent ?? '').slice(0, 60) })
      }
      requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  })

  await row.click()
  await page.waitForTimeout(5000)

  const trace = await page.evaluate(
    () => (window as unknown as { __trace: Array<{ top: number; h: number; mid: string }> }).__trace,
  )
  const rendered = trace.filter((f) => f.h > 1000 && f.mid !== '')
  expect(rendered.length).toBeGreaterThan(10)

  let changes = 0
  for (let i = 1; i < rendered.length; i++) if (rendered[i].mid !== rendered[i - 1].mid) changes++

  const settled = await readScroll(page)
  console.log(`REAL landing: ${rendered.length} frames, content changed ${changes}x, gap ${settled?.gap}px, height ${settled?.height}px`)

  // It is at the end, and the reader did not watch it travel there.
  expect(settled!.gap).toBeLessThan(8)
  expect(changes).toBeLessThanOrEqual(2)

  // The height stops moving.
  await page.waitForTimeout(2000)
  const later = await readScroll(page)
  expect(Math.abs(later!.height - settled!.height)).toBeLessThan(50)

  // Read a long way back, then jump: the button reaches the bottom.
  await page.mouse.move(550, 400)
  for (let i = 0; i < 30; i++) {
    await page.mouse.wheel(0, -700)
    await page.waitForTimeout(25)
  }
  const away = await readScroll(page)
  expect(away!.gap).toBeGreaterThan(500)

  const button = page.getByRole('button', { name: 'Scroll to the latest message' })
  await expect(button).toBeVisible()
  await button.click()
  await page.waitForTimeout(2000)
  const back = await readScroll(page)
  console.log(`REAL jump: gap ${away!.gap}px -> ${back!.gap}px`)
  expect(back!.gap).toBeLessThan(8)
})
