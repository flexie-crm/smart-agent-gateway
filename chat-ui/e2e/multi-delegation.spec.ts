import { test, expect } from '@playwright/test'
import { RESULT, signInFresh, send, enableAutoApprove, noAgentTextOnUserSide } from './helpers'

// Functionality #12: two background specialists at once, their completion turns
// interleaving with the person's own messages. This is where the reported bug
// lived: agent narration rendered into the user's right-hand bubbles. The whole
// point of this spec is to catch that, so the role assertions are the crux.
test('multi-delegation: two background tasks narrate, agent text never on the user side', async ({ page }) => {
  await signInFresh(page)
  await enableAutoApprove(page)

  // The scripted master reads "two" and delegates two background tasks at once.
  await send(page, 'run two background jobs please')

  // Both tasks complete; their two completion turns interleave on the session's
  // one lane and each narrates its result. (The chips are deliberately not
  // asserted: under auto-approve the tasks finish in about a second, so the chips
  // are transient; the durable signal is the two narrations.)
  await expect(page.locator('.is-assistant', { hasText: RESULT })).toHaveCount(2)

  // The crux: NOTHING an agent said leaked into a user (right-hand) bubble, and
  // the person's own words are on the user side, where they belong.
  await noAgentTextOnUserSide(page)
  await expect(page.locator('.is-user', { hasText: /two background jobs/i })).toHaveCount(1)
})
