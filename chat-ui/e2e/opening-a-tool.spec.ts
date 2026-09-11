import { expect, test } from '@playwright/test'

import { send, signInFresh } from './helpers'

// Opening a tool call, in the real product.
//
// The panel, the permission and the endpoint each have their own test, and none
// of those runs the chat. What a person actually does is send a message, watch a
// row appear, click it, and read what the tool was sent and what it answered.
// Between the click and the reading lie the frame, the id, the request and the
// rendering, which is where every defect in this has actually been: the id was
// missing from an agent's frame, so the one row somebody most wants to read (what
// did the specialist run?) was the one row that would not open.

// The delegation hands off inline, the agent asks the time, the Gateway narrates:
// one conversation with both kinds of row in it, a tool somebody's work went
// through and the wiring that got it there.
const ONE_OF_EACH = 'inline continue, please'

const settled = async (page: import('@playwright/test').Page) =>
  expect(page.locator('button[type="submit"] .animate-spin')).toHaveCount(0, { timeout: 30_000 })

test('a tool row opens and shows what the tool did', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await send(page, ONE_OF_EACH)
  await settled(page)

  const row = page.getByRole('button').filter({ hasText: 'Current time' }).first()
  await expect(row).toBeVisible()

  // Nothing is carried until it is asked for: the row alone shows no panel.
  await expect(page.locator('.fx-machine')).toHaveCount(0)

  await row.click()
  const panel = page.locator('.fx-machine').first()
  await expect(panel).toBeVisible()
  // What the tool really answered, fetched from the server on the click.
  await expect(panel).toContainText('Answered')
  await expect(panel).toContainText(/\d{4}-\d{2}-\d{2}|Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday/)

  // And it closes again.
  await row.click()
  await expect(page.locator('.fx-machine')).toHaveCount(0)
})

// Our own wiring is not somebody's business, and it is withheld structurally:
// the row carries no id, so there is nothing to open rather than a panel that
// refuses when clicked.
test('the assistant own wiring cannot be opened', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await send(page, ONE_OF_EACH)
  await settled(page)

  // The delegation is there to read...
  await expect(page.getByText('Delegate to an agent').first()).toBeVisible()
  // ...and unlike the tool row beside it, it is not something you can open.
  await expect(page.getByRole('button').filter({ hasText: 'Delegate to an agent' })).toHaveCount(0)
  await expect(page.getByRole('button').filter({ hasText: 'Current time' })).not.toHaveCount(0)
})

// What a person may read is decided on the wire. Somebody without the permission
// is refused by the endpoint: withholding it in the page would mean it had
// already arrived, which is not withholding.
test('a person without the permission is refused what a tool carried', async ({ page, browser }) => {
  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await send(page, ONE_OF_EACH)
  await settled(page)
  await expect(page.getByRole('button').filter({ hasText: 'Current time' }).first()).toBeVisible()

  // The seeded onlooker holds chats:delete and nothing else.
  const other = await browser.newPage()
  await other.goto('/')
  await other.locator('input[type="email"]').fill('watcher@acme.test')
  await other.locator('input[type="password"]').fill('e2e-password-1234')
  await other.getByRole('button', { name: /^Sign in$/ }).click()
  await expect(other.locator('textarea').first()).toBeVisible()

  // The chat talks to the gateway directly (a different origin in dev), so the
  // request has to go where the app's own do, or this proves nothing about the
  // API: a relative path lands on the page server and 404s whoever asks.
  const api = process.env.VITE_SAG_API_URL ?? 'http://localhost:8090'
  const refused = await other.evaluate(async (base) => {
    // Signed in properly: the access token lives in memory, so it is minted
    // from the session cookie exactly as the app does on a page load. Asking
    // without it would answer 401, which says nothing about the permission.
    const bootstrap = await fetch(`${base}/v1/auth/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'include',
    })
    const { access_token } = await bootstrap.json()
    const res = await fetch(`${base}/v1/chat/tool-call`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${access_token}` },
      body: JSON.stringify({ id: '1' }),
      credentials: 'include',
    })
    return res.status
  }, api)
  // 403 is the answer we want: they are signed in and were refused. A 401 would
  // mean the session, not the permission, is what stopped them, and this test
  // would be proving the wrong thing.
  expect(refused).toBe(403)
  await other.close()
})
