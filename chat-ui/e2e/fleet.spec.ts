import { test, expect, type Page } from '@playwright/test'
import { signInFresh, send, noAgentTextOnUserSide } from './helpers'

// Mode B, end to end and through the real seams: the Gateway calls one tool with
// a list of tasks, jobs go on a real broker, a real worker claims them and runs
// the agents, the join counts rows, and the Gateway is woken ONCE with all of it.
//
// Three things here are only visible in a browser, and every one of them was
// broken when it was first tried by hand:
//
//   1. the narration has to arrive LIVE. A completion turn has no request behind
//      it, so nobody hears it unless the person's tabs are told it started.
//   2. a batch is ONE chip that counts. A member with a chip of its own turns
//      thirteen agents into thirteen identical cards down the column.
//   3. a member can stop and ask, and the answer has to reach it in another
//      process.
//
// So nothing here ever reloads the page.

// FLEET is the token the scripted Gateway puts in its completion narration.
const FLEET = 'E2E-FLEET-DONE'

// The chips, by what they ARE.
//
// Not by the words they render. A chip is a card while it works and a line once
// it is over, and every word in it changes with that ("Working…", "2 of 3
// finished", an elapsed time): its text is a label written for a person to read.
// What identifies it is the attribute the rail puts on the element.
const batchChips = (page: Page) => page.locator('aside [data-chip="fleet"]')
const memberChips = (page: Page) => page.locator('aside [data-chip="agent"]')

// startBatch says hello first, then asks for the batch.
//
// Not politeness: a brand-new chat learns its own id from a frame partway
// through its first turn, and the scripted model parks agents in microseconds,
// so a batch's cards can be pushed at a client that does not yet know which
// conversation it is. That is an artefact of a model with no latency, and a spec
// that races it passes on luck rather than on the code being right.
async function startBatch(page: Page): Promise<void> {
  await send(page, 'hello')
  await expect(page.locator('.is-assistant').first()).toBeVisible()
  await send(page, 'run a fleet that asks')
}

test('fleet: one chip counts the batch, and the narration arrives without a reload', async ({ page }) => {
  await signInFresh(page)
  // The batch runs the agent whose tool does not ask, so this spec is about the
  // batch and nothing else. Turning approvals off through the UI would do the
  // same thing and race: that write is optimistic, so the label flips before the
  // server has it, and a batch started in that gap parks instead of running.
  await send(page, 'run a fleet of lookups')

  // ONE chip for the batch, and never one per member.
  await expect(batchChips(page)).toHaveCount(1, { timeout: 20_000 })
  await expect(memberChips(page)).toHaveCount(0)

  // The crux. No reload, no second prompt: the completion turn is announced over
  // the socket and the tab attaches to it on its own.
  await expect(page.locator('.is-assistant', { hasText: FLEET })).toHaveCount(1, { timeout: 30_000 })

  // It carried the AGENTS' work, not just the fact that something finished.
  await expect(page.locator('.is-assistant', { hasText: /All 3 agents reported/ })).toHaveCount(1)

  // Still one chip at the end: the column is a record of what was done, and a
  // finished batch is quieter rather than absent.
  await expect(batchChips(page)).toHaveCount(1)
  await expect(memberChips(page)).toHaveCount(0)

  // The same role attribution the background specs protect: nothing an agent
  // said leaked into a user bubble.
  await noAgentTextOnUserSide(page)
  await expect(page.locator('.is-user', { hasText: FLEET })).toHaveCount(0)
})

test('fleet: the Gateway can say how a batch is going, as one batch', async ({ page }) => {
  await signInFresh(page)
  await send(page, 'run a fleet of lookups')
  await expect(page.locator('.is-assistant', { hasText: FLEET })).toHaveCount(1, { timeout: 30_000 })

  // Asking after it must not throw the Gateway at some unrelated agent, which is
  // what it did when the status tool listed a batch as N separate agents it had
  // no record of starting.
  await send(page, 'what is the status?')
  await expect(page.locator('.is-assistant', { hasText: /still running|finished|done/i }).last())
    .toBeVisible({ timeout: 20_000 })
})

// The one that needed real machinery on both sides of a process boundary.
//
// A member runs on a worker with no socket, so it writes its card as a durable
// row and says so; the server, which has the socket, puts it on screen; and the
// person's answer becomes a job that sends the agent back where it stopped.
test('fleet: a member stops to ask, and the answer reaches it on the worker', async ({ page }) => {
  await signInFresh(page)
  // Deliberately NO auto-approve: the seeded agent's one tool is gated, so every
  // member has to stop and ask.
  await startBatch(page)

  const approve = page.getByRole('button', { name: /^Approve$/ })

  // One card at a time, never three: the queue background agents already use
  // covers a batch, because a member's park is an ordinary detached park.
  //
  // And ONE chip on every pass, not just at the end. That is the assertion that
  // was missing: a parked member used to push a chip of its own, and it never
  // showed because every other spec auto-approves, and a member that does not
  // park does not push one.
  // Counted by what was APPROVED, not by the card going away.
  //
  // "One card, click it, now there are none" reads as one at a time, and it is
  // really an assertion that a gap exists between one member's card and the
  // next. That gap is not a property of the queue, it is how long the server
  // took, so the test passed while the chat was slow to render and failed the
  // moment it got faster. What one-at-a-time means is that there is never more
  // than one card, which is asserted every pass, and that every approval lands.
  const approved = page.getByText('Approved', { exact: true })
  for (let i = 0; i < 3; i++) {
    await expect(approve).toHaveCount(1, { timeout: 20_000 })
    await expect(batchChips(page)).toHaveCount(1)
    await expect(memberChips(page)).toHaveCount(0)
    await approve.first().click()
    await expect(approved).toHaveCount(i + 1, { timeout: 20_000 })
  }
  await expect(approve).toHaveCount(0, { timeout: 20_000 })

  // Every answer was carried out on a worker and every member came back, so the
  // batch closes and the Gateway is woken once with all three.
  await expect(page.locator('.is-assistant', { hasText: FLEET })).toHaveCount(1, { timeout: 30_000 })
  await expect(page.locator('.is-assistant', { hasText: /All 3 agents reported/ })).toHaveCount(1)
  await expect(batchChips(page)).toHaveCount(1)
  await expect(memberChips(page)).toHaveCount(0)
  await noAgentTextOnUserSide(page)
})

// "Approve, and stop asking" across a batch.
//
// This is where the worst of it lived. The queue drain answered every waiting
// card through the IN-PROCESS resume, whatever mode the card belonged to, and
// chose which agent to resume by the parent tool call, which a batch's members
// share. So one member was re-launched once per card, in the wrong process, and
// each finish woke the Gateway as a single finished agent, which then started a
// second batch nobody asked for.
test('fleet: approve-all lets the whole batch through, and starts nothing new', async ({ page }) => {
  await signInFresh(page)
  await startBatch(page)

  await expect(page.getByRole('button', { name: /^Approve$/ })).toHaveCount(1, { timeout: 20_000 })
  await page.getByRole('button', { name: /Approve, and stop asking/i }).click()
  await expect(page.getByRole('button', { name: /^Approve$/ })).toHaveCount(0, { timeout: 20_000 })

  // The batch finishes and narrates ONCE.
  await expect(page.locator('.is-assistant', { hasText: FLEET })).toHaveCount(1, { timeout: 30_000 })

  // And exactly one batch ever existed. A second one here is the Gateway having
  // been told an agent finished when a BATCH had not, and starting more.
  await expect(batchChips(page)).toHaveCount(1)
  await expect(memberChips(page)).toHaveCount(0)
})
