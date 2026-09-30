import { test, expect } from '@playwright/test'

import { signInFresh, openConversation } from './helpers'

// Typing in a long conversation renders neither the chat's root nor any message.
//
// Counted through React's developer-tools hook, which a development build
// reports every commit to, so this asserts what rendered rather than how long it
// took. The root does render ONCE, when the first character wakes the send
// button: that render is also the proof this counter works, so a counter that
// silently stopped seeing anything fails here instead of passing.

const LONG = 'A conversation worth scrolling'

declare global {
  interface Window { __rendered: Record<string, number> }
}

test('typing in a long conversation renders nothing but the send button waking', async ({ page }) => {
  await page.addInitScript(() => {
    const rendered: Record<string, number> = {}
    window.__rendered = rendered
    // PerformedWork, the flag React sets on a component that actually ran; the
    // same one React's own developer tools read to say what rendered.
    const performedWork = 1
    const watched = new Set(['FlexieAiAgent', 'MessageItem'])
    type Fiber = { type?: { name?: string; displayName?: string }; alternate: Fiber | null; flags: number; child: Fiber | null; sibling: Fiber | null }
    ;(window as unknown as Record<string, unknown>).__REACT_DEVTOOLS_GLOBAL_HOOK__ = {
      supportsFiber: true,
      renderers: new Map(),
      inject(renderer: unknown) {
        const id = this.renderers.size + 1
        this.renderers.set(id, renderer)
        return id
      },
      onCommitFiberRoot(_id: number, root: { current: Fiber }) {
        const stack: (Fiber | null)[] = [root.current]
        while (stack.length) {
          const fiber = stack.pop()
          if (!fiber) continue
          const name = fiber.type?.displayName || fiber.type?.name
          if (name && watched.has(name) && fiber.alternate && (fiber.flags & performedWork)) {
            rendered[name] = (rendered[name] || 0) + 1
          }
          stack.push(fiber.child, fiber.sibling)
        }
      },
      onCommitFiberUnmount() {},
      onPostCommitFiberRoot() {},
    }
  })

  await page.setViewportSize({ width: 1100, height: 800 })
  await signInFresh(page)
  await openConversation(page, LONG)
  await expect(page.locator('.fx-scroll [data-index]').first()).toBeVisible()
  await page.waitForTimeout(2000)
  const box = page.locator('textarea').first()
  await box.click()
  await page.waitForTimeout(500)

  await page.evaluate(() => { for (const k of Object.keys(window.__rendered)) delete window.__rendered[k] })
  await page.keyboard.type('what does this cost to type, forty keys')
  await page.waitForTimeout(500)
  const rendered = await page.evaluate(() => ({ ...window.__rendered }))

  expect(rendered.MessageItem ?? 0, 'a message rendered while typing').toBe(0)
  // At least once: the send button waking. At most twice: nothing per keystroke.
  expect(rendered.FlexieAiAgent ?? 0, 'the chat root, counted over forty keystrokes').toBeGreaterThanOrEqual(1)
  expect(rendered.FlexieAiAgent ?? 0, 'the chat root, counted over forty keystrokes').toBeLessThanOrEqual(2)
})
