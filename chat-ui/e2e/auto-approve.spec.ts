import { test, expect } from '@playwright/test'
import { signInFresh, send, enableAutoApprove } from './helpers'

// Functionality #4: with auto-approve on, a background specialist's approval-gated
// tool runs WITHOUT a card. This is the "auto is on and it still parked" bug,
// asserted live: the invariant is that no card ever surfaces.
//
// The completion narration is deliberately not asserted here: whether it renders
// live is the concurrent/fast-completion race that multi-delegation.spec.ts
// documents, and it would make this test flaky. This test owns one thing, that
// auto-approve suppresses the card, and asserts it deterministically.
test('auto-approve: a background task runs with no card', async ({ page }) => {
  await signInFresh(page)
  await enableAutoApprove(page)
  await expect(page.getByRole('button', { name: /Auto-approving/i })).toBeVisible()

  await send(page, 'run the background job please')

  // Give the specialist time to reach and pass the point a card would appear.
  await page.waitForTimeout(3000)
  // Auto is on, so the approval-gated tool ran without ever asking.
  await expect(page.getByRole('button', { name: /^Approve$/ })).toHaveCount(0)
})
