import { defineConfig } from '@playwright/test'

// The browser end-to-end gate. It drives the real chat UI against the real
// orchestrator run by cmd/sag-e2e (a scripted, deterministic model), so it can
// assert the live behaviour unit tests cannot see: a socket push turning into a
// DOM change with no reload. It is a separate gate from `make ci` (it needs a
// browser and a running server); `make e2e` starts the server, then runs this.
//
// The Go server must already be listening on :8090 before this runs (the run
// script starts it). This config starts the chat UI dev server pointed at it.
export default defineConfig({
  testDir: './e2e',
  timeout: 90_000,
  // Generous, because a loaded CI box runs the whole delegate/park/approve/
  // complete chain behind each assertion; a slow machine must not false-fail.
  expect: { timeout: 30_000 },
  fullyParallel: false,
  // ONE worker, and it is not a performance decision.
  //
  // The gate runs the real server, with ONE seeded person in ONE workspace, so
  // parallel spec files are four browsers that the server correctly sees as four
  // tabs belonging to the same human. They then race each other over state the
  // specs do not own: whose approval card is live, whose conversation the run
  // manager's lane belongs to, whose turn is next. Anything that fails there is
  // a fact about four tabs of one person, not about the feature under test.
  //
  // Testing that IS worth doing, and it wants specs written for it, with their
  // own people and their own conversations. It is not worth having every other
  // spec fail on it at random.
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL: 'http://localhost:5273',
    trace: 'retain-on-failure',
  },
  webServer: {
    command: 'npm run dev -- --port 5273 --strictPort',
    url: 'http://localhost:5273',
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
    // The chat UI talks to the orchestrator DIRECTLY (no dev proxy), so point it
    // at the E2E server. VITE_SAG_API_URL is the var src/lib/api.ts reads.
    env: { VITE_SAG_API_URL: 'http://localhost:8090' },
  },
})
