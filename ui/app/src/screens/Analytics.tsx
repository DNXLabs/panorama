// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { query } from '../api'
import type { Range } from '../api'
import { usePageChrome } from '../chrome'
import { count, money, percent, tokens, windowAdjective, windowLabel } from '../format'
import { cappingBudget, everyWindow } from '../people'
import { Link, useRouter } from '../router'
import { useSession } from '../session'
import type {
  Allocation,
  BudgetRow,
  Bucket,
  Coverage,
  Outcomes,
  Policy,
  Seats,
  TeamRow,
  Totals,
  Usage,
  Work,
  WorkBucket,
} from '../types'
import {
  CoverageBar,
  DailySpend,
  Empty,
  ErrorBanner,
  Kpi,
  Loading,
  RangePicker,
  RefreshButton,
  Share,
  Track,
  useApi,
} from '../ui'
import { PeopleTab, SessionsTab, TeamsTab, WorkTab } from './AnalyticsTables'

// The start page, and three questions on one screen.
//
// "Is everything fine", "what are we spending" and "what did it buy" are asked
// of the same window over the same four endpoints, share the daily chart, and
// all lean on attribution coverage - so a reader answering one passes the
// answer to the other two on the way. They are one screen with tabs.
//
// TABS RATHER THAN ANCHORS, and only the active one is mounted: as one scroll
// the screen would be four thousand pixels of bands with a jump list at the
// top, which is a table of contents admitting the page is too long rather than
// a page that is short enough.
//
// The two questions it exists to answer inside five seconds are on the first
// tab, side by side, at 52px: are we on budget, and is the spend attached to
// real work. Everything else is a drill-down behind a tab.

type Tab =
  | 'spend'
  | 'attribution'
  | 'teams'
  | 'people'
  | 'work'
  | 'costcenters'
  | 'sessions'
  | 'models'
  | 'attention'

type TabSpec = {
  id: Tab
  label: string
  /** Tabs that rank named people, or aggregate the whole organization, are for
   *  a reader entitled to that. The server refuses the data regardless - this
   *  only decides what is worth offering, exactly as the sidebar does. */
  leaders?: boolean
}

const TABS: TabSpec[] = [
  { id: 'spend', label: 'Spend' },
  { id: 'attribution', label: 'Attribution' },
  { id: 'teams', label: 'Teams', leaders: true },
  { id: 'people', label: 'People', leaders: true },
  { id: 'work', label: 'Work' },
  { id: 'costcenters', label: 'Cost centers', leaders: true },
  { id: 'sessions', label: 'Sessions' },
  { id: 'models', label: 'Models & tools' },
  { id: 'attention', label: 'Attention' },
]

export function Analytics() {
  const { session } = useSession()
  const { search, setParam } = useRouter()
  const everyone = session!.user.can_see_everyone
  const scope = everyone ? '' : 'me'

  const range = (search.get('range') as Range) || '30d'
  const asked = (search.get('tab') as Tab) || 'spend'
  const visible = TABS.filter((t) => !t.leaders || everyone)
  const tab = visible.some((t) => t.id === asked) ? asked : 'spend'

  const usage = useApi<Usage>(`/api/v1/usage${query({ since: range, scope })}`, [range])
  const work = useApi<Work>(`/api/v1/work${query({ since: range, scope })}`, [range])
  const outcomes = useApi<Outcomes>(`/api/v1/outcomes${query({ since: range, scope })}`, [range])
  const seats = useApi<Seats>(`/api/v1/seats${query({ since: range })}`, [range])
  const policy = useApi<Policy>('/api/v1/policy')
  // Only for a reader entitled to it. The server refuses it either way; a
  // developer is never served somebody else's accounting line.
  const allocation = useApi<Allocation | null>(
    everyone ? `/api/v1/allocation${query({ since: range })}` : null,
    [range, everyone],
  )
  // Every team, not only the ones with traffic: `usage.by_team` joins requests,
  // so a team that routed nothing is absent from it - and a team that routed
  // nothing is precisely what the attention item is about.
  const teams = useApi<{ teams: TeamRow[] }>(everyone ? '/api/v1/teams' : null)

  const attention = attentionItems({
    coverage: work.data?.coverage,
    seats: seats.data,
    budgets: policy.data?.budgets ?? [],
    teams: teams.data?.teams ?? [],
    active: usage.data?.by_team ?? [],
    allocation: allocation.data,
  })

  usePageChrome(
    {
      // The tab bar under the header is the rule that closes it.
      flush: true,
      actions: (
        <>
          <RangePicker value={range} onChange={(next) => setParam('range', next)} />
          <RefreshButton />
          {/* The export follows the tab. On Cost centers the file somebody came
              for is the one keyed on their finance codes, with the unallocated
              row in it - handing them usage.csv there would be handing them a
              different question's answer. */}
          <a
            className="btn"
            href={
              tab === 'costcenters'
                ? `/api/v1/export/allocation.csv${query({ since: range })}`
                : `/api/v1/export/usage.csv${query({ since: range, scope })}`
            }
          >
            Export CSV
          </a>
        </>
      ),
    },
    [range, scope, tab],
  )

  return (
    <>
      <nav className="screen-tabs" aria-label="Analytics sections">
        {visible.map((spec) => (
          <button
            key={spec.id}
            type="button"
            className={spec.id === tab ? 'screen-tab active' : 'screen-tab'}
            aria-current={spec.id === tab ? 'page' : undefined}
            onClick={() => setParam('tab', spec.id === 'spend' ? null : spec.id)}
          >
            {spec.label}
            {spec.id === 'attention' && attention.length > 0 && (
              <span className="screen-tab-count">{attention.length}</span>
            )}
          </button>
        ))}
      </nav>

      <main className="bands">
        {Boolean(usage.error || work.error) && (
          <div className="band">
            <ErrorBanner error={usage.error ?? work.error} />
          </div>
        )}

        {tab === 'spend' && (
          <SpendTab
            range={range}
            usage={usage.data}
            loading={usage.loading}
            coverage={work.data?.coverage}
            items={work.data?.by_work_item ?? []}
            budgets={policy.data?.budgets ?? []}
            seats={seats.data}
            onBreakdown={() => setParam('tab', 'attribution')}
          />
        )}

        {tab === 'attribution' && <AttributionTab work={work.data} loading={work.loading} />}

        {tab === 'teams' && <TeamsTab usage={usage.data} loading={usage.loading} seats={seats.data} />}

        {tab === 'people' && <PeopleTab range={range} seats={seats.data} loading={seats.loading} />}

        {tab === 'work' && (
          <WorkTab
            work={work.data}
            outcomes={outcomes.data}
            loading={work.loading}
            search={search}
            setParam={setParam}
          />
        )}

        {tab === 'costcenters' && (
          <CostCentersTab data={allocation.data} loading={allocation.loading} />
        )}

        {tab === 'sessions' && (
          <SessionsTab range={range} scope={scope} search={search} setParam={setParam} />
        )}

        {tab === 'models' && <ModelsTab usage={usage.data} loading={usage.loading} />}

        {tab === 'attention' && <AttentionTab items={attention} />}
      </main>
    </>
  )
}

/* --------------------------------------------------------------- tab: spend --
 *
 * Two figures at 52px, side by side, answering the two questions this screen
 * exists for. Nothing else is above the fold on purpose.
 */

function SpendTab({
  range,
  usage,
  loading,
  coverage,
  items,
  budgets,
  seats,
  onBreakdown,
}: {
  range: Range
  usage: Usage | null
  loading: boolean
  coverage?: Coverage
  items: WorkBucket[]
  budgets: BudgetRow[]
  seats: Seats | null
  onBreakdown: () => void
}) {
  const totals = usage?.totals
  // Which organization budget is "the cap" is people.ts's decision and not a
  // second one made here: the monthly window first, and any other named as
  // what it is. Taking the first org budget with a hard limit would, on a
  // deployment with a daily circuit breaker, read "$597.87 of a $50.00 cap" -
  // thirty days of spend against one day's ceiling, under a heading that asks
  // whether the deployment is on budget.
  const cap = cappingBudget(
    budgets.filter((b) => b.enabled && b.spend_visible),
    'org',
    '',
  )

  // WHAT A PIECE OF WORK COSTS, which is the question a budget holder is
  // actually asking. Not cost per request, which answers a different question:
  // it is the average rate of a model, and it moves when somebody changes
  // model rather than when the work changes. Nobody sets out to buy a request.
  //
  // Attributed spend over the number of things it was attributed to - a branch,
  // a pull request, a ticket. Both halves come from the same rollup, so this
  // cannot divide one window's spend by another's item count. It is blank
  // rather than zero when nothing has been attributed: an average over no items
  // is not a small number, it is not a number.
  const attributedItems = items.filter((item) => item.attributed)
  const perItem =
    attributedItems.length > 0 && coverage
      ? Math.round(coverage.attributed_cost_nanos / attributedItems.length)
      : null

  return (
    <>
      <section className="band split" style={{ ['--split-left' as string]: '1.1fr' }}>
        <div>
          <span className="band-kicker spaced">Spend · {windowLabel(range)}</span>
          <div className="hero">
            <span className="hero-figure">
              {totals ? money(totals.cost.total_nanos) : '-'}
            </span>
            {cap?.limits.hard ? (
              <span className="hero-of">of a {money(cap.limits.hard)} {everyWindow(cap)} cap</span>
            ) : (
              <span className="hero-of">with no cap over it</span>
            )}
          </div>

          {cap?.limits.hard ? (
            <>
              <Track
                value={cap.spent_nanos ?? 0}
                of={cap.limits.hard}
                notch={cap.limits.soft}
                thick
                tone={cap.state === 'over_hard' ? 'neutral' : cap.state === 'over_soft' ? 'warn' : 'accent'}
              />
              <div className="band-meta">
                <span>
                  {cap.percent_of_hard_limit ?? 0}% of the {everyWindow(cap)} cap used
                </span>
                {cap.limits.soft ? <span>warns at {money(cap.limits.soft)}</span> : <span />}
              </div>
            </>
          ) : (
            // Amber rather than reassuring, and the same wording People & teams
            // uses: an account with nothing above it is a fact to act on, not
            // a clean bill of health.
            <p className="prose" style={{ maxWidth: '46ch' }}>
              No budget governs this deployment, so nothing here warns and nothing blocks.{' '}
              <Link to="/people">Set one</Link> on the organization, a team, or a person.
            </p>
          )}

          {totals && totals.unpriced_billable > 0 && (
            <div className="band-meta">
              <span style={{ color: 'var(--state-warn)' }}>
                {count(totals.unpriced_billable)} requests could not be priced
              </span>
              <span />
            </div>
          )}
        </div>

        <div>
          <span className="band-kicker spaced">Attributed to work</span>
          <div className="hero">
            <span className="hero-figure accent">
              {coverage && coverage.cost_nanos > 0
                ? percent(coverage.attributed_cost_nanos, coverage.cost_nanos)
                : '-'}
            </span>
            <span className="hero-of">of spend tied to an item</span>
          </div>
          {coverage ? (
            <CoverageBar
              attributed={coverage.attributed_cost_nanos}
              total={coverage.cost_nanos}
            />
          ) : (
            <Loading rows={1} />
          )}
          <p
            className="prose"
            style={{ maxWidth: '46ch', marginTop: 22, color: 'var(--text-tertiary)' }}
          >
            The remainder could not be placed. Exploratory sessions and work outside a branch
            cannot be tied to an item, and nothing is inferred.
          </p>
          <button type="button" className="btn link" onClick={onBreakdown}>
            See the breakdown →
          </button>
        </div>
      </section>

      <section className="band tight">
        <div className="grid k3" style={{ gap: 48 }}>
          <Kpi
            label="Requests"
            value={totals ? count(totals.requests) : '-'}
            note={`${windowAdjective(range)} window`}
          />
          <Kpi
            label="Cost / work item"
            value={perItem === null ? '-' : money(perItem)}
            note={
              attributedItems.length > 0
                ? `across ${count(attributedItems.length)} attributed ${attributedItems.length === 1 ? 'item' : 'items'}`
                : 'nothing has been attributed yet'
            }
          />
          <Kpi
            label="Active developers"
            value={seats ? `${seats.seats.active} of ${seats.seats.provisioned}` : '-'}
            note={
              seats && seats.seats.idle > 0
                ? `${seats.seats.idle} ${seats.seats.idle === 1 ? 'seat' : 'seats'} spent nothing`
                : 'every provisioned seat was used'
            }
          />
        </div>
      </section>

      <section className="band last">
        <div className="band-head">
          <span className="band-title">Daily spend</span>
          <span className="band-sub">
            One column per day in the window. Hover a column for that day&rsquo;s total.
          </span>
        </div>
        {loading ? (
          <Loading />
        ) : (
          <DailySpend days={usage?.by_day ?? []} since={usage?.since} until={usage?.until} />
        )}
      </section>
    </>
  )
}

/* --------------------------------------------------- tab: attribution --------
 *
 * What the attributed half bought, and why the other half is the other half.
 * The right-hand column is the one that makes the claim honest: every gap is
 * stated with its reason rather than absorbed into a total.
 */

/** How a work item's kind reads in a sentence. */
const KIND_LABELS: Record<string, string> = {
  ticket: 'Tickets',
  pull_request: 'Pull requests',
  branch: 'Branches',
}

function AttributionTab({ work, loading }: { work: Work | null; loading: boolean }) {
  if (loading || !work) {
    return (
      <section className="band last">
        <Loading rows={4} />
      </section>
    )
  }

  // Grouped by the kind of thing the money was tied to, which is the one
  // grouping the ladder actually produces. A ticket is what the work is for, a
  // pull request is how it was done, a branch is where.
  const kinds = new Map<string, { cost: number; items: number; sessions: number }>()
  for (const item of work.by_work_item) {
    if (!item.attributed) continue
    const at = kinds.get(item.kind) ?? { cost: 0, items: 0, sessions: 0 }
    at.cost += item.cost.total_nanos
    at.items += 1
    at.sessions += item.sessions
    kinds.set(item.kind, at)
  }
  const bought = [...kinds.entries()].sort((a, b) => b[1].cost - a[1].cost)
  const boughtMax = Math.max(1, ...bought.map(([, v]) => v.cost))

  const unplaced = work.unplaced ?? []
  const unplacedMax = Math.max(1, ...unplaced.map((u) => u.cost_nanos))
  const unplacedTotal = unplaced.reduce((sum, u) => sum + u.cost_nanos, 0)

  return (
    <section className="band split last">
      <div>
        <div className="band-head">
          <span className="band-title">What attributed spend bought</span>
          <span className="band-sub">Attributed spend by the kind of work it was tied to.</span>
        </div>
        {bought.length === 0 ? (
          <Empty title="Nothing has been attributed yet">
            Spend appears here as soon as a session can be tied to a branch, a pull request, or a
            ticket.
          </Empty>
        ) : (
          <div className="shares">
            {bought.map(([kind, value]) => (
              <Share
                key={kind}
                name={KIND_LABELS[kind] ?? kind.replace('_', ' ')}
                value={value.cost}
                of={boughtMax}
                meta={
                  <>
                    <span>
                      {count(value.items)} {value.items === 1 ? 'item' : 'items'}
                    </span>
                    <span>
                      {count(value.sessions)} {value.sessions === 1 ? 'session' : 'sessions'}
                    </span>
                  </>
                }
              />
            ))}
          </div>
        )}
      </div>

      <div>
        <div className="band-head">
          <span className="band-title">Why the rest could not be placed</span>
          <span className="band-sub">Every gap is stated with its reason rather than absorbed.</span>
        </div>
        {unplaced.length === 0 ? (
          <Empty title="Everything in this window was placed">
            Nothing is unattributed, which is unusual and worth not taking for granted - the
            number is measured, not assumed.
          </Empty>
        ) : (
          <>
            <div className="shares">
              {unplaced.map((reason) => (
                <Share
                  key={reason.reason}
                  name={reason.label}
                  value={reason.cost_nanos}
                  of={unplacedMax}
                  tone="neutral"
                  meta={<span>{reason.detail}</span>}
                />
              ))}
            </div>
            <div className="band-total">
              <span>Unplaced total</span>
              <span>{money(unplacedTotal)}</span>
            </div>
          </>
        )}
      </div>
    </section>
  )
}

/* -------------------------------------------------------- tab: models & tools */

function ModelsTab({ usage, loading }: { usage: Usage | null; loading: boolean }) {
  return (
    <section className="band last">
      <div className="band-head">
        <span className="band-title">Models and tools</span>
        <span className="band-sub">
          Priced per token class from the rate table on every request.
        </span>
      </div>

      {loading ? (
        <Loading rows={3} />
      ) : (
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'minmax(0, 1.6fr) minmax(0, 1fr)',
            gap: 56,
            alignItems: 'start',
          }}
        >
          {/* BOTH LISTS, ONE UNDER THE OTHER, rather than a switch between them.
              They are two answers to two different questions - which model the
              money went to, and which harness sent it - and a reader comparing
              a cache rate across the two had to hold one in their head while
              they clicked to the other. Six rows fit in the space the switch
              was saving. */}
          <div style={{ display: 'flex', flexDirection: 'column', gap: 34 }}>
            <Breakdown title="Models" buckets={usage?.by_model ?? []} />
            <Breakdown title="Tools" buckets={usage?.by_tool ?? []} />
          </div>

          {usage && <TokenClasses totals={usage.totals} />}
        </div>
      )}
    </section>
  )
}

function Breakdown({ title, buckets }: { title: string; buckets: Bucket[] }) {
  const sorted = [...buckets].sort((a, b) => b.cost.total_nanos - a.cost.total_nanos)
  const max = Math.max(1, ...sorted.map((b) => b.cost.total_nanos))

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
      <span className="band-kicker">{title}</span>
      {sorted.length === 0 ? (
        <Empty title="No traffic in this window" />
      ) : (
        <div className="shares">
          {sorted.map((bucket) => (
            <Share
              key={bucket.key || 'unidentified'}
              name={bucket.name || bucket.key || 'unidentified'}
              value={bucket.cost.total_nanos}
              of={max}
              meta={<BucketMeta bucket={bucket} />}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function BucketMeta({ bucket }: { bucket: Bucket }) {
  const context = bucket.tokens.input + bucket.tokens.cache_read
  return (
    <>
      <span>
        {count(bucket.requests)} {bucket.requests === 1 ? 'request' : 'requests'}
      </span>
      <span>
        {context > 0 ? `${percent(bucket.tokens.cache_read, context)} from cache` : 'not caching'}
      </span>
      {bucket.unpriced_billable > 0 && (
        <span style={{ color: 'var(--state-warn)' }}>{bucket.unpriced_billable} unpriced</span>
      )}
    </>
  )
}

/** Where the tokens went, which is where the money went.
 *
 *  The one-hour cache write is its own row when there is one, because it is
 *  priced about 1.6x the five-minute write and a single cache-write number
 *  cannot be costed correctly. */
function TokenClasses({ totals }: { totals: Totals }) {
  const t = totals.tokens
  const classes = [
    { name: 'Cache write', value: t.cache_write },
    { name: 'Cache write · 1 hour', value: t.cache_write_1h },
    { name: 'Input', value: t.input },
    { name: 'Cache read', value: t.cache_read },
    { name: 'Output', value: t.output },
  ].filter((c) => c.value > 0)

  const all = classes.reduce((sum, c) => sum + c.value, 0)
  const max = Math.max(1, ...classes.map((c) => c.value))
  const writes = t.cache_write + t.cache_write_1h

  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        gap: 16,
        paddingLeft: 40,
        borderLeft: '1px solid var(--rule)',
      }}
    >
      <span className="band-kicker">Where the tokens went</span>
      {classes.map((c) => (
        <div key={c.name} style={{ display: 'flex', flexDirection: 'column', gap: 7 }}>
          <div className="detail-line">
            <span>{c.name}</span>
            <span>{tokens(c.value)}</span>
          </div>
          <Track value={c.value} of={max} thin />
        </div>
      ))}
      {all > 0 && (
        <p style={{ margin: '8px 0 0', color: 'var(--text-muted)', fontSize: 'var(--size-note)' }}>
          {percent(writes, all)} of tokens are cache writes. The five-minute and one-hour caches
          are priced separately, so a single cache-write number cannot be costed correctly.
        </p>
      )}
    </div>
  )
}

/* ----------------------------------------------------------- tab: attention --
 *
 * What is worth doing something about, derived rather than configured. Each one
 * names the thing, what it is worth, and the single next action.
 */

type AttentionItem = {
  id: string
  title: string
  figure: string
  detail: string
  action: { label: string; to: string }
}

function attentionItems({
  coverage,
  seats,
  budgets,
  teams,
  active,
  allocation,
}: {
  coverage?: Coverage
  seats: Seats | null
  budgets: BudgetRow[]
  teams: TeamRow[]
  active: Bucket[]
  allocation?: Allocation | null
}): AttentionItem[] {
  const items: AttentionItem[] = []

  // A budget at its limit is not in the handoff's three, and it goes first
  // anyway: everything else on this list is money spent less well than it could
  // have been, and this one is requests that are not being served at all.
  for (const budget of budgets) {
    if (budget.state === 'over_hard') {
      items.push({
        id: `budget-hard-${budget.id}`,
        title: `${budget.name} is at its hard limit`,
        figure: money(budget.spent_nanos ?? 0),
        detail:
          'Requests governed by it are being refused. Raising the limit is the only thing that ' +
          'resumes them - there is no per-request exemption, because a budget that can be ' +
          'stepped around is not a budget.',
        action: { label: 'Open budgets', to: '/people' },
      })
    } else if (budget.state === 'over_soft') {
      items.push({
        id: `budget-soft-${budget.id}`,
        title: `${budget.name} is past its soft limit`,
        figure: money(budget.spent_nanos ?? 0),
        detail:
          'Routes marked as not serving past a soft limit are being refused, and the refusal ' +
          'names the routes that are still served so an engineer can switch. Everything else ' +
          'continues.',
        action: { label: 'Review routes', to: '/routing' },
      })
    }
  }

  if (coverage && coverage.cost_nanos > 0) {
    const unattributed = coverage.cost_nanos - coverage.attributed_cost_nanos
    if (unattributed / coverage.cost_nanos > 0.3) {
      items.push({
        id: 'coverage',
        title: `${percent(unattributed, coverage.cost_nanos)} of spend could not be tied to a work item`,
        figure: money(unattributed),
        detail:
          'Branch naming conventions raise this first; optional per-request stamping raises it ' +
          'further. Nothing is inferred, so the number stays honest until the convention lands.',
        action: { label: 'See the breakdown', to: '/?tab=attribution' },
      })
    }
  }

  // Work a rule could claim and none does. This is the whole reason there is no
  // fallback rung: absorbed into some team's line it would look correct, and the
  // rule would never get written. Here it is a worklist with a repository on it.
  for (const row of allocation?.unallocated ?? []) {
    if (!row.actionable || row.cost_nanos === 0) continue
    items.push({
      id: `unallocated-${row.repo}`,
      title: `${row.repo} is allocated to no cost center`,
      figure: money(row.cost_nanos),
      detail:
        'Spend on this repository reached no accounting line, so it is reported as unallocated ' +
        'rather than absorbed into one. A single rule places it, and every period is restated ' +
        'the moment the rule is saved.',
      action: { label: 'Add a rule', to: '/settings?tab=cost-centers' },
    })
  }

  // A stamp naming no line is invisible from the client's side: the request
  // succeeded, and the money went somewhere the sender did not choose.
  for (const row of allocation?.rejected_stamps ?? []) {
    items.push({
      id: `stamp-${row.stamp}`,
      title: `Requests are stamped ${row.stamp}, which is no cost center`,
      figure: money(row.cost_nanos),
      detail:
        'The header named a line that does not exist, or one that refuses stamps. These requests ' +
        'were served and allocated by the rules instead - nothing failed, which is exactly why ' +
        'this would otherwise go unnoticed.',
      action: { label: 'Open cost centers', to: '/settings?tab=cost-centers' },
    })
  }

  for (const row of seats?.cache_hygiene ?? []) {
    if (!row.uncached || row.requests === 0) continue
    items.push({
      id: `cache-${row.tool}`,
      title: `${row.tool} read nothing from the prompt cache`,
      figure: `${count(row.requests)} req`,
      detail:
        'Coding agents resend their whole conversation each turn, so that context is paid for ' +
        'at the full input rate every time rather than at the cache-read rate - typically ' +
        'several times the cost for identical work.',
      action: { label: 'Review client', to: `/requests?tool=${encodeURIComponent(row.tool)}` },
    })
  }

  // Per team rather than per deployment, because "four seats somewhere spent
  // nothing" is not something anybody can act on and "QA spent nothing" is.
  const routed = new Set(active.filter((t) => t.requests > 0).map((t) => t.key))
  for (const team of teams) {
    if (team.members === 0 || routed.has(team.slug)) continue
    items.push({
      id: `idle-team-${team.slug}`,
      title: `${team.name} has ${team.members} provisioned ${team.members === 1 ? 'seat' : 'seats'} and no traffic`,
      figure: `${team.members} ${team.members === 1 ? 'seat' : 'seats'}`,
      detail:
        'The team is provisioned on the gateway but has never routed a request in this window. ' +
        'Either onboarding has not happened or the seats can be released.',
      action: { label: 'Review seats', to: '/people' },
    })
  }

  return items
}

function AttentionTab({ items }: { items: AttentionItem[] }) {
  return (
    <section className="band last">
      <div className="inline" style={{ alignItems: 'baseline', gap: 12, marginBottom: 24 }}>
        <span className="band-title">Needs attention</span>
        {items.length > 0 && (
          <span
            className="mono"
            style={{ color: 'var(--accent)', fontSize: 'var(--size-mono-data)' }}
          >
            {items.length}
          </span>
        )}
      </div>

      {items.length === 0 ? (
        <Empty title="Nothing needs you">
          Budgets are within their limits, every harness is reading from the prompt cache, and
          coverage is healthy.
        </Empty>
      ) : (
        // NOT A DISCLOSURE. Every other expandable table on this screen holds
        // rows a reader is scanning to find one of - a person, a work item -
        // and the detail is what they open once they have. This list is the
        // opposite: it is three or four things, it exists because each of them
        // is already worth reading, and a `+` in front of "32% of spend could
        // not be tied to a work item" asks somebody to click to find out
        // whether the thing they were just told about matters.
        <div className="attention">
          {items.map((item) => (
            <div key={item.id} className="dtable-group">
              <div className="attention-row static">
                <span className="dot accent" />
                <span className="title">{item.title}</span>
                <span className="figure">{item.figure}</span>
              </div>
              <div className="attention-detail">
                <p>{item.detail}</p>
                <Link to={item.action.to} className="btn primary">
                  {item.action.label}
                </Link>
              </div>
            </div>
          ))}
        </div>
      )}

      <div className="footnote">
        Prompt and response bodies are never stored. The audit trail is metadata.
      </div>
    </section>
  )
}

/* -------------------------------------------------- tab: cost centers --------
 *
 * Where the money landed on somebody's chart of accounts, and - beside it, at
 * the same weight - what landed nowhere.
 *
 * The gap is not a footnote here. Allocation is by evidence or it does not
 * happen: a repository no rule claims is reported as unallocated rather than
 * absorbed into a plausible line, because absorbing it is what stops the rule
 * from ever being written. The right-hand column is that worklist.
 */
function CostCentersTab({ data, loading }: { data: Allocation | null; loading: boolean }) {
  if (loading || !data) {
    return (
      <section className="band last">
        <Loading rows={4} />
      </section>
    )
  }

  const lines = data.lines.filter((l) => l.cost_nanos > 0 || !l.archived)
  const placed = [...lines].sort((a, b) => b.cost_nanos - a.cost_nanos)
  const max = Math.max(1, ...placed.map((l) => l.cost_nanos))
  const gap = data.unallocated.filter((u) => u.cost_nanos > 0)
  const gapMax = Math.max(1, ...gap.map((u) => u.cost_nanos))

  return (
    <>
      <section className="band split">
        <div>
          <div className="band-head">
            <span className="band-title">Allocated by cost center</span>
            <span className="band-sub">
              {money(data.totals.allocated_nanos)} of {money(data.totals.cost_nanos)} reached an
              accounting line. A plan warns and stops nobody - a budget on a person or a team is
              what stops an account.
            </span>
          </div>
          {placed.length === 0 ? (
            <Empty
              title="No cost center has spend yet"
              action={
                <Link className="btn" to="/settings?tab=cost-centers">
                  Set up cost centers
                </Link>
              }
            >
              Spend is reported against the accounting lines you define, and everything is
              unallocated until a rule places it.
            </Empty>
          ) : (
            <div className="shares">
              {placed.map((line) => (
                <Share
                  key={line.id}
                  name={`${line.code} · ${line.name}`}
                  value={line.cost_nanos}
                  of={max}
                  tone={line.over_warn ? 'warn' : 'accent'}
                  meta={
                    line.plan_nanos
                      ? `${money(line.month_to_date)} of ${money(line.plan_nanos)} this month` +
                        (line.over_warn ? ' - past its warning line' : '')
                      : 'no plan set'
                  }
                />
              ))}
            </div>
          )}
        </div>

        <div>
          <div className="band-head">
            <span className="band-title">Reached no cost center</span>
            <span className="band-sub">
              {money(data.totals.unallocated_nanos)}, stated rather than absorbed. These rows sum to
              exactly the remainder.
            </span>
          </div>
          {gap.length === 0 ? (
            <Empty title="Every priced request reached a line">
              Nothing in this window fell outside the allocation rules.
            </Empty>
          ) : (
            <div className="shares">
              {gap.map((row, at) => (
                <Share
                  key={`${row.reason}-${row.repo}-${at}`}
                  name={row.repo || row.label}
                  value={row.cost_nanos}
                  of={gapMax}
                  tone={row.actionable ? 'warn' : 'neutral'}
                  meta={row.actionable ? 'no rule claims this repository' : row.detail}
                />
              ))}
            </div>
          )}
        </div>
      </section>

      {data.rejected_stamps.length > 0 && (
        <section className="band last">
          <div className="band-head">
            <span className="band-title">Stamps that named no line</span>
            <span className="band-sub">
              A client sent the x-panorama-cost-center header naming a cost center that does not
              exist, or one that refuses stamps. The requests were served and
              allocated by the rules - nothing failed, which is why this is worth saying out loud.
            </span>
          </div>
          <div className="shares">
            {data.rejected_stamps.map((row) => (
              <Share
                key={row.stamp}
                name={row.stamp}
                value={row.cost_nanos}
                of={Math.max(1, ...data.rejected_stamps.map((s) => s.cost_nanos))}
                tone="warn"
                meta={`${count(row.requests)} requests`}
              />
            ))}
          </div>
        </section>
      )}
    </>
  )
}
