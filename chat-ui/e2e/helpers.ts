import { expect, type Page } from '@playwright/test'

export const EMAIL = 'e2e@acme.test'
export const PASSWORD = 'e2e-password-1234'
// The distinctive token the scripted specialist returns, so a completion
// narration can be proven to carry the specialist's actual result.
export const RESULT = 'E2E-RESULT-42'

// signInFresh signs in through the real login and opens a brand-new chat, so
// each test starts from an empty conversation even though the specs share one
// seeded database (the login otherwise lands on the most recent chat).
export async function signInFresh(page: Page): Promise<void> {
  await page.goto('/')
  await page.locator('input[type="email"]').fill(EMAIL)
  await page.locator('input[type="password"]').fill(PASSWORD)
  await page.getByRole('button', { name: /^Sign in$/ }).click()
  await expect(page.locator('textarea').first()).toBeVisible()
  // A fresh draft chat, so this test's transcript is its own.
  await page.getByText('New chat').first().click()
  await page.waitForTimeout(400)
}

// send types a prompt and submits it. The scripted master reads keywords in it
// ("two", "status") to steer the flow; anything else delegates one background
// task. It waits for any in-flight turn to finish first (the submit button stops
// spinning), because the client silently drops a send while it is streaming.
export async function send(page: Page, text: string): Promise<void> {
  const box = page.locator('textarea').first()
  await expect(box).toBeVisible()
  await expect(page.locator('button[type="submit"] .animate-spin')).toHaveCount(0)
  await box.fill(text)
  await box.press('Enter')
}

// Turn on this conversation's auto-approve, via the real toggle. The toggle only
// exists once the conversation does, so this greets first to create it, then
// flips it from "Ask to approve" to "Auto-approving".
export async function enableAutoApprove(page: Page): Promise<void> {
  await send(page, 'hello')
  const toggle = page.getByRole('button', { name: /Ask to approve/i })
  await expect(toggle).toBeVisible()
  await toggle.click()
  await expect(page.getByRole('button', { name: /Auto-approving/i })).toBeVisible()
}

// noAgentTextOnUserSide asserts the role attribution the reported bug broke:
// nothing an agent said is rendered in a user (right-hand) bubble.
export async function noAgentTextOnUserSide(page: Page): Promise<void> {
  await expect(page.locator('.is-user', { hasText: RESULT })).toHaveCount(0)
  await expect(page.locator('.is-user', { hasText: /in the background/i })).toHaveCount(0)
  await expect(page.locator('.is-user', { hasText: /still running/i })).toHaveCount(0)
}

// Open a seeded conversation by NAME, through the sidebar's own search.
//
// Clicking it in the list only works while it is still in the list. The specs
// share one database and one person, so by the fiftieth spec the sidebar is
// full of chats those specs created and a seeded conversation has dropped off
// the end of it: the click then waits for something that is not there and the
// test times out looking like a hung page. Searching finds it whatever else
// has been created.
export async function openConversation(page: Page, title: string): Promise<void> {
  const search = page.getByPlaceholder(/Search chats/i)
  if (await search.count()) {
    await search.first().fill(title)
    await page.waitForTimeout(500)
  }
  await page.getByText(title).first().click({ timeout: 20_000 })
  if (await search.count()) await search.first().fill('')
  await page.waitForTimeout(300)
}
