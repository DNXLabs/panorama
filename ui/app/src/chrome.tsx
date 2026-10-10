// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'

// What a screen contributes to the shell's header.
//
// The header belongs to the shell - every screen gets the same kicker, the same
// 26px title in the same place, and a screen cannot forget to draw one. But two
// screens own a control that has to live up there: the range picker decides
// what every figure below it means, so putting it inside the first band would
// have it scroll away from the numbers it governs, and putting a second copy in
// each screen would give the console two headers that drift.
//
// So a screen hands the shell a sub-line and some actions, and the shell draws
// them. It is a context rather than a prop because `screens.tsx` renders the
// element and the shell does not know which screen it is holding - and it is
// its own module rather than App's so that a screen importing this does not
// import the thing that renders it.
//
// SET FROM AN EFFECT, CLEARED ON UNMOUNT. A screen that navigates away must not
// leave its Export CSV button sitting over the next one, and the clean-up is
// what guarantees that rather than each screen remembering to.

type Chrome = {
  sub?: ReactNode
  actions?: ReactNode
  /** The screen closes the header itself - Analytics does, with its tab bar,
   *  which is a rule with words on it. Without this the header draws its own
   *  and the two hairlines sit two pixels apart. */
  flush?: boolean
}

type ChromeValue = Chrome & { set: (next: Chrome) => void }

const ChromeContext = createContext<ChromeValue | null>(null)

export function ChromeProvider({ children }: { children: ReactNode }) {
  const [chrome, setChrome] = useState<Chrome>({})
  const value = useMemo<ChromeValue>(() => ({ ...chrome, set: setChrome }), [chrome])
  return (
    <ChromeContext.Provider value={value}>
      <RefreshProvider>{children}</RefreshProvider>
    </ChromeContext.Provider>
  )
}

// Asking the screen's questions again, from the header.
//
// A screen reading traffic is out of date the moment a request completes, and
// the only way to see the next one was a browser reload, which drops whatever
// the reader had open. So the shell holds one number, and `useApi` puts it in
// its dependencies: bumping it re-asks every question the mounted screen asked,
// including those of a tab's own table, without each screen listing its
// fetches. A reload keeps what is on screen until the answer arrives, which is
// `useApi`'s rule and not this one's.
//
// `refresh` settles when the fetches it caused have, and fails when any of
// them did, so the button reports the outcome of the questions and not of the
// click. Each failing card still shows its own error; this does not replace it.

type RefreshValue = {
  epoch: number
  refresh: () => Promise<void>
  /** useApi hands every fetch it starts to this, so `refresh` can wait on them. */
  track: (fetch: Promise<unknown>) => void
}

const RefreshContext = createContext<RefreshValue | null>(null)

function RefreshProvider({ children }: { children: ReactNode }) {
  const [epoch, setEpoch] = useState(0)
  const started = useRef<Promise<unknown>[]>([])

  const track = useCallback((fetch: Promise<unknown>) => {
    started.current.push(fetch)
  }, [])

  const refresh = useCallback(async () => {
    started.current = []
    setEpoch((n) => n + 1)
    // The fetches start in effects after the commit, not in this call. A task
    // boundary is after both.
    await new Promise((resolve) => window.setTimeout(resolve, 0))
    const outcomes = await Promise.allSettled(started.current)
    const failed = outcomes.find((o): o is PromiseRejectedResult => o.status === 'rejected')
    if (failed) throw failed.reason
  }, [])

  const value = useMemo(() => ({ epoch, refresh, track }), [epoch, refresh, track])
  return <RefreshContext.Provider value={value}>{children}</RefreshContext.Provider>
}

/** useRefresh is what `useApi` and the Refresh button read. Outside the shell
 *  it is null: the sign-in gate and the setup console have nothing to refresh. */
export function useRefresh(): RefreshValue | null {
  return useContext(RefreshContext)
}

/** useChrome is what the shell reads. Outside a provider it is simply empty,
 *  because the sign-in gate and the setup console have no header to fill. */
export function useChrome(): Chrome {
  return useContext(ChromeContext) ?? {}
}

/**
 * usePageChrome puts a screen's sub-line and controls in the shell's header.
 *
 * `deps` is the caller's, for the same reason `useApi` takes one: the nodes are
 * rebuilt on every render, so comparing them would re-set the header forever.
 * Pass what the controls actually depend on - the selected range, a count.
 */
export function usePageChrome(chrome: Chrome, deps: unknown[] = []) {
  const context = useContext(ChromeContext)
  const set = context?.set

  useEffect(() => {
    if (!set) return
    set(chrome)
    return () => set({})
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [set, ...deps])
}
