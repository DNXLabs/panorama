// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, useCallback, useContext, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { get, onSignedOut, post, setCsrfToken } from './api'
import type { AuthState, Session } from './types'

// Who is signed in, and what a visitor who is not should be offered.
//
// Resolved once, at the top of the tree, so no screen has to reason about the
// unauthenticated case: either the console renders, or the sign-in screen does.

type SessionValue = {
  session: Session | null
  auth: AuthState | null
  loading: boolean
  reload: () => Promise<void>
  signOut: () => Promise<void>
}

const SessionContext = createContext<SessionValue | null>(null)

export function SessionProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<Session | null>(null)
  const [auth, setAuth] = useState<AuthState | null>(null)
  const [loading, setLoading] = useState(true)

  const reload = useCallback(async () => {
    // The auth state first, and always: it is what tells a visitor whether to
    // expect an SSO button or a first-run code, and it answers for an
    // unauthenticated caller.
    try {
      setAuth(await get<AuthState>('/api/v1/auth/state', true))
    } catch {
      setAuth(null)
    }

    try {
      // quiet: a 401 here is the answer to "is anyone signed in", not an event
      // that should bounce a console that is already on the sign-in screen.
      const current = await get<Session>('/api/v1/auth/session', true)
      setCsrfToken(current.csrf_token ?? '')
      setSession(current)
    } catch {
      setCsrfToken('')
      setSession(null)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void reload()
  }, [reload])

  useEffect(
    () =>
      onSignedOut(() => {
        setCsrfToken('')
        setSession(null)
      }),
    [],
  )

  const signOut = useCallback(async () => {
    try {
      await post('/api/v1/auth/signout')
    } finally {
      setCsrfToken('')
      setSession(null)
      await reload()
    }
  }, [reload])

  return (
    <SessionContext.Provider value={{ session, auth, loading, reload, signOut }}>
      {children}
    </SessionContext.Provider>
  )
}

export function useSession(): SessionValue {
  const value = useContext(SessionContext)
  if (!value) throw new Error('useSession outside a SessionProvider')
  return value
}

/** useMe returns the signed-in user, and throws if called outside the console. */
export function useMe() {
  const { session } = useSession()
  if (!session) throw new Error('useMe outside an authenticated view')
  return session.user
}
