import { test, expect, type Page } from '@playwright/test'
import { RESULT, signInFresh, send, enableAutoApprove, noAgentTextOnUserSide } from './helpers'

// The chips have to be there WHILE the work is running.
//
// That is the whole of it, and it is the one thing nothing else asserted. A chip
// stays after its delegation finishes (deliberately: the column is a record of
// what was done), so counting chips at the end of a spec cannot tell the feature
// working from the bug we shipped, where they only ever appeared once everything
// was over. Every other background spec auto-approves, the scripted agents finish
// in about a second, and all of them pass either way.
//
// So these two hold the agents open, by asking for the tasks the scripted model
// takes a moment over, and look at the rail while they are still going. What is
// asserted is the chip's STATE rather than its words: running is a card that is
// counting, done is a line that is not, and the words in both change constantly.

const running = (page: Page) => page.locator('aside [data-chip="agent"][data-chip-state="running"]')
const finished = (page: Page) => page.locator('aside [data-chip="agent"][data-chip-state="done"]')
const runningBatch = (page: Page) => page.locator('aside [data-chip="fleet"][data-chip-state="running"]')

// Two at once is the case that was reported and the case nothing covered:
// multi-delegation.spec.ts says outright that it does not assert the chips.
test('two background agents each get a chip while they are working', async ({ page }) => {
  await signInFresh(page)
  await enableAutoApprove(page)

  await send(page, 'run two slow background jobs please')

  // The crux. Both chips, both RUNNING, before either has finished. A chip that
  // only surfaces on completion gives nought here and two at the end.
  await expect(running(page)).toHaveCount(2, { timeout: 20_000 })
  await expect(finished(page)).toHaveCount(0)

  // And then they finish, both narrate, and both chips become lines. The rail
  // keeps the record; what it stops doing is counting. (What a running chip
  // SHOWS, the live spend, is background-delegation.spec.ts's subject; this one
  // is about when a chip is on screen at all, and a spend cannot be asserted
  // during the pause anyway, because a model reports what it used at the end of
  // a call rather than the start.)
  await expect(page.locator('.is-assistant', { hasText: RESULT })).toHaveCount(2, { timeout: 30_000 })
  await expect(finished(page)).toHaveCount(2)
  await expect(running(page)).toHaveCount(0)

  await noAgentTextOnUserSide(page)
})

// The same question of a batch, which counts rather than lists. fleet.spec.ts
// proves ONE chip and not one per member; what it cannot prove is that the one
// chip is there while the batch runs, because its members park and the spec is
// held open by the cards. This one has nothing holding it open but the work.
test('a batch gets its one chip while its agents are working', async ({ page }) => {
  await signInFresh(page)

  await send(page, 'run a fleet that lingers')

  await expect(runningBatch(page)).toHaveCount(1, { timeout: 20_000 })
  // Never one per member, at the moment it would be most tempting to draw them:
  // while they are all going at once.
  await expect(running(page)).toHaveCount(0)

  await expect(page.locator('.is-assistant', { hasText: 'E2E-FLEET-DONE' })).toHaveCount(1, {
    timeout: 30_000,
  })
  await expect(page.locator('aside [data-chip="fleet"][data-chip-state="done"]')).toHaveCount(1)
  await expect(runningBatch(page)).toHaveCount(0)
})
