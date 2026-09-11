import { defineConfig } from '@playwright/test'

/**
 * The DESKTOP end-to-end gate. Its specs live in desktop/personal/e2e; this
 * config sits here because this is where the Playwright toolchain is installed,
 * and a second copy of a browser runner to keep one file tidy would be a poor
 * trade.
 *
 * The path was `../desktop/e2e` until the reorganisation into personal/ and
 * enterprise/ moved the specs and left this behind. Playwright does not treat a
 * missing directory as an error: it finds no tests, says "No tests found", and
 * the gate fails on something that reads like a configuration slip rather than
 * what it was, which is that this gate had not been run since the move.
 *
 * One worker and no retries on purpose: the specs share one installation and are
 * ORDERED. A first run happens once, and "the second application" only means
 * anything after the first has been in. Retrying a spec that has already changed
 * the installation would test a different situation and call it the same one.
 */
export default defineConfig({
  testDir: '../desktop/personal/e2e',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 60_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  use: {
    baseURL: process.env.SAG_DESKTOP_E2E_URL ?? 'http://127.0.0.1:8199',
    trace: 'retain-on-failure',
  },
})
