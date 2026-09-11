import { expect, test } from '@playwright/test'

import { signInFresh, send } from './helpers'

// Staying with an answer while it arrives.
//
// A virtual list follows rows being ADDED. An answer is not added, it grows: one
// row gets taller, first with the model thinking and then with it speaking, and
// nothing about that is a new row. So the words run off the bottom of the screen
// while the view sits where it was, and the person watches an answer they cannot
// read.
//
// Both halves are here on purpose. While the model is THINKING there is no
// content at all and everything is happening in the parts of the row, which is
// the half a naive "has the text got longer" check sits still through, and it is
// the longer half.

/** The element the conversation actually scrolls in. */
async function scroller(page: import('@playwright/test').Page) {
  return page.evaluateHandle(() =>
    [...document.querySelectorAll('*')].find(
      (e) => e.scrollHeight > e.clientHeight + 50 && e.clientHeight > 200,
    ),
  )
}

// How far the reader is from the newest message.
//
// The conversation is a reversed column: the scroll origin IS the bottom, so
// being at the newest message is nought left to scroll, and scrolling up
// counts upward. This was still the old top-down sum, which in a reversed column
// computes the entire scrollable extent and has nothing to do with where anybody
// is. It therefore grew with the answer and passed only while the answer was
// short enough, which is a test agreeing with the code by accident.
const fromBottom = (page: import('@playwright/test').Page) =>
  page.evaluate(() => {
    const view = document.querySelector('.fx-scroll') as HTMLElement | null
    return view ? Math.round(view.scrollHeight - view.scrollTop - view.clientHeight) : 0
  })

test('the conversation stays with an answer while it arrives', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)

  await send(page, 'think out loud about this')

  // Watched all the way through rather than checked at the end: the end is
  // where a broken one catches up, because the last flush is a small one and
  // the list settles. What must hold is every frame in between.
  const drifts: number[] = []
  let grew = 0
  let said = 0
  for (let i = 0; i < 90; i++) {
    await page.waitForTimeout(100)
    const [gap, text] = await Promise.all([
      fromBottom(page),
      page.evaluate(() => document.body.innerText),
    ])
    if (text.length > said) {
      grew++
      said = text.length
    }
    drifts.push(gap)
    if (text.includes('Point 29,')) break
  }

  // The answer really did arrive a piece at a time, which is what makes the
  // frames in between worth measuring at all.
  expect(grew).toBeGreaterThan(5)
  await expect(page.getByText('Point 29,')).toBeVisible()

  // And the newest words were on screen the whole way. A screenful of slack,
  // because a flush lands between two measurements and the list catches up on
  // the next frame; what this refuses is the answer running away entirely.
  const worst = Math.max(...drifts)
  expect(worst).toBeLessThan(700)
})

// The other half of the same rule: if the person has scrolled UP to read
// something, an answer arriving must not drag them back down to it.
test('an answer arriving does not drag a reader back down', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)

  await send(page, 'think out loud about this')
  // Enough of it to be worth scrolling. Waiting on a clock is not enough: the
  // first half of this answer is the model THINKING, which renders as one
  // collapsed line, so at a second and a bit there is nothing on screen to
  // scroll and the test proves nothing.
  await expect(page.getByText('Point 8,')).toBeVisible({ timeout: 15_000 })

  await page.mouse.move(550, 400)
  for (let i = 0; i < 6; i++) {
    await page.mouse.wheel(0, -400)
    await page.waitForTimeout(40)
  }
  const left = await fromBottom(page)
  expect(left).toBeGreaterThan(200) // really did scroll away from the newest words

  await page.waitForTimeout(2000) // more of the answer arrives while they read
  const still = await fromBottom(page)
  // Still away from the bottom: the view was not dragged back.
  expect(still).toBeGreaterThan(150)
})

// Reading back through a conversation while the assistant keeps talking.
//
// A message landing at the bottom pushes everything above it along. If nothing
// holds the reader, the words slide out from under them: measured at three
// paragraphs for one short message, which is exactly "it does not let me read
// while new messages are coming in".
test('a message landing at the bottom does not move what you are reading', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await signInFresh(page)
  await send(page, 'think out loud about this')
  await expect(page.getByText('Point 6,')).toBeVisible({ timeout: 20_000 })
  await expect(page.locator('button[type="submit"] .animate-spin')).toHaveCount(0, {
    timeout: 30_000,
  })

  // Read some way up.
  await page.mouse.move(560, 400)
  for (let i = 0; i < 3; i++) {
    await page.mouse.wheel(0, -300)
    await page.waitForTimeout(60)
  }
  await page.waitForTimeout(300)

  // Where one particular line sits on the screen, which is the only measure of
  // "did the page move under me" that means anything.
  const lineAt = () =>
    page.evaluate(() => {
      const line = [...document.querySelectorAll('p')].find((e) =>
        (e.textContent ?? '').startsWith('Point 13,'),
      )
      const view = document.querySelector('.fx-scroll') as HTMLElement
      return {
        top: line ? Math.round(line.getBoundingClientRect().top) : null,
        height: view.scrollHeight,
      }
    })

  const before = await lineAt()
  expect(before.top).not.toBeNull()

  // Something arrives at the bottom while they are up here.
  await send(page, 'hello')
  await expect(page.getByText(/Ask me to run a background job/)).toBeVisible({ timeout: 20_000 })
  await page.waitForTimeout(700)

  const after = await lineAt()
  expect(after.height).toBeGreaterThan(before.height) // something really did arrive
  const moved = Math.abs((after.top ?? 0) - (before.top ?? 0))
  console.log(`the line moved ${moved}px while ${after.height - before.height}px arrived below it`)
  // A third of a line, and it is the residue of a row being placed at an
  // estimated height and measured a moment later. Before the reader was held it
  // was the whole height of what arrived.
  expect(moved).toBeLessThan(60)
})

// The other half: AT the newest message, a new one arriving is exactly what you
// want to be shown. Holding the reader there would be the opposite mistake.
test('at the newest message, a new one still arrives under your eyes', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await signInFresh(page)
  await send(page, 'hello')
  await expect(page.getByText(/Ask me to run a background job/)).toBeVisible({ timeout: 20_000 })
  await page.waitForTimeout(500)

  const atNewest = await page.evaluate(() => {
    const v = document.querySelector('.fx-scroll') as HTMLElement
    return Math.round(v.scrollHeight - v.scrollTop - v.clientHeight)
  })
  expect(atNewest).toBeLessThan(8)
})
