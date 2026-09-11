import { test, expect } from '@playwright/test'
import { RESULT, signInFresh, send } from './helpers'

// Functionality #13 (KB/27): several background specialists hitting an
// approval-gated tool at once must not flood the person with a card each. The
// server shows them one at a time, and "approve all" lets the rest through with
// no further cards. The scripted "two" flow delegates two background specialists;
// in the default (manual) mode both park on their approval-gated tool.

const approve = (page) => page.getByRole('button', { name: /^Approve$/ })

test('approval queue: background cards are shown one at a time', async ({ page }) => {
  await signInFresh(page)
  await send(page, 'run two background jobs please')

  // Two specialists parked, but only ONE card is ever on screen.
  await expect(approve(page)).toHaveCount(1)

  // Answering it surfaces the next one, still exactly one card, never both.
  await approve(page).click()
  await expect(approve(page)).toHaveCount(1)

  // Answering the second empties the queue; both tasks then narrate.
  await approve(page).click()
  await expect(page.locator('.is-assistant', { hasText: RESULT })).toHaveCount(2)
  await expect(approve(page)).toHaveCount(0)
})

test('approval queue: approve all lets the rest through with no further card', async ({ page }) => {
  await signInFresh(page)
  await send(page, 'run two background jobs please')
  await expect(approve(page)).toHaveCount(1)

  // "Approve all this session" clears the live card and drains the queue: the
  // second specialist runs with no card of its own.
  await page.getByRole('button', { name: /Approve, and stop asking/i }).click()
  await expect(page.locator('.is-assistant', { hasText: RESULT })).toHaveCount(2)
  await expect(approve(page)).toHaveCount(0)
})
