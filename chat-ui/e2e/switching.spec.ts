import { expect, test } from '@playwright/test'

import { signInFresh, openConversation } from './helpers'

// Moving between conversations of different lengths.
//
// The virtual list is told how many rows there are and asks, by index, what each
// one's identity is. When the conversation changes underneath it, it can ask
// about an index from the list it had a moment ago: the count it is holding is
// the old one and the rows are the new ones. Answering that with a crash takes
// the whole chat down to an error boundary, which is what a person saw.

test('switching between conversations of different lengths does not break the chat', async ({
  page,
}) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await signInFresh(page)

  const broke = page.getByText('Something went wrong rendering the chat')

  // A long one, then a shorter one, then back, then a brand-new empty one:
  // every direction the count can jump.
  for (const round of [0, 1]) {
    await openConversation(page, 'A conversation worth scrolling')
    await expect(page.getByText(/answer \d+/).first()).toBeVisible({ timeout: 15_000 })
    await expect(broke).toHaveCount(0)

    await openConversation(page, 'A conversation full of code')
    await expect(page.getByText(/answer \d+/).first()).toBeVisible({ timeout: 15_000 })
    await expect(broke).toHaveCount(0)

    await page.getByText('New chat').first().click()
    await page.waitForTimeout(600)
    await expect(broke).toHaveCount(0)
    expect(round).toBeGreaterThanOrEqual(0)
  }

  // And the composer still works afterwards, which is what "the chat is alive"
  // actually means.
  await expect(page.locator('textarea').first()).toBeVisible()
})
