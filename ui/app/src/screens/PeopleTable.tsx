// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { ago, count, money } from '../format'
import {
  capFor,
  cappingBudget,
  leadsOf,
  membersOf,
  orgBudget,
  statusOf,
  teamBudget,
  windowSuffix,
} from '../people'
import type { BudgetRow, PersonRow, TeamRow } from '../types'
import { Avatar } from '../ui'
import { SpendTrack } from './PeopleBudget'
import { StatusPill } from './PeopleDrawers'

// The roster, the flag strip above it, and the team cards beside it.
//
// The one column this table does not have is spend. That is the second trust
// commitment and it is held by the server - the endpoint carries no per-user
// money - but it is worth saying here too, because a table is exactly where
// somebody would add the column back.

/** The roster's tracks. Every one but the person and the governing cap is
 *  fixed: a grid row is its own container, so a track sized from content would
 *  resolve to a different width on every row and step every column after it. */
export const ROSTER_COLUMNS = 'minmax(0, 1.5fr) 150px minmax(0, 1.1fr) 110px 120px 110px'

/**
 * Flag is one fact worth acting on, and the way to act on it.
 *
 * A hairline row rather than three cards. Cards would say the same three
 * sentences in three bordered boxes above a table that can show the accounts
 * they are about - a third of the screen to be a filter with a paragraph
 * attached. This is the filter, and clicking it is what it is for.
 */
export function Flag({
  tone,
  label,
  active,
  onClick,
}: {
  tone: 'warn' | 'accent'
  label: string
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      className={active ? 'flag on' : 'flag'}
      aria-pressed={active}
      onClick={onClick}
    >
      <span className={`dot ${tone}`} aria-hidden />
      <span>{label}</span>
    </button>
  )
}

export function PersonRowView({
  person,
  budgets,
  teams,
  mapped,
  onOpen,
  onAssign,
}: {
  person: PersonRow
  budgets: BudgetRow[]
  teams: TeamRow[]
  /** Whether a group mapping exists for a role to have come from. Without one
   *  every account is assigned by hand and the mark says nothing. */
  mapped: boolean
  onOpen: () => void
  onAssign: () => void
}) {
  const cap = capFor(person, budgets, teams)

  return (
    <div
      className="roster-row"
      style={{ gridTemplateColumns: ROSTER_COLUMNS }}
      role="button"
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onOpen()
        }
      }}
    >
      <span className="roster-person">
        <Avatar name={person.display_name || person.email} />
        <span style={{ minWidth: 0 }}>
          <span className="name truncate">{person.display_name || person.email}</span>
          <span className="email truncate">{person.email}</span>
        </span>
      </span>

      {/* Two teams and a count, never a wrapping cell: the row is a fixed
          height because the list is virtualised, and somebody on five teams
          would push their own name out of view. */}
      <span className="roster-teams">
        {person.teams.length === 0 ? (
          <button
            type="button"
            className="assign"
            onClick={(e) => {
              e.stopPropagation()
              onAssign()
            }}
          >
            + assign
          </button>
        ) : (
          <>
            {person.teams.slice(0, 2).map((team) => (
              <span key={team.id} className="chip truncate">
                {team.name}
              </span>
            ))}
            {person.teams.length > 2 && (
              <span className="chip" title={person.teams.map((t) => t.name).join(', ')}>
                +{person.teams.length - 2}
              </span>
            )}
          </>
        )}
      </span>

      <span className="roster-cap">
        <span className={cap.uncapped ? 'head warn' : 'head'}>{cap.head}</span>
        <span className="sub truncate">{cap.sub}</span>
      </span>

      <span className="roster-meta">
        {person.last_request_at ? ago(person.last_request_at) : 'never'}
      </span>

      <span className="roster-role">
        {person.role}
        {/* A role somebody assigned outlives the next sign-in and a role the
            group mapping granted does not, which is the difference between a
            promotion that holds and one that quietly reverts. */}
        {mapped && person.role_source === 'manual' && (
          <span
            className="profile-pill"
            title="Assigned in Settings › Single sign-on, so the group mapping will not change it."
          >
            pinned
          </span>
        )}
      </span>

      <span style={{ display: 'flex', justifyContent: 'flex-end' }}>
        <StatusPill status={statusOf(person)} budgetState={person.budget_state} />
      </span>
    </div>
  )
}

/* ------------------------------------------------------------------ teams -- */

export function OrgCard({ budgets, onOpen }: { budgets: BudgetRow[]; onOpen: () => void }) {
  // The capping budget for the figures, the display budget for the spend: a
  // month with a soft limit and no hard one still has spend worth showing.
  const budget = cappingBudget(budgets, 'org', '') ?? orgBudget(budgets)
  const hard = budget?.limits.hard ?? null
  const soft = budget?.limits.soft ?? null
  const spent = budget?.spent_nanos ?? 0

  return (
    <button type="button" className="tcard outermost" onClick={onOpen}>
      <span className="tcard-head">
        <span style={{ minWidth: 0 }}>
          <span className="tcard-name">This organization</span>
          <span className="tcard-slug">all teams · all people</span>
        </span>
        <span className="profile-pill">outermost</span>
      </span>

      <span className="tcard-lead">
        <span className="none">Everything a team or a person spends is inside it.</span>
      </span>

      {/* The same budget block as a team's, because it is the same question one
          rung out - and a card that answered it differently would make the
          outermost limit the one thing on this screen nobody could read at a
          glance. */}
      <span>
        <span className="tcard-line">
          <span>Budget</span>
          <span className={hard === null ? 'warn' : undefined}>
            {hard === null ? 'not set' : `${money(spent)} of ${money(hard)}`}
          </span>
        </span>
        <SpendTrack spent={spent} hard={hard} soft={soft} />
        <span className="tcard-note mono">
          {hard === null
            ? 'Nothing caps the organization as a whole'
            : soft !== null
              ? `Soft ${money(soft)} → routes that stop${windowSuffix(budget)}`
              : `No soft limit - no warning before the block${windowSuffix(budget)}`}
        </span>
      </span>
    </button>
  )
}

export function TeamCard({
  team,
  users,
  budgets,
  onOpen,
}: {
  team: TeamRow
  users: PersonRow[]
  budgets: BudgetRow[]
  onOpen: () => void
}) {
  const members = membersOf(team, users)
  const leads = leadsOf(team, users)
  const lead = members.find((m) => leads.has(m.id))
  const budget = cappingBudget(budgets, 'team', team.id) ?? teamBudget(budgets, team.id)
  const hard = budget?.limits.hard ?? null
  const soft = budget?.limits.soft ?? null
  const spent = budget?.spent_nanos ?? 0
  const warn = hard !== null && spent / hard >= 0.85

  return (
    <button type="button" className="tcard" onClick={onOpen}>
      <span className="tcard-head">
        <span style={{ minWidth: 0 }}>
          <span className="tcard-name truncate">{team.name}</span>
          <span className="tcard-slug truncate">{team.slug}</span>
        </span>
        <span className="tcard-count">
          {count(members.length)} {members.length === 1 ? 'member' : 'members'}
        </span>
      </span>

      <span className="tcard-lead">
        {lead ? (
          <>
            <Avatar name={lead.display_name || lead.email} size="sm" />
            <span className="truncate">{lead.display_name || lead.email}</span>
          </>
        ) : (
          <span className="none">No lead assigned</span>
        )}
      </span>

      <span>
        <span className="tcard-line">
          <span>Budget</span>
          <span className={hard === null || warn ? 'warn' : undefined}>
            {hard === null ? 'not set' : `${money(spent)} of ${money(hard)}`}
          </span>
        </span>
        <SpendTrack spent={spent} hard={hard} soft={soft} />
        <span className="tcard-note mono">
          {/* Never "reroutes to a cheaper model": past a soft limit a route
              stops and names the ones that still serve. The engineer switches;
              nothing switches under them. */}
          {soft !== null
            ? `Soft ${money(soft)} → routes that stop${windowSuffix(budget)}`
            : hard === null
              ? 'No limit - nothing stops this team'
              : `No soft limit - no warning before the block${windowSuffix(budget)}`}
        </span>
      </span>
    </button>
  )
}

export function NewTeamCard({ onClick }: { onClick: () => void }) {
  return (
    <button type="button" className="tcard tcard-new" onClick={onClick}>
      <span className="plus">+</span>
      <span>New team</span>
    </button>
  )
}
