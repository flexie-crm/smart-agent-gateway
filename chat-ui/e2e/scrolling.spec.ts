import { expect, test } from '@playwright/test'

import { signInFresh, openConversation } from './helpers'

// What scrolling a real conversation feels like.
//
// This is the first thing anybody touches, so a conversation that stutters under
// the wheel is the product stuttering, whatever else it can do. This one is the
// fixture whose answers are CODE, because a fenced block is a tokeniser run and
// that is the most expensive thing a conversation can contain. The other
// fixture, a working session of hundreds of tool calls, contains none at all,
// which is exactly why there are two of them.

const CONVERSATION = 'A conversation full of code'

test('scrolling a conversation with code in it does not stutter', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, CONVERSATION)
  await expect(page.locator('pre').first()).toBeVisible({ timeout: 15_000 })
  await page.waitForTimeout(1500)

  // Every frame the browser draws while the wheel is turning.
  await page.evaluate(() => {
    ;(window as unknown as { __frames: number[] }).__frames = []
    let last = performance.now()
    const tick = () => {
      const now = performance.now()
      ;(window as unknown as { __frames: number[] }).__frames.push(now - last)
      last = now
      requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  })

  // While the wheel turns, watch for a code block that is ON SCREEN but not yet
  // coloured in. That is the whole design in one observation: highlighting is
  // the most expensive thing a conversation contains, and it used to run in the
  // frame that was supposed to be moving the page, so a block appearing meant a
  // dropped frame. Deferred, a block that flies past is never tokenised at all.
  //
  // Asserted this way round because a frame counter is a measurement of the
  // machine as much as of the code: the same build gave 1 dropped frame on a
  // quiet run and 11 on a busy one. Whether a block is coloured while it is
  // moving is a fact about the code, and nothing else.
  await page.mouse.move(550, 400)
  let sawPlain = false
  for (let i = 0; i < 80; i++) {
    await page.mouse.wheel(0, -300)
    await page.waitForTimeout(16)
    if (!sawPlain) {
      sawPlain = await page.evaluate(() =>
        [...document.querySelectorAll('pre')].some(
          (pre) =>
            pre.getBoundingClientRect().height > 0 &&
            pre.querySelectorAll('span[class*="token"]').length === 0,
        ),
      )
    }
  }

  const drawn = await page.evaluate(() => {
    const f = [...(window as unknown as { __frames: number[] }).__frames].slice(5)
    f.sort((a, b) => a - b)
    return {
      frames: f.length,
      p95: Math.round(f[Math.floor(f.length * 0.95)]),
      worst: Math.round(f[f.length - 1]),
      dropped: f.filter((x) => x > 32).length,
    }
  })
  console.log(`scroll: ${drawn.frames} frames, p95 ${drawn.p95}ms, worst ${drawn.worst}ms, ${drawn.dropped} over 32ms`)

  // The wheel really did turn for a while.
  expect(drawn.frames).toBeGreaterThan(120)
  // Code went past uncoloured, which is what keeps the frame free.
  expect(sawPlain).toBe(true)
  // And a catastrophic hitch is still a failure: this is loose enough to be
  // about the code rather than the machine, and the old behaviour reached 93ms
  // on a quiet run.
  expect(drawn.worst).toBeLessThan(250)
})

// The other half of the same change: deferring the colour must not lose it.
test('code is coloured in once the view settles', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, CONVERSATION)
  await expect(page.locator('pre').first()).toBeVisible({ timeout: 15_000 })

  // Tokens, not just a <pre>: plain text renders in a <pre> too, so counting
  // those would pass on a block that was never highlighted at all.
  await expect
    .poll(
      () => page.evaluate(() => document.querySelectorAll('pre span[class*="token"]').length),
      { timeout: 10_000 },
    )
    .toBeGreaterThan(20)
})
