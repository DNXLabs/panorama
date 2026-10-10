// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'

// A router, in fifty lines.
//
// The console has ten screens and no nested layouts, no route parameters worth
// parsing, and no data loaders. A routing library would be the largest
// dependency in the bundle and would earn none of it. This is the History API
// with a subscription, which is what the library would be wrapping.

type RouterValue = {
  path: string
  search: URLSearchParams
  navigate: (to: string, options?: { replace?: boolean }) => void
  /** setParam updates one query parameter in place, so a filter survives a
   *  reload and can be linked to. */
  setParam: (name: string, value: string | null) => void
  /** setParams updates several at once. The audit trail's four search boxes
   *  commit together when typing stops, and four sequential setParam calls
   *  would be four history entries and four re-renders of a table. */
  setParams: (next: Record<string, string | null>) => void
}

const RouterContext = createContext<RouterValue | null>(null)

export function Router({ children }: { children: ReactNode }) {
  const [location, setLocation] = useState(() => ({
    path: window.location.pathname,
    search: window.location.search,
  }))

  useEffect(() => {
    const onPop = () =>
      setLocation({ path: window.location.pathname, search: window.location.search })
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  const navigate = useCallback((to: string, options: { replace?: boolean } = {}) => {
    const url = new URL(to, window.location.origin)
    if (url.origin !== window.location.origin) return
    if (options.replace) {
      window.history.replaceState(null, '', url)
    } else {
      window.history.pushState(null, '', url)
    }
    setLocation({ path: url.pathname, search: url.search })
    window.scrollTo(0, 0)
  }, [])

  const search = useMemo(() => new URLSearchParams(location.search), [location.search])

  const setParams = useCallback(
    (changes: Record<string, string | null>) => {
      const next = new URLSearchParams(window.location.search)
      for (const [name, value] of Object.entries(changes)) {
        if (value === null || value === '') {
          next.delete(name)
        } else {
          next.set(name, value)
        }
      }
      const encoded = next.toString()
      navigate(window.location.pathname + (encoded ? `?${encoded}` : ''), { replace: true })
    },
    [navigate],
  )

  const setParam = useCallback(
    (name: string, value: string | null) => setParams({ [name]: value }),
    [setParams],
  )

  const value = useMemo<RouterValue>(
    () => ({ path: location.path, search, navigate, setParam, setParams }),
    [location.path, search, navigate, setParam, setParams],
  )

  return <RouterContext.Provider value={value}>{children}</RouterContext.Provider>
}

export function useRouter(): RouterValue {
  const value = useContext(RouterContext)
  if (!value) throw new Error('useRouter outside a Router')
  return value
}

/** Link renders an anchor that navigates without a page load.
 *
 *  It stays a real anchor with a real href, so middle-click, copy-link, and
 *  open-in-new-tab all work - which is most of why people use a browser.
 */
export function Link({
  to,
  children,
  className,
  onClick,
}: {
  to: string
  children: ReactNode
  className?: string
  onClick?: () => void
}) {
  const { navigate } = useRouter()
  return (
    <a
      href={to}
      className={className}
      onClick={(event) => {
        if (event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0) return
        event.preventDefault()
        onClick?.()
        navigate(to)
      }}
    >
      {children}
    </a>
  )
}
