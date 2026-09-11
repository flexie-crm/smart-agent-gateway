import type { ReactElement, ReactNode } from 'react'
import { render as rtlRender } from '@testing-library/react'
import type { RenderOptions } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import type { AuthState } from '@/lib/auth'
import { SERVER_POSTURE } from '@/lib/api'
import { AuthContext } from '@/lib/auth-context'
import { NotifyProvider } from '@/lib/notify'

/**
 * The render every component test uses.
 *
 * A screen in this console lives inside the providers the app mounts around it;
 * a test that renders one without them is testing a component that cannot exist.
 * So the custom render wraps the tree in exactly those providers, and the tests
 * import `render` from here instead of from the library. Toasts and confirms go
 * through NotifyProvider, so a delete that asks a question, or a save that
 * reports a failure, has somewhere to say it. A router is here for the same
 * reason: the app mounts one, so a screen carrying a link to another screen is
 * an ordinary screen, and without it every one of its tests fails on the link
 * rather than on anything the test is about.
 *
 * Auth is provided as a VALUE rather than by mounting the real provider, which
 * fetches on load: a test asserting what a screen draws should not need a
 * bootstrap round trip to render a button. It answers yes to every permission,
 * so a test sees the whole screen; a test about what a permission hides says so
 * by rendering its own provider.
 */
const SIGNED_IN: AuthState = {
  identity: { id: 1, email: 'test@test', name: 'Test' },
  permissions: ['*'],
  workspace: null,
  workspaces: [],
  loading: false,
  can: () => true,
  signIn: async () => {},
  signInLocally: async () => {},
  refresh: async () => {},
  // A deployment, because that is the posture with the most on screen: a test
  // about a desktop's narrower console renders its own provider and says so.
  dev: false,
  posture: SERVER_POSTURE,
  signOut: async () => {},
  switchWorkspace: async () => {},
}

function Providers({ children, path }: { children: ReactNode; path?: string }) {
  return (
    <MemoryRouter initialEntries={[path ?? '/']}>
      <AuthContext.Provider value={SIGNED_IN}>
        <NotifyProvider>{children}</NotifyProvider>
      </AuthContext.Provider>
    </MemoryRouter>
  )
}

/**
 * `path` starts the router somewhere other than the root.
 *
 * Most screens are reached by clicking, and a test that clicks is testing more.
 * Some are reached by an address alone (a link in the menu, a bookmark), and
 * those cannot be got at from here otherwise: nesting a second router inside
 * this one is not allowed by react-router, so it has to be this one that knows.
 */
function render(
  ui: ReactElement,
  options?: Omit<RenderOptions, 'wrapper'> & { path?: string },
) {
  const { path, ...rest } = options ?? {}
  return rtlRender(ui, {
    wrapper: ({ children }) => <Providers path={path}>{children}</Providers>,
    ...rest,
  })
}

export * from '@testing-library/react'
export { render }
