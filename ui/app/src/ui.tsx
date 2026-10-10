// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import type { ReactNode, RefObject } from 'react'
import { ApiError, RANGES, get } from './api'
import type { Range } from './api'
import { useRefresh } from './chrome'
import { Icon } from './icons'
import { money, percent, ratio } from './format'
import type { Cost, Totals } from './types'

// The pieces every screen is built from.
//
// Small and unopinionated on purpose: the design lives in styles.css and the
// tokens behind it, so a component here decides structure and never a colour.

/* ------------------------------------------------------------------ motion --
 *
 * Four hooks below make the console's motion honest. The rule they share:
 * every one of them checks whether the reader asked for less motion, and none
 * of them is the only thing carrying a piece of information.
 *
 * The CSS reduced-motion block in design-tokens.css collapses animations and
 * transitions, which covers everything expressed in CSS. It cannot reach a
 * number being interpolated in JavaScript, so that is checked here instead.
 */

/** prefersReducedMotion reads the media query at the moment it is asked.
 *
 *  Not cached: a reader can change the setting without reloading the console,
 *  and a cached answer would keep animating at them until they did. */
function prefersReducedMotion(): boolean {
  return window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false
}

/**
 * useMounted returns false for the first paint and true immediately after.
 *
 * This is what makes the bar and chart transitions in styles.css actually run.
 * A CSS transition fires on a *change* of value, and an element that mounts at
 * its final width has never changed - so without this the transitions the mock
 * specifies would be dead code on every screen. Rendering zero first and
 * the real value one frame later gives them the change they need.
 *
 * Two frames rather than one: the first schedules the paint at zero, the second
 * runs after it has actually happened. With a single frame the browser is free
 * to coalesce both values into one style recalculation, which lands at the
 * final width with no transition - the same bug, one frame later.
 */
export function useMounted(): boolean {
  const [mounted, setMounted] = useState(false)
  useEffect(() => {
    if (prefersReducedMotion()) {
      setMounted(true)
      return
    }
    let second = 0
    const first = requestAnimationFrame(() => {
      second = requestAnimationFrame(() => setMounted(true))
    })
    return () => {
      cancelAnimationFrame(first)
      cancelAnimationFrame(second)
    }
  }, [])
  return mounted
}

/**
 * useExit plays an overlay's exit animation after React has already removed it.
 *
 * Every overlay in this console arrived over about a fifth of a second and then
 * stopped existing between two frames. That is not a fast dismissal, it is a
 * discontinuity: the panel that was being read is simply not where it was, and
 * the eye reports that as a fault rather than as a closing.
 *
 * **Why this works on the DOM rather than on a prop.** `Drawer` and `Modal`
 * both take `open`, and every one of the nine call sites passes it bare - the
 * prop has never once been false. Presence is decided one level up, by the
 * parent rendering `{person && <PersonDrawer …/>}`, and it has to be: a drawer
 * is keyed on its subject so it never opens holding the last person's staged
 * edits, and its props require a subject that no longer exists once the panel
 * is closed. So there is no boolean to watch. What actually happens is that
 * React deletes the node, and the exit has to be played on the node after that.
 *
 * The cleanup re-attaches the real element - not a clone, so a half-typed field
 * still reads as it did - marks it `data-closing`, and removes it when the
 * animation ends.
 *
 * Four things about it are load-bearing.
 *
 * **`isConnected` is what tells a dismissal from StrictMode.** In development
 * React mounts, tears down and remounts every effect to prove it is
 * re-entrant, and without this check every drawer would play its own exit
 * animation the instant it opened. A simulated remount leaves the node in the
 * document; a real deletion detaches it first. That is the only difference, and
 * it is exactly the question being asked.
 *
 * **It asks the animation how long it is rather than being told.** `--dur-exit`
 * lives in the token file and nowhere else; a constant here would be a second
 * copy of it, and the day somebody tuned one the overlay would either vanish
 * mid-exit or sit invisible for the difference. `getAnimations()` flushes style
 * and reports what the browser just resolved, so the number is read back out of
 * the CSS that owns it. Reduced motion needs no special case for the same
 * reason: the global block collapses the duration, and what is read back is the
 * collapsed one.
 *
 * **It does not wait for `animationend`, which is a trap.** That event is only dispatched during a rendering update, so in a tab
 * that is not painting - backgrounded, occluded, in another window - the
 * animation runs to completion on the document timeline and the event never
 * arrives. Measured here: the panel reached `playState: "finished"` while no
 * event had fired and none ever did. Waiting on it would leave an orphan on
 * `document.body` in every backgrounded tab until a fallback timer noticed. A
 * timer keyed to the duration the browser itself reports has neither problem.
 *
 * **`animates` names the element that carries the exit, not the layer.** A
 * drawer contains things that animate - the check mark drawing itself in a
 * footer, the indeterminate sweep under a pending button, which never ends at
 * all - so the duration is read from the panel rather than from whatever the
 * subtree happens to be doing.
 *
 * No animation resolved means no exit to wait for, and the element goes at
 * once. A hard cut is the right thing to degrade to.
 */
function useExit<T extends HTMLElement>(animates?: string): RefObject<T | null> {
  const layer = useRef<T>(null)

  useEffect(() => {
    const node = layer.current
    if (!node) return

    return () => {
      if (node.isConnected) return

      const panel = animates ? node.querySelector<HTMLElement>(animates) : node
      if (!panel) return

      // Re-attached to the body rather than to where it was: every one of
      // these is `position: fixed`, so it lands in exactly the same place, and
      // the element it came out of is being torn down.
      document.body.append(node)
      // Set before the browser resolves style, so the first frame is already
      // the exit rather than a restart of the entrance.
      node.setAttribute('data-closing', '')
      node.setAttribute('inert', '')

      // Infinite ones are skipped rather than clamped: a sweep that never ends
      // is not this element's exit, and taking its duration would keep the
      // panel on screen forever.
      const exit = panel.getAnimations().reduce((longest, animation) => {
        const active = Number(animation.effect?.getComputedTiming().activeDuration ?? 0)
        return Number.isFinite(active) ? Math.max(longest, active) : longest
      }, 0)

      if (exit === 0) {
        node.remove()
        return
      }
      window.setTimeout(() => node.remove(), exit)
    }
  }, [animates])

  return layer
}

/**
 * useCountUp animates a figure from zero to its value on mount.
 *
 * The design tokens call this "the one flourish", and it is the only motion in
 * the console that touches a number. Two things keep it from being a lie:
 *
 * The interpolation runs on the integer the server sent - nanodollars, request
 * counts - and the caller formats the result. Interpolating the *formatted*
 * string would mean parsing "$1,284.02" back into a float and rounding it
 * again, which is exactly the arithmetic `format.ts` exists to prevent.
 *
 * It lands on the exact value. A ratio-driven animation that stops when time
 * runs out can finish a pixel short and leave a total reading $1,284.01 for the
 * rest of the session, so the final frame is assigned rather than computed.
 */
export function useCountUp(value: number | null | undefined): number | null {
  const [shown, setShown] = useState<number | null>(value ?? null)
  const from = useRef(0)

  useEffect(() => {
    if (value === null || value === undefined) {
      setShown(null)
      return
    }
    if (prefersReducedMotion()) {
      setShown(value)
      return
    }

    const start = performance.now()
    const origin = from.current
    const span = value - origin
    // A figure that has not moved is not worth animating, and a re-render that
    // returns the same total should not replay the count.
    if (span === 0) {
      setShown(value)
      return
    }

    const duration = 1400 // --dur-countup
    let frame = requestAnimationFrame(function step(now: number) {
      const t = Math.min(1, (now - start) / duration)
      // ease-out-cubic: fast enough to read the magnitude immediately, slow
      // enough at the end that the last digits settle rather than snap.
      const eased = 1 - Math.pow(1 - t, 3)
      if (t >= 1) {
        setShown(value)
        from.current = value
        return
      }
      setShown(Math.round(origin + span * eased))
      frame = requestAnimationFrame(step)
    })
    return () => cancelAnimationFrame(frame)
  }, [value])

  return shown
}

/* ------------------------------------------------------------------ action --
 *
 * Every mutation in the console goes through this, so a person who saves a
 * setting and a person who applies a policy are told the same thing, in the
 * same words and on the same timings, about the same kind of outcome. A
 * hand-rolled `busy` boolean per screen is how those drift apart.
 */

export type ActionState = 'idle' | 'pending' | 'settled' | 'failed'

/** How long a pending state is held before it is allowed to resolve, and how
 *  long the result stays on the button. Timers rather than animations, so they
 *  live here rather than in the token file - nothing in CSS reads them, and a
 *  second copy over there would be a second thing to keep in step. */
const PENDING_MIN = 380
const SETTLED_FOR = 1900

/**
 * useAction runs one mutation and owns what the reader is told about it.
 *
 * The minimum pending duration is the part worth explaining. Against a local
 * SQLite deployment a save resolves in single-digit milliseconds - faster than
 * a person can perceive a change - so a button that flips straight to "Saved"
 * reads as a button that did nothing. Holding the pending state to 380ms is not
 * a fake delay to look busy; it is what makes the transition legible at all.
 * Beyond that the button tracks the real request exactly, however long it takes.
 */
export function useAction(run: () => Promise<unknown>): {
  state: ActionState
  error: unknown
  act: () => void
  reset: () => void
} {
  const [state, setState] = useState<ActionState>('idle')
  const [error, setError] = useState<unknown>(null)
  const timer = useRef<number | undefined>(undefined)
  const live = useRef(true)

  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
      window.clearTimeout(timer.current)
    }
  }, [])

  const act = useCallback(() => {
    // A second click while the first is in flight would run the mutation twice.
    if (state === 'pending') return
    window.clearTimeout(timer.current)
    setState('pending')
    setError(null)

    const started = performance.now()
    const settle = (next: ActionState, cause?: unknown) => {
      const elapsed = performance.now() - started
      const hold = Math.max(0, PENDING_MIN - elapsed)
      timer.current = window.setTimeout(() => {
        if (!live.current) return
        setState(next)
        if (cause !== undefined) setError(cause)
        // A failure stays until the next attempt: an error that clears itself
        // after two seconds is one the reader can miss entirely.
        if (next === 'settled') {
          timer.current = window.setTimeout(() => {
            if (live.current) setState('idle')
          }, SETTLED_FOR)
        }
      }, hold)
    }

    run().then(
      () => settle('settled'),
      (cause: unknown) => settle('failed', cause),
    )
  }, [run, state])

  const reset = useCallback(() => {
    window.clearTimeout(timer.current)
    setState('idle')
    setError(null)
  }, [])

  return { state, error, act, reset }
}

/**
 * useKeyedAction is useAction for a list, where the row is the unit.
 *
 * A table of people has one control per row and no button to hang state on, so
 * the state is keyed by row id. Same timings as useAction - a role change and a
 * settings save should take the same amount of time to tell you they worked,
 * because from where the reader sits they are the same event.
 */
export function useKeyedAction<A>(run: (arg: A) => Promise<unknown>): {
  stateOf: (key: string) => ActionState
  act: (key: string, arg: A) => void
  busy: boolean
} {
  const [states, setStates] = useState<Record<string, ActionState>>({})
  const timers = useRef<Record<string, number>>({})
  const live = useRef(true)

  useEffect(() => {
    const pending = timers.current
    live.current = true
    return () => {
      live.current = false
      Object.values(pending).forEach((id) => window.clearTimeout(id))
    }
  }, [])

  const set = useCallback((key: string, state: ActionState | null) => {
    setStates((current) => {
      if (state === null) {
        const { [key]: _removed, ...rest } = current
        return rest
      }
      return { ...current, [key]: state }
    })
  }, [])

  const act = useCallback(
    (key: string, arg: A) => {
      window.clearTimeout(timers.current[key])
      set(key, 'pending')
      const started = performance.now()

      const settle = (next: ActionState) => {
        const hold = Math.max(0, PENDING_MIN - (performance.now() - started))
        timers.current[key] = window.setTimeout(() => {
          if (!live.current) return
          set(key, next)
          if (next === 'settled') {
            timers.current[key] = window.setTimeout(() => {
              if (live.current) set(key, null)
            }, SETTLED_FOR)
          }
        }, hold)
      }

      run(arg).then(
        () => settle('settled'),
        () => settle('failed'),
      )
    },
    [run, set],
  )

  return {
    stateOf: (key) => states[key] ?? 'idle',
    act,
    busy: Object.values(states).some((s) => s === 'pending'),
  }
}

/** The check mark that draws itself when an action settles. */
function CheckMark() {
  return (
    <svg className="btn-check" viewBox="0 0 16 16" width="13" height="13" aria-hidden focusable="false">
      <path d="M3.5 8.5 L6.5 11.5 L12.5 4.5" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

/**
 * The mark a failed action carries.
 *
 * It exists because the failed state was otherwise nothing but a change of hue,
 * and a button that says "Save" in amber and a button that says "Save" in lime
 * are the same button to a reader who cannot separate the two colours. The
 * shape is the signal; the colour agrees with it.
 */
function FailMark() {
  return (
    <svg className="btn-fail" viewBox="0 0 16 16" width="13" height="13" aria-hidden focusable="false">
      <path d="M8 3.5 L8 9" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
      <circle cx="8" cy="12.2" r="1.05" fill="currentColor" />
    </svg>
  )
}

/**
 * The three labels an action carries are one verb in three tenses - Save,
 * Saving…, Saved. That is the rule the whole console follows: the word a button
 * is pressed under is the word it reports back with. A button that says "Apply"
 * and confirms with "Success" leaves the reader to do the matching.
 */
type ActionLabels = {
  label: string
  /** Past tense, said beside the control once it has happened: Saved, Applied,
   *  Minted. There is no present-participle label - the button keeps its verb
   *  through the whole lifecycle rather than narrating itself. */
  settled?: string
}

type ActionLook = {
  primary?: boolean
  small?: boolean
  /** The only place the danger colour appears: deactivating an account,
   *  deleting a team. It is a look rather than a confirmation step because the
   *  action reports what it did and both are reversible by an administrator -
   *  what it must never be is indistinguishable from Save. */
  danger?: boolean
  disabled?: boolean
  title?: string
}

/**
 * The button's own face never changes, and that is the point.
 *
 * The label is the verb and stays the verb - a button that reads "Save", then
 * "Saving…", then "Saved" changes width three times per press, which moves it
 * under the pointer and shoves whatever sits beside it. So the button carries
 * the sweep and nothing else, and the outcome is reported next to it by
 * ActionSlot.
 */
function actionFace(state: ActionState, { label }: ActionLabels) {
  return (
    <>
      <span>{label}</span>
      {state === 'pending' && <span className="btn-sweep" aria-hidden />}
    </>
  )
}

/**
 * ActionSlot is where an action's outcome is said, beside the control.
 *
 * It reserves its width from the moment it renders, using an invisible copy of
 * the longest thing it will ever hold. Without that the mark's arrival would
 * push a right-aligned button leftwards - which is the same layout shift the
 * stable label exists to avoid, moved one element along.
 */
function ActionSlot({
  state,
  settled = 'saved',
  failed = 'failed',
}: {
  state: ActionState
  settled?: string
  failed?: string
}) {
  const widest = settled.length >= failed.length ? settled : failed
  return (
    <span className="action-slot">
      <span className="action-slot-ghost" aria-hidden>
        <CheckMark />
        <span>{widest}</span>
      </span>
      <span className={`action-slot-live ${state}`}>
        {state === 'pending' && <span className="action-mark-dot" aria-hidden />}
        {state === 'settled' && (
          <>
            <CheckMark />
            <span>{settled}</span>
          </>
        )}
        {state === 'failed' && (
          <>
            <FailMark />
            <span>{failed}</span>
          </>
        )}
      </span>
    </span>
  )
}

/**
 * The announcement, and why it is the button's sibling rather than its child.
 *
 * The visible label changes on its own, but a change to the accessible name of
 * the element that still holds focus is not reliably re-announced, so the
 * outcome needs a live region. Putting that region *inside* the button folds it
 * into the button's accessible name, and the button starts introducing itself
 * as "Open a setup session, Open a setup session failed". Outside, the name
 * stays the verb and the outcome is announced once.
 *
 * It is absolutely positioned, so it takes no space in the flex row it lands in.
 */
function ActionAnnouncement({ state, label, settled }: { state: ActionState } & ActionLabels) {
  return (
    <span className="sr-only" role="status" aria-live="polite">
      {state === 'settled' ? (settled ?? label) : state === 'failed' ? `${label} failed` : ''}
    </span>
  )
}

function actionClass({ primary, small, danger }: ActionLook) {
  return ['btn', primary ? 'primary' : '', small ? 'small' : '', danger ? 'danger' : '']
    .filter(Boolean)
    .join(' ')
}

/** ActionButton owns its own mutation. The common case: a button that does one
 *  thing when clicked. */
export function ActionButton({
  onAct,
  ...rest
}: ActionLabels & ActionLook & { onAct: () => Promise<unknown> }) {
  const { state, act } = useAction(onAct)
  return (
    <span className="action">
      <button
        type="button"
        className={actionClass(rest)}
        data-state={state}
        onClick={act}
        disabled={rest.disabled || state === 'pending'}
        aria-busy={state === 'pending'}
        title={rest.title}
      >
        {actionFace(state, rest)}
      </button>
      <ActionSlot state={state} settled={rest.settled ?? 'done'} />
      <ActionAnnouncement state={state} {...rest} />
    </span>
  )
}

/**
 * ActionMark is the acknowledgement for a control that is not a button.
 *
 * A `<select>` that saves on change has nowhere to put a label, so the same
 * three states are said beside it instead - and said in words, not only in
 * colour, because a mark that means "saved" purely by being lime is a mark
 * nobody with a red-green deficiency can distinguish from one that means
 * "failed" purely by being amber.
 *
 * It reserves no space when idle. A row that shifted every time somebody
 * changed a role would move the next row out from under the pointer.
 */
export function ActionMark({ state, settled = 'saved' }: { state: ActionState; settled?: string }) {
  return (
    <>
      <ActionSlot state={state} settled={settled} />
      <span className="sr-only" role="status" aria-live="polite">
        {state === 'settled' ? settled : state === 'failed' ? 'failed' : ''}
      </span>
    </>
  )
}

/** SubmitButton is the same face driven from outside, for a real `<form>`.
 *
 *  Forms stay forms rather than becoming a button with a click handler, because
 *  a form is what makes Enter submit the fields - and an administrator filling
 *  in an issuer URL should not have to reach for the mouse to save it. */
export function SubmitButton({
  state,
  ...rest
}: ActionLabels & ActionLook & { state: ActionState }) {
  return (
    <span className="action">
      <button
        type="submit"
        className={actionClass(rest)}
        data-state={state}
        disabled={rest.disabled || state === 'pending'}
        aria-busy={state === 'pending'}
        title={rest.title}
      >
        {actionFace(state, rest)}
      </button>
      <ActionSlot state={state} settled={rest.settled ?? 'done'} />
      <ActionAnnouncement state={state} {...rest} />
    </span>
  )
}

export function Card({
  title,
  sub,
  actions,
  children,
  flush,
  tall,
  footer,
}: {
  title?: ReactNode
  sub?: ReactNode
  actions?: ReactNode
  children?: ReactNode
  flush?: boolean
  tall?: boolean
  footer?: ReactNode
}) {
  const className = ['card', flush ? 'flush' : '', tall ? 'tall' : ''].filter(Boolean).join(' ')
  return (
    <section className={className}>
      {(title || actions) && (
        <div className="card-head">
          <div style={{ minWidth: 0 }}>
            {title && <div className="card-title">{title}</div>}
            {sub && <div className="card-sub">{sub}</div>}
          </div>
          {actions && <div className="inline">{actions}</div>}
        </div>
      )}
      {children}
      {footer && <div className="card-foot">{footer}</div>}
    </section>
  )
}

export function Kpi({
  label,
  value,
  note,
  accent,
  children,
}: {
  label: string
  value: ReactNode
  note?: ReactNode
  accent?: boolean
  children?: ReactNode
}) {
  return (
    <section className="card">
      <div className="kpi-label">{label}</div>
      <div className={accent ? 'kpi-figure accent' : 'kpi-figure'}>{value}</div>
      {note && <div className="kpi-note">{note}</div>}
      {children}
    </section>
  )
}

/**
 * Counted renders a figure that counts up to itself once, at mount.
 *
 * The formatter is the caller's, so the number on screen is still whatever
 * `format.ts` says it should be - this only decides which number is handed to
 * it on the way there.
 */
export function Counted({
  value,
  format,
}: {
  value: number | null | undefined
  format: (n: number) => string
}) {
  const shown = useCountUp(value)
  return <>{shown === null ? '-' : format(shown)}</>
}

export function Track({
  value,
  of,
  tone,
  thick,
  thin,
  notch,
}: {
  value: number
  of: number
  tone?: 'accent' | 'warn' | 'neutral'
  thick?: boolean
  thin?: boolean
  /** A soft limit, on the same scale, marked on the track.
   *
   *  Two bars would say a soft limit is a second quantity. It is a point on the
   *  one quantity there is - the place past which the routes an administrator
   *  marked stop being served - so it is a mark on this bar. */
  notch?: number | null
}) {
  const className = ['track', thick ? 'thick' : '', thin ? 'thin' : ''].filter(Boolean).join(' ')
  const mounted = useMounted()
  return (
    <div className={className}>
      <span
        className={tone === 'warn' ? 'warn' : tone === 'neutral' ? 'neutral' : undefined}
        style={{ width: mounted ? `${ratio(value, of)}%` : '0%' }}
      />
      {notch !== null && notch !== undefined && notch > 0 && notch < of && (
        <span className="track-notch" style={{ left: `${ratio(notch, of)}%` }} aria-hidden />
      )}
    </div>
  )
}

export function Pill({ tone, children }: { tone?: 'good' | 'warn' | 'off'; children: ReactNode }) {
  return <span className={tone ? `pill ${tone}` : 'pill'}>{children}</span>
}

export function Tabs<T extends string>({
  value,
  options,
  onChange,
}: {
  value: T
  options: { id: T; label: string }[]
  onChange: (id: T) => void
}) {
  return (
    <div className="tabs">
      {options.map((option) => (
        <button
          key={option.id}
          type="button"
          className={option.id === value ? 'tab active' : 'tab'}
          onClick={() => onChange(option.id)}
        >
          {option.label}
        </button>
      ))}
    </div>
  )
}

/**
 * Empty is the state a fresh deployment spends its first hour in.
 *
 * Half the console is empty on day one, so these carry real weight: each one
 * names what the screen will hold, why it is holding nothing, and - where there
 * is one - the single next thing to do. The `action` slot exists so that last
 * part looks the same everywhere instead of being a `<div style>` improvised
 * per screen.
 *
 * The rule the copy follows: describe the state, never apologise for it. An
 * empty audit trail on a gateway nobody has pointed a tool at yet is the
 * correct state, and saying so is more useful than "no data".
 */
export function Empty({
  title,
  children,
  action,
}: {
  title: string
  children?: ReactNode
  action?: ReactNode
}) {
  return (
    <div className="empty">
      <strong>{title}</strong>
      {children && <p>{children}</p>}
      {action && <div className="empty-action">{action}</div>}
    </div>
  )
}

export function ErrorBanner({ error }: { error: unknown }) {
  if (!error) return null
  const message = error instanceof Error ? error.message : String(error)
  const requestId = error instanceof ApiError ? error.requestId : ''
  return (
    <div className="error-banner">
      {message}
      {requestId && (
        <>
          {' '}
          <span className="mono" style={{ opacity: 0.8 }}>
            ({requestId})
          </span>
        </>
      )}
    </div>
  )
}

export function Loading({ rows = 3 }: { rows?: number }) {
  return (
    <div className="list" style={{ padding: '18px 22px' }}>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton" style={{ height: 14, width: `${90 - i * 12}%` }} />
      ))}
    </div>
  )
}

/**
 * Money renders a cost.
 *
 * Always through the nanodollar integer, never the decimal string, so that the
 * console's own rounding is one function rather than one per screen - and an
 * unpriced total is never quietly rendered as zero.
 */
export function Money({ cost, compact }: { cost?: Cost | null; compact?: boolean }) {
  if (!cost) return <span className="mono">-</span>
  return <span className="mono">{money(cost.total_nanos, { compact: compact ?? false })}</span>
}

/**
 * Unpriced states the other gap.
 *
 * A request whose model has no rate is recorded with a null cost and counted
 * separately, and that count appears beside every total. Pricing an unknown at
 * zero would understate spend and look like a discount.
 */
export function Unpriced({ totals }: { totals: Totals }) {
  if (!totals.unpriced_billable) return null
  return (
    <span className="pill warn" title="These requests used tokens Panorama has no rate for. They are counted, not costed at zero.">
      {totals.unpriced_billable} unpriced
    </span>
  )
}

/**
 * Provenance is the product's central claim, rendered.
 *
 * Every attributed figure can say what was detected, from what signal, at what
 * confidence. The design brief calls it a first-class UI concept; this is the
 * one component that expresses it, so it looks the same everywhere.
 */
export function Provenance({
  rung,
  confidence,
  evidence,
  confirmed,
}: {
  rung?: string
  confidence?: string
  evidence?: string
  confirmed?: boolean
}) {
  if (!rung) {
    return <span className="pill off">unattributed</span>
  }
  const tone = rung === 'content' ? 'warn' : 'good'
  const title = evidence
    ? `${evidence} - ${rung}, ${confidence ?? 'unknown'} confidence`
    : `${rung}, ${confidence ?? 'unknown'} confidence`
  return (
    <span className="inline" style={{ gap: 6 }}>
      <span className={`pill ${tone}`} title={title}>
        {rung}
      </span>
      {confirmed && (
        <span className="pill good" title="A system of record recognised this work item.">
          confirmed
        </span>
      )}
    </span>
  )
}

/** Coverage is the attributed share, with the remainder always beside it. */
export function CoverageBar({
  attributed,
  total,
}: {
  attributed: number
  total: number
}) {
  const mounted = useMounted()
  return (
    <>
      {/* Both series grow together from the left, so the attributed share is
          read as a share rather than as a bar that happens to have something
          after it. The unattributed remainder is never the thing that arrives
          late. */}
      <div className="split-track">
        <span
          style={{
            width: mounted ? `${ratio(attributed, total)}%` : '0%',
            background: 'var(--accent)',
          }}
        />
        <span
          style={{
            width: mounted ? `${ratio(total - attributed, total)}%` : '0%',
            background: 'var(--series-neutral)',
          }}
        />
      </div>
      <div className="legend" style={{ marginTop: 12 }}>
        <span>
          <i className="swatch" style={{ background: 'var(--accent)' }} />
          attributed {money(attributed)}
        </span>
        <span>
          <i className="swatch" style={{ background: 'var(--series-neutral)' }} />
          unattributed {money(Math.max(0, total - attributed))}
        </span>
      </div>
    </>
  )
}

/** Bars is the daily chart: one column per bucket, most recent highlighted. */
export function Bars({
  values,
  labels,
  short,
}: {
  values: number[]
  labels?: [string, string, string]
  short?: boolean
}) {
  const max = Math.max(1, ...values)
  const mounted = useMounted()
  return (
    <>
      {/* The columns grow out of the axis on mount, left to right. The delay is
          per column and capped, so a 90-day window finishes in the same beat as
          a 7-day one instead of taking thirteen times as long. */}
      <div className={short ? 'chart short' : 'chart'}>
        {values.map((value, index) => (
          <div
            key={index}
            className={index >= values.length - 3 ? 'recent' : undefined}
            style={{
              height: mounted ? `${Math.max(2, (value / max) * 100)}%` : '0%',
              transitionDelay: `${Math.min(index, 24) * 12}ms`,
            }}
            title={money(value)}
          />
        ))}
      </div>
      {labels && values.length >= 3 && (
        <div className="chart-axis">
          <span>{labels[0]}</span>
          <span>{labels[1]}</span>
          <span>{labels[2]}</span>
        </div>
      )}
      {labels && values.length > 0 && values.length < 3 && (
        <div className="chart-axis">
          <span>{labels[labels.length - 1]}</span>
        </div>
      )}
    </>
  )
}

/* ---------------------------------------------------------------- controls --
 *
 * Segmented: one bordered group of two to four short options with exactly one
 * of them on. The range picker in the header, a group-by, every filter in a
 * filter bar - all the same control, so all of them are this.
 *
 * Two to four, and short. Six options wrap the group onto three rows and want a
 * `<select>` instead, which is why the repository filter is one.
 */
export function Segmented<T extends string>({
  value,
  options,
  onChange,
  dense,
  disabled,
  label,
}: {
  value: T
  options: { id: T; label: string; count?: string }[]
  onChange: (id: T) => void
  /** Tighter padding, for a group sitting in a filter bar beside three others. */
  dense?: boolean
  /** Shown, readable, and not movable - a setting this deployment has but this
   *  build will not let anybody change yet. Stated rather than hidden, because
   *  a reader who cannot find the switch assumes there is no switch; the
   *  sentence beside it is what says why, and the server refuses the write
   *  whatever this renders. */
  disabled?: boolean
  /** What the group selects, for a reader who cannot see the label beside it. */
  label?: string
}) {
  return (
    <div className={dense ? 'seg dense' : 'seg'} role="group" aria-label={label}>
      {options.map((option) => (
        <button
          key={option.id}
          type="button"
          className={option.id === value ? 'seg-option active' : 'seg-option'}
          aria-pressed={option.id === value}
          disabled={disabled}
          onClick={() => onChange(option.id)}
        >
          {option.label}
          {/* How many rows the option would select, as part of the option
              rather than beside it - a reader deciding whether "Errors" is
              worth clicking is asking exactly this. */}
          {option.count !== undefined && <span className="seg-count">{option.count}</span>}
        </button>
      ))}
    </div>
  )
}

/** RangePicker is the window every figure on a screen is measured over.
 *
 *  It lives in the shell's header rather than in the first band, because it
 *  governs everything below it and a control that scrolls away from the numbers
 *  it changes gets used once and then forgotten. */
/** RefreshButton asks every question on the screen again. It sits beside the
 *  control that decides what the figures mean - the range picker, or on the
 *  audit trail the window - because it is that same question, asked now. */
export function RefreshButton() {
  const shell = useRefresh()
  if (!shell) return null
  return <ActionButton label="Refresh" settled="updated" onAct={shell.refresh} />
}

export function RangePicker({ value, onChange }: { value: Range; onChange: (r: Range) => void }) {
  return <Segmented value={value} options={RANGES} onChange={onChange} label="Time range" />
}

/**
 * DailySpend is the chart both analytics screens open with, and its own
 * tooltip.
 *
 * NOT THE BROWSER'S `title`. That waits a second, appears at the pointer rather
 * than at the column, and cannot be styled, so the one number a reader hovers
 * for arrives in the operating system's font on a screen that has two.
 *
 * Three things about the panel are deliberate:
 *
 *  - The reserved band. `padding-top` on the wrapper holds the room whether or
 *    not anything is hovered, so the panel can never cover the sentence above
 *    the chart and opening it never moves the layout.
 *  - The clamp. A panel centred on the first column of a 90-day window hangs
 *    off the page, so the first three anchor left and the last three anchor
 *    right. It is computed here rather than in CSS because it depends on how
 *    many columns there are.
 *  - Dimming the rest. The hovered column takes the accent and everything else
 *    steps back to 30%, so the answer reads as picked rather than as merely not
 *    dimmed.
 *
 * Pointer only, and that is a stated limit rather than an oversight: the values
 * are also in the CSV the header exports, and a 90-column tab stop is not an
 * accessible chart. What a keyboard reader gets is the table, not this.
 */
export function DailySpend({
  days: sparse,
  since,
  until,
}: {
  days: { key: string; cost: Cost }[]
  /** The window the rollup covered, so the days nothing happened on can be
   *  drawn. Both are RFC 3339 and come from the same response as the days. */
  since?: string
  until?: string
}) {
  const [hover, setHover] = useState<{ index: number; x: number } | null>(null)
  const mounted = useMounted()

  // ONE COLUMN PER DAY IN THE WINDOW, WHICH IS WHAT THE CAPTION SAYS.
  //
  // The rollup groups by day and a day with no requests produces no row, so a
  // month with traffic on three days comes back as three rows - and drawing
  // those three side by side says "we spent money on three consecutive days",
  // which is a different and wrong claim. The zero days are the shape of the
  // month; a chart that omits them is not sparse, it is misleading.
  const days = fillDays(sparse, since, until)

  if (days.length === 0) {
    return <Empty title="No traffic in this window" />
  }

  const max = Math.max(1, ...days.map((d) => d.cost.total_nanos))
  const hovered = hover === null ? undefined : days[hover.index]

  // THE CHART IS AS WIDE AS ITS COLUMNS ARE, NOT AS WIDE AS THE BAND.
  //
  // A column is capped at 46px so that a two-day window does not render as two
  // half-page slabs. On a full-bleed band that cap means a seven-day window
  // fills a quarter of the width and leaves three quarters empty - under an
  // axis whose three dates would be spread across the whole of it, so the
  // label reading "the end of the window" would sit 900px to the right of the
  // last column, lying about which day was where.
  //
  // Capping the wrapper instead makes the chart and its axis one object: they
  // are the same width by construction, and a short window is a small chart
  // rather than a wide chart with a gap in it.
  const widest = days.length * (CHART_COLUMN + CHART_GAP) - CHART_GAP

  return (
    <div
      className={hover === null ? 'chart-hover' : 'chart-hover hovering'}
      style={{ maxWidth: widest }}
      onMouseLeave={() => setHover(null)}
    >
      <div className="chart">
        {days.map((day, index) => (
          <div
            key={day.key || index}
            className={
              [index >= days.length - 3 ? 'recent' : '', index === hover?.index ? 'on' : '']
                .filter(Boolean)
                .join(' ') || undefined
            }
            style={{
              height: mounted ? `${Math.max(2, (day.cost.total_nanos / max) * 100)}%` : '0%',
            }}
            // Measured off the column rather than computed from its index.
            // `offsetLeft` is relative to `.chart-hover`, which is the only
            // positioned ancestor, so this is where the panel has to sit
            // whatever the flex layout decided the columns were - and it stays
            // correct if the cap, the gap or the window length ever change.
            onMouseEnter={(event) =>
              setHover({
                index,
                x: event.currentTarget.offsetLeft + event.currentTarget.offsetWidth / 2,
              })
            }
          />
        ))}
      </div>

      <div className="chart-axis">
        <span>{days[0]?.key}</span>
        {days.length >= 3 && <span>{days[Math.floor((days.length - 1) / 2)]?.key}</span>}
        {days.length >= 2 && <span>{days[days.length - 1]?.key}</span>}
      </div>

      {hovered && hover && (
        <div
          className="chart-tip"
          style={{
            left: hover.x,
            // Centred over the column, except near an edge where centring would
            // hang the panel off the page. The clamp is by position rather than
            // by index because a column's position is what actually decides
            // whether there is room.
            transform: `translateX(${
              hover.x < TIP_HALF ? '0' : hover.x > widest - TIP_HALF ? '-100%' : '-50%'
            })`,
          }}
        >
          <span className="value">{money(hovered.cost.total_nanos)}</span>
          <span className="when">{hovered.key}</span>
        </div>
      )}
    </div>
  )
}

/**
 * fillDays returns one entry per day between `since` and `until`.
 *
 * Days the rollup reported keep their cost; the rest are zero. Falls back to
 * what the server sent when the window is unknown or absurd - a chart drawn
 * from a bad date is worse than a sparse one, and the guard is what stops a
 * clock skew or a malformed parameter from asking the browser to lay out ten
 * thousand columns.
 *
 * UTC throughout, because the keys are UTC dates. Building them from a local
 * `Date` would shift every label by a day for anyone west of Greenwich.
 */
function fillDays(
  days: { key: string; cost: Cost }[],
  since?: string,
  until?: string,
): { key: string; cost: Cost }[] {
  if (!since || !until) return days

  const from = Date.parse(since)
  const to = Date.parse(until)
  if (Number.isNaN(from) || Number.isNaN(to) || to < from) return days

  const DAY = 86_400_000
  const span = Math.floor((to - from) / DAY) + 1
  if (span < 1 || span > 400) return days

  const byKey = new Map(days.map((d) => [d.key, d]))
  const start = Date.UTC(
    new Date(from).getUTCFullYear(),
    new Date(from).getUTCMonth(),
    new Date(from).getUTCDate(),
  )

  const out: { key: string; cost: Cost }[] = []
  for (let i = 0; i < span; i += 1) {
    const key = new Date(start + i * DAY).toISOString().slice(0, 10)
    out.push(byKey.get(key) ?? { key, cost: ZERO_COST })
  }
  return out
}

const ZERO_COST: Cost = { total_nanos: 0, total_usd: '0', by_class: {} }

/** The column geometry, matching `.chart > div` in styles.css. Duplicated here
 *  because the wrapper's width has to be computed in JavaScript and CSS cannot
 *  hand it over; a comment on each side names the other. */
const CHART_COLUMN = 46
const CHART_GAP = 4
/** Roughly half a tooltip. Only decides which edge the panel anchors to, so an
 *  approximation is the right precision - measuring it would mean a layout read
 *  on every hover. */
const TIP_HALF = 60

/** Share renders one row of a breakdown: name, bar, figure. */
export function Share({
  name,
  meta,
  value,
  of,
  tone,
}: {
  name: ReactNode
  meta?: ReactNode
  value: number
  of: number
  tone?: 'accent' | 'warn' | 'neutral'
}) {
  return (
    <div>
      <div className="list-item-head">
        <span className="cell-strong truncate" style={{ maxWidth: '70%' }}>
          {name}
        </span>
        <span className="mono" style={{ color: 'var(--text-primary)' }}>
          {money(value)}
        </span>
      </div>
      <div style={{ marginTop: 7 }}>
        <Track value={value} of={of} tone={tone} />
      </div>
      {meta && <div className="meta">{meta}</div>}
      {!meta && <div className="meta">{percent(value, of)} of the period</div>}
    </div>
  )
}

/** CopyBlock is a copy-paste-ready block. Copy-paste is a first-class citizen
 *  in a terminal-adjacent product, so it gets a button rather than a selection
 *  instruction. */
export function CopyBlock({ text, label }: { text: string; label?: string }) {
  const [state, setState] = useState<'idle' | 'settled' | 'failed'>('idle')
  const timer = useRef<number | undefined>(undefined)

  useEffect(() => () => window.clearTimeout(timer.current), [])

  const copy = useCallback(() => {
    window.clearTimeout(timer.current)
    // Not routed through useAction: a clipboard write is instantaneous and
    // holding it in a pending state for 380ms to make that visible would make
    // the fastest thing in the console feel like the slowest.
    const clipboard = navigator.clipboard
    if (!clipboard) {
      // The clipboard API is absent outside a secure context, which is a real
      // deployment shape - a gateway reached over plain HTTP on an internal
      // address. Say so rather than appearing to copy and copying nothing.
      console.error('clipboard unavailable: the console must be served over HTTPS or from localhost')
      setState('failed')
      timer.current = window.setTimeout(() => setState('idle'), 4000)
      return
    }
    clipboard.writeText(text).then(
      () => {
        setState('settled')
        timer.current = window.setTimeout(() => setState('idle'), 1900)
      },
      (cause: unknown) => {
        console.error('clipboard write failed', cause)
        setState('failed')
        timer.current = window.setTimeout(() => setState('idle'), 4000)
      },
    )
  }, [text])

  return (
    <div>
      <div className="code-head">
        <span className="kpi-label">{label ?? 'copy and run'}</span>
        <button type="button" className="btn small" data-state={state} onClick={copy}>
          {state === 'settled' && <CheckMark />}
          <span>{state === 'settled' ? 'Copied' : state === 'failed' ? 'Select and copy' : 'Copy'}</span>
          <span className="sr-only" role="status" aria-live="polite">
            {state === 'settled' ? 'Copied to clipboard' : state === 'failed' ? 'Copy failed' : ''}
          </span>
        </button>
      </div>
      <div className="code">{text}</div>
    </div>
  )
}

/**
 * useApi fetches once and re-fetches when a dependency changes.
 *
 * `loading` means "there is nothing to show yet", not "a request is in flight",
 * and the difference is load-bearing. Every screen renders
 * `loading ? <Loading/> : <TheCard/>`, so a `loading` that went true on every
 * refetch would unmount the card each time anything called `reload()` - which
 * is what a save does on success. The button that had just been pressed would
 * be destroyed and remounted in its initial state, so its confirmation would
 * never survive long enough to be seen: "Saving…" and then straight back to
 * "Save".
 *
 * So a *reload* - the same question asked again - keeps what is already on
 * screen. A change of *path* is a different question, and the previous answer
 * must not sit there pretending to be the new one, so that still blanks to
 * skeletons.
 */
export function useApi<T>(path: string | null, deps: unknown[] = []): {
  data: T | null
  error: unknown
  loading: boolean
  reload: () => void
} {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [loading, setLoading] = useState(path !== null)
  const [nonce, setNonce] = useState(0)
  const lastPath = useRef<string | null>(null)
  // The header's Refresh button re-asks through this, and it is a reload like
  // any other: the same path, so what is on screen stays until it is replaced.
  const shell = useRefresh()
  const epoch = shell?.epoch ?? 0
  const track = shell?.track

  useEffect(() => {
    if (path === null) {
      setLoading(false)
      return
    }
    let live = true
    const isNewQuestion = lastPath.current !== path
    lastPath.current = path
    if (isNewQuestion) setLoading(true)
    const fetch = get<T>(path)
    track?.(fetch)
    fetch
      .then((value) => {
        if (!live) return
        setData(value)
        setError(null)
      })
      .catch((cause) => {
        if (!live) return
        setError(cause)
      })
      .finally(() => {
        if (live) setLoading(false)
      })
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, nonce, epoch, ...deps])

  return { data, error, loading, reload: () => setNonce((n) => n + 1) }
}

/* ------------------------------------------------------------------ theme --
 *
 * Two themes out of one token set, and the choice is the reader's. It is stored
 * per browser rather than on the account: which one is legible depends on the
 * room somebody is sitting in, and a deployment has no business having an
 * opinion about that.
 *
 * The dark console is the default and is what an unset preference gets, so a
 * fresh browser and an air-gapped one agree. The attribute is written on the
 * root element because that is where the token file listens for it.
 */
const THEME_KEY = 'panorama.theme'

export type Theme = { current: 'dark' | 'light'; toggle: () => void }

export function useTheme(): Theme {
  const [current, setCurrent] = useState<'dark' | 'light'>(() =>
    localStorage.getItem(THEME_KEY) === 'light' ? 'light' : 'dark',
  )

  useEffect(() => {
    // Only the light theme is an attribute. Removing it rather than writing
    // "dark" keeps the default the one the stylesheet already describes, so a
    // page painted before this effect runs is not briefly the wrong one.
    if (current === 'light') document.documentElement.setAttribute('data-theme', 'light')
    else document.documentElement.removeAttribute('data-theme')
    localStorage.setItem(THEME_KEY, current)
  }, [current])

  return {
    current,
    toggle: useCallback(() => setCurrent((t) => (t === 'light' ? 'dark' : 'light')), []),
  }
}

/** Section is a page heading between card grids. */
export function Section({ title, sub, actions }: { title: string; sub?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="spread">
      <div>
        <div className="card-title">{title}</div>
        {sub && <div className="card-sub">{sub}</div>}
      </div>
      {actions}
    </div>
  )
}

/** Trust is one of the three stances, placed where the doubt is. */
export function Trust({ children }: { children: ReactNode }) {
  return (
    <div className="card-foot" style={{ marginTop: 0, paddingTop: 0, borderTop: 'none' }}>
      <span aria-hidden style={{ color: 'var(--accent)', flexShrink: 0, display: 'flex' }}>
        <Icon name="shield" />
      </span>
      <span>{children}</span>
    </div>
  )
}

/* ----------------------------------------------------------------- overlay --
 *
 * Two shapes, and which one to use is a rule rather than a taste.
 *
 * A **Drawer** is where something is edited. It comes in from the right and
 * leaves the list it was opened from visible behind the scrim, because what is
 * being edited - which model a route runs on, what a team may spend - is only
 * meaningful beside that list: "is this the cheap one" is answered by
 * comparison, and a box in the middle of the screen hides exactly the context
 * the decision needs.
 *
 * A **Modal** is where something is *picked for* the surface underneath. It is
 * centred and it deliberately covers the page rather than the panel: the panel
 * is the thing being filled in, and closing it to choose what goes in it is how
 * an administrator loses an edit they had half made. Choosing is also a
 * momentary task with one question in it, which is what a centred dialog is
 * for.
 *
 * Both are dialogs in every way a keyboard cares about: Escape closes, focus
 * moves in on open and returns on close, and the scrim takes a click.
 */

// Which overlays are open, the one on top last.
//
// An overlay opened from inside another - the people picker over the team it is
// filling in - puts two Escape listeners on the window, and one keypress runs
// both: stopPropagation does not stop a sibling listener on the same target,
// and the outer listener was registered first, so it would close the editor out
// from under the picker and lose the edit. Only the one on top answers.
const overlayStack: symbol[] = []

/** useOverlay is the dialog behaviour Drawer and Modal share. */
function useOverlay(open: boolean, onClose: () => void) {
  const panel = useRef<HTMLDivElement>(null)
  const returnTo = useRef<HTMLElement | null>(null)
  const id = useRef<symbol>(null as unknown as symbol)
  if (id.current === null) id.current = Symbol('overlay')
  // Unique, because two overlays can be open at once and a duplicated id would
  // have the one on top labelled by the one underneath it.
  const titleID = useId()

  // Every caller passes an inline arrow, so onClose is a different function on
  // every render of the screen behind the panel. As a dependency it would make
  // the effect below tear down and set itself up again on each of those renders
  // - and setting up moves focus into the panel. So the first letter typed into
  // a route's name would re-render the screen, take focus off the field
  // mid-word, and send the rest of the name nowhere. Opening and closing are what this
  // effect is about; Escape reads the current handler through a ref.
  const close = useRef(onClose)
  useEffect(() => {
    close.current = onClose
  })

  useEffect(() => {
    if (!open) return

    returnTo.current = document.activeElement as HTMLElement | null
    const self = id.current
    overlayStack.push(self)

    // The first field, or the panel itself. Focusing the panel rather than
    // nothing is what keeps a screen reader's cursor from staying behind on the
    // page the reader can no longer see. Close is skipped rather than matched:
    // it is the first focusable thing in the document order of both shapes, so
    // taking the first match opened every panel with leaving it selected.
    const candidates = panel.current?.querySelectorAll<HTMLElement>(
      'input:not([type="hidden"]), select, textarea, button',
    )
    const first = Array.from(candidates ?? []).find(
      (el) => !el.hasAttribute('data-overlay-dismiss'),
    )
    ;(first ?? panel.current)?.focus()

    const onKey = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      if (overlayStack[overlayStack.length - 1] !== self) return
      event.stopPropagation()
      close.current()
    }
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('keydown', onKey)
      const at = overlayStack.lastIndexOf(self)
      if (at >= 0) overlayStack.splice(at, 1)
      returnTo.current?.focus()
    }
  }, [open])

  return { panel, titleID }
}

/**
 * Drawer is where something is edited, and every setting is edited.
 *
 * A person, a team, a route, a limit, an integration: all of them are
 * configured here rather than in a centred dialog, because the decision is
 * comparative and the panel leaves the list it is being compared against
 * visible behind the scrim.
 */
export function Drawer({
  open,
  title,
  sub,
  onClose,
  onSubmit,
  footer,
  wide,
  children,
}: {
  open: boolean
  title: string
  sub?: ReactNode
  onClose: () => void
  /** Given, the body and footer become a real `<form>`, so Enter saves. A
   *  drawer that made somebody reach for the mouse to commit three fields
   *  would be a dialog wearing a panel's clothes. */
  onSubmit?: () => void
  footer?: ReactNode
  /** Wider, for a panel holding a table rather than a form. The cap grid is
   *  four columns of figures and a scope label; at the form width the labels
   *  ellipsise and the figures wrap, which is the one thing a grid comparing
   *  numbers must not do. */
  wide?: boolean
  children: ReactNode
}) {
  const { panel, titleID } = useOverlay(open, onClose)
  const layer = useExit<HTMLDivElement>('.drawer')
  if (!open) return null

  return (
    <div className="drawer-layer" ref={layer}>
      {/* The scrim is a button so it is reachable by every input device rather
          than only by a mouse. */}
      <button type="button" className="drawer-scrim" aria-label="Close" onClick={onClose} />
      <div
        className={wide ? 'drawer wide' : 'drawer'}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
        tabIndex={-1}
        ref={panel}
      >
        <div className="drawer-head">
          <div style={{ minWidth: 0 }}>
            <div className="card-title" id={titleID}>
              {title}
            </div>
            {sub && <div className="card-sub">{sub}</div>}
          </div>
          <button type="button" className="btn small" data-overlay-dismiss onClick={onClose}>
            Close
          </button>
        </div>
        {onSubmit ? (
          <form
            className="drawer-form"
            onSubmit={(event) => {
              event.preventDefault()
              onSubmit()
            }}
          >
            <div className="drawer-body">{children}</div>
            {footer && <div className="drawer-foot">{footer}</div>}
          </form>
        ) : (
          <>
            <div className="drawer-body">{children}</div>
            {footer && <div className="drawer-foot">{footer}</div>}
          </>
        )}
      </div>
    </div>
  )
}

/**
 * Modal is a choice made for the surface underneath it.
 *
 * Picking only - people, teams, models - and never a settings form: a panel is
 * open behind it holding the edit this choice goes into, and closing that panel
 * to choose what goes in it is how a half-made edit gets lost. Anything being
 * configured rather than chosen belongs in a Drawer.
 *
 * `size` is about what is being chosen from, not about importance: `wide` is
 * the model catalogue, which is six columns and unreadable narrower.
 */
export function Modal({
  open,
  title,
  sub,
  onClose,
  onSubmit,
  footer,
  size,
  children,
}: {
  open: boolean
  title: string
  sub?: ReactNode
  onClose: () => void
  onSubmit?: () => void
  footer?: ReactNode
  size?: 'wide'
  children: ReactNode
}) {
  const { panel, titleID } = useOverlay(open, onClose)
  const layer = useExit<HTMLDivElement>('.modal')
  if (!open) return null

  const body = (
    <>
      <div className="modal-body">{children}</div>
      {footer && <div className="modal-foot">{footer}</div>}
    </>
  )

  return (
    <div className="modal-layer" ref={layer}>
      <button type="button" className="modal-scrim" aria-label="Close" onClick={onClose} />
      <div
        className={size === 'wide' ? 'modal wide' : 'modal'}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
        tabIndex={-1}
        ref={panel}
      >
        <div className="modal-head">
          <div style={{ minWidth: 0 }}>
            <div className="card-title" id={titleID}>
              {title}
            </div>
            {sub && <div className="card-sub">{sub}</div>}
          </div>
          <button type="button" className="btn small" data-overlay-dismiss onClick={onClose}>
            Close
          </button>
        </div>
        {onSubmit ? (
          <form
            className="modal-form"
            onSubmit={(event) => {
              event.preventDefault()
              onSubmit()
            }}
          >
            {body}
          </form>
        ) : (
          body
        )}
      </div>
    </div>
  )
}

/**
 * Choice is a set of alternatives one of which is always chosen.
 *
 * Used where a select would hide the options behind a click and the options are
 * the point - a route's rule condition, a budget's period. Two or three short
 * words each, all visible, one selected.
 */
export function Choice<T extends string>({
  value,
  options,
  onChange,
  label,
}: {
  value: T
  options: { id: T; label: string }[]
  onChange: (id: T) => void
  label: string
}) {
  return (
    <div className="choice" role="radiogroup" aria-label={label}>
      {options.map((option) => (
        <button
          key={option.id}
          type="button"
          role="radio"
          aria-checked={option.id === value}
          className={option.id === value ? 'choice-option active' : 'choice-option'}
          onClick={() => onChange(option.id)}
        >
          {option.label}
        </button>
      ))}
    </div>
  )
}


/* ------------------------------------------------------------------ select --
 *
 * The console drew every control it has except this one.
 *
 * A native `<select>` renders its own popup, and nothing on the page reaches
 * inside it: the operating system picks the type face, the row height, the
 * highlight colour and the arrow. On a console whose whole argument is that
 * structure speaks in mono caps and that panels are rules rather than boxes,
 * sixteen controls would open a translucent sheet in the system sans with a
 * blue selection band - the one surface in the product that belonged to no
 * design system.
 *
 * So this is the select, drawn. Three variants, and the popup built out of the vocabulary every other list in
 * the console is built from.
 *
 * **What it keeps from the native control**, because these are the reasons a
 * native select is usually the right answer and giving them up is the cost of
 * drawing one:
 *
 * - the full keyboard model - Up, Down, Home, End, Enter, Escape, Alt+Down to
 *   open, Tab to leave and commit nothing;
 * - typeahead, closed or open: typing `pl` moves to Platform, and the buffer
 *   clears after a second, which is what a native select does;
 * - the ARIA select-only combobox pattern, so a screen reader is told what a
 *   native select tells it - a collapsed control with a value, a listbox with
 *   options, and which option is active;
 * - a value that is always visible in the trigger, never a placeholder standing
 *   in for one.
 *
 * **Search appears only when the list is long enough to need it.** Ten is the
 * threshold. Most of these have three to six options, where a filter field is
 * furniture in front of a list you can already see all of; the team filter is
 * the case that earns it, because the comment on it in People.tsx says some
 * deployments have hundreds. A control that grows a search box when the data
 * grows is one control, not two.
 *
 * **The popup is portalled to the body and positioned fixed.** It opens inside
 * drawer bodies and modal bodies, both of which scroll and clip; anything laid
 * out in place would be cut off by its own container. Fixed coordinates are
 * recomputed on scroll and resize, and the popup flips above the trigger when
 * there is more room there - the model catalogue's four filters sit near the
 * bottom of a modal, which is exactly where a downward popup has nowhere to go.
 */

/** One option. `label` is what is read; `value` is what is stored. */
export type Option = { value: string; label: string }

/** Beyond this many options the popup grows a filter field. */
const SEARCH_AT = 10

/** How long a typeahead buffer survives, matching the platform's own. */
const TYPEAHEAD_MS = 1000

export function Select({
  value,
  options,
  onChange,
  label,
  className = 'input',
  id,
}: {
  value: string
  options: Option[]
  onChange: (value: string) => void
  /** What the control is.
   *
   *  It names the trigger, the listbox and the filter field, so a screen reader
   *  is told what is being filtered rather than being handed an unlabelled list
   *  of options.
   *
   *  **Where a visible `<label htmlFor>` also points at this control, `label`
   *  must be that label's words.** `aria-label` overrides an associated label
   *  element rather than adding to it, so a control captioned "Everyday work"
   *  and named "Everyday route" is one a voice-control user cannot operate:
   *  they say what they can see, and nothing on the page answers to it. That is
   *  WCAG 2.5.3, and it is the one way to get this component wrong from the
   *  outside. */
  label?: string
  /** The variant, exactly as the native selects carried it: `input`,
   *  `filter-select` or `setup-select`. */
  className?: string
  id?: string
}) {
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(0)
  const [query, setQuery] = useState('')
  const trigger = useRef<HTMLButtonElement>(null)
  const list = useRef<HTMLDivElement>(null)
  const search = useRef<HTMLInputElement>(null)
  const typed = useRef({ buffer: '', at: 0 })
  const listID = useId()

  const searchable = options.length >= SEARCH_AT
  const shown = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!searchable || q === '') return options
    return options.filter((o) => o.label.toLowerCase().includes(q))
  }, [options, query, searchable])

  const selected = options.find((o) => o.value === value)

  // Where the popup goes. Held in state rather than read during render so the
  // measurement happens after the trigger has been laid out, and recomputed
  // while open because a drawer scrolling under a popup that stayed put would
  // leave it pointing at nothing.
  const [box, setBox] = useState<{ left: number; top: number; width: number; above: boolean } | null>(null)
  const place = useCallback(() => {
    const el = trigger.current
    if (!el) return
    const r = el.getBoundingClientRect()
    const below = window.innerHeight - r.bottom
    // 280 is the popup's own max height. Flipping on the measurement rather
    // than on a guess is what keeps the four filters at the bottom of the model
    // catalogue from opening off the screen.
    const above = below < 200 && r.top > below
    setBox({ left: r.left, top: above ? r.top : r.bottom, width: r.width, above })
  }, [])

  useLayoutEffect(() => {
    if (!open) return
    place()
    // `true` for capture, so a scroll inside the drawer body reaches this and
    // not only a scroll of the document.
    window.addEventListener('scroll', place, true)
    window.addEventListener('resize', place)
    return () => {
      window.removeEventListener('scroll', place, true)
      window.removeEventListener('resize', place)
    }
  }, [open, place])

  // Escape, through the same stack Drawer and Modal use. A select opened inside
  // a drawer has to answer Escape without the drawer also answering it, and
  // stopPropagation cannot do that for two listeners on one target - which is
  // the whole reason the stack exists.
  const self = useRef<symbol>(null as unknown as symbol)
  if (self.current === null) self.current = Symbol('select')
  useEffect(() => {
    if (!open) return
    const mine = self.current
    overlayStack.push(mine)
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      if (overlayStack[overlayStack.length - 1] !== mine) return
      event.stopPropagation()
      setOpen(false)
      trigger.current?.focus()
    }
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('keydown', onKey)
      const at = overlayStack.lastIndexOf(mine)
      if (at >= 0) overlayStack.splice(at, 1)
    }
  }, [open])

  // A press anywhere else closes it. Down rather than click, so the popup is
  // gone before whatever was pressed acts on it - which is also what makes
  // opening a second select close the first. `pointerdown` rather than
  // `mousedown`, so a touch closes it on the first tap instead of waiting for
  // the compatibility mouse event a browser synthesises afterwards.
  useEffect(() => {
    if (!open) return
    const onDown = (event: PointerEvent) => {
      const target = event.target as Node
      if (trigger.current?.contains(target) || list.current?.contains(target)) return
      setOpen(false)
    }
    window.addEventListener('pointerdown', onDown)
    return () => window.removeEventListener('pointerdown', onDown)
  }, [open])

  // Opening lands on the current value rather than at the top, which is where
  // a native select opens and what makes Down-from-open mean "the next one".
  const show = useCallback(() => {
    const at = shown.findIndex((o) => o.value === value)
    setActive(at < 0 ? 0 : at)
    setQuery('')
    setOpen(true)
  }, [shown, value])

  const commit = useCallback(
    (next: string) => {
      onChange(next)
      setOpen(false)
      trigger.current?.focus()
    },
    [onChange],
  )

  // Keep the active option in view, including when the filter has just cut the
  // list down to something shorter than the scroll position.
  useEffect(() => {
    if (!open) return
    list.current?.querySelector('[data-active="true"]')?.scrollIntoView({ block: 'nearest' })
  }, [open, active, shown.length])

  /* Focused by a callback ref rather than by an effect, because an effect
     cannot see this element on the render that opens the popup.
     `setOpen(true)` renders once with no measurement yet and therefore no
     portal, the layout effect measures, and only the *second* render puts the
     field in the document - by which time an effect keyed on `open` has
     already run and found nothing to focus. A callback ref fires when the node
     appears, which is the moment being waited for. It is stable, so it does
     not re-fire on the repositioning renders a scroll causes, which would drag
     the caret back to the start of whatever somebody was typing. */
  const takeSearch = useCallback((el: HTMLInputElement | null) => {
    search.current = el
    el?.focus()
  }, [])

  /** Typeahead: the letters somebody types, resolved against the labels.
   *  Returns the index it moved to, or -1. */
  const seek = (key: string, from: Option[]): number => {
    const now = Date.now()
    const buffer = now - typed.current.at > TYPEAHEAD_MS ? key : typed.current.buffer + key
    typed.current = { buffer, at: now }
    const found = from.findIndex((o) => o.label.toLowerCase().startsWith(buffer.toLowerCase()))
    return found
  }

  const onTriggerKey = (event: React.KeyboardEvent) => {
    if (open) return onListKey(event)
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp' || event.key === 'Enter' || event.key === ' ') {
      event.preventDefault()
      show()
      return
    }
    // Closed typeahead moves the value directly, as a native select does: the
    // list never opens and the control simply becomes the thing you named.
    if (event.key.length === 1 && !event.metaKey && !event.ctrlKey && !event.altKey) {
      const found = options[seek(event.key, options)]
      if (found) {
        event.preventDefault()
        onChange(found.value)
      }
    }
  }

  const onListKey = (event: React.KeyboardEvent) => {
    const last = shown.length - 1
    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault()
        setActive((at) => Math.min(last, at + 1))
        return
      case 'ArrowUp':
        event.preventDefault()
        setActive((at) => Math.max(0, at - 1))
        return
      case 'Home':
        event.preventDefault()
        setActive(0)
        return
      case 'End':
        event.preventDefault()
        setActive(last)
        return
      case 'Enter':
        event.preventDefault()
        if (shown[active]) commit(shown[active].value)
        return
      case 'Tab':
        // Leaving without choosing, which is what Tab means in a native select.
        setOpen(false)
        return
      default:
        break
    }
    // While a filter field has focus its own typing is the filter, so typeahead
    // would be a second, conflicting interpretation of the same keystroke.
    if (searchable) return
    if (event.key.length === 1 && !event.metaKey && !event.ctrlKey && !event.altKey) {
      const at = seek(event.key, shown)
      if (at >= 0) {
        event.preventDefault()
        setActive(at)
      }
    }
  }

  return (
    <>
      <button
        type="button"
        id={id}
        ref={trigger}
        className={`sel ${className}`}
        role="combobox"
        aria-expanded={open}
        aria-haspopup="listbox"
        aria-controls={open ? listID : undefined}
        aria-activedescendant={
          open && !searchable && shown[active] ? `${listID}-${active}` : undefined
        }
        aria-label={label}
        onClick={() => (open ? setOpen(false) : show())}
        onKeyDown={onTriggerKey}
      >
        {/* The value, and never a placeholder: a select that shows nothing
            until it is used is a select nobody can read the state of. An
            option that has gone missing shows its raw value rather than an
            empty control, so a stale filter is visible instead of silent. */}
        <span className="sel-value">{selected ? selected.label : value}</span>
        <Icon name="caret" className="sel-caret" />
      </button>

      {open &&
        box &&
        createPortal(
          <div
            ref={list}
            className={box.above ? 'sel-pop above' : 'sel-pop'}
            style={{
              left: box.left,
              width: box.width,
              ...(box.above
                ? { bottom: window.innerHeight - box.top }
                : { top: box.top }),
            }}
          >
            {searchable && (
              <div className="sel-search">
                <Icon name="search" className="sel-search-mark" />
                {/* The combobox state rides on whichever element has focus.
                    Once this field exists it does, so it is the one that has to
                    carry `aria-activedescendant` - a screen reader follows
                    focus, and pointing at the active option from the trigger
                    the reader has just left would announce a list nobody is
                    on. The trigger keeps the role for the collapsed state,
                    which is the only state it is focused in. */}
                <input
                  ref={takeSearch}
                  className="sel-search-field"
                  value={query}
                  role="combobox"
                  aria-expanded
                  aria-controls={listID}
                  aria-activedescendant={shown[active] ? `${listID}-${active}` : undefined}
                  aria-autocomplete="list"
                  aria-label={`Filter ${label ?? 'options'}`}
                  placeholder="Filter"
                  onChange={(e) => {
                    setQuery(e.target.value)
                    setActive(0)
                  }}
                  onKeyDown={onListKey}
                />
              </div>
            )}
            <div className="sel-list" role="listbox" id={listID} aria-label={label}>
              {/* Two different absences, and they are not the same news: a
                  filter that matched nothing is the reader's own doing and
                  undone by clearing it, while a control with nothing in it at
                  all is the deployment's state and nothing they type will
                  help. */}
              {shown.length === 0 && (
                <div className="sel-none">{options.length === 0 ? 'Nothing to choose' : 'No match'}</div>
              )}
              {shown.map((option, at) => (
                <div
                  key={option.value}
                  id={`${listID}-${at}`}
                  role="option"
                  aria-selected={option.value === value}
                  data-active={at === active}
                  className="sel-option"
                  onMouseEnter={() => setActive(at)}
                  onClick={() => commit(option.value)}
                >
                  <span className="sel-option-label">{option.label}</span>
                  {/* The mark rather than a fill: the accent in this system
                      stands for a quantity or the one active thing, and a
                      selected row in a menu of five is neither. */}
                  {option.value === value && <span className="sel-tick" aria-hidden>✓</span>}
                </div>
              ))}
            </div>
          </div>,
          document.body,
        )}
    </>
  )
}

/**
 * Avatar is somebody's initials in a block.
 *
 * Initials rather than a photograph: Panorama does not own identities, so it
 * has no picture of anyone and asking a provider for one would be a network
 * call to a third-party origin on a console that deliberately makes none.
 */
export function Avatar({ name, size }: { name: string; size?: 'sm' | 'lg' }) {
  // Split on the punctuation an address uses as well as on spaces. Half the
  // accounts in a fresh deployment have no display name yet, and splitting
  // "ada.lovelace@example.com" on whitespace alone yields one letter for everyone
  // whose email happens to start the same way.
  const initials = name
    .split(/[\s._@+-]+/)
    .filter(Boolean)
    .map((word) => word[0])
    .join('')
    .slice(0, 2)
    .toUpperCase()
  return (
    <span className={size ? `avatar ${size}` : 'avatar'} aria-hidden>
      {initials || '·'}
    </span>
  )
}

export type CheckOption = {
  id: string
  label: string
  sub?: string
  /** A short word on the right saying what saving would do to this row -
   *  "adding", "removing". A roster is a diff, and a checkbox on its own does
   *  not say which way it moved. */
  change?: 'add' | 'remove'
  /** Already true for a reason outside this list, and not this list's to
   *  change. Rendered checked, dimmed, and inert. */
  fixed?: boolean
  fixedNote?: string
}

/**
 * CheckList is the console's one multi-select: a searchable list of rows with a
 * box each.
 *
 * Shared by every surface that picks people or teams - a route's conditions, a
 * team's roster, the teams one person is on - because they are the same
 * interaction and three implementations of it would drift into three
 * behaviours.
 */
export function CheckList({
  options,
  selected,
  onChange,
  empty = 'Nothing matches that search.',
}: {
  options: CheckOption[]
  selected: string[]
  onChange: (next: string[]) => void
  empty?: string
}) {
  if (options.length === 0) {
    return <div className="check-empty">{empty}</div>
  }
  return (
    <div className="check-list">
      {options.map((option) => (
        <CheckRow
          key={option.id}
          option={option}
          on={option.fixed || selected.includes(option.id)}
          onToggle={() => toggleIn(selected, option.id, onChange)}
        />
      ))}
    </div>
  )
}

/** CheckRow is one row of a CheckList, separately so a virtualised list can
 *  render the same row without the list around it. */
export function CheckRow({
  option,
  on,
  onToggle,
}: {
  option: CheckOption
  on: boolean
  onToggle: () => void
}) {
  const note = option.fixed && option.fixedNote ? option.fixedNote : option.sub
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={on}
      aria-disabled={option.fixed || undefined}
      className={['check-row', on ? 'on' : '', option.fixed ? 'fixed' : ''].filter(Boolean).join(' ')}
      onClick={() => {
        if (!option.fixed) onToggle()
      }}
    >
      <span className={on ? 'check-box on' : 'check-box'} aria-hidden>
        {on ? '✓' : ''}
      </span>
      <span className="check-text">
        <span className="check-label truncate">{option.label}</span>
        {note && <span className="check-sub truncate">{note}</span>}
      </span>
      {option.change && !option.fixed && (
        <span className={option.change === 'add' ? 'check-change add' : 'check-change remove'}>
          {option.change === 'add' ? 'adding' : 'removing'}
        </span>
      )}
    </button>
  )
}

/** toggleIn adds or removes one id, which is the whole of what every picker in
 *  the console does with a click. */
export function toggleIn(selected: string[], id: string, onChange: (next: string[]) => void) {
  onChange(selected.includes(id) ? selected.filter((v) => v !== id) : [...selected, id])
}

/**
 * useToast acknowledges a change that happened somewhere the reader is no
 * longer looking.
 *
 * The console's usual acknowledgement is the button settling into "Saved",
 * which works while the button is on screen. It is not, for the changes these
 * two screens make: a route saves and its panel closes, a person is taken off a
 * team from inside a drawer that then goes away. This says what happened, once,
 * where the eye already is.
 *
 * No undo. The design mock holds its data in the browser, where restoring a
 * snapshot is one assignment; here the change is already at the
 * server and another administrator may have acted since, so an "Undo" would be
 * a second write pretending to be a rewind.
 */
/** The toast itself, as its own component so that it unmounts - which is what
 *  gives it an exit, on the same path as every other overlay. It is the one
 *  nobody dismisses, so a hard cut would be easiest to miss here and worst to
 *  keep: an acknowledgement that blinks out of existence reads as the console
 *  retracting what it just said. */
function Toast({ message }: { message: string }) {
  const self = useExit<HTMLDivElement>()
  return (
    <div className="toast" role="status" ref={self}>
      <span className="toast-dot" aria-hidden />
      <span>{message}</span>
    </div>
  )
}

export function useToast(): { say: (message: string) => void; toast: ReactNode } {
  const [message, setMessage] = useState<string | null>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current)
  }, [])

  const say = useCallback((next: string) => {
    if (timer.current) clearTimeout(timer.current)
    setMessage(next)
    timer.current = setTimeout(() => setMessage(null), 5200)
  }, [])

  // No key on the message: a second toast arriving while one is up replaces
  // the words in place rather than remounting, so it does not animate the old
  // acknowledgement out from under the new one.
  const toast = message === null ? null : <Toast message={message} />

  return { say, toast }
}

/**
 * VirtualList renders only the rows a reader can see.
 *
 * An identity provider with three thousand accounts is a normal deployment, and
 * a picker that puts three thousand rows in the document takes a second to open
 * and stutters on every keystroke of the search that was meant to rescue it.
 * This keeps the DOM at roughly a screenful however long the list is.
 *
 * Fixed row height, which is why the rows it is given are plain: the moment a
 * row can be two lines tall, the scrollbar lies about where things are. Every
 * caller here renders a name, a line under it, and a box.
 *
 * Hand-rolled rather than a dependency, for the same reason the router is: this
 * is the whole of what a windowing library would be doing, and it is thirty
 * lines.
 */
export function VirtualList<T>({
  items,
  rowHeight,
  height,
  render,
  empty,
}: {
  items: T[]
  rowHeight: number
  /** The visible height of the scroller, in pixels. */
  height: number
  render: (item: T, index: number) => ReactNode
  empty?: ReactNode
}) {
  const [top, setTop] = useState(0)
  const scroller = useRef<HTMLDivElement>(null)

  // A filter that shortens the list under a scrolled viewport would otherwise
  // leave the reader looking at blank space below the last row.
  useEffect(() => {
    if (scroller.current) scroller.current.scrollTop = 0
    setTop(0)
  }, [items])

  if (items.length === 0) return <>{empty}</>

  // Three rows of overscan, so a fast flick does not show a band of nothing
  // before React catches up.
  const first = Math.max(0, Math.floor(top / rowHeight) - 3)
  const last = Math.min(items.length, Math.ceil((top + height) / rowHeight) + 3)
  const shown = items.slice(first, last)

  return (
    <div
      className="vlist"
      ref={scroller}
      style={{ height, maxHeight: items.length * rowHeight }}
      onScroll={(event) => setTop(event.currentTarget.scrollTop)}
    >
      {/* The spacer carries the real length, so the scrollbar is honest about
          how much list there is. */}
      <div style={{ height: items.length * rowHeight, position: 'relative' }}>
        <div style={{ position: 'absolute', top: first * rowHeight, left: 0, right: 0 }}>
          {shown.map((item, i) => (
            <div key={first + i} style={{ height: rowHeight }}>
              {render(item, first + i)}
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

/**
 * useDebounced calls `run` once the value has stopped changing.
 *
 * What auto-save is built on. Saving on every keystroke would write "1", "18",
 * "180" and "1800" on the way to eighteen hundred - four budgets an engineer
 * could be blocked by, three of them numbers nobody typed on purpose.
 */
export function useDebounced(value: unknown, delay: number, run: () => void) {
  const latest = useRef(run)
  latest.current = run

  // The first render is the value as it was loaded, not an edit.
  const settled = useRef(false)

  useEffect(() => {
    if (!settled.current) {
      settled.current = true
      return
    }
    const timer = setTimeout(() => latest.current(), delay)
    return () => clearTimeout(timer)
  }, [value, delay])
}
