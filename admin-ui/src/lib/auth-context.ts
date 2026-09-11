import { createContext } from 'react'

import type { AuthState } from '@/lib/auth'

/**
 * The auth context, in a module of its own.
 *
 * It lives apart from `auth.tsx` so that a test mocking `@/lib/auth` (to give a
 * screen a fixed `useAuth`) does not also have to reproduce the context that the
 * shared test wrapper provides. Two different reasons to reach for auth, so two
 * modules, and neither one breaks the other.
 */
export const AuthContext = createContext<AuthState | null>(null)
