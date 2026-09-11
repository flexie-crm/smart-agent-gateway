import { expect, test } from '@playwright/test'

import { signInFresh, send } from './helpers'

// Saying something while the assistant is still answering.
//
// A conversation answers one thing at a time and the server is strict about it:
// a second turn is refused outright with "this conversation is already
// answering". That rule is right. What was wrong is what the chat did with it,
// which was nothing at all: you typed, pressed enter, and the message was
// dropped on the floor without a word.
//
// It now goes INTO the running turn. The loop folds it in at its next step, the
// same place a tool result goes, so the model sees it before deciding what to do
// next: a correction arrives in time to change what happens rather than after it
// has happened.

/** Every chat request the page makes, so the MECHANISM can be asserted. */
function watch(page: import('@playwright/test').Page) {
  const asked: string[] = []
  page.on('request', (r) => {
    const url = r.url()
    if (url.includes('/chat/')) asked.push(url.replace(/^.*\/chat\//, ''))
  })
  return asked
}

test('what you say mid-answer goes into the turn that is running', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await signInFresh(page)

  // A turn that takes its time, so it is genuinely still running when we type.
  // Without that this test passes on a broken implementation: the message is
  // simply sent as a second turn, and a scripted model that answers either way
  // cannot tell the difference. It did exactly that, and said so.
  await send(page, 'keep checking the time')
  // A turn is RUNNING, which is the precondition and the thing to wait for. The
  // submit button spinning is what says so; waiting for assistant text instead
  // waits for the turn to be over, which is the opposite of what this needs.
  await expect(page.locator('button[type="submit"] .animate-spin')).toBeVisible({
    timeout: 15_000,
  })

  const asked = watch(page)

  // The composer is usable while the assistant works, which is the change.
  const box = page.locator('textarea').first()
  await expect(box).toBeEnabled()
  await box.fill('actually make it Vienna')
  await box.press('Enter')

  // It appears at once, because it HAS been said.
  await expect(page.getByText('actually make it Vienna', { exact: true })).toBeVisible()

  // Handed to the running turn rather than started as a new one. This is the
  // assertion that has teeth: `say` is the endpoint that reaches a live turn,
  // and `stream` is the one that starts another.
  await expect.poll(() => asked.filter((a) => a === 'say').length).toBe(1)
  expect(asked.filter((a) => a === 'stream')).toEqual([])

  // And the model SAW it: the scripted model repeats back what it was told
  // mid-turn, which it can only do if the words reached the model.
  await expect(page.getByText(/HEARD actually make it Vienna/)).toBeVisible({ timeout: 20_000 })
})

test('it is written into the conversation where it was said', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await signInFresh(page)
  await send(page, 'keep checking the time')
  await expect(page.locator('button[type="submit"] .animate-spin')).toBeVisible({
    timeout: 15_000,
  })

  const box = page.locator('textarea').first()
  await box.fill('actually make it Vienna')
  await box.press('Enter')
  await expect(page.getByText(/HEARD actually make it Vienna/)).toBeVisible({ timeout: 20_000 })

  // Wait for the turn to be completely over, or the reload rejoins a live run
  // and paints it on top of the history, which is its own thing to get right.
  await expect(page.locator('button[type="submit"] .animate-spin')).toHaveCount(0, {
    timeout: 20_000,
  })

  // A reload reads it back from the server, so this proves it was stored as a
  // message of the conversation rather than only drawn in this browser.
  await page.reload()
  // Exact, or this also matches the model's reply, which repeats the words back.
  await expect(page.getByText('actually make it Vienna', { exact: true })).toBeVisible({
    timeout: 15_000,
  })
  await expect(page.getByText(/HEARD actually make it Vienna/)).toBeVisible({ timeout: 15_000 })
})

test('said when nothing is running, it is an ordinary message', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await signInFresh(page)
  await send(page, 'hello')
  await expect(page.getByText(/Ask me to run a background job/)).toBeVisible({ timeout: 15_000 })

  // Nothing in flight: this is just a message, and it gets its own turn.
  await send(page, 'hello again')
  await expect(page.getByText(/Ask me to run a background job/).nth(1)).toBeVisible({
    timeout: 15_000,
  })
})

test('after a reload it is in the order it was said', async ({ page }) => {
  await page.setViewportSize({ width: 1180, height: 820 })
  await signInFresh(page)
  await send(page, 'keep checking the time')
  await expect(page.locator('button[type="submit"] .animate-spin')).toBeVisible({
    timeout: 15_000,
  })

  const box = page.locator('textarea').first()
  await box.fill('actually make it Vienna')
  await box.press('Enter')
  await expect(page.getByText(/HEARD actually make it Vienna/)).toBeVisible({ timeout: 20_000 })
  await expect(page.locator('button[type="submit"] .animate-spin')).toHaveCount(0, {
    timeout: 20_000,
  })

  const orderNow = await page.evaluate(() =>
    [...document.querySelectorAll('.is-user, .is-assistant')].map((e) =>
      `${e.classList.contains('is-user') ? 'you' : 'it'}: ${(e.textContent ?? '').slice(0, 30)}`,
    ),
  )
  await page.reload()
  await expect(page.getByText(/HEARD actually make it Vienna/)).toBeVisible({ timeout: 15_000 })
  await page.waitForTimeout(1200)
  const orderAfter = await page.evaluate(() =>
    [...document.querySelectorAll('.is-user, .is-assistant')].map((e) =>
      `${e.classList.contains('is-user') ? 'you' : 'it'}: ${(e.textContent ?? '').slice(0, 30)}`,
    ),
  )
  console.log('BEFORE ' + JSON.stringify(orderNow, null, 0))
  console.log('AFTER  ' + JSON.stringify(orderAfter, null, 0))

  // Once, not twice. The composer shows it the moment it is said and the turn
  // then says where it landed; if both add a copy, the person watches themselves
  // say the same thing twice.
  const saidByYou = (rows: string[]) =>
    rows.filter((r) => r.startsWith('you:') && r.includes('actually make it Vienna'))
  expect(saidByYou(orderNow)).toHaveLength(1)
  expect(saidByYou(orderAfter)).toHaveLength(1)
  // The live order is the stored order. A conversation that rearranges itself on
  // reload is telling somebody they had a different conversation from the one
  // they remember.
  expect(orderNow).toEqual(orderAfter)

  // What was said mid-turn belongs where it was said: after the words that
  // prompted it and before the answer to it. A reload that moves it to the end,
  // or that runs two answers together into one, is telling the person a
  // different conversation from the one they had.
  const you = orderAfter.findIndex((r) => r.includes('actually make it Vienna'))
  const heard = orderAfter.findIndex((r) => r.includes('HEARD'))
  expect(you).toBeGreaterThan(-1)
  expect(heard).toBeGreaterThan(you)
})
