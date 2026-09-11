import { expect, test } from '@playwright/test'

import { signInFresh, openConversation } from './helpers'

// Opening a conversation LANDS at the end. It does not travel there.
//
// The difference is the whole complaint. A scroll position that is corrected
// once per measurement, a hundred times over as a hundred rows discover how tall
// they are, is not a landing: it is the page visibly falling down its own
// history while you watch. This measures the journey, not the destination,
// because the destination was already right and it still looked wrong.

const CONVERSATION = 'A conversation worth scrolling'

// How far the reader is from the newest message.
//
// The conversation is a reversed column, so the browser counts upward from the
// newest message as what is left to scroll, so being AT it is zero. That
// is not an implementation detail leaking into the test: it is the whole point,
// because zero is the position the browser is in before anything runs.
const readScroll = (page: import('@playwright/test').Page) =>
  page.evaluate(() => {
    const v = document.querySelector('.fx-scroll') as HTMLElement | null
    if (!v) return null
    return {
      top: Math.round(v.scrollTop),
      height: Math.round(v.scrollHeight),
      client: Math.round(v.clientHeight),
      gap: Math.round(v.scrollHeight - v.scrollTop - v.clientHeight),
    }
  })

test('opening a conversation lands at the end without travelling there', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)

  // Watch every frame from the click onwards, in the page, so nothing is missed
  // between two polls. What is recorded is what the READER sees: the words
  // sitting in the middle of the screen. The scroll offset is not that. With the
  // end anchored, measuring a row above the fold moves the offset and the
  // content deliberately stays where it is, so counting pixels of scrollTop
  // would call a perfectly still page a journey.
  await page.evaluate(() => {
    const seen: Array<{ t: number; top: number; gap: number; h: number; mid: string }> = []
    ;(window as unknown as { __scrollTrace: typeof seen }).__scrollTrace = seen
    const tick = () => {
      const v = document.querySelector('.fx-scroll') as HTMLElement | null
      if (v) {
        const box = v.getBoundingClientRect()
        const at = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2)
        seen.push({
          t: Math.round(performance.now()),
          top: Math.round(v.scrollTop),
          gap: Math.round(v.scrollHeight - v.scrollTop - v.clientHeight),
          h: Math.round(v.scrollHeight),
          mid: (at?.textContent ?? '').slice(0, 60),
        })
      }
      requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  })

  await openConversation(page, CONVERSATION)
  await page.waitForTimeout(4000)

  const trace = await page.evaluate(
    () =>
      (window as unknown as {
        __scrollTrace: Array<{ t: number; top: number; gap: number; h: number; mid: string }>
      }).__scrollTrace,
  )
  const withRows = trace.filter((f) => f.h > 1000 && f.mid !== '')
  expect(withRows.length).toBeGreaterThan(10) // the conversation really did open

  // How many times the words under the middle of the screen CHANGED. Landing is
  // one change: from nothing to the end of the conversation. Every change after
  // that is a frame in which the page moved under the reader, which is what
  // "it scrolls down instead of just being there" means.
  let changes = 0
  for (let i = 1; i < withRows.length; i++) {
    if (withRows[i].mid !== withRows[i - 1].mid) changes++
  }
  let offsetMoved = 0
  for (let i = 1; i < withRows.length; i++) {
    offsetMoved += Math.abs(withRows[i].top - withRows[i - 1].top)
  }
  const settled = await readScroll(page)
  console.log(
    `landing: ${withRows.length} frames, content changed ${changes}x, ` +
      `offset moved ${offsetMoved}px, final gap ${settled?.gap}px, height ${settled?.height}px`,
  )

  // It ends at the end.
  expect(settled!.gap).toBeLessThan(8)
  // And it never TRAVELLED, which is asked of the words rather than of the
  // pixels. Landing is one change, from nothing to the end of the conversation;
  // every change after that is a frame in which the page moved under the reader.
  //
  // Not asked as "the first frame's gap was nought". That was the same question
  // while the list counted from the newest message, because the origin WAS the
  // end and the browser rested there before anything ran. In an ordinary list
  // the end is scrollTop at its maximum, and the maximum is still settling while
  // rows are measured, so the gap reads as hundreds of pixels on a page whose
  // words never moved: the mechanism, not the property.
  expect(changes).toBeLessThanOrEqual(1)
  // And the reader did not watch it get there. Two allows for the frame the
  // rows appear and one settle after it.
  expect(changes).toBeLessThanOrEqual(2)
})

test('the scrollbar does not resize itself after the conversation opens', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, CONVERSATION)
  await page.waitForTimeout(2500)

  const first = await readScroll(page)
  await page.waitForTimeout(2500)
  const later = await readScroll(page)

  // A total height that keeps changing after everything has settled is the
  // scrollbar growing and shrinking under the hand holding it.
  const drift = Math.abs(later!.height - first!.height)
  console.log(`height ${first!.height} then ${later!.height} (drift ${drift}px)`)
  expect(drift).toBeLessThan(50)
})

test('the jump button goes to the very bottom', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, CONVERSATION)
  await page.waitForTimeout(2500)

  // Get well away from the end.
  await page.mouse.move(550, 400)
  for (let i = 0; i < 25; i++) {
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
  console.log(`jump: gap ${away!.gap}px -> ${back!.gap}px`)
  // At the bottom. Not near it.
  expect(back!.gap).toBeLessThan(8)
})

test('the jump to the bottom arrives at once, not as a slideshow', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, CONVERSATION)
  await page.waitForTimeout(2500)

  // A long way up, which is where the button is actually used.
  await page.mouse.move(550, 400)
  for (let i = 0; i < 30; i++) {
    await page.mouse.wheel(0, -700)
    await page.waitForTimeout(25)
  }
  await page.waitForTimeout(500)

  await page.evaluate(() => {
    ;(window as unknown as { __f: number[] }).__f = []
    let last = performance.now()
    const tick = () => {
      const now = performance.now()
      ;(window as unknown as { __f: number[] }).__f.push(now - last)
      last = now
      requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  })

  const started = Date.now()
  await page.getByRole('button', { name: 'Scroll to the latest message' }).click()
  await page.waitForFunction(
    () => {
      const v = document.querySelector('.fx-scroll') as HTMLElement | null
      return !!v && v.scrollHeight - v.scrollTop - v.clientHeight < 8
    },
    undefined,
    { timeout: 10_000 },
  )
  const took = Date.now() - started

  const frames = await page.evaluate(() => {
    const f = [...(window as unknown as { __f: number[] }).__f].slice(2)
    f.sort((a, b) => a - b)
    return {
      n: f.length,
      p95: Math.round(f[Math.floor(f.length * 0.95)] ?? 0),
      worst: Math.round(f[f.length - 1] ?? 0),
      dropped: f.filter((x) => x > 32).length,
    }
  })
  console.log(`jump feel: arrived in ${took}ms, ${frames.n} frames, p95 ${frames.p95}ms, worst ${frames.worst}ms, ${frames.dropped} over 32ms`)

  // Arriving is not enough: it has to arrive without grinding through fifteen
  // screenfuls of rows, mounting and measuring every one of them on the way. It
  // used to take 1541ms with five dropped frames, which is what being animated
  // across the whole distance costs in a virtual list.
  expect(took).toBeLessThan(500)
  expect(frames.dropped).toBeLessThan(4)
})

// Opening the APPLICATION, rather than clicking a conversation in it.
//
// This is the path the desktop app actually takes and the one every test here
// was missing. It does not click anything: it starts with a conversation already
// chosen, restores it from storage, and draws it. The id is known before the
// list is, which is the reverse of clicking, where the list exists and then the
// id changes. Reported from use, on a desktop application that was demonstrably
// running the right code, while every test passed.
test('reopening the app on a conversation lands at its end', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, CONVERSATION)
  await expect(page.getByText(/answer \d+/).first()).toBeVisible({ timeout: 15_000 })
  await page.waitForTimeout(1500)

  // Now do what opening the application does: load the page again, with that
  // conversation already the current one.
  await page.reload()

  await page.evaluate(() => {
    const seen: Array<{ top: number; h: number; mid: string }> = []
    ;(window as unknown as { __t: typeof seen }).__t = seen
    const tick = () => {
      const v = document.querySelector('.fx-scroll') as HTMLElement | null
      if (v) {
        const box = v.getBoundingClientRect()
        const at = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2)
        seen.push({ top: Math.round(v.scrollTop), h: Math.round(v.scrollHeight), mid: (at?.textContent ?? '').slice(0, 60) })
      }
      requestAnimationFrame(tick)
    }
    requestAnimationFrame(tick)
  })
  await page.waitForTimeout(5000)

  const trace = await page.evaluate(
    () => (window as unknown as { __t: Array<{ top: number; h: number; mid: string }> }).__t,
  )
  const rendered = trace.filter((f) => f.h > 1000 && f.mid !== '')
  expect(rendered.length).toBeGreaterThan(10)
  let changes = 0
  for (let i = 1; i < rendered.length; i++) if (rendered[i].mid !== rendered[i - 1].mid) changes++

  const settled = await readScroll(page)
  console.log(`reopen: ${rendered.length} frames, content changed ${changes}x, gap ${settled?.gap}px, height ${settled?.height}px`)

  expect(settled!.gap).toBeLessThan(8)
  expect(changes).toBeLessThanOrEqual(2)
})
