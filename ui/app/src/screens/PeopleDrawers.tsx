// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from 'react'
import { del, patch, post } from '../api'
import { ago, count, dateTime, money } from '../format'
import {
  WINDOWS,
  leadsOf,
  limitsOf,
  membersOf,
  orgBudget,
  slugify,
  statusOf,
  teamBudget,
} from '../people'
import type { CapScope, Status } from '../people'
import type { BudgetRow, PersonRow, Role, TeamRow } from '../types'
import {
  ActionButton,
  Avatar,
  Drawer,
  ErrorBanner,
  Pill,
  Select,
  SubmitButton,
  useAction,
} from '../ui'
import { Link } from '../router'
import { CapGrid, LimitsPanel, SpendTrack, stopNote, useLimits } from './PeopleBudget'
import { PeoplePicker, TeamPicker } from './Pickers'

// The four things this screen edits one at a time: a person, a team, the
// deployment they both sit inside, and a team that does not exist yet.
//
// Panels rather than dialogs, for the same reason a route is: what is being
// decided here is comparative - is this cap tighter than the team's, does
// anybody else lead this team - and a box in the middle of the screen hides the
// list that answers it.
//
// NOTHING SAVES ON ITS OWN. A drawer holds every edit - the role, three windows
// of limits, the roster - until Save changes commits them together, and Cancel
// discards them. The two destructive actions are the exception and are meant to
// be: deactivating an account and deleting a team are not part of the form, and
// staging them behind the same button that saves a limit is how somebody
// deletes a team while raising its budget.

export function StatusPill({ status, budgetState }: { status: Status; budgetState?: string }) {
  if (budgetState === 'over_hard') return <Pill tone="off">at limit</Pill>
  if (budgetState === 'over_soft') return <Pill tone="warn">over soft</Pill>
  if (status === 'disabled') return <Pill tone="off">disabled</Pill>
  if (status === 'unused') return <Pill tone="warn">never used</Pill>
  if (status === 'dormant') return <Pill tone="off">dormant</Pill>
  // Untoned, deliberately. `good` is the accent, and the accent on this screen
  // means the binding cap and the active tab - an "active" tag in it on every
  // row of the roster is the one thing that would drown the grid's one mark.
  return <Pill>active</Pill>
}

const ROLES: { id: Role; help: string }[] = [
  { id: 'admin', help: 'Sees every team aggregate, mints tokens, sets budgets and routes.' },
  { id: 'manager', help: 'Sees the aggregates of teams they lead, and sets caps inside them.' },
  {
    id: 'developer',
    help: 'Sees their own usage and the budget that governs them, nothing else.',
  },
]

/* ----------------------------------------------------------------- person -- */

export function PersonDrawer({
  person,
  me,
  users,
  teams,
  budgets,
  canEdit,
  picking,
  onPick,
  onClose,
  onSaved,
}: {
  person: PersonRow
  me: string
  users: PersonRow[]
  teams: TeamRow[]
  budgets: BudgetRow[]
  canEdit: boolean
  /** Opened straight onto the team picker, which is what "+ assign" in the
   *  roster does - the row it was clicked on is the person being edited, so
   *  there is nothing to choose between opening the drawer and opening the
   *  picker over it. */
  picking: boolean
  onPick: (open: boolean) => void
  onClose: () => void
  onSaved: (message: string) => void
}) {
  const name = person.display_name || person.email
  // An administrator may cap themselves and change which teams they are on;
  // what they may not do is promote or deactivate their own account, which is
  // the one edit with nobody left to undo it.
  const mine = person.id === me
  const ownAccount = canEdit && !mine

  const [teamIDs, setTeamIDs] = useState<string[]>(person.teams.map((t) => t.id))
  const limits = useLimits(budgets, 'user', person.id, name)
  const [error, setError] = useState<unknown>(null)

  const joined = teams.filter((t) => teamIDs.includes(t.id))
  const org = limitsOf(budgets, 'org', '')

  // Personal first, then every team they are on, then the deployment. The order
  // is inside-out on purpose: it is the order the limits actually apply in, so
  // the accent moving down the column reads as "nothing narrower caps them".
  const scopes: CapScope[] = [
    { key: 'personal', label: 'Personal cap', hard: limits.live.hard, soft: limits.live.soft },
    ...joined.map((team) => ({
      key: team.id,
      label: `${team.name} team`,
      ...limitsOf(budgets, 'team', team.id),
    })),
    { key: 'org', label: 'Organization', ...org },
  ]

  const dirty =
    limits.dirty ||
    JSON.stringify([...teamIDs].sort()) !==
      JSON.stringify(person.teams.map((t) => t.id).sort())

  const save = useAction(async () => {
    setError(null)
    try {
      // One call per team that actually changed, rather than rewriting each
      // team's roster: this edits one person, and a roster write would
      // overwrite whatever somebody else changed meanwhile.
      const before = person.teams.map((t) => t.id)
      for (const team of teams) {
        const was = before.includes(team.id)
        const now = teamIDs.includes(team.id)
        if (was === now) continue
        await post(`/api/v1/teams/${team.slug}`, { email: person.email, remove: !now })
      }

      await limits.save()
      onSaved(`${name} saved. In force from their next request.`)
    } catch (cause) {
      setError(cause)
      throw cause
    }
  })

  return (
    <>
      <Drawer
        open
        wide
        title={name}
        sub={<span className="mono">{person.email}</span>}
        onClose={onClose}
        onSubmit={canEdit ? save.act : undefined}
        footer={
          <>
            {ownAccount ? (
              <ActionButton
                danger
                onAct={() =>
                  patch(`/api/v1/users/${person.id}`, {
                    status: person.status === 'disabled' ? 'active' : 'disabled',
                  }).then(() =>
                    onSaved(
                      person.status === 'disabled'
                        ? `${name} can sign in again.`
                        : `${name} is disabled. Their tokens stop working.`,
                    ),
                  )
                }
                label={person.status === 'disabled' ? 'Reactivate account' : 'Deactivate account'}
                settled="Done"
              />
            ) : (
              <span className="drawer-note">{mine ? 'Your own account' : ''}</span>
            )}
            <span className="inline">
              <button type="button" className="btn" onClick={onClose}>
                Cancel
              </button>
              {canEdit && (
                <SubmitButton
                  state={save.state}
                  disabled={!dirty || limits.midEdit}
                  label="Save changes"
                  settled="Saved"
                  primary
                />
              )}
            </span>
          </>
        }
      >
        <ErrorBanner error={error} />

        <div className="drawer-boxes">
          <div className="drawer-box">
            <span className="drawer-kicker">Last request</span>
            <span className="drawer-figure" title={dateTime(person.last_request_at)}>
              {person.last_request_at ? ago(person.last_request_at) : 'never'}
            </span>
          </div>
          <div className="drawer-box">
            <span className="drawer-kicker">Status</span>
            <span className="inline">
              <StatusPill status={statusOf(person)} budgetState={person.budget_state} />
            </span>
          </div>
        </div>

        <section className="field">
          <span className="drawer-kicker">Role</span>
          <span className="inline">
            <span className="pill">{person.role}</span>
            {person.role_source === 'manual' && <span className="profile-pill">pinned</span>}
          </span>
          <span className="drawer-help">{ROLES.find((r) => r.id === person.role)?.help}</span>
          {/* READ-ONLY, AND THE ONLY PLACE THAT SAYS WHERE IT IS DECIDED. A role
              has a rule above it - the group mapping - and an exception to that
              rule is invisible from beside the person it was made for, so the
              control lives beside the rule instead. What is left here is the
              answer and the address of the half that sets it. */}
          <span className="drawer-note">
            {person.role_source === 'manual'
              ? 'Assigned directly, so the group mapping will not change it. '
              : 'Decided by the group mapping at every sign-in. '}
            {canEdit && <Link to="/settings?tab=sso">Settings › Single sign-on</Link>}
          </span>
        </section>

        <LimitsPanel
          kicker="Budget"
          limits={limits}
          canEdit={canEdit}
          softWhy="Past this, the routes marked as not serving over a soft limit refuse them and name the ones that still work."
          hardWhy="Cuts this person off, even if the team above them has room left."
        />

        <CapGrid
          kicker="Every cap that reaches them"
          scopes={scopes}
          period={limits.period}
          note={stopNote(scopes, limits.period, 'person')}
          warn={scopes.every((scope) => scope.hard[limits.period] === null)}
        />

        <section className="field">
          <div className="spread" style={{ alignItems: 'baseline' }}>
            <span className="drawer-kicker">Teams</span>
            {canEdit && (
              <button type="button" className="btn link" onClick={() => onPick(true)}>
                + Add to team
              </button>
            )}
          </div>
          {joined.length === 0 ? (
            <div className="drawer-empty">
              On no team. Their spend rolls up to the organization and nothing narrower governs them.
            </div>
          ) : (
            joined.map((team) => (
              <div key={team.id} className="drawer-row">
                <span className="drawer-row-name">{team.name}</span>
                {person.teams.find((t) => t.id === team.id)?.role === 'lead' && (
                  <span className="profile-pill global">lead</span>
                )}
                {canEdit && (
                  <button
                    type="button"
                    className="btn small"
                    onClick={() => setTeamIDs((ids) => ids.filter((id) => id !== team.id))}
                  >
                    Remove
                  </button>
                )}
              </div>
            ))
          )}
        </section>

        {/* What an administrator can actually check, rather than a fabricated
            activity feed: every line here is a fact the server already holds. */}
        <section className="field">
          <span className="drawer-kicker">Account</span>
          <div>
            <Fact label="Signed up" value={dateTime(person.created_at)} />
            <Fact
              label="First request"
              value={person.first_request_at ? dateTime(person.first_request_at) : '-'}
            />
            <Fact label="Gateway tokens" value={count(person.tokens)} />
            <Fact label="Signs in with SSO" value={person.sso_linked ? 'yes' : 'no'} />
            <Fact
              label="Tools seen"
              value={person.tools && person.tools.length > 0 ? person.tools.join(', ') : '-'}
            />
          </div>
        </section>
      </Drawer>

      {/* Over the panel, never in place of it: the panel is the thing being
          filled in, and replacing it to choose what goes in it is how a
          half-made edit gets lost. */}
      {picking && canEdit && (
        <TeamPicker
          title={`Teams for ${name}`}
          sub="Select every team this person belongs to. Unchecking removes them."
          teams={teams}
          users={users}
          selected={teamIDs}
          confirmLabel="Save teams"
          onClose={() => onPick(false)}
          onApply={(next) => {
            setTeamIDs(next)
            onPick(false)
          }}
        />
      )}
    </>
  )
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div className="drawer-fact">
      <span className="key">{label}</span>
      <span className="value">{value}</span>
    </div>
  )
}

/* ------------------------------------------------------------------- team -- */

export function TeamDrawer({
  team,
  users,
  teams,
  budgets,
  canEdit,
  picking,
  onPick,
  onClose,
  onSaved,
  onDeleted,
}: {
  team: TeamRow
  users: PersonRow[]
  teams: TeamRow[]
  budgets: BudgetRow[]
  canEdit: boolean
  picking: boolean
  onPick: (open: boolean) => void
  onClose: () => void
  onSaved: (message: string) => void
  onDeleted: (message: string) => void
}) {
  const stored = membersOf(team, users)
  const storedLeads = leadsOf(team, users)

  const [memberIDs, setMemberIDs] = useState<string[]>(stored.map((m) => m.id))
  const [leads, setLeads] = useState<string[]>([...storedLeads])
  const limits = useLimits(budgets, 'team', team.id, team.name)
  const [error, setError] = useState<unknown>(null)

  const members = users.filter((u) => memberIDs.includes(u.id))
  const budget = teamBudget(budgets, team.id)
  const spent = budget?.spent_nanos ?? 0
  const hard = budget?.limits.hard ?? null

  const scopes: CapScope[] = [
    { key: team.id, label: `${team.name} team`, hard: limits.live.hard, soft: limits.live.soft },
    { key: 'org', label: 'Organization', ...limitsOf(budgets, 'org', '') },
  ]

  const roster = () =>
    memberIDs.map((id) => ({ user_id: id, role: leads.includes(id) ? 'lead' : 'member' }))

  const rosterChanged =
    JSON.stringify([...memberIDs].sort()) !== JSON.stringify(stored.map((m) => m.id).sort()) ||
    JSON.stringify([...leads].sort()) !== JSON.stringify([...storedLeads].sort())

  const dirty = limits.dirty || rosterChanged

  const save = useAction(async () => {
    setError(null)
    try {
      // The roster is written whole, in one transaction. Translating a screen
      // full of checkboxes into a stream of add and remove calls leaves a
      // half-applied roster on the first failure, with nothing on the screen
      // saying which half landed.
      //
      // Only when it changed, though: a replace is how somebody else's
      // concurrent edit gets undone, and saving a budget is no reason to
      // rewrite a roster nobody touched.
      if (rosterChanged) await post(`/api/v1/teams/${team.slug}`, { members: roster() })
      await limits.save()
      onSaved(`${team.name} saved. In force from the next request.`)
    } catch (cause) {
      setError(cause)
      throw cause
    }
  })

  return (
    <>
      <Drawer
        open
        wide
        title={team.name}
        sub={<span className="mono">{team.slug}</span>}
        onClose={onClose}
        onSubmit={canEdit ? save.act : undefined}
        footer={
          <>
            {canEdit ? (
              <ActionButton
                danger
                onAct={() =>
                  del(`/api/v1/teams/${team.slug}`).then(() =>
                    onDeleted(`${team.name} deleted. Its budget stays until somebody removes it.`),
                  )
                }
                label="Delete team"
                settled="Deleted"
              />
            ) : (
              <span />
            )}
            <span className="inline">
              <button type="button" className="btn" onClick={onClose}>
                Cancel
              </button>
              {canEdit && (
                <SubmitButton
                  state={save.state}
                  disabled={!dirty || limits.midEdit}
                  label="Save changes"
                  settled="Saved"
                  primary
                />
              )}
            </span>
          </>
        }
      >
        <ErrorBanner error={error} />

        <div className="drawer-box wide">
          <div className="spread" style={{ alignItems: 'baseline' }}>
            <span className="drawer-kicker">
              Spend this {budget ? budget.period : 'month'}
            </span>
            <span
              className="drawer-figure"
              style={{ color: hard === null ? 'var(--state-warn)' : undefined }}
            >
              {budget === undefined
                ? 'no budget set'
                : hard === null
                  ? `${money(spent)} · no hard limit`
                  : `${money(spent)} of ${money(hard)}`}
            </span>
          </div>
          <SpendTrack spent={spent} hard={hard} soft={budget?.limits.soft ?? null} thick />
          {budget?.unpriced_requests !== undefined && budget.unpriced_requests > 0 && (
            <span className="drawer-note">
              {count(budget.unpriced_requests)} requests in this window had no published rate and
              are not in that figure.
            </span>
          )}
        </div>

        <LimitsPanel
          kicker="Limits"
          limits={limits}
          canEdit={canEdit}
          softWhy="Past this, the routes marked as not serving over a soft limit refuse the team and name the ones that still work."
          hardWhy="Cuts off every member without a tighter cap of their own."
        />

        <CapGrid
          kicker="Every cap that reaches this team"
          scopes={scopes}
          period={limits.period}
          note={stopNote(scopes, limits.period, 'team')}
          warn={scopes.every((scope) => scope.hard[limits.period] === null)}
        />

        <section className="field">
          <div className="spread" style={{ alignItems: 'baseline' }}>
            <span className="drawer-kicker">Members · {count(members.length)}</span>
            {canEdit && (
              <button type="button" className="btn link" onClick={() => onPick(true)}>
                + Add people
              </button>
            )}
          </div>
          {members.length === 0 ? (
            <div className="drawer-empty">
              No members yet. A team with nobody on it aggregates nothing.
            </div>
          ) : (
            members.map((person) => {
              const lead = leads.includes(person.id)
              return (
                <div key={person.id} className="drawer-row">
                  <Avatar name={person.display_name || person.email} size="sm" />
                  <span className="drawer-row-text">
                    <span className="name truncate">{person.display_name || person.email}</span>
                    <span className="sub">{person.role}</span>
                  </span>
                  {canEdit && (
                    <>
                      <button
                        type="button"
                        className={lead ? 'wtoggle on' : 'wtoggle'}
                        aria-pressed={lead}
                        onClick={() =>
                          setLeads((current) =>
                            lead
                              ? current.filter((id) => id !== person.id)
                              : [...current, person.id],
                          )
                        }
                      >
                        {lead ? 'lead' : 'make lead'}
                      </button>
                      <button
                        type="button"
                        className="btn small"
                        onClick={() =>
                          setMemberIDs((ids) => ids.filter((id) => id !== person.id))
                        }
                      >
                        Remove
                      </button>
                    </>
                  )}
                  {!canEdit && lead && <span className="profile-pill global">lead</span>}
                </div>
              )
            })
          )}
        </section>
      </Drawer>

      {picking && (
        <PeoplePicker
          title={`Members of ${team.name}`}
          sub="Check to add, uncheck to remove. People can belong to more than one team."
          users={users}
          teams={teams}
          selected={memberIDs}
          confirmLabel="Save members"
          onClose={() => onPick(false)}
          onApply={(next) => {
            setMemberIDs(next)
            // A lead who is removed from the roster stops being one; a lead who
            // stays keeps it. Rewriting the roster is not a way to quietly
            // demote somebody.
            setLeads((current) => current.filter((id) => next.includes(id)))
            onPick(false)
          }}
        />
      )}
    </>
  )
}

/* --------------------------------------------------------------- the org -- */

export function OrgDrawer({
  budgets,
  canEdit,
  onClose,
  onSaved,
}: {
  budgets: BudgetRow[]
  canEdit: boolean
  onClose: () => void
  onSaved: (message: string) => void
}) {
  const limits = useLimits(budgets, 'org', '', 'the organization')
  const [error, setError] = useState<unknown>(null)

  const budget = orgBudget(budgets)
  const spent = budget?.spent_nanos ?? 0
  const hard = budget?.limits.hard ?? null

  const scopes: CapScope[] = [
    { key: 'org', label: 'Organization', hard: limits.live.hard, soft: limits.live.soft },
  ]

  const save = useAction(async () => {
    setError(null)
    try {
      await limits.save()
      onSaved('Organization limits saved. In force from the next request.')
    } catch (cause) {
      setError(cause)
      throw cause
    }
  })

  return (
    <Drawer
      open
      wide
      title="This organization"
      sub="The outermost bound. Everything a team or a person spends is inside it."
      onClose={onClose}
      onSubmit={canEdit ? save.act : undefined}
      footer={
        <>
          <span />
          <span className="inline">
            <button type="button" className="btn" onClick={onClose}>
              Cancel
            </button>
            {canEdit && (
              <SubmitButton
                state={save.state}
                disabled={!limits.dirty || limits.midEdit}
                label="Save changes"
                settled="Saved"
                primary
              />
            )}
          </span>
        </>
      }
    >
      <ErrorBanner error={error} />

      <div className="drawer-box wide">
        <div className="spread" style={{ alignItems: 'baseline' }}>
          <span className="drawer-kicker">Spend this {budget ? budget.period : 'month'}</span>
          <span className="drawer-figure">
            {money(spent)}
            {hard === null && <span style={{ color: 'var(--state-warn)' }}> · no budget yet</span>}
            {hard !== null && ` of ${money(hard)}`}
          </span>
        </div>
        <SpendTrack spent={spent} hard={hard} soft={budget?.limits.soft ?? null} thick />
      </div>

      <LimitsPanel
        kicker="Limits"
        limits={limits}
        canEdit={canEdit}
        softWhy="Past this, every route marked as not serving over a soft limit refuses, organization-wide."
        hardWhy="Blocks every request in the organization, whoever sends it."
      />

      {/* One row, deliberately: a deployment can have hundreds of teams, and a
          row per team here would be a second, worse teams tab. */}
      <CapGrid
        kicker="Limits set here"
        scopes={scopes}
        period={limits.period}
        note={
          WINDOWS.some((each) => limits.live.hard[each] !== null)
            ? 'Everything a team or a person spends counts against these.'
            : 'Nothing caps the organization as a whole. A team or a personal cap is the only thing stopping anybody.'
        }
        warn={WINDOWS.every((each) => limits.live.hard[each] === null)}
      />
    </Drawer>
  )
}

/* --------------------------------------------------------------- new team -- */

/**
 * NewTeamDrawer creates a team, and sets no budget.
 *
 * A limit belongs on the team once the team exists: asking for one here made
 * the form four decisions long, and a monthly figure typed before anybody is on
 * the roster is a guess. The drawer says so in one line instead.
 */
export function NewTeamDrawer({
  users,
  teams,
  seed,
  onClose,
  onCreated,
}: {
  users: PersonRow[]
  teams: TeamRow[]
  seed?: string
  onClose: () => void
  onCreated: (message: string) => void
}) {
  const [name, setName] = useState('')
  const [lead, setLead] = useState(seed ?? '')
  const [members, setMembers] = useState<string[]>(seed ? [seed] : [])
  const [picking, setPicking] = useState(false)
  const [error, setError] = useState<unknown>(null)

  const slug = slugify(name)
  const chosen = users.filter((u) => members.includes(u.id))
  // A lead sees the team's aggregates, which they cannot do from outside it.
  const leadable = users.filter((u) => u.role !== 'developer')

  const create = useAction(async () => {
    setError(null)
    try {
      const team = await post<{ id: string; slug: string }>('/api/v1/teams', { name: name.trim() })

      const roster = [...new Set(lead === '' ? members : [...members, lead])]
      if (roster.length > 0) {
        await post(`/api/v1/teams/${team.slug}`, {
          members: roster.map((id) => ({ user_id: id, role: id === lead ? 'lead' : 'member' })),
        })
      }
      onCreated(
        `${name.trim()} created with ${count(roster.length)} ${roster.length === 1 ? 'member' : 'members'}.`,
      )
    } catch (cause) {
      setError(cause)
      throw cause
    }
  })

  return (
    <>
      <Drawer
        open
        wide
        title="New team"
        sub="Budgets and aggregates hang off teams. You can change everything later."
        onClose={onClose}
        onSubmit={create.act}
        footer={
          <>
            <button type="button" className="btn" onClick={onClose}>
              Cancel
            </button>
            <SubmitButton
              state={create.state}
              disabled={name.trim() === ''}
              label="Create team"
              settled="Created"
              primary
            />
          </>
        }
      >
        <ErrorBanner error={error} />

        <section className="field">
          <label className="drawer-kicker" htmlFor="team-name">
            Team name
          </label>
          <input
            id="team-name"
            className="input mono focus"
            value={name}
            placeholder="e.g. Growth Engineering"
            onChange={(e) => setName(e.target.value)}
          />
          <span className="drawer-note mono">slug · {slug || '-'}</span>
        </section>

        <section className="field">
          <label className="drawer-kicker" htmlFor="team-lead">
            Team lead
          </label>
          <Select
            id="team-lead"
            value={lead}
            label="Team lead"
            onChange={setLead}
            options={[
              { value: '', label: 'Unassigned' },
              ...leadable.map((person) => ({
                value: person.id,
                label: person.display_name || person.email,
              })),
            ]}
          />
          <span className="drawer-note">
            A lead sees this team&rsquo;s aggregates. They are added to the roster automatically.
          </span>
        </section>

        <span className="drawer-help">
          No budget yet - nothing stops this team until you set one. Open the team once it exists to
          add limits.
        </span>

        <section className="field">
          <div className="spread" style={{ alignItems: 'baseline' }}>
            <span className="drawer-kicker">Members</span>
            <button type="button" className="btn link" onClick={() => setPicking(true)}>
              Choose people
            </button>
          </div>
          {chosen.length === 0 ? (
            <div className="drawer-empty centred">
              Nobody yet. A team with no members aggregates nothing - you can add them later.
            </div>
          ) : (
            chosen.map((person) => (
              <div key={person.id} className="drawer-row">
                <Avatar name={person.display_name || person.email} size="sm" />
                <span className="drawer-row-name truncate">
                  {person.display_name || person.email}
                </span>
                <button
                  type="button"
                  className="btn small"
                  onClick={() => setMembers((ids) => ids.filter((id) => id !== person.id))}
                >
                  Remove
                </button>
              </div>
            ))
          )}
        </section>
      </Drawer>

      {picking && (
        <PeoplePicker
          title="Members of the new team"
          sub="Check to add, uncheck to remove. They join when the team is created."
          users={users}
          teams={teams}
          selected={members}
          confirmLabel="Choose"
          onClose={() => setPicking(false)}
          onApply={(next) => {
            setMembers(next)
            setPicking(false)
          }}
        />
      )}
    </>
  )
}
