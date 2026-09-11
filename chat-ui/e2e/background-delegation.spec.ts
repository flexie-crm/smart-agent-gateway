import { test, expect } from '@playwright/test'
import { RESULT, signInFresh, send, noAgentTextOnUserSide } from './helpers'

// Functionality #1,2,3,5,7: the master delegates in the background, a card
// surfaces LIVE (no reload), approving it narrates the specialist's result, and
// that narration lands on the assistant side of the chat, never the user side.
test('background delegation: card surfaces live, approve narrates on the assistant side', async ({ page }) => {
  await signInFresh(page)
  await send(page, 'run the background job please')

  // The specialist's chip appears in the right-side rail: ONE chip, standing for
  // one agent. Located by what it is rather than by the words it renders, which
  // change as the work runs.
  await expect(page.locator('aside [data-chip="agent"]')).toHaveCount(1)
  await expect(page.getByText('Background Worker')).toBeVisible()

  // The card surfaces LIVE, with no reload (the notification-envelope bug).
  const approve = page.getByRole('button', { name: /^Approve$/ })
  await expect(approve).toBeVisible()

  // The chip shows the specialist's live token spend, pushed as the specialist
  // ran its first model call, and it SURVIVES the park for approval: the waiting
  // state must not wipe the running total (the progress record is replaced, not
  // merged, so a token update and the waiting flag share one accumulator).
  await expect(page.getByText(/\btok\b/)).toBeVisible()

  // Approving lets the specialist finish; the completion turn narrates the
  // result, streamed in live.
  await approve.click()
  await expect(page.getByText(new RegExp(RESULT))).toBeVisible()

  await expect(page.locator('.is-assistant', { hasText: RESULT })).toBeVisible()
  await noAgentTextOnUserSide(page)

  // The specialist finished, and its chip STAYS. The column is a record of what
  // was done here, not a live view: work that vanished the moment it finished
  // left somebody with no idea what had happened for them. What changes is the
  // shape, from a card that is counting to a line that is not, so the live
  // spend it was showing while it worked is gone.
  await expect(page.locator('aside [data-chip="agent"]')).toHaveCount(1)
  await expect(page.locator('aside [data-chip="fleet"]')).toHaveCount(0)
  await expect(page.getByText(/\btok\b/)).toHaveCount(0)
})
