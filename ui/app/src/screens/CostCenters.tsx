// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from 'react'
import type { CSSProperties, ReactNode } from 'react'
import { del, post } from '../api'
import { money } from '../format'
import { Icon } from '../icons'
import type { AllocationRuleRow, CostCenterRow, CostCentersData } from '../types'
import {
  ActionButton,
  Choice,
  CopyBlock,
  Drawer,
  Empty,
  ErrorBanner,
  Loading,
  Modal,
  Pill,
  Select,
  SubmitButton,
  useAction,
  useApi,
} from '../ui'

/**
 * The chart of accounts, and the ordered list that fills it.
 *
 * This is configuration, which is why it is a Settings tab and not a screen of
 * its own: an administrator writes these rules once and then reads the numbers
 * on Analytics for a year. The screens are split along that line - what the
 * deployment is set to do lives here, what it did lives there.
 *
 * Nothing on this tab enforces anything, and the tab says so rather than
 * leaving it to be discovered. A plan warns; what stops an account is a budget
 * on a person or a team, which is People & teams. Both screens say in words
 * where the other half is.
 */

const CENTER_TRACKS = ['minmax(0, 1fr)', '200px', '150px', '110px', '32px']
const RULE_TRACKS = ['110px', 'minmax(0, 1fr)', '200px', '76px', '32px']

const grid = (tracks: string[]): CSSProperties => ({
  gridTemplateColumns: tracks.join(' '),
  gap: 16,
})

export function CostCenters() {
  const data = useApi<CostCentersData>('/api/v1/cost-centers')

  const [editing, setEditing] = useState<CostCenterRow | null>(null)
  const [ruling, setRuling] = useState<AllocationRuleRow | null>(null)
  const [testing, setTesting] = useState(false)

  const centers = data.data?.cost_centers ?? []
  const rules = data.data?.rules ?? []
  const live = centers.filter((c) => !c.archived)

  const reload = () => {
    setEditing(null)
    setRuling(null)
    data.reload()
  }

  if (data.error) {
    return (
      <section className="band">
        <ErrorBanner error={data.error} />
      </section>
    )
  }
  if (data.loading) {
    return (
      <section className="band">
        <Loading rows={4} />
      </section>
    )
  }

  return (
    <>
      <section className="band">
        <div className="band-head-row top">
          <div className="band-head lead">
            <span className="band-title">Cost centers</span>
            <span className="band-sub">
              The accounting lines this deployment reports against. A plan warns and stops nobody -
              what actually stops an account is a budget on a person or a team, set on People &amp;
              teams.
            </span>
          </div>
          <button
            type="button"
            className="btn"
            onClick={() =>
              setEditing({
                id: '',
                code: '',
                name: '',
                owner: '',
                description: '',
                plan_nanos: null,
                warn_at_percent: 80,
                accepts_stamp: true,
                archived: false,
              })
            }
          >
            Add a cost center
          </button>
        </div>

        {/* The head stays up when there is nothing here, for the same reason
            the group mapping's does: what the columns are is part of the
            answer to "what would a cost center even be". */}
        <div className="maptable">
          <div className="dtable-head" style={grid(CENTER_TRACKS)}>
            <span>Code</span>
            <span>Owner</span>
            <span className="num">Monthly plan</span>
            <span>Stamps</span>
            <span />
          </div>

          {centers.length === 0 ? (
            <Empty title="No cost center yet">
              Spend is reported against accounting lines you define here, and everything is
              unallocated until one exists. A line is a code your finance system already uses.
            </Empty>
          ) : (
            centers.map((center) => (
              <CenterRow key={center.id} center={center} onOpen={() => setEditing(center)} onDeleted={reload} />
            ))
          )}
        </div>
      </section>

      <section className="band">
        <div className="band-head-row top">
          <div className="band-head lead">
            <span className="band-title">Allocation rules</span>
            <span className="band-sub">
              Read top to bottom, first match wins. Work that no rule claims stays unallocated and
              is listed on Analytics - deliberately, because a repository with real spend and no
              rule is the only thing that ever gets one written.
            </span>
          </div>
          <div className="head-actions">
            <button
              type="button"
              className="btn"
              disabled={rules.length === 0}
              onClick={() => setTesting(true)}
            >
              Check a rule
            </button>
            <button
              type="button"
              className="btn"
              disabled={live.length === 0}
              onClick={() =>
                setRuling({
                id: '',
                cost_center_id: live[0]?.id ?? '',
                position: rules.length,
                  match_on: 'repo',
                  pattern: '',
                  unreachable: false,
                  shadowed: false,
                })
              }
            >
              Add a rule
            </button>
          </div>
        </div>

        <div className="maptable">
          <div className="dtable-head" style={grid(RULE_TRACKS)}>
            <span>Matches</span>
            <span>Pattern</span>
            <span>Cost center</span>
            <span>Order</span>
            <span />
          </div>

          {rules.length === 0 ? (
            <Empty title="Nothing is allocated yet">
              A rule maps a repository or a ticket prefix to a cost center. Ticket rules need no
              integration: a branch named feat/PAY-1183-x is attributed to the ticket PAY-1183 from
              the branch name alone, which is what lets a monorepo be split across several lines.
            </Empty>
          ) : (
            rules.map((rule, index) => (
              <RuleRow
                key={rule.id}
                rule={rule}
                index={index}
                total={rules.length}
                centers={centers}
                order={rules.map((r) => r.id)}
                onOpen={() => setRuling(rule)}
                onChanged={reload}
              />
            ))
          )}
        </div>
      </section>

      <section className="band last">
        <div className="band-head lead">
          <span className="band-title">Naming a line from a request</span>
          <span className="band-sub">
            A client may override the rules for its own traffic. It is the top rung, because
            somebody who declares which line their work belongs to knows better than a pattern
            inferring it - a pipeline building one product, or a contractor billing an engagement,
            is not expressible as a repository pattern.
          </span>
        </div>
        <div className="stack">
          <CopyBlock text="x-panorama-cost-center: CC-100" label="Request header" />
          <p className="prose">
            A stamp is the spender&rsquo;s claim rather than something the gateway observed, so a
            line can refuse them - turn <em>Accepts stamps</em> off on any line whose integrity
            matters more than its coverage. A stamp naming no line never fails the request: it falls
            through to the rules, and the code that was sent is listed on Analytics so a deployment
            full of misspelt stamps is visible rather than quietly allocating somewhere else.
          </p>
        </div>
      </section>

      {testing && <Tester centers={centers} onClose={() => setTesting(false)} />}
      {editing && <CenterDrawer center={editing} onClose={() => setEditing(null)} onSaved={reload} />}
      {ruling && (
        <RuleDrawer rule={ruling} centers={live} onClose={() => setRuling(null)} onSaved={reload} />
      )}
    </>
  )
}

function CenterRow({
  center,
  onOpen,
  onDeleted,
}: {
  center: CostCenterRow
  onOpen: () => void
  onDeleted: () => void
}) {
  return (
    <div className="maprow" style={{ gridTemplateColumns: `minmax(0, 1fr) 32px`, gap: 16 }}>
      <button
        type="button"
        className="maprow-open"
        style={grid(CENTER_TRACKS.slice(0, 4))}
        onClick={onOpen}
      >
        <span className="maprow-group">
          {center.code}
          <span className="maprow-role"> · {center.name}</span>
          {center.archived && (
            <>
              {' '}
              <Pill tone="off">archived</Pill>
            </>
          )}
        </span>
        <span className="maprow-role">{center.owner || '-'}</span>
        {/* A plan nobody set reads "not set". The word "unlimited" appears
            nowhere in this console, and a plan is not a limit anyway. */}
        <span className="cell-fig">{center.plan_nanos ? money(center.plan_nanos) : 'not set'}</span>
        <span className="maprow-role">{center.accepts_stamp ? 'accepted' : 'refused'}</span>
      </button>
      {/* Outside the open target: "change this" and "delete this" are two
          different aims, and a button inside a button is not markup. */}
      <ActionButton
        onAct={async () => {
          await del(`/api/v1/cost-centers/${center.id}`)
          onDeleted()
        }}
        label="×"
        settled="Removed"
        small
        title={`Delete ${center.code}`}
      />
    </div>
  )
}

/**
 * The two halves of one control.
 *
 * Not ActionButton, twice. That component owns a settle animation and an
 * announcement, which are right for a save and wrong here: the acknowledgement
 * for moving a rule is the rule moving, and a row that flashed "Moved" beside a
 * list that had already reordered itself would say the same thing twice, in a
 * table where every other row is a hairline and a word.
 *
 * The announcement it does keep is the one that is not visual. A reader who
 * cannot see the list reorder is told where the row landed, because "first
 * match wins" makes the position the whole meaning of the row.
 */
function Nudge({
  pattern,
  position,
  total,
  onMove,
}: {
  pattern: string
  position: number
  total: number
  onMove: (to: number) => Promise<void>
}) {
  const [said, setSaid] = useState('')

  const go = (to: number) => async () => {
    await onMove(to)
    setSaid(`${pattern} moved to position ${to + 1} of ${total}`)
  }

  return (
    <>
      <span className="nudge">
        <button
          type="button"
          onClick={go(position - 1)}
          disabled={position === 0}
          title={`Evaluate ${pattern} earlier`}
          aria-label={`Evaluate ${pattern} earlier`}
        >
          <Icon name="caret" className="up" />
        </button>
        <button
          type="button"
          onClick={go(position + 1)}
          disabled={position === total - 1}
          title={`Evaluate ${pattern} later`}
          aria-label={`Evaluate ${pattern} later`}
        >
          <Icon name="caret" />
        </button>
      </span>
      <span className="sr-only" role="status" aria-live="polite">
        {said}
      </span>
    </>
  )
}

function RuleRow({
  rule,
  index,
  total,
  centers,
  order,
  onOpen,
  onChanged,
}: {
  rule: AllocationRuleRow
  index: number
  total: number
  centers: CostCenterRow[]
  order: string[]
  onOpen: () => void
  onChanged: () => void
}) {
  const center = centers.find((c) => c.id === rule.cost_center_id)

  // The whole list in one request, like a team's roster: a stream of per-rule
  // updates leaves a half-applied order on the first failure, and a
  // half-applied order allocates money nobody chose.
  // Both directions, disabled at the ends. A single button that swapped with
  // whichever neighbour existed meant nothing could ever be promoted - every
  // row but the last offered "down", so a rule added at the bottom could only
  // ever move further down. On an ordered list where the first match wins,
  // that is the one operation somebody actually needs.
  const move = (to: number) => async () => {
    const next = order.filter((id) => id !== rule.id)
    next.splice(to, 0, rule.id)
    await post('/api/v1/cost-centers/order', { ids: next })
    onChanged()
  }

  // Three cells open the drawer; the order control and the delete do not. Both
  // sit outside the open-target for the same reason the × does - a
  // button inside a button is not markup, and the browser silently repairs it
  // into something neither control behaves predictably in.
  return (
    <div className="maprow" style={{ gridTemplateColumns: `minmax(0, 1fr) 76px 32px`, gap: 16 }}>
      <button
        type="button"
        className="maprow-open"
        style={grid(RULE_TRACKS.slice(0, 3))}
        onClick={onOpen}
      >
        <span className="maprow-role">{rule.match_on === 'ticket' ? 'ticket' : 'repository'}</span>
        <span className="maprow-group mono">
          {rule.pattern}
          {rule.unreachable && (
            <>
              {' '}
              <Pill tone="warn">never matches</Pill>
            </>
          )}
          {rule.shadowed && (
            <>
              {' '}
              <Pill tone="warn">almost never matches</Pill>
            </>
          )}
        </span>
        <span className="maprow-role">{center ? center.code : 'unknown'}</span>
      </button>
      <span className="order-cell">
        <Nudge pattern={rule.pattern} position={index} total={total} onMove={(to) => move(to)()} />
      </span>
      <ActionButton
        onAct={async () => {
          await del(`/api/v1/cost-centers/rules/${rule.id}`)
          onChanged()
        }}
        label="×"
        settled="Removed"
        small
        title={`Delete the rule for ${rule.pattern}`}
      />
    </div>
  )
}

/**
 * Which rule claims this?
 *
 * The price of an ordered list, paid rather than argued away. It calls the same
 * resolver the recompute calls, so an answer here is the allocation that will
 * actually happen - a second implementation, however small, would eventually
 * promise one the recompute did not deliver.
 *
 * **Why this is a modal, when the rule says a modal is a picker.**
 * The rule in ui.tsx is that a modal is for picking on behalf of an open panel,
 * and what it protects against is an overlay that replaces a half-made edit and
 * loses it. Nothing is being edited here and nothing is staged: this asks the
 * rule list a question and reports what it said. It is also the wrong thing to
 * leave on the screen: a field and a button under the table would occupy the
 * band permanently to answer a question somebody asks twice a quarter.
 *
 * What it is not is configuration. A modal is still never where a rule is
 * authored; that is the drawer beside it.
 */
function Tester({ centers, onClose }: { centers: CostCenterRow[]; onClose: () => void }) {
  const [value, setValue] = useState('')
  const [result, setResult] = useState<{ pattern?: string; center?: string } | null>(null)

  const run = useAction(async () => {
    // A ticket key is recognised by its shape rather than asked for, because
    // the two things somebody pastes here - a repository and a ticket - are not
    // confusable, and a control to say which would be a control about nothing.
    const asked = value.trim()
    const ticket = /^[A-Za-z][A-Za-z0-9]*-\d+$/.test(asked)
    const answer = await post<{
      allocated: boolean
      pattern?: string
      cost_center_id?: string
    }>(
      '/api/v1/cost-centers/test',
      ticket ? { kind: 'ticket', key: asked } : { kind: 'branch', repo: asked },
    )
    setResult(
      answer.allocated
        ? {
            pattern: answer.pattern,
            center: centers.find((c) => c.id === answer.cost_center_id)?.code,
          }
        : {},
    )
  })

  return (
    <Modal
      open
      title="Which rule claims this?"
      sub="Answered by the same resolver that allocates recorded work, so this is what would actually happen."
      onClose={onClose}
      onSubmit={run.act}
      footer={<SubmitButton state={run.state} label="Check" settled="Checked" primary />}
    >
      <div className="stack">
        <Field
          label="Repository or ticket"
          hint="A repository like acme/payments-api, or a ticket key like PAY-1183."
        >
          <input
            className="input"
            value={value}
            autoFocus
            placeholder="acme/payments-api"
            onChange={(e) => {
              setValue(e.target.value)
              setResult(null)
            }}
          />
        </Field>

        {result &&
          (result.pattern ? (
            <p className="prose">
              Allocated to <strong>{result.center ?? 'a cost center'}</strong> by the rule{' '}
              <span className="mono">{result.pattern}</span>.
            </p>
          ) : (
            // Not an error, and said in the words the product uses for it. This
            // is the answer a worklist entry is made of.
            <p className="prose">
              No rule claims this. Its spend would be reported as unallocated, and listed on
              Analytics with a repository beside it.
            </p>
          ))}
      </div>
    </Modal>
  )
}

function CenterDrawer({
  center,
  onClose,
  onSaved,
}: {
  center: CostCenterRow
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState({
    code: center.code,
    name: center.name,
    owner: center.owner,
    description: center.description,
    plan: center.plan_nanos ? (center.plan_nanos / 1e9).toFixed(2) : '',
    warn: String(center.warn_at_percent),
    stamps: center.accepts_stamp,
    archived: center.archived,
  })

  const save = useAction(async () => {
    const body = {
      code: form.code,
      name: form.name,
      owner: form.owner,
      description: form.description,
      plan: form.plan.trim() === '' ? null : form.plan.trim(),
      warn_at_percent: Number(form.warn) || 80,
      accepts_stamp: form.stamps,
      archived: form.archived,
    }
    if (center.id) await post(`/api/v1/cost-centers/${center.id}`, body)
    else await post('/api/v1/cost-centers', body)
    onSaved()
  })

  return (
    <Drawer
      open
      title={center.id ? center.code : 'New cost center'}
      sub="A line your finance system already knows by its code."
      onClose={onClose}
      onSubmit={save.act}
      footer={<SubmitButton state={save.state} label="Save changes" settled="Saved" primary />}
    >
      <div className="stack">
        <Field label="Code" hint="The key your ledger joins on. Case does not matter.">
          <input
            className="input"
            value={form.code}
            onChange={(e) => setForm({ ...form, code: e.target.value })}
          />
        </Field>
        <Field label="Name">
          <input
            className="input"
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
          />
        </Field>
        <Field label="Owner" hint="Who to ask when this line runs hot. A person or a list.">
          <input
            className="input"
            value={form.owner}
            onChange={(e) => setForm({ ...form, owner: e.target.value })}
          />
        </Field>
        <Field
          label="Monthly plan"
          hint="What this line is expected to spend in a calendar month. It warns; it stops nobody."
        >
          <input
            className="input"
            value={form.plan}
            placeholder="not set"
            onChange={(e) => setForm({ ...form, plan: e.target.value })}
          />
        </Field>
        <Field label="Warn at" hint="Percent of the plan at which this line is worth raising.">
          <input
            className="input"
            value={form.warn}
            onChange={(e) => setForm({ ...form, warn: e.target.value })}
          />
        </Field>
        <div className="field">
          <span className="fieldlabel">Accepts stamps</span>
          <Choice
            label="Accepts stamps"
            value={form.stamps ? 'yes' : 'no'}
            onChange={(v) => setForm({ ...form, stamps: v === 'yes' })}
            options={[
              { id: 'yes', label: 'Accepted' },
              { id: 'no', label: 'Refused' },
            ]}
          />
          <span className="drawer-help">
            {form.stamps
              ? 'A client may name this line with x-panorama-cost-center.'
              : 'Only a rule may place work here. For a line whose integrity matters more than its coverage.'}
          </span>
        </div>
        {center.id && (
          <div className="field">
            <span className="fieldlabel">Status</span>
            <Choice
              label="Status"
              value={form.archived ? 'archived' : 'open'}
              onChange={(v) => setForm({ ...form, archived: v === 'archived' })}
              options={[
                { id: 'open', label: 'Open' },
                { id: 'archived', label: 'Archived' },
              ]}
            />
            <span className="drawer-help">
              An archived line is hidden from pickers and takes no new rules. Its existing rules
              keep matching, so its history stays where it is - which is why archiving exists and
              deleting a line with spend is refused.
            </span>
          </div>
        )}
      </div>
    </Drawer>
  )
}

function RuleDrawer({
  rule,
  centers,
  onClose,
  onSaved,
}: {
  rule: AllocationRuleRow
  centers: CostCenterRow[]
  onClose: () => void
  onSaved: () => void
}) {
  const [form, setForm] = useState({
    match: rule.match_on,
    pattern: rule.pattern,
    center: rule.cost_center_id,
  })

  const save = useAction(async () => {
    await post('/api/v1/cost-centers/rules', {
      id: rule.id,
      cost_center_id: form.center,
      match_on: form.match,
      pattern: form.pattern.trim(),
      position: rule.position,
    })
    onSaved()
  })

  return (
    <Drawer
      open
      title={rule.id ? 'Allocation rule' : 'New allocation rule'}
      sub="Read top to bottom, first match wins."
      onClose={onClose}
      onSubmit={save.act}
      footer={<SubmitButton state={save.state} label="Save changes" settled="Saved" primary />}
    >
      <div className="stack">
        <div className="field">
          <span className="fieldlabel">Matches</span>
          <Choice
            label="Matches"
            value={form.match}
            onChange={(v) => setForm({ ...form, match: v })}
            options={[
              { id: 'repo' as const, label: 'Repository' },
              { id: 'ticket' as const, label: 'Ticket' },
            ]}
          />
          <span className="drawer-help">
            {form.match === 'repo'
              ? 'Matched against a repository, like acme/payments-api.'
              : 'Matched against a ticket key, like PAY-1183. This is what splits a monorepo across several lines.'}
          </span>
        </div>
        <Field
          label="Pattern"
          hint="* stands for any run of characters, including none, and crosses a slash. Case does not matter."
        >
          <input
            className="input"
            value={form.pattern}
            placeholder={form.match === 'ticket' ? 'PAY-*' : 'acme/payments-*'}
            onChange={(e) => setForm({ ...form, pattern: e.target.value })}
          />
        </Field>
        <Field label="Cost center">
          <Select
            value={form.center}
            onChange={(v) => setForm({ ...form, center: v })}
            options={centers.map((c) => ({ value: c.id, label: `${c.code} · ${c.name}` }))}
          />
        </Field>
      </div>
    </Drawer>
  )
}

function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: ReactNode
}) {
  return (
    <label className="field">
      <span className="fieldlabel">{label}</span>
      {children}
      {hint && <span className="drawer-help">{hint}</span>}
    </label>
  )
}
