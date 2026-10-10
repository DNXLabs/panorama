// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState } from 'react'
import { RANGES, get, query } from '../api'
import type { Range } from '../api'
import { usePageChrome } from '../chrome'
import { count, date, duration, money, percent, time, tokens } from '../format'
import { useRouter } from '../router'
import { useSession } from '../session'
import type { RequestGroupRow, RequestRow } from '../types'
import { Empty, ErrorBanner, Loading, Pill, RefreshButton, Segmented, useApi, useDebounced } from '../ui'

// The audit trail. Every request the gateway saw, a sitting to a row.
//
// A header, a filter bar, and one table - no tabs, no hero, no chart. The
// screen answers one question at a time and the question is always "what
// happened here", so anything above the table that is not a control is
// something a reader has to scroll past to reach the thing they came for.
//
// A ROW IS A SITTING, NOT A CALL. A coding agent makes hundreds of calls an
// hour, and a table of them is a table nobody reads. A row is the calls one
// conversation made with one outcome, with no more than thirty idle minutes
// between them: its main model, Claude Code's fast model, and the tool calls
// the model asked for, together, with the routes named on the row; the server decides membership when a request is
// written, so a sitting still going on is a row that is still growing. Every
// call stays one click away, opened under its row, and the CSV export stays
// per call, because an audit trail that can only show sums is not one.
//
// The "metadata only" claim is made once, in the footnote under the table,
// which is where a commitment belongs, and not as a banner defending the
// product to somebody who came to look up a request. The outcome is a filter,
// not a section, so it sits beside the window and the search boxes rather than
// as a tab bar, and every control is in one place.
//
// EVERY FILTER IS THE SERVER'S, AND EVERY ONE IS IN THE URL. The trail holds
// weeks and loads fifty rows at a time, so a control that narrowed only what
// was loaded would report "nothing matches" about a page while appearing to
// report it about the window - the one wrong answer that matters here. The
// same filters narrow the calls an opened row lists, so a row's total and the
// calls under it are always the same set.

type Outcome = 'all' | 'routed' | 'blocked' | 'error'

const OUTCOMES: { id: Outcome; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'routed', label: 'Routed' },
  { id: 'blocked', label: 'Blocked & refused' },
  { id: 'error', label: 'Errors' },
]

/** How the window divides, before the outcome filter is applied to it. Every
 *  option carries the count it would produce, and the three sum to `all`. */
type Outcomes = { all: number; routed: number; blocked: number; error: number }

type Page = {
  groups: RequestGroupRow[]
  next_cursor: string
  /** Groups, which is what the control and the rows count. */
  outcomes?: Outcomes
  /** The calls inside those groups, which is what the footer adds. */
  request_outcomes?: Outcomes
}

type CallPage = { requests: RequestRow[]; next_cursor: string }

/** What the four filter boxes hold. The keys are the query parameters, so
 *  spreading this into a request is the whole of applying them. */
type Filters = { user: string; repo: string; branch: string; model: string }

const NO_FILTERS: Filters = { user: '', repo: '', branch: '', model: '' }

const BOXES: { key: keyof Filters; label: string }[] = [
  { key: 'user', label: 'User email' },
  { key: 'repo', label: 'Repository' },
  { key: 'branch', label: 'Branch' },
  { key: 'model', label: 'Model' },
]

const PAGE = 50

// Eleven columns, and EVERY TRACK BUT ONE IS A FIXED WIDTH - which is the whole
// reason the header lines up with the rows.
//
// The head and each row are separate grid containers; nothing makes them agree
// except being handed the same tracks and resolving them the same way. A track
// that sizes to content (`auto`, `min-content`, `max-content`) resolves against
// whatever that row holds, so `minmax(222px, auto)` on the model column gave a
// row with `global.anthropic.claude-sonnet-5` one width and the row above it,
// holding an em dash, another - and the two columns after it stepped left and
// right down the table. One `1fr` is safe because it takes the same leftover
// every time once everything beside it is fixed.
//
// 236px on the model is a floor found by measuring the shipped face rather than
// taken from the design, which asked for 222px: at 13px Space Grotesk
// `global.anthropic.claude-sonnet-4-5` is 227px and would have ellipsised on
// every row. The prefix and the family have to read - two catalogue entries
// differing only in `us.` or `global.` bill about 10% apart and process in
// different countries.
//
// What still does not fit is the whole provider-native id: a real one carries a
// revision date and a version (`…-haiku-4-5-20251001-v1:0`, 311px), and a column
// wide enough for that would take a quarter of the table for the least
// discriminating part of the string. It is truncated from the right, so what is
// lost is the tail, and the full id is on the cell's title and in the export.
//
// Calls is 60px: four digits of figure face over the sitting's length, `1h05`.
//
// Route is 140px because a sitting can hold more than its main route: under the
// name goes `+1 route · 2 tool calls`, which is 132px in the small face.
const COLUMNS = '72px 164px minmax(0, 1fr) 140px 236px 80px 60px 58px 78px 92px 18px'

const GRID = { gridTemplateColumns: COLUMNS, gap: 12 }

// The calls inside an opened row. Time, model and outcome line up with the
// row's own columns in spirit rather than in pixels: the panel is indented, so
// its tracks are its own, and what it adds is the two things a sitting has no
// single value for - how long each call took and which request it was.
const CALL_COLUMNS = '72px minmax(0, 1fr) 58px 78px 176px 92px 150px'

const CALL_GRID = { gridTemplateColumns: CALL_COLUMNS, gap: 12 }

export function Requests() {
  const { session } = useSession()
  const { search, setParam, setParams } = useRouter()

  const range = (search.get('range') as Range) || '7d'
  const outcome = (search.get('outcome') as Outcome) || 'all'

  const [rows, setRows] = useState<RequestGroupRow[]>([])
  const [cursor, setCursor] = useState('')
  const [more, setMore] = useState(false)
  const [loadingMore, setLoadingMore] = useState(false)
  const [open, setOpen] = useState<Record<string, boolean>>({})

  // Two copies of the filters: what is being typed, and what has been asked
  // for. The URL holds the second. Pushing every keystroke into it would send
  // four queries on the way to "panorama" and race them against each other, and
  // would put four entries in the address bar for one search.
  const applied: Filters = {
    user: search.get('user') ?? '',
    repo: search.get('repo') ?? '',
    branch: search.get('branch') ?? '',
    model: search.get('model') ?? '',
  }
  const [draft, setDraft] = useState<Filters>(applied)
  useDebounced(`${draft.user}\n${draft.repo}\n${draft.branch}\n${draft.model}`, 300, () =>
    setParams(draft),
  )

  const everyone = session!.user.can_see_everyone
  const scope = everyone ? '' : 'me'

  const params = {
    since: range,
    scope,
    ...applied,
    outcome: outcome === 'all' ? '' : outcome,
  }
  const page = useApi<Page>(`/api/v1/requests/groups${query({ ...params, limit: PAGE })}`)

  // The counts describe the window and are sent with the first page only, so
  // they are held across a "load more" rather than dropped when the next
  // response arrives without them.
  const [outcomes, setOutcomes] = useState<Outcomes | null>(null)
  const [calls, setCalls] = useState<Outcomes | null>(null)

  useEffect(() => {
    if (!page.data) return
    setRows(page.data.groups)
    setCursor(page.data.next_cursor)
    setMore(Boolean(page.data.next_cursor))
    setOpen({})
    if (page.data.outcomes) setOutcomes(page.data.outcomes)
    if (page.data.request_outcomes) setCalls(page.data.request_outcomes)
  }, [page.data])

  // No `flush`: the header closes itself with its own full-bleed rule, like
  // every other screen. Drawn as the filter bar's top border instead, the line
  // would sit inside the band's page padding and stop 56px short at both ends.
  usePageChrome({ sub: 'Every request the gateway saw, grouped into sittings.' })

  const filtering = Object.values(applied).some(Boolean)
  const typing = Object.values(draft).some(Boolean)
  const total = outcomes ? countFor(outcomes, outcome) : rows.length
  const callTotal = calls ? countFor(calls, outcome) : null

  const loadMore = async () => {
    if (!cursor) return
    setLoadingMore(true)
    try {
      const next = await get<Page>(
        `/api/v1/requests/groups${query({ ...params, limit: PAGE, cursor })}`,
      )
      setRows((current) => [...current, ...next.groups])
      setCursor(next.next_cursor)
      setMore(Boolean(next.next_cursor))
    } finally {
      setLoadingMore(false)
    }
  }

  // A click is a deliberate act, so it applies at once rather than waiting out
  // a delay that exists for typing.
  const clear = (alsoOutcome: boolean) => {
    setDraft(NO_FILTERS)
    setParams({ ...NO_FILTERS, ...(alsoOutcome ? { outcome: null } : {}) })
  }

  return (
    <main className="bands">
      <section className="band rows last">
        {Boolean(page.error) && <ErrorBanner error={page.error} />}

        <div className="filter-bar split">
          <div className="filter">
            <span className="filter-label">Outcome</span>
            <Segmented
              value={outcome}
              dense
              label="Outcome"
              options={OUTCOMES.map((option) => ({
                ...option,
                count: outcomes ? count(countFor(outcomes, option.id)) : undefined,
              }))}
              onChange={(next) => setParam('outcome', next === 'all' ? null : next)}
            />
          </div>

          {/* The window and the export sit with the filters rather than in the
              header. Nine controls in two places is nine controls somebody has
              to look for in two places, and unlike Analytics this screen has
              nothing above the bar for a header control to govern. */}
          <div className="filter-right">
            <span className="filter-label">Window</span>
            <Segmented
              value={range}
              dense
              label="Time range"
              options={RANGES}
              onChange={(next) => setParam('range', next)}
            />
            {/* Back to the newest first page: a refresh re-asks for page one, so
                what "load more" had appended is replaced rather than kept. */}
            <RefreshButton />
            {/* One line per call, not per row: the export is the record an
                auditor reconciles, and a sum cannot be reconciled. */}
            <a className="btn" href={`/api/v1/export/requests.csv${query(params)}`}>
              Export CSV
            </a>
          </div>

          <div className="filter" style={{ flexWrap: 'wrap' }}>
            {/* A developer is not offered a user box: every row they can see is
                already theirs, and a search that could only ever match one
                person is a box that suggests otherwise. The server decides this
                too. */}
            {BOXES.filter((box) => everyone || box.key !== 'user').map((box) => (
              <input
                key={box.key}
                className="filter-search tight"
                type="text"
                value={draft[box.key]}
                placeholder={box.label}
                aria-label={`Filter by ${box.label.toLowerCase()}`}
                onChange={(e) =>
                  setDraft((current) => ({ ...current, [box.key]: e.target.value }))
                }
              />
            ))}
            {(filtering || typing) && (
              <button type="button" className="filter-clear" onClick={() => clear(false)}>
                Clear
              </button>
            )}
          </div>

          <span className="filter-note">latest activity first</span>
        </div>

        {page.loading ? (
          <Loading rows={6} />
        ) : rows.length === 0 ? (
          <div style={{ padding: '16px 0 8px' }}>
            <Empty
              title={filtering || outcome !== 'all' ? 'No requests match these filters' : 'No requests in this window'}
              action={
                filtering || outcome !== 'all' ? (
                  <button type="button" className="btn" onClick={() => clear(true)}>
                    Clear filters
                  </button>
                ) : undefined
              }
            >
              {filtering || outcome !== 'all'
                ? 'Every request in this window is still here - the outcome filter and the search terms exclude all of them.'
                : 'Point a coding tool at this gateway and every request it makes lands here.'}
            </Empty>
          </div>
        ) : (
          <div className="trail-scroll">
            <div>
              <div className="dtable-head" style={GRID}>
                <span>Time</span>
                <span>User</span>
                <span>Repo · branch</span>
                <span>Route</span>
                <span>Model</span>
                <span>Work item</span>
                <span style={{ textAlign: 'right' }}>Calls</span>
                <span style={{ textAlign: 'right' }}>Tokens</span>
                <span style={{ textAlign: 'right' }}>Cost</span>
                <span>Outcome</span>
                <span />
              </div>

              {rows.map((row) => (
                <GroupRow
                  key={row.id}
                  row={row}
                  params={params}
                  open={Boolean(open[row.id])}
                  onToggle={() => setOpen((was) => ({ ...was, [row.id]: !was[row.id] }))}
                />
              ))}
            </div>
          </div>
        )}

        {rows.length > 0 && (
          <div className="dtable-foot">
            <span>
              Showing {count(rows.length)} of {count(total)} {total === 1 ? 'sitting' : 'sittings'}
              {callTotal !== null &&
                ` · ${count(callTotal)} ${callTotal === 1 ? 'request' : 'requests'} in all`}
            </span>
            {more && (
              <button
                type="button"
                className="btn small"
                onClick={() => void loadMore()}
                disabled={loadingMore}
              >
                {loadingMore ? 'Loading…' : `Load ${PAGE} more`}
              </button>
            )}
          </div>
        )}

        <div className="footnote">
          A row is the requests one conversation made with one outcome, model calls and tool calls
          together, with no more than thirty idle minutes between them; open it for each request. A blocked request never
          counts against the budget that blocked it. The predicate is whether a provider was
          actually invoked, applied identically in the ledger and in reconciliation. Prompt and
          response bodies are not stored - not here, not behind a flag, not in a debug mode.
        </div>
      </section>
    </main>
  )
}

/** countFor is how many rows an option would select. `all` is a key of the
 *  payload like the other three, so nothing here adds them up - the server does
 *  that once, and the sum a reader checks is the sum it computed. */
function countFor(outcomes: Outcomes, id: Outcome): number {
  return outcomes[id]
}

/** One sitting, and the calls it is made of underneath. */
function GroupRow({
  row,
  params,
  open,
  onToggle,
}: {
  row: RequestGroupRow
  params: Record<string, string>
  open: boolean
  onToggle: () => void
}) {
  const total =
    row.tokens.input + row.tokens.output + row.tokens.cache_read + row.tokens.cache_write

  return (
    <div className="dtable-group">
      <button
        type="button"
        className="dtable-open"
        style={GRID}
        aria-expanded={open}
        onClick={onToggle}
      >
        <span className="cell-stacked">
          <span className="top cell-key">{time(row.last_at)}</span>
          <span className="bottom">{date(row.last_at)}</span>
        </span>
        <span className="cell-text truncate">{row.user_email || 'service token'}</span>
        <span className="cell-stacked text">
          <span className="top">{row.repo || 'no repository'}</span>
          <span className="bottom">{row.branch || 'no branch'}</span>
        </span>
        <GroupRoute row={row} />
        <Models models={row.models} />
        <span className={row.work_item ? 'cell-item truncate' : 'cell-item none'}>
          {row.work_item ?? '-'}
        </span>
        <span className="cell-stacked" style={{ textAlign: 'right' }}>
          <span className="top cell-fig sm">{count(row.requests)}</span>
          <span className="bottom">{span(row)}</span>
        </span>
        <span className="cell-fig sm soft">{tokens(total)}</span>
        <GroupCost row={row} />
        <span>
          <GroupOutcome row={row} />
        </span>
        <span className="caret">{open ? '−' : '+'}</span>
      </button>

      {open && (
        <div
          className="detail indent"
          style={{
            gridTemplateColumns: 'repeat(4, minmax(0, 1fr))',
            // The time column's 72px, the 12px gap and the row's 12px padding:
            // the panel starts under the user, which is where the row's data
            // starts reading.
            ['--detail-indent' as string]: '96px',
          }}
        >
          <div className="detail-block">
            <span className="detail-label">Sitting</span>
            <span className="detail-value">{sittingOf(row)}</span>
          </div>
          <div className="detail-block">
            <span className="detail-label">Policy applied</span>
            <span className="detail-value">{groupPolicyOf(row)}</span>
          </div>
          <div className="detail-block">
            <span className="detail-label">Provenance</span>
            <span>
              {row.rung ? (
                <Pill tone={row.rung === 'stamp' ? 'good' : row.rung === 'content' ? 'warn' : undefined}>
                  {row.rung}
                </Pill>
              ) : (
                <Pill tone="off">unattributed</Pill>
              )}
            </span>
          </div>
          <div className="detail-block">
            <span className="detail-label">Time on the model · cache read</span>
            <GroupLatency row={row} />
          </div>
          <div className="detail-block" style={{ gridColumn: '1 / -1' }}>
            <Calls group={row.id} params={params} />
          </div>
        </div>
      )}
    </div>
  )
}

/**
 * span is how long a sitting ran, under its call count. The time column says
 * when somebody stopped, and nothing about how long they had been going.
 */
function span(row: RequestGroupRow): string {
  const minutes = Math.round((Date.parse(row.last_at) - Date.parse(row.first_at)) / 60_000)
  if (minutes < 1) return '<1m'
  if (minutes < 60) return `${minutes}m`
  return `${Math.floor(minutes / 60)}h${String(minutes % 60).padStart(2, '0')}`
}

/** One model, or both ends when the route was re-pointed mid-sitting, which is
 *  said rather than resolved to one of them. */
function Models({ models }: { models: string[] }) {
  if (models.length === 0) return <span className="cell-text none">-</span>
  if (models.length === 1) {
    return (
      <span className="cell-text truncate" title={models[0]}>
        {models[0]}
      </span>
    )
  }
  return (
    <span className="cell-stacked text" title={models.join('\n')}>
      <span className="top truncate">{models[0]}</span>
      <span className="bottom">and {count(models.length - 1)} more</span>
    </span>
  )
}

/**
 * The routes a sitting used, and its tool calls beside them, with the reason
 * under them when nothing was served. A sitting is a conversation, so it can
 * hold the main model, Claude Code's fast model and the searches the model
 * asked for: the first route is named and the rest are counted, the way the
 * model column does it.
 */
function GroupRoute({ row }: { row: RequestGroupRow }) {
  const reason = groupRefusalOf(row)
  const more = row.route_count > 1 ? `+${count(row.route_count - 1)} ${row.route_count === 2 ? 'route' : 'routes'}` : ''
  const tools = row.tool_calls > 0 ? toolCallsLabel(row.tool_calls) : ''

  if (row.routes.length === 0) {
    return tools ? (
      <span className="cell-stacked text">
        <span className={reason ? 'top dim' : 'top'}>
          <Pill>tool call</Pill>
        </span>
        <span className="bottom">{tools}</span>
        {reason && <span className="cell-reason">{reason}</span>}
      </span>
    ) : (
      <span className="cell-text none">-</span>
    )
  }

  const under = [more, tools].filter(Boolean).join(' · ')
  return (
    <span className="cell-stacked text" title={row.routes.join('\n')}>
      <span className={reason ? 'top dim truncate' : 'top truncate'}>{row.routes[0]}</span>
      {under && <span className="bottom">{under}</span>}
      {reason && <span className="cell-reason">{reason}</span>}
    </span>
  )
}

function toolCallsLabel(n: number): string {
  return `${count(n)} ${n === 1 ? 'tool call' : 'tool calls'}`
}

/**
 * groupRefusalOf is refusalOf for a sitting. A group of refusals holds one
 * outcome class and may hold more than one reason inside it, and a reason that
 * is not true of every call in the row is not printed as though it were.
 */
function groupRefusalOf(row: RequestGroupRow): string {
  const { refused, blocked_hard, blocked_soft } = row.decisions
  if (row.outcome === 'error') {
    return row.status_code ? `upstream returned ${row.status_code}` : 'the request failed'
  }
  if (row.outcome !== 'blocked') return ''

  if (refused === row.requests) {
    switch (row.error_kind) {
      case 'unknown_route':
      case 'route_not_available':
        return 'not a route for this caller'
    }
    return 'refused at admission'
  }
  if (blocked_hard === row.requests) return 'past the hard limit for this caller'
  if (blocked_soft === row.requests) {
    return row.error_kind === 'blocked_over_soft'
      ? 'route does not serve past the soft limit'
      : 'past the soft limit for this caller'
  }
  return 'refused and blocked, open for each'
}

/**
 * The sitting's cost, with the absences told apart as the per-call column does:
 * `-` for nothing that reached a provider, "unpriced" for calls that did and
 * have no rate. A sum with some calls unpriced says how many, because a total
 * that silently leaves them out looks authoritative and is short.
 */
function GroupCost({ row }: { row: RequestGroupRow }) {
  const unpriced = row.unpriced_billable
  if (row.attempts === 0 && row.outcome !== 'routed') {
    return <span className="cell-fig sm none">-</span>
  }
  if (unpriced > 0 && unpriced === row.requests) {
    return (
      <span
        className="cell-fig sm warn"
        title="This model has no rate, so these requests are recorded unpriced rather than costed at zero."
      >
        unpriced
      </span>
    )
  }
  if (unpriced > 0) {
    return (
      <span className="cell-stacked" title={`${count(unpriced)} of these requests have no rate and are not in the total.`}>
        <span className="top cell-fig sm">{money(row.cost.total_nanos)}</span>
        <span className="cell-reason">+{count(unpriced)} unpriced</span>
      </span>
    )
  }
  return <span className="cell-fig sm">{money(row.cost.total_nanos)}</span>
}

/**
 * The word for the sitting, from what its calls recorded. A group is one class,
 * so this is only ever choosing between the words inside it: warned or allowed,
 * refused or blocked or soft limit. Where a group of refusals holds more than
 * one of those, the stronger word is shown and the panel lists every call.
 */
function GroupOutcome({ row }: { row: RequestGroupRow }) {
  const { refused, blocked_hard, blocked_soft, warned } = row.decisions
  if (row.outcome === 'error') return <Pill tone="warn">error</Pill>
  if (row.outcome === 'blocked') {
    if (blocked_hard > 0) return <Pill tone="off">blocked</Pill>
    if (blocked_soft > 0 && refused === 0) return <Pill tone="warn">soft limit</Pill>
    return <Pill tone="off">refused</Pill>
  }
  if (warned > 0) return <Pill tone="warn">warned</Pill>
  return <Pill tone="good">allowed</Pill>
}

/** What the sitting was, in one line: how many calls over how long. */
function sittingOf(row: RequestGroupRow): string {
  const tools = row.tool_calls > 0 ? `, ${toolCallsLabel(row.tool_calls)} among them` : ''
  const calls = `${count(row.requests)} ${row.requests === 1 ? 'request' : 'requests'}${tools}`
  if (row.first_at === row.last_at) return `${calls} at ${time(row.first_at)}`
  return `${calls} from ${time(row.first_at)} to ${time(row.last_at)}`
}

/** policyOf for a sitting: every decision the class is made of, counted. */
function groupPolicyOf(row: RequestGroupRow): string {
  const { refused, blocked_hard, blocked_soft, warned } = row.decisions

  if (row.outcome === 'blocked') {
    const parts: string[] = []
    if (refused > 0) parts.push(`${count(refused)} refused at admission`)
    if (blocked_hard > 0) parts.push(`${count(blocked_hard)} past a hard limit`)
    if (blocked_soft > 0) parts.push(`${count(blocked_soft)} past a soft limit`)
    parts.push('provider never invoked, not counted against the budget')
    return parts.join(' · ')
  }
  if (row.outcome === 'error') {
    if (row.attempts === 0) return 'Failed before a provider was invoked · not counted against the budget'
    return `${count(row.attempts)} of ${count(row.requests)} reached a provider, nothing billed · not counted against the budget`
  }
  if (warned === 0) return 'Served · within the budget'
  if (warned === row.requests) return 'Served · past the soft limit'
  return `Served · ${count(warned)} of ${count(row.requests)} past the soft limit`
}

/** Time spent waiting on the model across the sitting, and how much of the
 *  context came back from the cache - the number that decides what a coding
 *  agent costs. */
function GroupLatency({ row }: { row: RequestGroupRow }) {
  if (row.attempts === 0 && row.outcome === 'blocked') {
    return <span className="detail-value">refused at admission</span>
  }
  const context = row.tokens.input + row.tokens.cache_read
  const cached = context > 0 ? percent(row.tokens.cache_read, context) : 'no context read'
  return (
    <span className="cell-key">
      {duration(row.duration_ms)} · {cached}
    </span>
  )
}

/**
 * The calls of one sitting, fetched when it is opened.
 *
 * Through /requests with `group` set and every filter the table is under, so
 * what is listed is exactly what the row summed - a row filtered to one model
 * does not open onto calls on another. The server scopes it like any other
 * read of the trail: a group id is a filter, never a way into somebody else's.
 */
function Calls({ group, params }: { group: string; params: Record<string, string> }) {
  const first = useApi<CallPage>(`/api/v1/requests${query({ ...params, group, limit: PAGE })}`)
  const [calls, setCalls] = useState<RequestRow[]>([])
  const [cursor, setCursor] = useState('')
  const [loadingMore, setLoadingMore] = useState(false)

  useEffect(() => {
    if (!first.data) return
    setCalls(first.data.requests)
    setCursor(first.data.next_cursor)
  }, [first.data])

  const loadMore = async () => {
    if (!cursor) return
    setLoadingMore(true)
    try {
      const next = await get<CallPage>(
        `/api/v1/requests${query({ ...params, group, limit: PAGE, cursor })}`,
      )
      setCalls((current) => [...current, ...next.requests])
      setCursor(next.next_cursor)
    } finally {
      setLoadingMore(false)
    }
  }

  if (first.error) return <ErrorBanner error={first.error} />
  if (first.loading) return <Loading rows={3} />

  return (
    <div className="trail-calls">
      <div className="trail-calls-head" style={CALL_GRID}>
        <span>Time</span>
        <span>Model or tool</span>
        <span style={{ textAlign: 'right' }}>Tokens</span>
        <span style={{ textAlign: 'right' }}>Cost</span>
        <span>Latency · cache read</span>
        <span>Outcome</span>
        <span>Request</span>
      </div>
      {calls.map((call) => (
        <CallRow key={call.id} row={call} />
      ))}
      {cursor && (
        <div className="trail-calls-more">
          <button
            type="button"
            className="btn small"
            onClick={() => void loadMore()}
            disabled={loadingMore}
          >
            {loadingMore ? 'Loading…' : `Load ${PAGE} more`}
          </button>
        </div>
      )}
    </div>
  )
}

/** One call: what ran, what it cost, what was decided, and its request id. */
function CallRow({ row }: { row: RequestRow }) {
  if (row.surface === 'mcp') return <ToolCallRow row={row} />
  const total =
    row.tokens.input + row.tokens.output + row.tokens.cache_read + row.tokens.cache_write
  const reason = refusalOf(row)

  return (
    <div className="trail-calls-row" style={CALL_GRID}>
      <span className="cell-key">{time(row.started_at)}</span>
      {row.resolved_model ? (
        <span className="cell-stacked text">
          <span className="top truncate" title={row.resolved_model}>
            {row.resolved_model}
          </span>
          {reason && <span className="cell-reason">{reason}</span>}
        </span>
      ) : (
        <span className="cell-stacked text">
          <span className="top dim">-</span>
          {reason && <span className="cell-reason">{reason}</span>}
        </span>
      )}
      <span className="cell-fig sm soft">{tokens(total)}</span>
      <Cost row={row} />
      <Latency row={row} />
      <span>
        <Outcome row={row} />
      </span>
      <span className="cell-key truncate" title={row.request_id}>
        {row.request_id}
      </span>
    </div>
  )
}

/**
 * A tool call among the model calls it was made for. The same columns, read
 * for a tool: the tool's name where a model would be, the searches it ran
 * where tokens would be, and the time it took. A search reads no context, so
 * there is no cache figure to give.
 */
function ToolCallRow({ row }: { row: RequestRow }) {
  const reason = toolRefusalOf(row)
  const searches = row.web_searches ?? 0
  return (
    <div className="trail-calls-row" style={CALL_GRID}>
      <span className="cell-key">{time(row.started_at)}</span>
      <span className="cell-stacked text">
        <span className="top truncate" title={row.requested_model}>
          <Pill>tool call</Pill> {row.requested_model}
        </span>
        {reason && <span className="cell-reason">{reason}</span>}
      </span>
      <span className="cell-fig sm soft">
        {searches > 0 ? `${count(searches)} ${searches === 1 ? 'search' : 'searches'}` : '-'}
      </span>
      <Cost row={row} />
      <span className="cell-key">{duration(row.duration_ms)}</span>
      <span>
        <Outcome row={row} />
      </span>
      <span className="cell-key truncate" title={row.request_id}>
        {row.request_id}
      </span>
    </div>
  )
}

/** Why a tool call went no further, in the words the gateway records. */
function toolRefusalOf(row: RequestRow): string {
  switch (row.error_kind) {
    case 'web_search_off':
      return 'web search is switched off'
    case 'invalid_arguments':
      return 'the model sent arguments the tool refused'
  }
  if (row.decision === 'error') return 'the search failed'
  return refusalOf(row)
}

/**
 * refusalOf is why the request went no further, in the words the gateway uses
 * for it. Empty for a request that was served.
 *
 * `error_kind` is read first because it is the gateway's own reason and is more
 * specific than the decision. The decision is the fallback for a row whose
 * reason this build has not been taught to phrase - which is a row from a newer
 * gateway, and saying "budget exhausted" about it would be inventing a cause.
 */
function refusalOf(row: RequestRow): string {
  switch (row.error_kind) {
    case 'unknown_route':
    case 'route_not_available':
      return 'not a route for this caller'
    case 'blocked_over_soft':
      return 'route does not serve past the soft limit'
  }
  switch (row.decision) {
    case 'blocked':
      return row.budget_state === 'over_soft'
        ? 'past the soft limit for this caller'
        : 'past the hard limit for this caller'
    case 'refused':
      return 'refused at admission'
    case 'error':
      return row.status_code > 0 ? `upstream returned ${row.status_code}` : 'the request failed'
  }
  return ''
}

/**
 * Cost, with the two absences told apart.
 *
 * A dash is a request that never reached a provider - a refusal or a block has
 * no cost, and `$0.00` would read as a free call. "Unpriced" is a request that
 * did reach one and whose model has no rate: recorded rather than costed at
 * zero, which is the same commitment the product makes about unattributed
 * spend, applied to pricing.
 */
function Cost({ row }: { row: RequestRow }) {
  if (row.priced && row.cost) return <span className="cell-fig sm">{money(row.cost.total_nanos)}</span>
  if ((row.attempts ?? 0) > 0 || row.decision === 'allowed') {
    return (
      <span
        className="cell-fig sm warn"
        title="This model has no rate, so the request is recorded unpriced rather than costed at zero."
      >
        unpriced
      </span>
    )
  }
  return <span className="cell-fig sm none">-</span>
}

/**
 * Outcome is what the gateway did, in the word it recorded.
 *
 * Four decisions under a three-way filter, and deliberately: the filter narrows and
 * the column states, so "soft limit" and "refused" both belong to "Blocked &
 * refused" while meaning opposite things to whoever is reading. Over a hard
 * limit nothing runs until somebody raises it; past a soft one the engineer
 * names another route and carries on.
 */
function Outcome({ row }: { row: RequestRow }) {
  if (row.decision === 'blocked') {
    return row.budget_state === 'over_soft' ? (
      <Pill tone="warn">soft limit</Pill>
    ) : (
      <Pill tone="off">blocked</Pill>
    )
  }
  if (row.decision === 'refused') return <Pill tone="off">refused</Pill>
  if (row.decision === 'error') return <Pill tone="warn">error</Pill>
  if (row.budget_state === 'over_soft') return <Pill tone="warn">warned</Pill>
  return <Pill tone="good">allowed</Pill>
}

/**
 * Latency and how much of the context came back from the cache.
 *
 * The face follows the value, which is the design's rule rather than a
 * flourish: "2.4s · 61%" is a pair of measures and is set in the mono, and
 * "refused at admission" is a sentence and is set in the UI face. A phrase in
 * the figure face reads as a value that failed to render.
 */
function Latency({ row }: { row: RequestRow }) {
  if ((row.attempts ?? 0) === 0 && (row.decision === 'blocked' || row.decision === 'refused')) {
    return <span className="detail-value">refused at admission</span>
  }

  const context = row.tokens.input + row.tokens.cache_read
  const cached = context > 0 ? percent(row.tokens.cache_read, context) : 'no context read'
  return (
    <span className="cell-key">
      {duration(row.duration_ms)} · {cached}
      {row.ttft_ms ? ` · ${row.ttft_ms.toFixed(0)}ms` : ''}
    </span>
  )
}
