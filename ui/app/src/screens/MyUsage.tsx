// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { query } from '../api'
import type { Range } from '../api'
import { usePageChrome } from '../chrome'
import { ago, count, dateTime, money, percent, windowLabel } from '../format'
import { Link, useRouter } from '../router'
import type { BudgetRow, Policy, SessionRow, Usage, Work } from '../types'
import {
  DailySpend,
  Empty,
  ErrorBanner,
  Kpi,
  Loading,
  Pill,
  RangePicker,
  RefreshButton,
  Share,
  Track,
  useApi,
} from '../ui'
import { ProvenancePill } from './AnalyticsTables'

// The developer's one recurring destination. Self-awareness without
// surveillance-feel.
//
// One scroll and no tabs, because there is one person's traffic here and it
// fits: what they spent against what governs them, when they spent it, which
// conversations it went on, and which models it went to. Five bands.
//
// Two things are deliberately absent. A terminal mock-up of the block message
// ("what your tool will show") would put a screenful of refusal in front of
// somebody who is within their quota, which is the state this screen is in
// almost every time it is opened; a limit that is approaching is already said
// in words above it. And there is no paragraph at the top arguing the case for
// the screen: the closing line makes the commitment in one sentence, which is
// where a commitment belongs - after the numbers, not defending them in
// advance.

const SESSION_COLUMNS = '100px minmax(0, 1fr) 130px 130px 110px'

export function MyUsage() {
  const { search, setParam } = useRouter()
  const range = (search.get('range') as Range) || '30d'

  const usage = useApi<Usage>(`/api/v1/usage${query({ since: range, scope: 'me' })}`, [range])
  const work = useApi<Work>(`/api/v1/work${query({ since: range, scope: 'me' })}`, [range])
  const sessions = useApi<{ sessions: SessionRow[] }>(
    `/api/v1/sessions${query({ since: range, scope: 'me', limit: 25 })}`,
    [range],
  )
  const policy = useApi<Policy>('/api/v1/policy')

  usePageChrome(
    {
      sub: 'Only you and the budgets that govern you see these figures.',
      actions: (
        <>
          <RangePicker value={range} onChange={(next) => setParam('range', next)} />
          <RefreshButton />
        </>
      ),
    },
    [range],
  )

  const totals = usage.data?.totals
  const coverage = work.data?.coverage
  // Every budget the server says governs this person and is theirs to see. The
  // tightest is what actually stops them, and it is the one the hero measures
  // against - the same derivation People & teams makes, for the same reason:
  // two answers to "what stops this account" is one too many.
  const mine = (policy.data?.budgets ?? []).filter((b) => b.spend_visible && b.enabled)
  const binding = tightest(mine)

  const rows = sessions.data?.sessions ?? []

  return (
    <main className="bands">
      {Boolean(usage.error) && (
        <div className="band">
          <ErrorBanner error={usage.error} />
        </div>
      )}

      <section className="band split" style={{ ['--split-left' as string]: '1.15fr' }}>
        <div>
          <div className="spread" style={{ alignItems: 'center', marginBottom: 20 }}>
            <span className="band-kicker">Your spend · {windowLabel(range)}</span>
            <Standing budget={binding} />
          </div>

          <div className="hero">
            <span className="hero-figure">
              {totals ? money(totals.cost.total_nanos) : '-'}
            </span>
            {binding?.limits.hard && (
              <span className="hero-of mono">/ {money(binding.limits.hard)}</span>
            )}
          </div>

          {binding?.limits.hard ? (
            <>
              <Track
                value={binding.spent_nanos ?? 0}
                of={binding.limits.hard}
                notch={binding.limits.soft}
                thick
                tone={
                  binding.state === 'over_hard'
                    ? 'neutral'
                    : binding.state === 'over_soft'
                      ? 'warn'
                      : 'accent'
                }
              />
              <div className="band-meta">
                <span>{binding.percent_of_hard_limit ?? 0}% used</span>
                {binding.window_end && <span>resets {dateTime(binding.window_end)}</span>}
              </div>
            </>
          ) : (
            <p className="prose" style={{ maxWidth: '46ch' }}>
              No budget governs you today. Nothing you send is capped, and if that changes this is
              where the limit and its reset date appear - before you meet one.
            </p>
          )}
        </div>

        {/* Two figures, not three, and no kicker over them. "Cost / request"
            is an efficiency measure, and an individual cannot act on it: the
            price of a request is the model's rate, which they did not choose.
            It belongs on Analytics, where somebody is comparing models. And
            "This window" labelled a column whose every figure already says
            which window it is measured over. */}
        {/* Side by side, not stacked. Two tiles down a column left most of the
            cell empty beside a hero figure that is only two lines tall, and
            they are not a sequence - neither one leads to the other. */}
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(2, minmax(0, 1fr))',
            gap: 26,
            alignContent: 'start',
          }}
        >
          <Kpi
            label="Your requests"
            value={totals ? count(totals.requests) : '-'}
            note={`across ${count(rows.length)} ${rows.length === 1 ? 'session' : 'sessions'}`}
          />
          <Kpi
            label="Read from cache"
            value={totals ? cacheShare(totals) : '-'}
            accent
            note="of your tokens, billed at a fraction of the input rate"
          />
        </div>
      </section>

      <section className="band">
        <div className="band-head">
          <span className="band-title">Your daily spend</span>
          <span className="band-sub">
            One column per day in the window. Hover a column for that day&rsquo;s total.
          </span>
        </div>
        {usage.loading ? (
          <Loading />
        ) : (
          <DailySpend
            days={usage.data?.by_day ?? []}
            since={usage.data?.since}
            until={usage.data?.until}
          />
        )}
      </section>

      {/* Full width, and it has to be: five columns with 410px of fixed ones
          need about a thousand pixels, and in a narrow cell beside something
          else the branch column collapses to nothing. */}
      <section className="band">
        <div className="band-head-row">
          <div className="band-head">
            <span className="band-title">Your recent sessions</span>
            <span className="band-sub">
              Each one carries what Panorama detected, and from what signal.
            </span>
          </div>
          <span className="filter-count">
            {count(rows.length)} {rows.length === 1 ? 'session' : 'sessions'}
          </span>
        </div>

        {sessions.loading ? (
          <Loading rows={4} />
        ) : rows.length === 0 ? (
          <Empty
            title="No sessions yet"
            action={
              <Link to="/setup" className="btn primary">
                Set up a tool
              </Link>
            }
          >
            Point a coding tool at this gateway and your first conversation appears here, with
            whatever Panorama could read from it.
          </Empty>
        ) : (
          <>
            <div
              className="dtable-head"
              style={{ gridTemplateColumns: SESSION_COLUMNS, gap: 20 }}
            >
              <span>Last seen</span>
              <span>Repository · branch</span>
              <span>Work item</span>
              <span>Provenance</span>
              <span style={{ textAlign: 'right' }}>Cost</span>
            </div>

            {rows.map((session) => (
              <div
                key={session.id}
                className="dtable-row"
                style={{ gridTemplateColumns: SESSION_COLUMNS, gap: 20 }}
              >
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
                <span className="cell-fig">{money(session.cost.nanos)}</span>
              </div>
            ))}
          </>
        )}

        <p
          style={{
            margin: '26px 0 0',
            maxWidth: '84ch',
            color: 'var(--text-muted)',
            fontSize: 'var(--size-note)',
            textWrap: 'pretty',
          }}
        >
          This is the whole of what Panorama read from your traffic. No prompt, no response, no
          file contents - the extraction rules are open source so you can check.
        </p>
      </section>

      <section className="band">
        <div style={{ maxWidth: 820 }}>
          <div className="band-head">
            <span className="band-title">Where your spend went</span>
            <span className="band-sub">Your own traffic, by model.</span>
          </div>
          <Models usage={usage.data} loading={usage.loading} />
          {coverage && coverage.cost_nanos > 0 && (
            <div className="band-total">
              <span>Attributed to a work item</span>
              <span>{percent(coverage.attributed_cost_nanos, coverage.cost_nanos)}</span>
            </div>
          )}
        </div>
      </section>

      <section className="band last" style={{ paddingTop: 'var(--band-pad-y-tight)' }}>
        <div className="footnote" style={{ marginTop: 0 }}>
          Only you and your team lead see this. There is no comparison to another named individual
          anywhere in Panorama.
        </div>
      </section>
    </main>
  )
}

function Models({ usage, loading }: { usage: Usage | null; loading: boolean }) {
  if (loading) return <Loading rows={3} />

  const models = [...(usage?.by_model ?? [])]
    .filter((m) => m.cost.total_nanos > 0)
    .sort((a, b) => b.cost.total_nanos - a.cost.total_nanos)
  if (models.length === 0) {
    return <Empty title="No traffic in this window" />
  }
  const max = Math.max(1, ...models.map((m) => m.cost.total_nanos))

  return (
    <div className="shares">
      {models.map((model) => {
        const context = model.tokens.input + model.tokens.cache_read
        return (
          <Share
            key={model.key}
            name={model.name || model.key || 'unidentified'}
            value={model.cost.total_nanos}
            of={max}
            meta={
              <>
                <span>
                  {count(model.requests)} {model.requests === 1 ? 'request' : 'requests'}
                </span>
                <span>
                  {context > 0
                    ? `${percent(model.tokens.cache_read, context)} from cache`
                    : 'not caching'}
                </span>
              </>
            }
          />
        )
      })}
    </div>
  )
}

/** cacheShare is how much of the context this person paid for came back from
 *  the cache rather than being re-sent at the full input rate. */
function cacheShare(totals: Usage['totals']): string {
  const context = totals.tokens.input + totals.tokens.cache_read
  return context > 0 ? percent(totals.tokens.cache_read, context) : '-'
}

/**
 * tightest is which budget actually stops this person.
 *
 * Whichever is nearest its hard limit binds, because that is the one they will
 * meet first - a personal cap of $500 does not matter if the team's remaining
 * headroom is $40. A budget with no hard limit cannot stop anybody and is not a
 * candidate.
 */
function tightest(budgets: BudgetRow[]): BudgetRow | undefined {
  return budgets
    .filter((b) => b.limits.hard)
    .sort((a, b) => (b.percent_of_hard_limit ?? 0) - (a.percent_of_hard_limit ?? 0))[0]
}

/** Standing is where this person is against the budget that binds them.
 *
 *  The word "unlimited" never appears: a budget nobody set reads as not set,
 *  and an account with nothing above it at all is stated rather than
 *  congratulated. */
function Standing({ budget }: { budget?: BudgetRow }) {
  if (!budget) return <Pill>no cap</Pill>
  if (budget.state === 'over_hard') return <Pill tone="off">at the limit</Pill>
  if (budget.state === 'over_soft') return <Pill tone="warn">past the soft limit</Pill>
  return <Pill tone="good">within quota</Pill>
}
