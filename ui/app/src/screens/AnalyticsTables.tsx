// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from 'react'
import { query } from '../api'
import type { Range } from '../api'
import { ago, count, money, percent } from '../format'
import { Link } from '../router'
import type {
  Bucket,
  Outcomes,
  PersonRow,
  Seats,
  SessionRow,
  UserSpendRow,
  Usage,
  Work,
  WorkBucket,
} from '../types'
import { Empty, Loading, Pill, Segmented, Select, useApi } from '../ui'

// The four tabs of Analytics that are tables.
//
// They are one file because they are one pattern: a grid whose header row and
// whose rows are handed the same `grid-template-columns`, a `+` that opens a
// detail block under a row, and a bar under each name measuring it against the
// largest. Four copies of that in four files is four places for a header to
// drift one column away from the rows beneath it.
//
// A column shows an em dash rather than a zero wherever the value is an absence
// - no cost per developer on a team nobody has used, no cost per request on a
// person who made none. `$0.00` and "we have not measured this" are different
// claims and a table that renders them identically is making the wrong one.

/** Columns are declared once and spread onto both the header and the rows. */
function grid(columns: string, gap: number) {
  return { gridTemplateColumns: columns, gap }
}

/** dash renders an absent figure the way every table here does. */
function Dash() {
  return <span className="cell-fig none">-</span>
}

/* ---------------------------------------------------------------- tab: teams */

const TEAM_COLUMNS = 'minmax(0, 2.1fr) 110px 110px 130px 120px 20px'

export function TeamsTab({
  usage,
  loading,
  seats,
}: {
  usage: Usage | null
  loading: boolean
  seats: Seats | null
}) {
  const [open, setOpen] = useState<Record<string, boolean>>({})

  const teams = [...(usage?.by_team ?? [])].sort(
    (a, b) => b.cost.total_nanos - a.cost.total_nanos,
  )
  const max = Math.max(1, ...teams.map((t) => t.cost.total_nanos))

  // Who is on a team comes from the roster; what they spent comes from the
  // seats table. Both are already loaded for other parts of this screen, so an
  // opened row costs nothing - and both are refused to a reader who may not see
  // named spend, which is why the block is simply absent rather than empty.
  const roster = useApi<{ users: PersonRow[] }>('/api/v1/users')
  const spendByUser = new Map((seats?.people ?? []).map((p) => [p.user_id, p]))

  if (loading) {
    return (
      <section className="band last">
        <Loading rows={4} />
      </section>
    )
  }

  return (
    <section className="band last">
      <div className="band-head-row">
        <div className="band-head">
          <span className="band-title">By team</span>
          <span className="band-sub">
            {teams.length === 0
              ? 'No team has routed through the gateway in this window.'
              : `${count(teams.length)} ${teams.length === 1 ? 'team routes' : 'teams route'} through the gateway. Open a row for the people in it.`}
          </span>
        </div>
        <span className="filter-count">sorted by spend</span>
      </div>

      {teams.length === 0 ? (
        <Empty title="No team spend in this window">
          Spend appears here once somebody on a team routes a request.{' '}
          <Link to="/people">Create a team</Link> if there is none yet.
        </Empty>
      ) : (
        <>
          <div className="dtable-head" style={grid(TEAM_COLUMNS, 20)}>
            <span>Team</span>
            <span style={{ textAlign: 'right' }}>Spend</span>
            <span style={{ textAlign: 'right' }}>Attributed</span>
            <span style={{ textAlign: 'right' }}>Requests</span>
            <span style={{ textAlign: 'right' }}>Cost / dev</span>
            <span />
          </div>

          {teams.map((team) => (
            <TeamRow
              key={team.key}
              team={team}
              max={max}
              open={Boolean(open[team.key])}
              onToggle={() => setOpen((was) => ({ ...was, [team.key]: !was[team.key] }))}
              members={(roster.data?.users ?? []).filter((u) =>
                u.teams.some((t) => t.slug === team.key),
              )}
              spendByUser={spendByUser}
            />
          ))}
        </>
      )}

      {usage?.teams_note && <div className="footnote">{usage.teams_note}</div>}
    </section>
  )
}

function TeamRow({
  team,
  max,
  open,
  onToggle,
  members,
  spendByUser,
}: {
  team: Bucket
  max: number
  open: boolean
  onToggle: () => void
  members: PersonRow[]
  spendByUser: Map<string, UserSpendRow>
}) {
  const spend = team.cost.total_nanos
  const attributed = team.attributed_cost_nanos ?? 0
  const people = team.members ?? 0

  const ranked = members
    .map((m) => ({ person: m, spend: spendByUser.get(m.id)?.cost.nanos ?? 0 }))
    .sort((a, b) => b.spend - a.spend)

  return (
    <div className="dtable-group">
      <button
        type="button"
        className="dtable-open"
        style={grid(TEAM_COLUMNS, 20)}
        aria-expanded={open}
        onClick={onToggle}
      >
        <span className="cell-named">
          <span className="name">{team.name ?? team.key}</span>
          <span className="row-bar">
            <span style={{ width: `${(spend / max) * 100}%` }} />
          </span>
        </span>
        {spend > 0 ? <span className="cell-fig">{money(spend)}</span> : <Dash />}
        {spend > 0 ? (
          <span className="cell-fig">{percent(attributed, spend)}</span>
        ) : (
          <Dash />
        )}
        {team.requests > 0 ? (
          <span className="cell-fig soft">{count(team.requests)}</span>
        ) : (
          <Dash />
        )}
        {people > 0 && spend > 0 ? (
          <span className="cell-fig soft">{money(Math.round(spend / people))}</span>
        ) : (
          <Dash />
        )}
        <span className="caret">{open ? '−' : '+'}</span>
      </button>

      {open && (
        <div className="detail" style={{ gridTemplateColumns: 'minmax(0, 1fr) minmax(0, 1fr)' }}>
          <div className="detail-block">
            <span className="detail-label">People</span>
            {ranked.length === 0 ? (
              <span className="detail-value">Nobody is on this team yet.</span>
            ) : (
              ranked.map(({ person, spend: theirs }) => (
                <div key={person.id} className="detail-line">
                  <span>{person.display_name || person.email}</span>
                  <span>{theirs > 0 ? money(theirs) : '-'}</span>
                </div>
              ))
            )}
          </div>
          <div className="detail-block">
            <span className="detail-label">Attribution</span>
            <div className="detail-line">
              <span>Tied to a work item</span>
              <span>{money(attributed)}</span>
            </div>
            <div className="detail-line">
              <span>Could not be placed</span>
              <span>{money(Math.max(0, spend - attributed))}</span>
            </div>
            <span className="detail-value">
              Every gap is stated rather than absorbed. The reasons are on the Attribution tab.
            </span>
          </div>
        </div>
      )}
    </div>
  )
}

/* --------------------------------------------------------------- tab: people */

const PERSON_COLUMNS = 'minmax(0, 1.9fr) 130px 120px 110px 130px 120px 20px'

export function PeopleTab({
  range,
  seats,
  loading,
}: {
  range: Range
  seats: Seats | null
  loading: boolean
}) {
  const [open, setOpen] = useState<Record<string, boolean>>({})
  const roster = useApi<{ users: PersonRow[] }>('/api/v1/users')

  if (loading) {
    return (
      <section className="band last">
        <Loading rows={5} />
      </section>
    )
  }

  // Decided on the server, not by which screen was opened. A developer who
  // reaches this tab is told plainly why it is not theirs rather than being
  // shown an empty table they would read as "nobody spent anything".
  if (seats && !seats.can_see_people) {
    return (
      <section className="band last">
        <Empty title="Not yours to see">
          {seats.people_note ??
            'Per-person spend is for administrators and for managers of a person’s teams.'}
        </Empty>
      </section>
    )
  }

  const people = seats?.people ?? []
  const max = Math.max(1, ...people.map((p) => p.cost.nanos))
  const teamsFor = new Map(
    (roster.data?.users ?? []).map((u) => [u.id, u.teams.map((t) => t.name)]),
  )

  return (
    <section className="band last">
      <div className="band-head-row">
        <div className="band-head lead">
          <span className="band-title">By person</span>
          <span className="band-sub">
            {people.length === 0
              ? 'Nobody in your scope routed traffic in this window.'
              : `${count(people.length)} ${people.length === 1 ? 'developer' : 'developers'} routed traffic in this window. Open a row for what they worked on.`}
          </span>
        </div>
        <span className="filter-count">sorted by spend</span>
      </div>

      {people.length > 0 && (
        <>
          <div className="dtable-head" style={grid(PERSON_COLUMNS, 18)}>
            <span>Person</span>
            <span>Team</span>
            <span style={{ textAlign: 'right' }}>Spend</span>
            <span style={{ textAlign: 'right' }}>Attributed</span>
            <span style={{ textAlign: 'right' }}>Requests</span>
            <span style={{ textAlign: 'right' }}>Cost / req</span>
            <span />
          </div>

          {people.map((person) => (
            <PersonRowView
              key={person.user_id}
              person={person}
              teams={teamsFor.get(person.user_id) ?? []}
              max={max}
              range={range}
              open={Boolean(open[person.user_id])}
              onToggle={() =>
                setOpen((was) => ({ ...was, [person.user_id]: !was[person.user_id] }))
              }
            />
          ))}
        </>
      )}

      <div className="footnote">
        Named per-person spend is for administrators and for managers of that person&rsquo;s teams.
        Individuals are never ranked against individuals.
      </div>
    </section>
  )
}

function PersonRowView({
  person,
  teams,
  max,
  range,
  open,
  onToggle,
}: {
  person: UserSpendRow
  teams: string[]
  max: number
  range: Range
  open: boolean
  onToggle: () => void
}) {
  const spend = person.cost.nanos

  // Asked for only once the row is opened, and asked of the same endpoint the
  // Work tab reads - so what a person worked on and what the organization
  // worked on are the same rollup narrowed, rather than a second definition of
  // "worked on". `filterFrom` ANDs the user filter with the caller's own scope,
  // so this cannot widen what the reader may see.
  const work = useApi<Work>(
    open ? `/api/v1/work${query({ since: range, user: person.email })}` : null,
    [open, range],
  )
  const worked = [...(work.data?.by_work_item ?? [])]
    .filter((item) => item.attributed)
    .sort((a, b) => b.cost.total_nanos - a.cost.total_nanos)

  return (
    <div className="dtable-group">
      <button
        type="button"
        className="dtable-open"
        style={grid(PERSON_COLUMNS, 18)}
        aria-expanded={open}
        onClick={onToggle}
      >
        <span className="cell-named">
          <span className="name">{person.display_name || person.email}</span>
          <span className="row-bar">
            <span style={{ width: `${(spend / max) * 100}%` }} />
          </span>
        </span>
        <span className="cell-text truncate">{teams.join(', ') || '-'}</span>
        {spend > 0 ? <span className="cell-fig">{money(spend)}</span> : <Dash />}
        {spend > 0 ? (
          <span className="cell-fig">{percent(person.attributed_cost.nanos, spend)}</span>
        ) : (
          <Dash />
        )}
        {person.requests > 0 ? (
          <span className="cell-fig soft">{count(person.requests)}</span>
        ) : (
          <Dash />
        )}
        {person.requests > 0 && spend > 0 ? (
          <span className="cell-fig soft">{money(Math.round(spend / person.requests))}</span>
        ) : (
          <Dash />
        )}
        <span className="caret">{open ? '−' : '+'}</span>
      </button>

      {open && (
        <div className="detail" style={{ gridTemplateColumns: 'minmax(0, 1fr) 200px' }}>
          <div className="detail-block">
            <span className="detail-label">Worked on</span>
            {work.loading ? (
              <span className="detail-value">Reading…</span>
            ) : worked.length === 0 ? (
              <span className="detail-value">
                None of this traffic could be tied to a work item.
              </span>
            ) : (
              worked.slice(0, 8).map((item) => (
                <div key={item.key} className="detail-line" style={{ maxWidth: 420 }}>
                  <span>{item.title || item.key}</span>
                  <span>{money(item.cost.total_nanos)}</span>
                </div>
              ))
            )}
          </div>
          <div className="detail-block">
            <span className="detail-label">Sessions</span>
            <span className="detail-value">{count(person.sessions)}</span>
            {person.unpriced > 0 && (
              <>
                <span className="detail-label">Unpriced</span>
                <span className="detail-value">{count(person.unpriced)} requests</span>
              </>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

/* ----------------------------------------------------------------- tab: work */

type Grouping = 'item' | 'repo' | 'project'
type StatusFilter = 'all' | 'landed' | 'open' | 'abandoned'

const ITEM_COLUMNS = '92px minmax(0, 1fr) 110px 100px 110px 90px 110px 100px 20px'
const GROUP_COLUMNS = 'minmax(0, 1fr) 130px 110px 130px 120px'
const PAGE = 20

const STATUSES: { id: StatusFilter; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'landed', label: 'Merged' },
  { id: 'open', label: 'Open' },
  { id: 'abandoned', label: 'Abandoned' },
]

export function WorkTab({
  work,
  outcomes,
  loading,
  search,
  setParam,
}: {
  work: Work | null
  outcomes: Outcomes | null
  loading: boolean
  search: URLSearchParams
  setParam: (name: string, value: string | null) => void
}) {
  const [open, setOpen] = useState<Record<string, boolean>>({})
  const [shown, setShown] = useState(PAGE)

  const grouping = (search.get('group') as Grouping) || 'item'
  const status = (search.get('status') as StatusFilter) || 'all'
  const term = search.get('q') ?? ''

  const coverage = work?.coverage

  return (
    <section className="band last">
      <div className="band-head-row">
        <div className="band-head lead">
          <span className="band-title">Work</span>
          <span className="band-sub">
            Attributed spend grouped by what it was tied to. Read from the traffic itself, with no
            client configuration.
          </span>
        </div>
        <div className="figure-pairs">
          <div className="figure-pair">
            <span className="label">Attributed</span>
            <span className="value">
              {coverage ? money(coverage.attributed_cost_nanos) : '-'}
            </span>
          </div>
          <div className="figure-pair">
            <span className="label">Per merged PR</span>
            <span className="value">
              {outcomes?.cost_per_merged_pr
                ? money(outcomes.cost_per_merged_pr.per_pr_nanos)
                : '-'}
            </span>
          </div>
        </div>
      </div>

      <div className="filter-bar">
        <input
          type="text"
          className="filter-search"
          placeholder="Search items"
          value={term}
          onChange={(event) => setParam('q', event.target.value || null)}
        />
        <div className="filter">
          <span className="filter-label">Group by</span>
          <Segmented
            dense
            value={grouping}
            options={[
              { id: 'item', label: 'Work item' },
              { id: 'repo', label: 'Repository' },
              { id: 'project', label: 'Project' },
            ]}
            onChange={(next) => setParam('group', next === 'item' ? null : next)}
            label="Group by"
          />
        </div>
        {grouping === 'item' && (
          <div className="filter">
            <span className="filter-label">Status</span>
            <Segmented
              dense
              value={status}
              options={STATUSES}
              onChange={(next) => setParam('status', next === 'all' ? null : next)}
              label="Status"
            />
          </div>
        )}
        <span className="filter-count">sorted by spend</span>
      </div>

      {loading ? (
        <Loading rows={5} />
      ) : grouping === 'item' ? (
        <WorkItems
          items={work?.by_work_item ?? []}
          status={status}
          term={term}
          shown={shown}
          onMore={() => setShown((n) => n + PAGE)}
          open={open}
          onToggle={(key) => setOpen((was) => ({ ...was, [key]: !was[key] }))}
        />
      ) : (
        <WorkGroups
          rows={grouping === 'repo' ? (work?.by_repository ?? []) : (outcomes?.by_project ?? [])}
          noun={grouping === 'repo' ? 'Repository' : 'Project'}
          integrated={grouping === 'repo' || Boolean(outcomes?.integration)}
          term={term}
        />
      )}
    </section>
  )
}

/** A work item's status, as a pill.
 *
 *  `outcome` decides the tone and `status` provides the word, which is the same
 *  split the metrics keep: "Merged", "Closed", "Done", "Released" and "Won't
 *  Do" are five words and three outcomes, and a pill coloured from the source's
 *  own word gains a sixth colour the day somebody renames a Jira state. */
function StatusPill({ item }: { item: WorkBucket }) {
  if (!item.outcome) {
    return <span className="cell-fig none" style={{ textAlign: 'left' }}>-</span>
  }
  const tone = item.outcome === 'landed' ? 'good' : item.outcome === 'abandoned' ? 'off' : undefined
  return <Pill tone={tone}>{item.status || item.outcome}</Pill>
}

function WorkItems({
  items,
  status,
  term,
  shown,
  onMore,
  open,
  onToggle,
}: {
  items: WorkBucket[]
  status: StatusFilter
  term: string
  shown: number
  onMore: () => void
  open: Record<string, boolean>
  onToggle: (key: string) => void
}) {
  const needle = term.trim().toLowerCase()
  const matching = items
    .filter((item) => item.attributed)
    .filter((item) => status === 'all' || item.outcome === status)
    .filter(
      (item) =>
        !needle ||
        item.key.toLowerCase().includes(needle) ||
        (item.title ?? '').toLowerCase().includes(needle) ||
        item.repo.toLowerCase().includes(needle),
    )
    .sort((a, b) => b.cost.total_nanos - a.cost.total_nanos)

  const max = Math.max(1, ...matching.map((i) => i.cost.total_nanos))
  const page = matching.slice(0, shown)

  if (matching.length === 0) {
    // Two different states, and telling them apart is the point: "the filters
    // exclude everything" is something the reader can undo, and "nothing has
    // been attributed" is the product reporting an honest zero.
    const filtering = Boolean(needle) || status !== 'all'
    return filtering ? (
      <Empty title="No work items match these filters">
        Every item in this window is still here - the current search and status filters exclude all
        of them.
      </Empty>
    ) : (
      <Empty title="Nothing has been attributed yet">
        Spend appears here as soon as a session can be tied to a branch, a pull request, or a
        ticket. Until then it is on the Attribution tab, with the reason it could not be placed.
      </Empty>
    )
  }

  return (
    <>
      <div className="dtable-head" style={grid(ITEM_COLUMNS, 16)}>
        <span>Key</span>
        <span>Item</span>
        <span>Provenance</span>
        <span>Status</span>
        <span style={{ textAlign: 'right' }}>Spend</span>
        <span style={{ textAlign: 'right' }}>PRs</span>
        <span style={{ textAlign: 'right' }}>Per PR</span>
        <span style={{ textAlign: 'right' }}>Last used</span>
        <span />
      </div>

      {page.map((item) => {
        const spend = item.cost.total_nanos
        const prs = item.merged_prs ?? 0
        const isOpen = Boolean(open[item.key])
        return (
          <div key={item.key} className="dtable-group">
            <button
              type="button"
              className="dtable-open"
              style={grid(ITEM_COLUMNS, 16)}
              aria-expanded={isOpen}
              onClick={() => onToggle(item.key)}
            >
              <span className="cell-key truncate">{item.key}</span>
              <span className="cell-named">
                <span className="name">{item.title || item.key}</span>
                <span className="row-bar">
                  <span style={{ width: `${(spend / max) * 100}%` }} />
                </span>
              </span>
              <span className="cell-text">{item.rung ?? '-'}</span>
              <span>
                <StatusPill item={item} />
              </span>
              <span className="cell-fig">{money(spend)}</span>
              {prs > 0 ? <span className="cell-fig soft">{count(prs)}</span> : <Dash />}
              {prs > 0 ? (
                <span className="cell-fig soft">{money(Math.round(spend / prs))}</span>
              ) : (
                <Dash />
              )}
              <span className="cell-key" style={{ textAlign: 'right' }}>
                {ago(item.last_seen_at)}
              </span>
              <span className="caret">{isOpen ? '−' : '+'}</span>
            </button>

            {isOpen && (
              <div
                className="detail indent"
                style={{
                  gridTemplateColumns: 'minmax(0, 1.2fr) 150px 200px 170px',
                  // The item key's 92px track, its 20px gap and the row's 12px
                  // padding: the panel starts where the row's second column does.
                  ['--detail-indent' as string]: '124px',
                }}
              >
                <div className="detail-block">
                  <span className="detail-label">Repository</span>
                  <span className="detail-value">{item.repo || 'none recorded'}</span>
                </div>
                <div className="detail-block">
                  <span className="detail-label">Sessions</span>
                  <span className="detail-value">{count(item.sessions)}</span>
                </div>
                <div className="detail-block">
                  <span className="detail-label">Source</span>
                  {item.url ? (
                    <a className="cell-key" style={{ color: 'var(--accent)' }} href={item.url}>
                      {item.repo} · {item.key}
                    </a>
                  ) : (
                    <span className="detail-value">not confirmed by an integration</span>
                  )}
                  <span style={{ color: 'var(--text-muted)', fontSize: 'var(--size-note)' }}>
                    {prs > 0
                      ? `${count(prs)} merged pull ${prs === 1 ? 'request' : 'requests'}`
                      : 'no merged pull requests yet'}
                  </span>
                </div>
                <div className="detail-block">
                  <span className="detail-label">Audit trail</span>
                  <Link
                    to={`/requests${query({ repo: item.repo, branch: item.key.split('@')[1] ?? '' })}`}
                    className="btn link"
                  >
                    Open requests →
                  </Link>
                </div>
              </div>
            )}
          </div>
        )
      })}

      <div className="dtable-foot">
        <span>
          Showing {count(page.length)} of {count(matching.length)} items
        </span>
        {page.length < matching.length && (
          <button type="button" className="btn" onClick={onMore}>
            Load {Math.min(PAGE, matching.length - page.length)} more
          </button>
        )}
      </div>
    </>
  )
}

function WorkGroups({
  rows,
  noun,
  integrated,
  term,
}: {
  rows: WorkBucket[]
  noun: string
  integrated: boolean
  term: string
}) {
  const needle = term.trim().toLowerCase()
  const matching = [...rows]
    .filter((row) => !needle || (row.key || 'none').toLowerCase().includes(needle))
    .sort((a, b) => b.cost.total_nanos - a.cost.total_nanos)
  const max = Math.max(1, ...matching.map((r) => r.cost.total_nanos))

  if (!integrated) {
    return (
      <Empty title="Projects need GitHub connected">
        Attribution works without it. What an integration adds is which project a system of record
        files each work item under - nothing in a request body says.
        <div style={{ marginTop: 12 }}>
          <Link to="/settings?tab=integrations" className="btn">
            Connect an integration
          </Link>
        </div>
      </Empty>
    )
  }

  if (matching.length === 0) {
    return <Empty title={`No ${noun.toLowerCase()} in this window`} />
  }

  return (
    <>
      <div className="dtable-head" style={grid(GROUP_COLUMNS, 20)}>
        <span>{noun}</span>
        <span style={{ textAlign: 'right' }}>Spend</span>
        <span style={{ textAlign: 'right' }}>Sessions</span>
        <span style={{ textAlign: 'right' }}>Requests</span>
        <span style={{ textAlign: 'right' }}>Attributed</span>
      </div>

      {matching.map((row) => {
        const spend = row.cost.total_nanos
        const share = row.attributed_cost_nanos
        return (
          <div key={row.key || 'none'} className="dtable-row" style={grid(GROUP_COLUMNS, 20)}>
            <span className="cell-named">
              <span className="name mono">{row.key || `no ${noun.toLowerCase()}`}</span>
              <span className="row-bar">
                <span style={{ width: `${(spend / max) * 100}%` }} />
              </span>
            </span>
            <span className="cell-fig">{money(spend)}</span>
            <span className="cell-fig soft">{count(row.sessions)}</span>
            <span className="cell-fig soft">{count(row.requests)}</span>
            <span className={share > 0 ? 'cell-fig' : 'cell-fig zero'}>
              {percent(share, spend)}
            </span>
          </div>
        )
      })}
    </>
  )
}

/* ------------------------------------------------------------- tab: sessions */

type AttrFilter = 'all' | 'attributed' | 'unattributed'
type ProvFilter = 'any' | 'stamp' | 'structural' | 'content'

const SESSION_COLUMNS = '100px minmax(0, 1fr) 130px 130px 70px'

const ATTRIBUTION: { id: AttrFilter; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'attributed', label: 'Attributed' },
  { id: 'unattributed', label: 'Unattributed' },
]

const PROVENANCE: { id: ProvFilter; label: string }[] = [
  { id: 'any', label: 'Any' },
  { id: 'stamp', label: 'Stamp' },
  { id: 'structural', label: 'Structural' },
  { id: 'content', label: 'Content' },
]

/** The pill a session's provenance gets.
 *
 *  A stamp is what the client stated, so it is the confident one; structural is
 *  what was read from the branch and git output; content is a mention in a
 *  sentence, which is the one worth flagging as weaker. */
export function ProvenancePill({ session }: { session: SessionRow }) {
  const rung = session.work_item?.rung
  if (!rung) return <Pill tone="off">unattributed</Pill>
  const tone = rung === 'stamp' ? 'good' : rung === 'content' ? 'warn' : undefined
  return <Pill tone={tone}>{rung}</Pill>
}

export function SessionsTab({
  range,
  scope,
  search,
  setParam,
}: {
  range: Range
  scope: string
  search: URLSearchParams
  setParam: (name: string, value: string | null) => void
}) {
  const sessions = useApi<{ sessions: SessionRow[] }>(
    `/api/v1/sessions${query({ since: range, scope, limit: 200 })}`,
    [range],
  )

  const attribution = (search.get('attr') as AttrFilter) || 'all'
  const provenance = (search.get('prov') as ProvFilter) || 'any'
  const repo = search.get('repo') ?? ''

  const all = sessions.data?.sessions ?? []
  const repos = [...new Set(all.map((s) => s.repo).filter(Boolean))].sort()

  const rows = all
    .filter((s) =>
      attribution === 'all' ? true : attribution === 'attributed' ? s.attributed : !s.attributed,
    )
    .filter((s) => provenance === 'any' || s.work_item?.rung === provenance)
    .filter((s) => !repo || s.repo === repo)

  const clear = () => {
    setParam('attr', null)
    setParam('prov', null)
    setParam('repo', null)
  }

  return (
    <section className="band last">
      <div className="band-head">
        <span className="band-title" style={{ fontSize: 'var(--size-section-lg)' }}>
          Recent sessions
        </span>
        <span className="band-sub" style={{ maxWidth: '96ch' }}>
          Each conversation, and what it was attributed to. Provenance is how the tie was
          established: a <strong>stamp</strong> is a header the client was configured to send,{' '}
          <strong>structural</strong> is read from the branch and git output with no configuration,
          and <strong>content</strong> is a ticket key mentioned in the conversation. Structure
          always beats content.
        </span>
      </div>

      <div className="filter-bar">
        <div className="filter">
          <span className="filter-label">Attribution</span>
          <Segmented
            dense
            value={attribution}
            options={ATTRIBUTION}
            onChange={(next) => setParam('attr', next === 'all' ? null : next)}
            label="Attribution"
          />
        </div>
        <div className="filter">
          <span className="filter-label">Provenance</span>
          <Segmented
            dense
            value={provenance}
            options={PROVENANCE}
            onChange={(next) => setParam('prov', next === 'any' ? null : next)}
            label="Provenance"
          />
        </div>
        {/* Six options and growing: a segmented control would break the bar onto
            three rows, which is the line between this and a select. */}
        <div className="filter">
          <span className="filter-label">Repository</span>
          <Select
            className="filter-select"
            value={repo}
            label="Repository"
            onChange={(next) => setParam('repo', next || null)}
            options={[
              { value: '', label: 'All repos' },
              ...repos.map((name) => ({ value: name, label: name })),
            ]}
          />
        </div>
        <span className="filter-count">
          {count(rows.length)} {rows.length === 1 ? 'session' : 'sessions'}
        </span>
      </div>

      {sessions.loading ? (
        <Loading rows={5} />
      ) : rows.length === 0 ? (
        <div style={{ padding: '40px 4px 8px' }}>
          <Empty
            title="No sessions match these filters"
            action={
              <button type="button" className="btn" onClick={clear}>
                Clear filters
              </button>
            }
          >
            Every conversation in this window is still here - the current attribution, provenance
            and repository filters exclude all of them.
          </Empty>
        </div>
      ) : (
        <>
          <div className="dtable-head" style={grid(SESSION_COLUMNS, 20)}>
            <span>Last seen</span>
            <span>Repository · branch</span>
            <span>Work item</span>
            <span>Provenance</span>
            <span style={{ textAlign: 'right' }}>Turns</span>
          </div>

          {rows.map((session) => (
            <div key={session.id} className="dtable-row" style={grid(SESSION_COLUMNS, 20)}>
              <span className="cell-key">{ago(session.last_seen_at)}</span>
              <span className="cell-stacked">
                <span className="top">{session.repo || 'no repository'}</span>
                <span className="bottom">{session.branch || 'no branch'}</span>
              </span>
              {session.work_item ? (
                <span className="cell-item truncate">{session.work_item.key}</span>
              ) : (
                <span className="cell-item none">-</span>
              )}
              <span title={session.describe}>
                <ProvenancePill session={session} />
              </span>
              <span className="cell-fig soft">{count(session.requests)}</span>
            </div>
          ))}
        </>
      )}

      <div className="footnote">
        Individuals are never ranked against individuals. Developers see their own usage; leaders
        see team aggregates.
      </div>
    </section>
  )
}
