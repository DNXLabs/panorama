// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { money } from './format'
import type { BudgetRow, PersonRow, TeamRow } from './types'

// What the People & teams screen works out for itself.
//
// Every derivation here is made once and read in three places - the table, the
// drawers, the attention strip. Two of them computing "what stops this person"
// slightly differently would be two answers to the question the screen exists
// to answer, and the disagreement would surface as an amber warning next to a
// limit that is in fact set.

/** After this long without a request, an account reads as dormant rather than
 *  active. A fortnight: long enough to survive a holiday, short enough that a
 *  seat nobody uses is visible before the next invoice. */
export const DORMANT_DAYS = 14

export type Status = 'active' | 'unused' | 'dormant' | 'disabled'

/**
 * statusOf is what an administrator is actually looking for in a roster.
 *
 * Only two of these are stored: an account is active or disabled. "Never used"
 * and "dormant" are derived from when the gateway last saw a request, because
 * an account that exists and has never sent one is the most common thing on a
 * new deployment and the one nobody thinks to look for.
 */
export function statusOf(person: PersonRow): Status {
  if (person.status === 'disabled') return 'disabled'
  if (!person.last_request_at) return 'unused'
  const days = (Date.now() - new Date(person.last_request_at).getTime()) / 86_400_000
  return days > DORMANT_DAYS ? 'dormant' : 'active'
}

export function membersOf(team: TeamRow, users: PersonRow[]): PersonRow[] {
  return users.filter((p) => p.teams.some((t) => t.id === team.id))
}

export function leadsOf(team: TeamRow, users: PersonRow[]): Set<string> {
  return new Set(
    membersOf(team, users)
      .filter((m) => m.teams.some((t) => t.id === team.id && t.role === 'lead'))
      .map((m) => m.id),
  )
}

/* ---------------------------------------------------------------- windows -- */

/**
 * A budget's window. The scope and the window together are a budget's
 * identity - two limits on one person for one window would make "which one did
 * I hit" unanswerable - so every derivation below is keyed by both.
 */
export type Window = 'day' | 'week' | 'month'

export const WINDOWS: Window[] = ['day', 'week', 'month']

export const WINDOW_LABEL: Record<Window, string> = {
  day: 'Daily',
  week: 'Weekly',
  month: 'Monthly',
}

/** The window as a noun, for "per month". */
export const WINDOW_NOUN: Record<Window, string> = { day: 'day', week: 'week', month: 'month' }

/** The window as an adverb, for "their monthly limit". */
export const WINDOW_ADVERB: Record<Window, string> = {
  day: 'daily',
  week: 'weekly',
  month: 'monthly',
}

/** Which clock a window runs on. Said beside the tabs rather than left to be
 *  discovered by a budget resetting at a time nobody expected. */
export const WINDOW_NOTE: Record<Window, string> = {
  day: 'UTC day, nothing carries forward',
  week: 'UTC week from Monday, nothing carries forward',
  month: 'UTC month, nothing carries forward',
}

/** Limits is one scope's ceilings across every window, in nanodollars, null
 *  where nothing is set. A limit that is not set is null and never zero: zero
 *  is a limit that stops everything. */
export type Limits = Record<Window, number | null>

export const NO_LIMITS: Limits = { day: null, week: null, month: null }

/** budgetAt is the one budget on a scope for one window, which is the only
 *  granularity the server stores or enforces at. */
export function budgetAt(
  budgets: BudgetRow[],
  scope: string,
  scopeID: string,
  period: Window,
): BudgetRow | undefined {
  return budgets.find(
    (b) => b.scope === scope && (b.scope_id ?? '') === scopeID && b.period === period,
  )
}

/** limitsOf is every window's soft and hard ceiling for one scope, which is
 *  what the cap grid draws a row from. */
export function limitsOf(
  budgets: BudgetRow[],
  scope: string,
  scopeID: string,
): { hard: Limits; soft: Limits } {
  const hard: Limits = { ...NO_LIMITS }
  const soft: Limits = { ...NO_LIMITS }
  for (const period of WINDOWS) {
    const budget = budgetAt(budgets, scope, scopeID, period)
    hard[period] = budget?.limits.hard ?? null
    soft[period] = budget?.limits.soft ?? null
  }
  return { hard, soft }
}

/** One row of the cap grid: a scope, and every ceiling it imposes. */
export type CapScope = {
  key: string
  label: string
  hard: Limits
  soft: Limits
}

/**
 * bindingScope is which row of the grid actually stops the subject, per window.
 *
 * The smallest hard limit set in that window, and nothing else - a soft limit
 * warns and a route stops, but neither is what cuts anybody off. Null where no
 * scope caps that window at all, which is the state the grid puts in amber
 * rather than leaving to be inferred from three em dashes.
 */
export function bindingScope(scopes: CapScope[]): Record<Window, number | null> {
  const out: Record<Window, number | null> = { day: null, week: null, month: null }
  for (const period of WINDOWS) {
    let index: number | null = null
    let tightest = 0
    for (let i = 0; i < scopes.length; i++) {
      const value = scopes[i]?.hard[period]
      if (value === null || value === undefined) continue
      if (index === null || value < tightest) {
        index = i
        tightest = value
      }
    }
    out[period] = index
  }
  return out
}

/* ---------------------------------------------------------------- budgets -- */

/**
 * budgetFor finds the budget on a subject.
 *
 * Monthly first when a subject has more than one window, because a month is
 * what an administrator means by "their budget" - a daily ceiling beside it is
 * a circuit breaker rather than the limit anybody negotiated.
 */
export function budgetFor(
  budgets: BudgetRow[],
  scope: string,
  scopeID: string,
): BudgetRow | undefined {
  const mine = budgets.filter((b) => b.scope === scope && (b.scope_id ?? '') === scopeID)
  return mine.find((b) => b.period === 'month') ?? mine[0]
}

/**
 * cappingBudget is the budget on a subject that actually stops it.
 *
 * Only the windows carrying a hard limit are candidates, because a budget with
 * a soft limit alone cuts nobody off - and a person whose only cap is a weekly
 * one is capped, which "the monthly budget, if any" would have reported as
 * uncapped.
 */
export function cappingBudget(
  budgets: BudgetRow[],
  scope: string,
  scopeID: string,
): BudgetRow | undefined {
  const capped = budgets.filter(
    (b) => b.scope === scope && (b.scope_id ?? '') === scopeID && b.limits.hard,
  )
  return capped.find((b) => b.period === 'month') ?? capped[0]
}

export function teamBudget(budgets: BudgetRow[], teamID: string): BudgetRow | undefined {
  return budgetFor(budgets, 'team', teamID)
}

export function orgBudget(budgets: BudgetRow[]): BudgetRow | undefined {
  return budgetFor(budgets, 'org', '')
}

export function orgHard(budgets: BudgetRow[]): number | null {
  return cappingBudget(budgets, 'org', '')?.limits.hard ?? null
}

/**
 * hardLabel is a hard limit with its window, and "not set" when there is none.
 *
 * The window is named whenever it is not a month, because a chain that put "$20"
 * next to "$18,000" without saying one is a day and the other a month would read
 * as an absurdly tight personal cap rather than as a circuit breaker.
 */
export function hardLabel(budget?: BudgetRow): string {
  const hard = budget?.limits.hard
  if (!hard) return 'not set'
  return budget!.period === 'month' ? money(hard) : `${money(hard)} ${everyWindow(budget!)}`
}

/** windowSuffix names a budget's window only when it is not a month.
 *
 *  Same rule as hardLabel, for the same reason: a month is what an
 *  administrator means by "their budget", so saying "monthly" on every line
 *  that mentions one is noise - and saying "weekly" on the one line that is
 *  not a month is the whole point. */
export function windowSuffix(budget?: BudgetRow): string {
  if (!budget || budget.period === 'month') return ''
  return ` · ${everyWindow(budget)}`
}

/** everyWindow reads a period as an adverb: daily, weekly, monthly. */
export function everyWindow(budget: BudgetRow): string {
  return WINDOW_ADVERB[(budget.period as Window) ?? 'month'] ?? 'monthly'
}

/**
 * teamGovernor is the tightest team hard limit above somebody.
 *
 * A team's hard limit always governs its members: a personal cap does not
 * replace it, it only cuts in earlier. Somebody on two teams is stopped by
 * whichever is tighter, which is why this is a minimum rather than the first
 * team that happens to have one.
 */
export function teamGovernor(
  person: PersonRow,
  budgets: BudgetRow[],
): { teamID: string; hard: number; budget: BudgetRow } | null {
  let best: { teamID: string; hard: number; budget: BudgetRow } | null = null
  for (const team of person.teams) {
    const budget = cappingBudget(budgets, 'team', team.id)
    const hard = budget?.limits.hard
    if (!hard || !budget) continue
    if (best === null || hard < best.hard) best = { teamID: team.id, hard, budget }
  }
  return best
}

export type CapKind = 'personal' | 'team' | 'org' | 'none'

/** capKind is which rung binds, and what the flag strip selects on. */
export function capKind(person: PersonRow, budgets: BudgetRow[]): CapKind {
  if (cappingBudget(budgets, 'user', person.id)) return 'personal'
  if (teamGovernor(person, budgets) !== null) return 'team'
  if (orgHard(budgets) !== null) return 'org'
  return 'none'
}

/**
 * capFor is the two lines the roster's "what stops them" column shows.
 *
 * The word "unlimited" appears nowhere. A limit nobody set is not permission to
 * spend without one - it means the rung above is what stops you, and when none
 * of them is set, that is a fact worth putting in amber rather than a
 * reassurance.
 */
export function capFor(
  person: PersonRow,
  budgets: BudgetRow[],
  teams: TeamRow[],
): { head: string; sub: string; uncapped: boolean } {
  const own = cappingBudget(budgets, 'user', person.id)
  const governor = teamGovernor(person, budgets)

  if (own) {
    return { head: 'Personal cap', sub: `${hardLabel(own)} hard`, uncapped: false }
  }
  if (governor !== null) {
    const teamName = teams.find((t) => t.id === governor.teamID)?.name ?? 'their team'
    return { head: 'Team budget', sub: `${teamName} ${hardLabel(governor.budget)}`, uncapped: false }
  }
  const org = cappingBudget(budgets, 'org', '')
  if (org) {
    return {
      head: 'Organization',
      sub: `${hardLabel(org)} across the organization`,
      uncapped: false,
    }
  }
  return { head: 'No cap', sub: 'nothing governs this account', uncapped: true }
}

/**
 * seedHard is what turning a limit on starts at.
 *
 * The point of the switch is to say "cap this", not to make somebody invent a
 * number before they can see what the screen does with one. Every seed is
 * meant to be edited.
 */
export function seedHard(scope: 'user' | 'team' | 'org'): string {
  return scope === 'user' ? '500.00' : scope === 'team' ? '10000.00' : '25000.00'
}

/** seedSoft sits below the hard limit it warns about, so the warning is
 *  reachable - a soft limit above a hard one never fires. */
export function seedSoft(hard: string, scope: 'user' | 'team' | 'org'): string {
  const base = Number(hard.trim() === '' ? seedHard(scope) : hard)
  if (!Number.isFinite(base) || base <= 0) return ''
  const share = scope === 'user' ? 0.7 : 0.75
  return (Math.round((base * share) / 10) * 10).toFixed(2)
}

/** slugify mirrors the server's rule, so the slug shown before a team is
 *  created is the slug it gets. */
export function slugify(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
}
