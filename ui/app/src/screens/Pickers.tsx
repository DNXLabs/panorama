// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useMemo, useState } from 'react'
import { count } from '../format'
import { Icon } from '../icons'
import { membersOf, statusOf } from '../people'
import type { PersonRow, TeamRow } from '../types'
import { CheckRow, Modal, Select, VirtualList, toggleIn } from '../ui'

// Choosing people, and choosing teams.
//
// One implementation each, used by every surface that needs them: a team's
// roster, the teams one person is on, a route's conditions, a bulk action from
// the table. They are modals rather than panels because they are opened *from*
// a panel and the panel is the thing being filled in - closing it to choose
// what goes in it is how a half-made edit gets lost.
//
// Both are built for an identity provider with thousands of accounts in it.
// Rows are virtualised, so the document holds a screenful however long the list
// is; the filters above them are what actually make a list of three thousand
// usable, and the count says how much of it is in view.

/** How tall one row is. Fixed, because the list is virtualised and a row that
 *  could be two lines tall would make the scrollbar lie - so it has to be kept
 *  in step with `.check-row` by hand: 13px of padding either side of a 13px
 *  name over an 11px email. */
const ROW = 58
const VIEW = 348

export function PeoplePicker({
  title,
  sub,
  users,
  teams,
  selected,
  fixed,
  fixedNote,
  confirmLabel = 'Apply',
  clearLabel,
  onApply,
  onClose,
}: {
  title: string
  sub: string
  users: PersonRow[]
  teams: TeamRow[]
  selected: string[]
  /** Already true for a reason this list cannot change - a person who is
   *  already on the team a bulk action is adding to. Checked, dimmed, inert. */
  fixed?: Set<string>
  fixedNote?: string
  confirmLabel?: string
  /** Offered only where an empty selection means something - a route condition,
   *  where none means everyone. A roster has no such reading. */
  clearLabel?: string
  onApply: (next: string[]) => void
  onClose: () => void
}) {
  const [staged, setStaged] = useState(selected)
  const [query, setQuery] = useState('')
  const [team, setTeam] = useState('all')
  const [status, setStatus] = useState('all')
  const [only, setOnly] = useState(false)

  const shown = useMemo(() => {
    const q = query.trim().toLowerCase()
    return users.filter((person) => {
      if (
        q !== '' &&
        !person.display_name.toLowerCase().includes(q) &&
        !person.email.toLowerCase().includes(q)
      ) {
        return false
      }
      if (team === 'none' && person.teams.length > 0) return false
      if (team !== 'all' && team !== 'none' && !person.teams.some((t) => t.id === team)) return false
      if (status !== 'all' && statusOf(person) !== status) return false
      if (only && !staged.includes(person.id) && !fixed?.has(person.id)) return false
      return true
    })
  }, [users, query, team, status, only, staged, fixed])

  const picked = staged.filter((id) => !fixed?.has(id)).length

  return (
    <Modal
      open
      title={title}
      sub={sub}
      onClose={onClose}
      onSubmit={() => onApply(staged)}
      footer={
        <>
          {clearLabel ? (
            // Clears and closes, rather than clearing and waiting to be
            // applied. "Everyone" is a decision somebody has made by the time
            // they reach for it, and leaving it staged behind Apply put a
            // second click between them and the only state this list can reach
            // by emptying it.
            <button type="button" className="btn link" onClick={() => onApply([])}>
              {clearLabel}
            </button>
          ) : (
            <span className="prose tight">
              {count(picked)} selected of {count(users.length)}
            </span>
          )}
          <div className="inline">
            <button type="button" className="btn" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="btn primary">
              {confirmLabel}
            </button>
          </div>
        </>
      }
    >
      <div className="filters">
        <label className="search">
          <Icon name="search" />
          <input
            value={query}
            placeholder="Search name or email"
            aria-label="Search people"
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <Select
          value={team}
          label="Team"
          onChange={setTeam}
          options={[
            { value: 'all', label: 'All teams' },
            { value: 'none', label: 'No team' },
            ...teams.map((t) => ({ value: t.id, label: t.name })),
          ]}
        />
        <Select
          value={status}
          label="Status"
          onChange={setStatus}
          options={[
            { value: 'all', label: 'Any status' },
            { value: 'active', label: 'Active' },
            { value: 'unused', label: 'Never used' },
            { value: 'dormant', label: 'Dormant' },
            { value: 'disabled', label: 'Disabled' },
          ]}
        />
        <button
          type="button"
          className={only ? 'limit-toggle on' : 'limit-toggle'}
          aria-pressed={only}
          onClick={() => setOnly((v) => !v)}
        >
          Selected only
        </button>
        <div style={{ flex: 1 }} />
        <span className="mono" style={{ color: 'var(--text-muted)', fontSize: 'var(--size-label-sm)' }}>
          {shown.length === users.length
            ? `${count(users.length)} people`
            : `${count(shown.length)} of ${count(users.length)}`}
        </span>
      </div>

      <VirtualList
        items={shown}
        rowHeight={ROW}
        height={VIEW}
        empty={<div className="check-empty">No one matches these filters.</div>}
        render={(person) => (
          <CheckRow
            option={{
              id: person.id,
              label: person.display_name || person.email,
              sub: person.email,
              fixed: fixed?.has(person.id),
              fixedNote,
              change: change(staged.includes(person.id), selected.includes(person.id)),
            }}
            on={fixed?.has(person.id) || staged.includes(person.id)}
            onToggle={() => toggleIn(staged, person.id, setStaged)}
          />
        )}
      />
    </Modal>
  )
}

export function TeamPicker({
  title,
  sub,
  teams,
  users,
  selected,
  confirmLabel = 'Apply',
  clearLabel,
  onApply,
  onClose,
}: {
  title: string
  sub: string
  teams: TeamRow[]
  users: PersonRow[]
  selected: string[]
  confirmLabel?: string
  clearLabel?: string
  onApply: (next: string[]) => void
  onClose: () => void
}) {
  const [staged, setStaged] = useState(selected)
  const [query, setQuery] = useState('')

  const q = query.trim().toLowerCase()
  const shown = teams.filter(
    (team) => q === '' || team.name.toLowerCase().includes(q) || team.slug.includes(q),
  )

  return (
    <Modal
      open
      title={title}
      sub={sub}
      onClose={onClose}
      onSubmit={() => onApply(staged)}
      footer={
        <>
          {clearLabel ? (
            <button type="button" className="btn link" onClick={() => onApply([])}>
              {clearLabel}
            </button>
          ) : (
            <span className="prose tight">
              {count(staged.length)} selected of {count(teams.length)}
            </span>
          )}
          <div className="inline">
            <button type="button" className="btn" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="btn primary">
              {confirmLabel}
            </button>
          </div>
        </>
      }
    >
      <label className="search" style={{ width: '100%' }}>
        <Icon name="search" />
        <input
          value={query}
          placeholder="Search teams"
          aria-label="Search teams"
          onChange={(e) => setQuery(e.target.value)}
        />
      </label>

      <VirtualList
        items={shown}
        rowHeight={ROW}
        height={VIEW}
        empty={<div className="check-empty">No team matches that search.</div>}
        render={(team) => {
          const members = membersOf(team, users).length
          return (
            <CheckRow
              option={{
                id: team.id,
                label: team.name,
                sub: `${team.slug} · ${count(members)} ${members === 1 ? 'member' : 'members'}`,
                change: change(staged.includes(team.id), selected.includes(team.id)),
              }}
              on={staged.includes(team.id)}
              onToggle={() => toggleIn(staged, team.id, setStaged)}
            />
          )
        }}
      />
    </Modal>
  )
}

/** change says what applying would do to this row. A checkbox on its own does
 *  not say which way it moved from where it started. */
function change(now: boolean, before: boolean): 'add' | 'remove' | undefined {
  if (now && !before) return 'add'
  if (!now && before) return 'remove'
  return undefined
}
