import { test, expect } from '@playwright/test'
import { signInFresh, send, noAgentTextOnUserSide } from './helpers'

// Functionality #6: rejecting a background specialist's card ends the task as
// "not done", and the master says so plainly, on the assistant side, rather than
// dead-ending the chat.
test('reject: declining the card narrates that the task did not complete', async ({ page }) => {
  await signInFresh(page)
  await send(page, 'run the background job please')

  // "Decline", which is what the button says. It used to say "Reject", and this
  // spec went on asserting the old word: the card surfaced, the button was
  // there, and the test timed out looking for a label nothing renders.
  const reject = page.getByRole('button', { name: /^Decline$/ })
  await expect(reject).toBeVisible()
  await reject.click()

  // The master narrates the decline (the completion turn on a rejected task).
  await expect(page.getByText(/did not complete|declined/i)).toBeVisible()
  await expect(page.locator('.is-assistant', { hasText: /did not complete|declined/i })).toBeVisible()
  await noAgentTextOnUserSide(page)
})
