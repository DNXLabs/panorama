// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useMemo, useState } from 'react'
import { del, put } from '../api'
import { amountForEdit, money, parseUSD } from '../format'
import {
  NO_LIMITS,
  WINDOWS,
  WINDOW_ADVERB,
  WINDOW_LABEL,
  WINDOW_NOTE,
  WINDOW_NOUN,
  bindingScope,
  budgetAt,
  seedHard,
  seedSoft,
} from '../people'
import type { CapScope, Limits, Window } from '../people'
import type { BudgetRow } from '../types'
import { Track } from '../ui'

// A limit, and every limit that reaches the thing it is set on.
//
// One editor for all three scopes, because a budget on a person, a team and the
// deployment differ only in whose spend they count. Two editors would be two
// places to get the window wrong, and the window is half of a budget's
// identity.
//
// THE PANEL IS THE PERIOD TAB'S CONTENT, which is the whole reason the tabs are
// attached to it rather than floating above it: a switch and a figure that
// silently mean "monthly" and then silently mean "daily" are how somebody caps
// a team at $600 a month. Every tab whose window carries a limit also carries
// an accent square, so which windows are governed is legible without clicking
// through all three.

/** One row's state. `on` is separate from `value` because a field somebody has
 *  cleared in order to retype it is still switched on, and reading the empty
 *  box as "no limit" would delete their budget between two keystrokes. */
type Limit = { on: boolean; value: string }

/** Both rows of one window. */
type Pair = { soft: Limit; hard: Limit }

type Draft = Record<Window, Pair>

function limitFrom(value?: string | null): Limit {
  const text = amountForEdit(value)
  return { on: text !== '', value: text }
}

function draftFrom(budgets: BudgetRow[], scope: string, scopeID: string): Draft {
  const out = {} as Draft
  for (const period of WINDOWS) {
    const budget = budgetAt(budgets, scope, scopeID, period)
    out[period] = {
      soft: limitFrom(budget?.limits.soft_usd),
      hard: limitFrom(budget?.limits.hard_usd),
    }
  }
  return out
}

/** nanosOf is what a half-typed box is worth to the grid: nothing. A switch
 *  that is on with no figure in it caps nobody yet, and showing the previous
 *  figure while somebody retypes would draw a binding cap that is not there. */
function nanosOf(limit: Limit): number | null {
  return limit.on ? parseUSD(limit.value) : null
}

export type LimitsEditor = {
  period: Window
  setPeriod: (next: Window) => void
  draft: Draft
  set: (row: 'soft' | 'hard', next: Limit) => void
  flip: (row: 'soft' | 'hard') => void
  /** What the grid draws, including the edit in progress - so switching a
   *  limit off moves the binding cap in the same frame. */
  live: { hard: Limits; soft: Limits }
  /** Which windows carry a limit, for the dot on each tab. */
  governed: Record<Window, boolean>
  dirty: boolean
  /** A row switched on with an empty box is somebody mid-edit, not a limit. */
  midEdit: boolean
  save: () => Promise<void>
}

/**
 * useLimits holds one subject's limits while they are being edited.
 *
 * All three windows at once, not just the one on screen: the cap grid draws
 * every window, and the save writes every window that changed - so an
 * administrator who sets a daily ceiling, switches to monthly and raises that
 * commits both with one button.
 */
export function useLimits(
  budgets: BudgetRow[],
  scope: 'user' | 'team' | 'org',
  scopeID: string,
  defaultName: string,
): LimitsEditor {
  const stored = useMemo(() => draftFrom(budgets, scope, scopeID), [budgets, scope, scopeID])
  const [draft, setDraft] = useState<Draft>(stored)
  // Monthly, always, whichever windows happen to be set. The drawer opens on
  // the window an administrator means by "their budget", and the accent squares
  // on the other two tabs say where the rest is.
  const [period, setPeriod] = useState<Window>('month')

  const set = (row: 'soft' | 'hard', next: Limit) =>
    setDraft((current) => ({ ...current, [period]: { ...current[period], [row]: next } }))

  const flip = (row: 'soft' | 'hard') => {
    const current = draft[period][row]
    if (current.on) {
      set(row, { on: false, value: '' })
      return
    }
    set(row, {
      on: true,
      value: row === 'hard' ? seedHard(scope) : seedSoft(draft[period].hard.value, scope),
    })
  }

  const live = { hard: { ...NO_LIMITS }, soft: { ...NO_LIMITS } }
  const governed: Record<Window, boolean> = { day: false, week: false, month: false }
  for (const each of WINDOWS) {
    live.hard[each] = nanosOf(draft[each].hard)
    live.soft[each] = nanosOf(draft[each].soft)
    governed[each] = draft[each].hard.on || draft[each].soft.on
  }

  const midEdit = WINDOWS.some((each) =>
    [draft[each].soft, draft[each].hard].some((row) => row.on && row.value.trim() === ''),
  )

  const save = async () => {
    for (const each of WINDOWS) {
      if (JSON.stringify(draft[each]) === JSON.stringify(stored[each])) continue
      const current = budgetAt(budgets, scope, scopeID, each)
      const { soft, hard } = draft[each]
      if (!soft.on && !hard.on) {
        // Every limit off is not a budget with no limits - the server refuses
        // that, correctly, because it would enforce nothing. It is the
        // instruction to remove the budget.
        if (current) await del(`/api/v1/budgets/${encodeURIComponent(current.id)}`)
        continue
      }
      await put('/api/v1/budgets', {
        scope,
        scope_id: scopeID,
        period: each,
        name: current?.name || defaultName,
        soft_limit: soft.on ? soft.value.trim() : '',
        hard_limit: hard.on ? hard.value.trim() : '',
      })
    }
  }

  return {
    period,
    setPeriod,
    draft,
    set,
    flip,
    live,
    governed,
    dirty: JSON.stringify(draft) !== JSON.stringify(stored),
    midEdit,
    save,
  }
}

/**
 * SpendTrack is spend against the limit that stops it.
 *
 * WITH NO HARD LIMIT THE BAR IS EMPTY, not full. A bar is a proportion of a
 * ceiling, so with no ceiling there is no proportion - and scaling it to the
 * spend itself would fill it to the end and read as "at the limit" for
 * precisely the scopes that nothing stops. The figure beside
 * it says "not set" and the note underneath says what that means.
 */
export function SpendTrack({
  spent,
  hard,
  soft,
  thick,
}: {
  spent: number
  hard: number | null
  soft: number | null
  thick?: boolean
}) {
  if (hard === null) return <Track value={0} of={1} thick={thick} />
  return (
    <Track
      value={spent}
      of={hard}
      notch={soft}
      thick={thick}
      tone={spent / hard >= 0.85 ? 'warn' : 'accent'}
    />
  )
}

/**
 * LimitsPanel is the period tabs and the two rows they contain.
 *
 * The switch and the figure always describe the window named on the tab above
 * them, and changing the tab re-reads both - which is what makes a panel
 * attached to its tabs the right shape rather than a decorative one.
 */
export function LimitsPanel({
  kicker,
  limits,
  softWhy,
  hardWhy,
  canEdit,
}: {
  kicker: string
  limits: LimitsEditor
  softWhy: string
  hardWhy: string
  canEdit: boolean
}) {
  const pair = limits.draft[limits.period]
  const nothingOn = !pair.soft.on && !pair.hard.on
  const halfTyped = [pair.soft, pair.hard].some((row) => row.on && row.value.trim() === '')

  return (
    <section className="field">
      <div className="spread" style={{ alignItems: 'baseline' }}>
        <span className="drawer-kicker">{kicker}</span>
        <span className="drawer-note">{WINDOW_NOTE[limits.period]}</span>
      </div>

      <div className="wpanel">
        <div className="wtabs" role="tablist" aria-label="Budget window">
          {WINDOWS.map((each) => (
            <button
              key={each}
              type="button"
              role="tab"
              aria-selected={each === limits.period}
              className={each === limits.period ? 'wtab active' : 'wtab'}
              onClick={() => limits.setPeriod(each)}
            >
              {WINDOW_LABEL[each]}
              {limits.governed[each] && <span className="wtab-mark" aria-label="has a limit" />}
            </button>
          ))}
        </div>

        <div className="wbody">
          <LimitRow
            name="Soft limit"
            why={softWhy}
            limit={pair.soft}
            canEdit={canEdit}
            onChange={(value) => limits.set('soft', { on: true, value })}
            onFlip={() => limits.flip('soft')}
          />
          <LimitRow
            name="Hard limit"
            why={hardWhy}
            limit={pair.hard}
            canEdit={canEdit}
            onChange={(value) => limits.set('hard', { on: true, value })}
            onFlip={() => limits.flip('hard')}
          />
          {canEdit && (nothingOn || halfTyped) && (
            <p className="wnote">
              {halfTyped
                ? 'Type an amount, or switch the limit off.'
                : `Nothing is capped for the ${WINDOW_ADVERB[limits.period]} window. Switch one on and save to cap it.`}
            </p>
          )}
        </div>
      </div>
    </section>
  )
}

function LimitRow({
  name,
  why,
  limit,
  canEdit,
  onChange,
  onFlip,
}: {
  name: string
  why: string
  limit: Limit
  canEdit: boolean
  onChange: (next: string) => void
  onFlip: () => void
}) {
  return (
    <div className="wrow">
      <span className="wrow-text">
        <span className="wrow-name">{name}</span>
        <span className="wrow-why">{why}</span>
      </span>
      <span className="wrow-control">
        {canEdit && (
          <span className="wswitch" role="group" aria-label={name}>
            <button
              type="button"
              className={limit.on ? 'wswitch-side on' : 'wswitch-side'}
              aria-pressed={limit.on}
              onClick={() => {
                if (!limit.on) onFlip()
              }}
            >
              On
            </button>
            <button
              type="button"
              className={limit.on ? 'wswitch-side' : 'wswitch-side on'}
              aria-pressed={!limit.on}
              onClick={() => {
                if (limit.on) onFlip()
              }}
            >
              Off
            </button>
          </span>
        )}
        {limit.on && canEdit ? (
          <input
            className="wvalue"
            value={limit.value}
            aria-label={`${name} amount`}
            inputMode="decimal"
            onChange={(e) => onChange(e.target.value)}
          />
        ) : (
          <span className="wvalue static">{limit.on ? limit.value : '-'}</span>
        )}
      </span>
    </div>
  )
}

/**
 * CapGrid is every cap that reaches the subject, scope down and window across.
 *
 * Not a single column of "what actually stops them", which could only ever
 * answer for one window and so quietly implies there is one. There are three,
 * they are set independently, and an administrator raising a monthly cap needs
 * to see the daily circuit breaker that will then stop the person first.
 *
 * The binding cell - the smallest hard limit set in that window - is the only
 * accent on the grid, and its row's label goes with it, so "what stops them"
 * reads off one line rather than being worked out from twelve figures.
 */
export function CapGrid({
  kicker,
  scopes,
  period,
  note,
  warn,
}: {
  kicker: string
  scopes: CapScope[]
  period: Window
  note: string
  warn?: boolean
}) {
  const binding = bindingScope(scopes)

  return (
    <section className="capgrid">
      <div className="spread" style={{ alignItems: 'baseline' }}>
        <span className="drawer-kicker">{kicker}</span>
        <span className="capgrid-legend">
          <span>
            <i className="swatch hard" aria-hidden />
            hard
          </span>
          <span>
            <i className="swatch soft" aria-hidden />
            soft
          </span>
        </span>
      </div>

      <div className="capgrid-table">
        <span className="capgrid-head">Scope</span>
        {WINDOWS.map((each) => (
          <span
            key={each}
            className={each === period ? 'capgrid-head col on' : 'capgrid-head col'}
          >
            {WINDOW_LABEL[each]}
          </span>
        ))}

        {scopes.map((scope, index) => {
          const binds = binding[period] === index
          return (
            <Row
              key={scope.key}
              scope={scope}
              binds={binds}
              period={period}
              bindingIn={(each) => binding[each] === index}
            />
          )
        })}
      </div>

      <p className={warn ? 'capgrid-note warn' : 'capgrid-note'}>{note}</p>
    </section>
  )
}

function Row({
  scope,
  binds,
  period,
  bindingIn,
}: {
  scope: CapScope
  binds: boolean
  period: Window
  bindingIn: (period: Window) => boolean
}) {
  return (
    <>
      <span className={binds ? 'capgrid-scope binds' : 'capgrid-scope'}>
        <i className="dot" aria-hidden />
        <span className="truncate">{scope.label}</span>
      </span>
      {WINDOWS.map((each) => {
        const hard = scope.hard[each]
        const soft = scope.soft[each]
        return (
          <span key={each} className={each === period ? 'capgrid-cell on' : 'capgrid-cell'}>
            <span className={bindingIn(each) ? 'hard binds' : hard === null ? 'hard none' : 'hard'}>
              {hard === null ? '-' : money(hard)}
            </span>
            <span className={soft === null ? 'soft none' : 'soft'}>
              {soft === null ? '·' : money(soft)}
            </span>
          </span>
        )
      })}
    </>
  )
}

/** stopNote says, in words, what the accent cell means for the window on
 *  screen. Derived from the same binding index the grid tints, because a
 *  sentence disagreeing with the figure above it is worse than no sentence. */
export function stopNote(scopes: CapScope[], period: Window, subject: 'person' | 'team'): string {
  const index = bindingScope(scopes)[period]
  const adverb = WINDOW_ADVERB[period]
  const binding = index === null ? undefined : scopes[index]

  if (index === null || binding === undefined) {
    return subject === 'person'
      ? `Nothing caps this account for the ${adverb} window, and nothing above them is capped either.`
      : `Nothing governs this team for the ${adverb} window, so nothing stops its members.`
  }
  const figure = money(binding.hard[period] ?? 0)

  if (index === 0) {
    return subject === 'person'
      ? `Their own ${adverb} limit is tighter than anything above them, so it is what stops them.`
      : `Members without a tighter cap of their own stop at ${figure} per ${WINDOW_NOUN[period]}.`
  }
  // "Organization" is the only scope label that is not a name, so it takes an
  // article; "Innovation team" drops the word and keeps the name.
  const above =
    binding.key === 'org' ? 'the organization' : binding.label.replace(/ team$/, '')
  return subject === 'person'
    ? `No personal ${adverb} cap, so ${above}’s limit of ${figure} is what stops them. Set a personal cap to cut them off earlier.`
    : `No ${adverb} limit here, so ${above}’s ${figure} is what stops this team.`
}
