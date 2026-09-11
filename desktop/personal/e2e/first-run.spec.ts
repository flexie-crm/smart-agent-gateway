import { expect, test, type BrowserContext, type Page } from '@playwright/test'

/**
 * The first run, end to end, against the built application.
 *
 * Every assertion here is a regression that actually shipped and was found by a
 * person clicking rather than by a test: a welcome shown twice, a button that
 * turned forever, a model created with nothing pointing at it, a chat that
 * offered a composer with nothing behind it.
 *
 * ONE context for all of it, because one application is one window is one
 * cookie jar. Giving each spec a fresh context modelled something that no
 * longer exists, and the gate said so: the chat spec failed because a brand new
 * context has no session and was shown the welcome instead of the setup notice.
 * The specs run in order for the same reason a first run happens once.
 *
 * There used to be a spec about "the second application" not asking again. It
 * went with the second application: the chat and the console are two front ends
 * on one origin, so the welcome happens once because it cannot happen twice,
 * which is why the marker, the launcher endpoint and the auto-sign-in effects
 * could all be deleted.
 */

test.describe.configure({ mode: 'serial' })

let context: BrowserContext
let page: Page

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext()
  page = await context.newPage()
})

test.afterAll(async () => {
  await context.close()
})

test('the window opens on the welcome, and asks nothing else', async () => {
  await page.goto('/')

  // Nobody has been in, so the welcome is correct. It is the only screen: a
  // personal installation issues no password, so a password field would be a
  // question with no possible answer.
  await expect(page.getByRole('button', { name: 'Continue' })).toBeVisible()
  await expect(page.getByLabel('Password')).toHaveCount(0)
  await expect(page.getByLabel('Email')).toHaveCount(0)

  // And no sign-in VOCABULARY either, in any state the button passes through.
  // There is no account here and nothing to sign in to, so the word is a lie
  // about what is happening, and it survived several passes at removing it
  // because a busy label is easy to forget.
  await expect(page.locator('body')).not.toContainText(/sign(ing)? in/i)
})

test('pressing Continue lands on setup', async () => {
  const button = page.getByRole('button', { name: 'Continue' })
  await button.click()
  // The busy state included. This is where "Signing in" kept coming back.
  await expect(page.locator('body')).not.toContainText(/sign(ing)? in/i)

  // Straight to the one thing there is to do. A dashboard first would be a
  // screen about an installation that cannot answer anything yet.
  await expect(page.getByRole('heading', { name: 'Set up the assistant' })).toBeVisible()
})

test('it asks what to call you, and remembers it', async ({ request }) => {
  await page.getByPlaceholder('What should the assistant call you?').fill('Sam')
  await page.getByRole('button', { name: 'Continue' }).click()

  // Not a preference kept in a browser: it is the users row, the only one there
  // is here, so nothing about identity is special-cased.
  const token = (await (await request.post('/v1/auth/local')).json()).access_token as string
  const me = await (await request.get('/v1/auth/me', {
    headers: { Authorization: `Bearer ${token}` },
  })).json()
  expect(me.user.name).toBe('Sam')
})

test('there is no user administration on a personal installation', async () => {
  // Not hidden by permissions, which could not hide it: the one person holds
  // all of them. The whole group is absent.
  for (const gone of ['Users', 'Groups', 'Roles', 'Workspaces']) {
    await expect(page.getByRole('link', { name: gone })).toHaveCount(0)
  }
})

test('the chat refuses to pretend it can answer, and offers the way out', async () => {
  await page.goto('/chat/')

  await expect(page.getByText('Nothing can answer yet')).toBeVisible()
  // A link now, not a button asking the gateway to launch a second application.
  const toConsole = page.getByRole('link', { name: 'Open the console' })
  await expect(toConsole).toBeVisible()
  // Its own address, so arriving here asks for setup and not for a dashboard
  // that is drawn and then thrown away. That flash was visible.
  await expect(toConsole).toHaveAttribute('href', '/setup')
  // A composer that types into nothing is worse than no composer.
  await expect(page.getByPlaceholder('How can I help you today?')).toHaveCount(0)
})

test('setup takes a provider and a model, and then it is done', async () => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Set up the assistant' })).toBeVisible()

  await page.getByRole('combobox').selectOption({ label: 'Anthropic' })
  await page.getByPlaceholder('API key').fill('sk-e2e-not-a-real-key')
  await page.getByRole('button', { name: 'Add provider' }).click()

  const addModel = page.getByRole('button', { name: 'Add model' })
  await expect(addModel).toBeVisible({ timeout: 30_000 })

  // A key this fake cannot ask the provider what it publishes, so the typing
  // fallback is the expected path. What must never happen is neither a list nor
  // a box, which would be a dead end.
  const typed = page.getByPlaceholder(/Model name/)
  if (await typed.count()) {
    await typed.fill('claude-opus-4-8')
  } else {
    await expect(page.getByRole('combobox')).toBeVisible()
  }
  // Whether it thinks before answering belongs here, with the model, because
  // it is a property of how the Gateway thinks rather than a separate step.
  await page.getByRole('checkbox').check()
  await addModel.click()

  // Setup FINISHES. Asserting the button came back was wrong: on the happy path
  // the screen is done with and unmounts, so a re-enabled button only means
  // anything when the step FAILED, which is covered in Setup.test.tsx.
  await expect(page.getByRole('heading', { name: 'Set up the assistant' })).toHaveCount(0, {
    timeout: 30_000,
  })
})

test('finishing setup at /setup goes to the chat', async () => {
  // The address that exists to show setup must not go on showing it once there
  // is nothing left to set up. It did: three ticks and no way forward.
  await page.goto('/setup')
  await expect(page).toHaveURL(/\/chat\//, { timeout: 15_000 })
})

test('the installation reports itself ready, with a memory of its own', async ({ request }) => {
  const token = (await (await request.post('/v1/auth/local')).json()).access_token as string
  const headers = { Authorization: `Bearer ${token}` }

  const state = await (await request.get('/v1/setup', { headers })).json()
  // A model exists AND the Gateway points at it. Creating one with nothing
  // pointing at it is the half-finished state that spun forever.
  expect(state.has_vendor).toBe(true)
  expect(state.has_model).toBe(true)
  expect(state.ready).toBe(true)
  expect(state.gateway_model).toBeTruthy()

  // Nobody was asked whether they would like their assistant to remember
  // things, because that is not a question with two reasonable answers.
  const brains = await (await request.get('/v1/brains', { headers })).json()
  const list = brains.brains ?? brains
  const memory = list.find((b: { slug: string }) => b.slug === 'working-memory')
  expect(memory).toBeTruthy()

  // Asked of the agent's own record rather than the Gateway SCREEN: that
  // endpoint is shaped like the screen and carries what the screen draws, which
  // does not include which brain the agent remembers into.
  const screen = await (await request.get('/v1/gateway', { headers })).json()
  const form = await (await request.get(`/v1/agents/${screen.gateway.id}/form`, { headers })).json()
  expect(form.agent.memory_brain_id).toBe(memory.id)
  expect(form.agent.brains).toContain(memory.id)
  // And the box that was ticked reached the agent.
  expect(form.agent.reasoning).toBe(true)
})

test('the chat comes alive without being restarted', async () => {
  await page.goto('/chat/')
  // It re-asks while it waits, so finishing setup brings this window to life on
  // its own rather than needing to be reopened.
  await expect(page.getByText('Nothing can answer yet')).toHaveCount(0, { timeout: 20_000 })
})

test('the console shows a name and nothing to sign out of', async () => {
  await page.goto('/')
  // No address: an account nobody made, at a domain that does not resolve.
  await expect(page.locator('aside')).not.toContainText('@')
  // And no way out of something there is no way into.
  await expect(page.getByRole('button', { name: 'Sign out' })).toHaveCount(0)
  // The name is there, and renaming is a pencil rather than a screen.
  await expect(page.locator('aside')).toContainText('Sam')
  await expect(page.getByRole('button', { name: 'Change your name' })).toHaveCount(1)
})

test('the console and the chat reach each other', async () => {
  // Starting from the chat, explicitly. A previous spec leaves the page
  // wherever it finished, and this one waited a full minute for a link that
  // only exists on the other surface.
  await page.goto('/chat/')
  // From the chat to the console: the foot of the sidebar, where an identity
  // and a way out would be on a deployment. There is neither here.
  await page.getByRole('link', { name: 'Console' }).click()
  await expect(page).toHaveURL(/127\.0\.0\.1:\d+\/$/)

  // And back: the first item in the menu, because on a personal installation
  // the chat is what the application is FOR.
  await page.getByRole('link', { name: 'Chat' }).click()
  await expect(page).toHaveURL(/\/chat\//)
})
