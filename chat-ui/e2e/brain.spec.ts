import { test, expect } from '@playwright/test'
import { signInFresh, send, noAgentTextOnUserSide } from './helpers'

// The brain tool through the socket-to-DOM path unit and API tests cannot see:
// a write to a shared knowledge base raises a real card in the browser and lands
// on approval, while a write to the agent's own memory brain never asks.

// A write to a SHARED knowledge base surfaces a card that names the write, and
// approving it saves and narrates, on the assistant side.
test('brain: a shared-brain write cards, and approving it saves and narrates', async ({ page }) => {
  await signInFresh(page)
  await send(page, 'save this decision to the knowledge base')

  // The card's copy names the actual write (the per-call approval copy from the
  // pre-park validator), so a person reads what will happen, not a generic line.
  const approve = page.getByRole('button', { name: /^Approve$/ })
  await expect(approve).toBeVisible()
  // The brain name appears in both the card's line and its details, so match the
  // first: the point is that the card names the write, not that it does so once.
  await expect(page.getByText(/Team Notes/i).first()).toBeVisible()
  await approve.click()

  // The write landed and the master narrated it in prose, on the assistant side.
  await expect(page.getByText('BRAIN-E2E-SAVED')).toBeVisible()
  await expect(page.locator('.is-assistant', { hasText: 'BRAIN-E2E-SAVED' })).toBeVisible()
  await noAgentTextOnUserSide(page)
})

// A write to the agent's OWN memory brain runs with NO card: approving your own
// note-taking would be absurd, so the memory tool never parks.
test('brain: a memory write runs silently, without a card', async ({ page }) => {
  await signInFresh(page)
  await send(page, 'remember this for next time')

  await expect(page.getByText('MEMORY-E2E-SAVED')).toBeVisible()
  await expect(page.getByRole('button', { name: /^Approve$/ })).toHaveCount(0)
})
