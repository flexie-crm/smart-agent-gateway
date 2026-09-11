import { expect, test } from '@playwright/test'

import { signInFresh, send, openConversation } from './helpers'

// Reading a conversation while it is being written.
//
// The complaint this exists for: scrolling up to read what has gone past, and
// being unable to, because the words keep sliding away and newer ones take
// their place. It went unfound for a long time because every measurement asked
// "did anything SCROLL", and while the bug is happening nothing does. The
// viewport sat at a fixed distance from the newest message, the answer grew at
// that end, and the text moved underneath a window that never budged. Zero
// scroll writes, and the screen moving the whole time.
//
// So these ask about the WORDS. Where a real line of text sits on the screen is
// the only measure of "did the page move under me" that means anything, and a
// count of scroll calls is not a proxy for it in either direction: the list
// legitimately writes the scroll when a row ABOVE the reader is measured, which
// is what holds them still, and asserting zero there would condemn the code
// doing the work.

const CODE = 'A conversation full of code'

/** Put the pointer over the conversation, because a wheel goes where it points. */
async function overTheConversation(page: import('@playwright/test').Page) {
  const box = await page.locator('.fx-scroll').boundingBox()
  if (!box) throw new Error('the conversation has no box to point at')
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
}

/** How far from the newest message: what is left to scroll. */
const fromNewest = (page: import('@playwright/test').Page) =>
  page.evaluate(() => {
    const v = document.querySelector('.fx-scroll') as HTMLElement | null
    return v ? Math.round(v.scrollHeight - v.scrollTop - v.clientHeight) : -1
  })

/** Pick a line on screen and follow where it sits. */
async function watchALine(page: import('@playwright/test').Page) {
  return page.evaluate(() => {
    const v = document.querySelector('.fx-scroll') as HTMLElement
    const box = v.getBoundingClientRect()
    // Any row with something on screen. A narrow band picks nothing when the
    // rows happen to be tall, and a helper that returns null reads as the
    // feature being broken.
    const rows = [...v.querySelectorAll('[data-index]')]
    const row =
      rows.find((e) => {
        const r = e.getBoundingClientRect()
        return r.top > box.top + 20 && r.top < box.bottom - 100
      }) ??
      rows.find((e) => {
        const r = e.getBoundingClientRect()
        return r.bottom > box.top && r.top < box.bottom
      })
    if (!row) return null
    ;(window as unknown as { __watch: Element }).__watch = row
    return { top: Math.round(row.getBoundingClientRect().top), tall: v.scrollHeight }
  })
}

const lineNow = (page: import('@playwright/test').Page) =>
  page.evaluate(() => {
    const r = (window as unknown as { __watch?: Element }).__watch
    return r?.isConnected ? Math.round(r.getBoundingClientRect().top) : null
  })

test('the conversation is an ordinary column', async ({ page }) => {
  await signInFresh(page)
  const direction = await page.evaluate(
    () => getComputedStyle(document.querySelector('.fx-scroll') as HTMLElement).flexDirection,
  )
  // It was reversed, so the scroll origin WAS the newest message. That made the
  // bottom free and made every arriving token move the words of anybody reading
  // further up, which cost 103 corrective scrolls in 88 seconds of somebody
  // sitting still. See KB/40.
  expect(direction).toBe('column')
})

test('at the newest message, an answer arriving keeps you there', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await send(page, 'talk for a long time about this')

  const seen: number[] = []
  for (let i = 0; i < 40; i++) {
    await page.waitForTimeout(100)
    seen.push(await fromNewest(page))
  }
  const worst = Math.max(...seen)
  console.log(`at the newest message, furthest away it got: ${worst}px`)
  // About a line, not about nothing. Being at the newest message is now held
  // rather than free: a token arrives, the conversation is taller, and the view
  // follows on the next layout. The gap between those two is one paragraph at
  // worst and it closes immediately, which is what somebody watching an answer
  // arrive cannot see. Nought would mean the correction ran BEFORE the content
  // it corrects for.
  expect(worst).toBeLessThan(120)
})

test('scrolling up during a live answer, you stay where you put yourself', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await send(page, 'talk for a long time about this')

  // Wait for something to scroll through, rather than for a number of
  // milliseconds: a test that scrolls a viewport with nothing overflowing it
  // measures nothing and reports it as a pass.
  let room = { overflow: 0, streaming: false }
  for (let i = 0; i < 90; i++) {
    room = await page.evaluate(() => {
      const v = document.querySelector('.fx-scroll') as HTMLElement
      return {
        overflow: v.scrollHeight - v.clientHeight,
        streaming: !!document.querySelector('button[type="submit"] .animate-spin'),
      }
    })
    if (room.overflow > 900) break
    await page.waitForTimeout(50)
  }
  expect(room.overflow).toBeGreaterThan(900)
  expect(room.streaming).toBe(true)

  await overTheConversation(page)
  for (let i = 0; i < 5; i++) {
    await page.mouse.wheel(0, -260)
    await page.waitForTimeout(30)
  }
  const after = await fromNewest(page)
  console.log(`scrolled to ${after}px from the newest message while it was still arriving`)
  expect(after).toBeGreaterThan(150)

  // Hands off. Being dragged back shows up as the distance SHRINKING on its own.
  const seen: number[] = []
  for (let i = 0; i < 20; i++) {
    await page.waitForTimeout(80)
    seen.push(await fromNewest(page))
  }
  console.log(`then, untouched: ${seen.join(', ')}`)
  expect(Math.min(...seen)).toBeGreaterThan(after - 60)
})

test('the words you are reading stay put while an answer grows below them', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await send(page, 'talk for a long time about this')

  let room = { overflow: 0, streaming: false }
  for (let i = 0; i < 90; i++) {
    room = await page.evaluate(() => {
      const v = document.querySelector('.fx-scroll') as HTMLElement
      return {
        overflow: v.scrollHeight - v.clientHeight,
        streaming: !!document.querySelector('button[type="submit"] .animate-spin'),
      }
    })
    if (room.overflow > 900) break
    await page.waitForTimeout(50)
  }
  expect(room.streaming).toBe(true)

  await overTheConversation(page)
  for (let i = 0; i < 4; i++) {
    await page.mouse.wheel(0, -240)
    await page.waitForTimeout(30)
  }
  await page.waitForTimeout(200)

  const watched = await watchALine(page)
  expect(watched).not.toBeNull()

  const drifts: number[] = []
  for (let i = 0; i < 25; i++) {
    await page.waitForTimeout(100)
    const now = await lineNow(page)
    if (now !== null) drifts.push(now - watched!.top)
  }
  const grew = await page.evaluate(
    () => (document.querySelector('.fx-scroll') as HTMLElement).scrollHeight,
  )
  const sorted = drifts.map(Math.abs).sort((a, b) => a - b)
  const typical = sorted[Math.floor(sorted.length / 2)]
  const settled = Math.abs(drifts[drifts.length - 1])
  console.log(`${grew - watched!.tall}px of answer arrived`)
  console.log(`the line: typically ${typical}px off, ended ${settled}px off`)
  console.log(`drifts: ${drifts.join(', ')}`)

  expect(grew - watched!.tall).toBeGreaterThan(300) // it really did grow
  expect(typical).toBeLessThan(8)
  expect(settled).toBeLessThan(8)
})

test('reading back through a long conversation is never pulled downward', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, CODE)
  await expect(page.locator('pre').first()).toBeVisible({ timeout: 15_000 })
  await page.waitForTimeout(1500)

  await overTheConversation(page)
  const seen: number[] = []
  for (let burst = 0; burst < 8; burst++) {
    for (let i = 0; i < 6; i++) {
      await page.mouse.wheel(0, -260)
      await page.waitForTimeout(40)
    }
    // The pause matters: it is when a row settles and its code is coloured in,
    // which is the re-measurement this is about.
    await page.waitForTimeout(400)
    seen.push(await fromNewest(page))
  }
  console.log(`reading back: ${seen.join(', ')}`)
  expect(seen[seen.length - 1]).toBeGreaterThan(600) // they really travelled
  // Only ever further back. A pull shows up as the distance shrinking.
  const pulled = seen.filter((v, i) => i > 0 && v < seen[i - 1] - 2)
  expect(pulled).toEqual([])
})
