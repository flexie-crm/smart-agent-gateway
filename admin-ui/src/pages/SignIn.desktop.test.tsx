import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { SignIn } from './SignIn'
import { AuthContext } from '@/lib/auth-context'
import type { AuthState } from '@/lib/auth'
import { SERVER_POSTURE } from '@/lib/api'

/**
 * One build of this console serves a deployment and a desktop, and the sign-in
 * is where the difference is most visible. Getting it backwards either asks one
 * person for a password they were never given, or offers a deployment a button
 * that cannot work.
 */
function renderWith(state: Partial<AuthState>) {
  const value = {
    identity: null,
    permissions: [],
    workspace: null,
    workspaces: [],
    loading: false,
    can: () => true,
    signIn: async () => {},
    signInLocally: async () => {},
    signOut: async () => {},
    switchWorkspace: async () => {},
    posture: SERVER_POSTURE,
    ...state,
  } as AuthState
  return render(
    <MemoryRouter>
      <AuthContext.Provider value={value}>
        <SignIn />
      </AuthContext.Provider>
    </MemoryRouter>,
  )
}

describe('SignIn', () => {
  it('asks a deployment for a password', () => {
    renderWith({})
    expect(screen.getByLabelText('Email')).toBeInTheDocument()
    expect(screen.getByLabelText('Password')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Continue' })).not.toBeInTheDocument()
  })

  it('offers one person a button and asks them nothing', () => {
    renderWith({ posture: { ...SERVER_POSTURE, local_sign_in: true } })
    expect(screen.getByRole('button', { name: 'Continue' })).toBeInTheDocument()
    // The fields are gone rather than hidden: a password box on a copy that
    // never issued a password is a question with no answer.
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Email')).not.toBeInTheDocument()
  })

  it('signs in locally when the button is pressed', async () => {
    const signInLocally = vi.fn().mockResolvedValue(undefined)
    renderWith({ posture: { ...SERVER_POSTURE, local_sign_in: true }, signInLocally })
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(signInLocally).toHaveBeenCalledOnce()
  })

  it('says so when the local sign-in is refused', async () => {
    const signInLocally = vi.fn().mockRejectedValue(new Error('this sign-in only answers the computer it is running on'))
    renderWith({ posture: { ...SERVER_POSTURE, local_sign_in: true }, signInLocally })
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }))
    // A button that fails silently is the failure people describe as "I clicked
    // it and nothing happened".
    expect(await screen.findByRole('alert')).toHaveTextContent('only answers the computer')
  })
})
