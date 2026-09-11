import { test, expect } from '@playwright/test'
import { signInFresh, send } from './helpers'

// The master asks for a tool it was never granted (http_request; it holds only
// current_time and the subagent tool). That is a model hallucination, and it is
// OUR code's job, not the model's, to handle it: the call must never become tool
// usage in the chat, and the model must be handed a plain "no such tool" so it
// recovers in words. Regression for the flow bug where an ungranted tool call
// surfaced as a failed tool card ({"error":"unknown tool"}) in the transcript.
test('a hallucinated tool call never surfaces as tool usage; the agent recovers', async ({ page }) => {
  await signInFresh(page)
  await send(page, 'ghost please')

  // The recovery lands: the model answered after the loop fed it the correction.
  await expect(page.getByText(/GHOST-RECOVERED/)).toBeVisible()

  // No tool card for the ungranted tool, by friendly name OR raw alias, and
  // nothing rendered as a failed tool call. The hallucination left no trace.
  await expect(page.getByText('API request')).toHaveCount(0)
  await expect(page.getByText('http_request')).toHaveCount(0)
  await expect(page.getByText(/unknown tool/i)).toHaveCount(0)
})
