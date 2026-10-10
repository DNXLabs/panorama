// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useMemo, useState } from 'react'
import { usePageChrome } from '../chrome'
import { count } from '../format'
import { Icon } from '../icons'
import { capKind, statusOf } from '../people'
import { useRouter } from '../router'
import { useSession } from '../session'
import type { PersonRow, Policy, TeamRow } from '../types'
import { Empty, ErrorBanner, Loading, Select, VirtualList, useApi, useToast } from '../ui'
import { NewTeamDrawer, OrgDrawer, PersonDrawer, TeamDrawer } from './PeopleDrawers'
import {
  Flag,
  NewTeamCard,
  OrgCard,
  PersonRowView,
  ROSTER_COLUMNS,
  TeamCard,
} from './PeopleTable'

// Who is governed, how they are grouped, and what caps their spend.
//
// Budgets live here rather than beside routing, because a limit is a property
// of a person or a team and not of a route. A route reads one and cannot change
// it; the screen that sets it is the screen that lists the people it is set on.
//
// There is still no per-user spend on this screen, and there is no query in the
// server that would produce one. A budget is the one place a number attaches to
// a named individual: an administrator sees the limit, so it can be raised, and
// never what that person has spent.

/** One roster row, and how much of the roster is on screen at once. Both are
 *  pixel values the virtualised list needs to be exact about; the row height
 *  matches `--roster-row-h` in styles.css and there is a comment on each saying
 *  so. */
const ROW_HEIGHT = 62
const ROSTER_HEIGHT = 620

/** What is being edited. A panel, and it stays open while something is picked
 *  for it. */
type Panel =
  | { kind: 'person'; id: string; picking?: boolean }
  | { kind: 'team'; slug: string; picking?: boolean }
  | { kind: 'org' }
  | { kind: 'new-team' }
  | null

export function People() {
  const session = useSession().session!
  const me = session.user
  const { search, setParam } = useRouter()
  const people = useApi<{ users: PersonRow[]; note: string }>('/api/v1/users')
  const teams = useApi<{ teams: TeamRow[] }>('/api/v1/teams')
  const policy = useApi<Policy>('/api/v1/policy')
  const { say, toast } = useToast()

  // In the URL, like Analytics' tabs, so a view of this screen is a link.
  const tab = search.get('tab') === 'teams' ? 'teams' : 'people'
  const [panel, setPanel] = useState<Panel>(null)

  const [query, setQuery] = useState('')
  const [byTeam, setByTeam] = useState('all')
  const [byRole, setByRole] = useState('all')
  const [byStatus, setByStatus] = useState('all')
  const [byBudget, setByBudget] = useState('all')

  const users = people.data?.users ?? []
  const teamList = teams.data?.teams ?? []
  const budgets = policy.data?.budgets ?? []
  const canEdit = policy.data?.can_edit ?? false

  const reload = () => {
    people.reload()
    teams.reload()
    policy.reload()
  }

  const saved = (message: string) => {
    setPanel(null)
    reload()
    say(message)
  }

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return users.filter((person) => {
      if (
        q !== '' &&
        !person.display_name.toLowerCase().includes(q) &&
        !person.email.toLowerCase().includes(q)
      ) {
        return false
      }
      if (byTeam === 'none' && person.teams.length > 0) return false
      if (byTeam !== 'all' && byTeam !== 'none' && !person.teams.some((t) => t.id === byTeam)) {
        return false
      }
      if (byRole !== 'all' && person.role !== byRole) return false
      if (byStatus !== 'all' && statusOf(person) !== byStatus) return false
      if (byBudget !== 'all' && capKind(person, budgets) !== byBudget) return false
      return true
    })
  }, [users, budgets, query, byTeam, byRole, byStatus, byBudget])

  const filtering =
    query !== '' || byTeam !== 'all' || byRole !== 'all' || byStatus !== 'all' || byBudget !== 'all'

  const clearFilters = () => {
    setQuery('')
    setByTeam('all')
    setByRole('all')
    setByStatus('all')
    setByBudget('all')
  }

  const uncapped = users.filter((p) => capKind(p, budgets) === 'none').length
  const unused = users.filter((p) => statusOf(p) === 'unused').length
  const teamless = users.filter((p) => p.teams.length === 0).length

  // Only the flags that are true, in a fixed order: an account nobody has
  // capped first, then one that has never been used, then a person on no team.
  //
  // A FLAG IS THE FILTER, NOT A LINK TO ONE. Clicking it narrows the roster to
  // the accounts it counted and the flag itself shows as pressed, so the
  // condition is visible where it was applied - which matters most for "no cap
  // above them", the one condition the three select boxes cannot express.
  const flags = [
    uncapped > 0 && {
      key: 'uncapped',
      tone: 'warn' as const,
      label: `${count(uncapped)} ${uncapped === 1 ? 'account has' : 'accounts have'} no cap above them`,
      active: byBudget === 'none',
      apply: () => setByBudget('none'),
    },
    unused > 0 && {
      key: 'unused',
      tone: 'warn' as const,
      label: `${count(unused)} ${unused === 1 ? 'account has' : 'accounts have'} never used the gateway`,
      active: byStatus === 'unused',
      apply: () => setByStatus('unused'),
    },
    teamless > 0 && {
      key: 'teamless',
      tone: 'accent' as const,
      label: `${count(teamless)} ${teamless === 1 ? 'person' : 'people'} on no team`,
      active: byTeam === 'none',
      apply: () => setByTeam('none'),
    },
  ].filter((flag): flag is Exclude<typeof flag, false | 0> => Boolean(flag))

  usePageChrome(
    {
      flush: true,
      sub: 'Accounts arrive by signing in - Panorama does not own identities and has no invite flow. What is decided here is what each person may do, which team their spend rolls up to, and what stops them.',
      actions: canEdit ? (
        <button
          type="button"
          className="btn primary"
          onClick={() => setPanel({ kind: 'new-team' })}
        >
          New team
        </button>
      ) : undefined,
    },
    [canEdit],
  )

  const person = panel?.kind === 'person' ? users.find((p) => p.id === panel.id) : undefined
  const team = panel?.kind === 'team' ? teamList.find((t) => t.slug === panel.slug) : undefined

  return (
    <>
      {flags.length > 0 && (
        <div className="flag-strip">
          <span className="flag-count">
            {count(flags.length)} {flags.length === 1 ? 'flag' : 'flags'}
          </span>
          {flags.map((flag) => (
            <Flag
              key={flag.key}
              tone={flag.tone}
              label={flag.label}
              active={flag.active}
              onClick={() => {
                if (flag.active) {
                  clearFilters()
                  return
                }
                clearFilters()
                flag.apply()
                setParam('tab', null)
              }}
            />
          ))}
        </div>
      )}

      <nav className="screen-tabs" aria-label="People and teams">
        <button
          type="button"
          className={tab === 'people' ? 'screen-tab active' : 'screen-tab'}
          aria-current={tab === 'people' ? 'page' : undefined}
          onClick={() => setParam('tab', null)}
        >
          People
          <span className="screen-tab-count">{count(users.length)}</span>
        </button>
        <button
          type="button"
          className={tab === 'teams' ? 'screen-tab active' : 'screen-tab'}
          aria-current={tab === 'teams' ? 'page' : undefined}
          onClick={() => setParam('tab', 'teams')}
        >
          Teams
          <span className="screen-tab-count">{count(teamList.length)}</span>
        </button>
      </nav>

      <main className="bands">
        {Boolean(people.error ?? teams.error ?? policy.error) && (
          <div className="band">
            <ErrorBanner error={people.error ?? teams.error ?? policy.error} />
          </div>
        )}

        {tab === 'people' ? (
          <section className="band last">
            <div className="roster-filters">
              <label className="search">
                <Icon name="search" />
                <input
                  value={query}
                  placeholder="Search name or email"
                  aria-label="Search people"
                  onChange={(e) => setQuery(e.target.value)}
                />
              </label>
              {/* Selects rather than a segmented group: the team list grows
                  unbounded, and some deployments have hundreds. */}
              <Select
                value={byTeam}
                label="Team"
                onChange={setByTeam}
                options={[
                  { value: 'all', label: 'All teams' },
                  { value: 'none', label: 'No team' },
                  ...teamList.map((each) => ({ value: each.id, label: each.name })),
                ]}
              />
              <Select
                value={byRole}
                label="Role"
                onChange={setByRole}
                options={[
                  { value: 'all', label: 'All roles' },
                  { value: 'admin', label: 'Admin' },
                  { value: 'manager', label: 'Manager' },
                  { value: 'developer', label: 'Developer' },
                ]}
              />
              <Select
                value={byStatus}
                label="Status"
                onChange={setByStatus}
                options={[
                  { value: 'all', label: 'Any status' },
                  { value: 'active', label: 'Active' },
                  { value: 'unused', label: 'Never used' },
                  { value: 'dormant', label: 'Dormant' },
                  { value: 'disabled', label: 'Disabled' },
                ]}
              />
              {filtering && (
                <button type="button" className="filter-clear" onClick={clearFilters}>
                  Clear
                </button>
              )}
              <span className="filter-count">
                {filtered.length === users.length
                  ? `${count(users.length)} people`
                  : `${count(filtered.length)} of ${count(users.length)}`}
              </span>
            </div>

            {people.loading ? (
              <Loading rows={5} />
            ) : users.length === 0 ? (
              <Empty title="Nobody has signed in yet">
                An account appears the first time somebody signs in, or when an administrator mints
                the first token for them.
              </Empty>
            ) : (
              <div className="roster">
                <div className="roster-head" style={{ gridTemplateColumns: ROSTER_COLUMNS }}>
                  <span>Person</span>
                  <span>Teams</span>
                  <span>What stops them</span>
                  <span>Last request</span>
                  <span>Role</span>
                  <span style={{ textAlign: 'right' }}>Status</span>
                </div>
                {filtered.length === 0 ? (
                  <p className="roster-empty">No account matches these filters.</p>
                ) : (
                  /* Virtualised, because an identity provider with three
                     thousand accounts in it is a normal deployment and three
                     thousand rows in the document is not. */
                  <VirtualList
                    items={filtered}
                    rowHeight={ROW_HEIGHT}
                    height={Math.min(filtered.length * ROW_HEIGHT, ROSTER_HEIGHT)}
                    render={(each) => (
                      <PersonRowView
                        person={each}
                        budgets={budgets}
                        teams={teamList}
                        mapped={session.sso_configured}
                        onOpen={() => setPanel({ kind: 'person', id: each.id })}
                        onAssign={() =>
                          setPanel({ kind: 'person', id: each.id, picking: true })
                        }
                      />
                    )}
                  />
                )}
              </div>
            )}

            <p className="footnote">
              {people.data?.note ??
                'This page carries no per-user spend. Panorama measures work, not workers: developers see their own usage and leaders see team aggregates.'}
            </p>
          </section>
        ) : (
          <section className="band last">
            <div className="tcards">
              <OrgCard budgets={budgets} onOpen={() => setPanel({ kind: 'org' })} />
              {teamList.map((each) => (
                <TeamCard
                  key={each.id}
                  team={each}
                  users={users}
                  budgets={budgets}
                  onOpen={() => setPanel({ kind: 'team', slug: each.slug })}
                />
              ))}
              {canEdit && <NewTeamCard onClick={() => setPanel({ kind: 'new-team' })} />}
            </div>

            <p className="footnote">
              A team is the smallest unit a leader is shown, and the middle rung of what stops
              somebody: a team&rsquo;s hard limit governs every member who has no tighter cap of
              their own.
            </p>
          </section>
        )}
      </main>

      {/* Remounted whenever the subject changes, so a drawer never opens
          holding the last person's staged edits. */}
      {person && (
        <PersonDrawer
          key={person.id}
          person={person}
          me={me.id}
          users={users}
          teams={teamList}
          budgets={budgets}
          canEdit={canEdit}
          picking={Boolean(panel?.kind === 'person' && panel.picking)}
          onPick={(open) => setPanel({ kind: 'person', id: person.id, picking: open })}
          onClose={() => setPanel(null)}
          onSaved={saved}
        />
      )}

      {team && (
        <TeamDrawer
          key={team.id}
          team={team}
          users={users}
          teams={teamList}
          budgets={budgets}
          canEdit={canEdit}
          picking={Boolean(panel?.kind === 'team' && panel.picking)}
          onPick={(open) => setPanel({ kind: 'team', slug: team.slug, picking: open })}
          onClose={() => setPanel(null)}
          onSaved={saved}
          onDeleted={saved}
        />
      )}

      {panel?.kind === 'org' && (
        <OrgDrawer
          budgets={budgets}
          canEdit={canEdit}
          onClose={() => setPanel(null)}
          onSaved={saved}
        />
      )}

      {panel?.kind === 'new-team' && (
        <NewTeamDrawer
          users={users}
          teams={teamList}
          onClose={() => setPanel(null)}
          onCreated={(message) => {
            setPanel(null)
            setParam('tab', 'teams')
            reload()
            say(message)
          }}
        />
      )}

      {toast}
    </>
  )
}
