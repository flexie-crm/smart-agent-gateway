import { expect, test } from '@playwright/test'

import { signInFresh, openConversation } from './helpers'

// What a big conversation actually costs the browser.
//
// The list is virtual, so the claim is that the DOM holds a screenful whatever
// the conversation's length, and that reading all the way back does not grow it.
// That is the kind of claim worth a number rather than a paragraph: the fixture
// is shaped after a real session (142 turns, 284 steps, about 400 tool calls).

const LONG = 'A conversation worth scrolling'

const measure = (page: import('@playwright/test').Page) =>
  page.evaluate(() => {
    const view = document.querySelector('.fx-scroll') as HTMLElement
    const perf = performance as unknown as { memory?: { usedJSHeapSize: number } }
    return {
      nodes: document.querySelectorAll('*').length,
      rows: view.querySelectorAll('[data-index]').length,
      scrollHeight: view.scrollHeight,
      heapMB: perf.memory ? Math.round(perf.memory.usedJSHeapSize / 1048576) : -1,
    }
  })

test('a big conversation holds a screenful of DOM, however far back you read', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, LONG)
  await page.waitForTimeout(2500)

  const atRest = await measure(page)
  console.log(`at rest:  ${atRest.nodes} nodes, ${atRest.rows} rows drawn, ${atRest.heapMB}MB heap`)

  // Read all the way back, which also pulls in every page of history.
  const box = await page.locator('.fx-scroll').boundingBox()
  await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2)
  const seen: number[] = []
  for (let i = 0; i < 120; i++) {
    await page.mouse.wheel(0, -900)
    await page.waitForTimeout(35)
    if (i % 20 === 19) {
      const now = await measure(page)
      seen.push(now.rows)
      console.log(`  after ${(i + 1) * 900}px: ${now.nodes} nodes, ${now.rows} rows, ${now.scrollHeight}px tall, ${now.heapMB}MB`)
    }
  }
  await page.waitForTimeout(1200)

  const after = await measure(page)
  console.log(`at the far end: ${after.nodes} nodes, ${after.rows} rows drawn, ${after.scrollHeight}px of conversation, ${after.heapMB}MB heap`)
  console.log(`rows drawn along the way: ${seen.join(', ')}`)

  // The conversation really is long, or this measures nothing.
  expect(after.scrollHeight).toBeGreaterThan(20_000)
  // And the DOM did not grow with it. A screenful plus overscan, not 284 steps.
  expect(after.rows).toBeLessThan(60)
  expect(after.nodes).toBeLessThan(atRest.nodes * 3)
})
