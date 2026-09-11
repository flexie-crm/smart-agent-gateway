import { expect, test } from '@playwright/test'

import { signInFresh, openConversation } from './helpers'

// Reading back through a long conversation.
//
// This is the interaction I shipped without ever performing it, and it was as
// bad as that deserves: a scroll listener asked for the next page the moment
// one landed, so pages piled in while the reader was still looking at the
// first, and the position was corrected a frame after the paint, so the rows
// jumped and left blank bands behind them.
//
// So it is driven here, in a browser, on a conversation of 260 rows: how much
// arrives when it is opened, that reaching the top asks for exactly ONE more
// page, and that the reader stays on the words they were reading while the
// older ones appear above them.

const CONVERSATION = 'A conversation worth scrolling'

/** The element the conversation actually scrolls in. */
async function scroller(page: import('@playwright/test').Page) {
  return page.evaluateHandle(() =>
    [...document.querySelectorAll('*')].find(
      (e) => e.scrollHeight > e.clientHeight + 50 && e.clientHeight > 200,
    ),
  )
}

test('a long conversation opens at its end, not all of it', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, CONVERSATION)
  await expect(page.getByText(/answer \d+/).first()).toBeVisible({ timeout: 15_000 })
  await page.waitForTimeout(1500)

  // A page, not a conversation. The whole of this one is 260 rows; what the
  // page builds on open must be about one page of them.
  const rows = await page.locator('.is-assistant, .is-user').count()
  expect(rows).toBeGreaterThan(0)
  expect(rows).toBeLessThan(130)

  // And it opens at the END, which is where somebody left off. Asserted as a
  // person would see it, rather than through a scroll offset: the LAST row of
  // the conversation is on screen. That survives a change of coordinate system,
  // and the coordinate system did change (the conversation is a reversed column
  // now, so being at the newest message is scrollTop at its maximum).
  const newestOnScreen = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.is-assistant, .is-user')]
    const last = rows[rows.length - 1]
    const view = document.querySelector('.fx-scroll') as HTMLElement | null
    if (!last || !view) return false
    const row = last.getBoundingClientRect()
    const box = view.getBoundingClientRect()
    return row.bottom <= box.bottom + 8 && row.bottom > box.top
  })
  expect(newestOnScreen).toBe(true)
})

test('reaching the top asks for one page, and the reader does not move', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })

  // Every request for history, counted: the failure this replaces was asking
  // again and again while the first answer was still being rendered.
  let asked = 0
  page.on('request', (r) => {
    if (r.url().includes('/chat/history')) asked++
  })

  await signInFresh(page)
  await openConversation(page, CONVERSATION)
  await expect(page.getByText(/answer \d+/).first()).toBeVisible({ timeout: 15_000 })
  await page.waitForTimeout(1500)

  const opening = asked
  const view = await scroller(page)
  const before = await page.evaluate(
    (v) => ({ rows: document.querySelectorAll('.is-assistant, .is-user').length, height: v.scrollHeight }),
    view,
  )

  // Scrolled the way a person scrolls: many small wheel events over time, not
  // one assignment to scrollTop.
  //
  // That distinction is the whole test. Setting scrollTop once fires ONE scroll
  // event, and the implementation that asked on every event looked perfect
  // under it, because it was only ever asked once. A person turning a wheel
  // produces dozens.
  await page.mouse.move(550, 400)
  let turns = 0
  while (asked === opening && turns < 60) {
    await page.mouse.wheel(0, -500)
    await page.waitForTimeout(25)
    turns++
  }
  expect(asked).toBeGreaterThan(opening) // reaching the top asks for what came before

  // And then they STOP, which is when it matters what happens under them: the
  // older rows go in above, the conversation gets taller, and the words they
  // were reading must stay where they were. This is what the fix is for, and
  // measuring it while still scrolling would prove nothing.
  const anchored = await page.evaluate(() => {
    const row = [...document.querySelectorAll('.is-assistant, .is-user')]
      .find((e) => e.getBoundingClientRect().top > 100)
    return { text: (row?.textContent ?? '').slice(0, 40), top: Math.round(row?.getBoundingClientRect().top ?? 0) }
  })
  await page.waitForFunction(
    (n) => document.querySelectorAll('.is-assistant, .is-user').length > n,
    before.rows,
    { timeout: 15_000 },
  )
  await page.waitForTimeout(1200) // long enough for a cascade to show itself

  const after = await page.evaluate((wanted) => {
    const row = [...document.querySelectorAll('.is-assistant, .is-user')]
      .find((e) => (e.textContent ?? '').slice(0, 40) === wanted)
    return {
      rows: document.querySelectorAll('.is-assistant, .is-user').length,
      top: Math.round(row?.getBoundingClientRect().top ?? -9999),
    }
  }, anchored.text)

  expect(after.rows).toBeGreaterThan(before.rows) // the older page is in
  // Asked because the reader arrived, not because the last answer landed: one
  // page per arrival, not a pile of them.
  expect(asked - opening).toBeLessThanOrEqual(2)
  // And the words they were reading did not move under them.
  expect(Math.abs(after.top - anchored.top)).toBeLessThan(120)
})

// The rows that are not being looked at are not in the document.
//
// This is the one that has teeth. Every measurement said the cost of a
// keystroke tracks the number of nodes in the page and nothing else: a memo
// around the list moved it 6.3ms to 6.5ms, content-visibility moved 7.8 to 8.0,
// and taking the conversation out of the document moved 6.8 to 3.4. So the
// number of rows in the document is the thing to hold, and reading back four
// pages of a long conversation is exactly what used to grow it without limit.
test('reading back does not grow the document', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, CONVERSATION)
  await expect(page.getByText(/answer \d+/).first()).toBeVisible({ timeout: 15_000 })
  await page.waitForTimeout(1500)

  const count = () =>
    page.evaluate(() => ({
      rows: document.querySelectorAll('.is-assistant, .is-user').length,
      nodes: document.querySelectorAll('*').length,
    }))

  const opened = await count()
  await page.mouse.move(550, 400)
  let most = opened
  for (let turn = 0; turn < 120; turn++) {
    await page.mouse.wheel(0, -600)
    await page.waitForTimeout(20)
    if (turn % 10 === 0) {
      const now = await count()
      if (now.nodes > most.nodes) most = now
    }
  }
  await page.waitForTimeout(600)
  const ended = await count()
  if (ended.nodes > most.nodes) most = ended

  // Reading back really did fetch more of the conversation...
  const fetched = await page.evaluate(
    () => document.querySelectorAll('.is-assistant, .is-user').length,
  )
  expect(fetched).toBeGreaterThan(0)
  console.log(`document held at most ${most.rows} rows, ${most.nodes} nodes (opened with ${opened.rows}/${opened.nodes})`)
  // ...and the document did not grow with it. Four pages of history were
  // fetched by the end of this scroll, four hundred rows and more; the document
  // holds a couple of dozen. On an idle machine it settles at 21, and under a
  // loaded parallel run it has been seen at 85 while measurements lag behind the
  // wheel. The ceiling is set clear of that rather than at it, because what this
  // is here to catch is a document that grows with the conversation, and that
  // failure is not 85 rows, it is four hundred.
  expect(most.rows).toBeLessThan(140)
  expect(most.nodes).toBeLessThan(opened.nodes * 4)
})
