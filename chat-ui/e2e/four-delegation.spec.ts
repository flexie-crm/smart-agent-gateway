import { test, expect } from '@playwright/test'
import { RESULT, signInFresh, send, enableAutoApprove, noAgentTextOnUserSide } from './helpers'

// Mirrors the reported scenario: the master delegates FOUR background specialists
// at once (like a CRM Database agent in background mode). The master ends its
// delegate turn and each specialist, on finishing, wakes it in its OWN completion
// turn. All four result messages must land on the assistant side, none dropped
// when several complete close together (the paint race, KB/29). This is the
// higher-N stress of multi-delegation.spec.ts.
test('four background tasks: every result narrates, none dropped', async ({ page }) => {
  await signInFresh(page)
  await enableAutoApprove(page)

  // The scripted master reads "four" and delegates four background tasks at once.
  await send(page, 'run four background jobs please')

  // Four separate completion turns, four result bubbles, all painted live with no
  // reload. If one fails to paint, this is the assertion that catches it.
  await expect(page.locator('.is-assistant', { hasText: RESULT })).toHaveCount(4)

  // Nothing an agent said leaked into a user (right-hand) bubble.
  await noAgentTextOnUserSide(page)
  await expect(page.locator('.is-user', { hasText: /four background jobs/i })).toHaveCount(1)
})
