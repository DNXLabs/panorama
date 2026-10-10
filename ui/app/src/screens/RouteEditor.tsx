// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from 'react'
import { del, put } from '../api'
import {
  API_ENDPOINTS,
  apiLabel,
  claudeCodeName,
  modelFor,
  modelMeta,
  reservedRouteName,
} from '../routes'
import type { Draft, Overlay } from '../routes'
import type { ModelRow, PersonRow, TeamRow } from '../types'
import { ActionButton, Drawer, ErrorBanner, Segmented, SubmitButton, useAction } from '../ui'

// Everything a route is, in one panel: the name a client asks for, the model it
// becomes, whether it keeps serving once a budget passes its soft limit, and
// who it exists for at all.
//
// Four blocks under four register labels, in the order the decisions are made.
// The name comes first because it is the only one that cannot be changed
// afterwards; the conditions come last because empty is the ordinary answer.

/** The two things a route can do once a budget governing its caller has passed
 *  a soft limit. Not a switch: the console has no knobs, and "on" would not say
 *  which of the two it meant. */
/** The tool id the data plane gives Claude Code, as the tool chips list it. */
const CLAUDE_CODE = 'claude-code'

const STANCES: { id: 'serves' | 'stops'; label: string }[] = [
  { id: 'serves', label: 'Keeps serving' },
  { id: 'stops', label: 'Stops' },
]

export function RouteEditor({
  draft,
  models,
  tools,
  people,
  teams,
  onEdit,
  onOverlay,
  onClose,
  onSaved,
  onRemoved,
}: {
  draft: Draft
  models: ModelRow[]
  tools: string[]
  people: PersonRow[]
  teams: TeamRow[]
  onEdit: (patch: Partial<Draft>) => void
  onOverlay: (overlay: Overlay) => void
  onClose: () => void
  onSaved: (name: string, created: boolean, warning?: string) => void
  onRemoved: (name: string) => void
}) {
  const [error, setError] = useState<unknown>(null)
  const existing = draft.original !== null

  const save = useAction(async () => {
    setError(null)
    try {
      const result = await put<{ warning?: string }>('/api/v1/routes', {
        name: draft.name.trim(),
        description: draft.description.trim(),
        users: draft.users,
        teams: draft.teams,
        tools: draft.tools,
        target: draft.target,
        serves_over_soft: draft.servesOverSoft,
      })
      // The route saved either way. A stop with nowhere to switch to may be
      // exactly what an administrator meant, so it is reported rather than
      // refused - and reported after the fact rather than as an error, which
      // would read as the save having failed.
      onSaved(draft.name.trim(), !existing, result.warning)
    } catch (cause) {
      setError(cause)
      throw cause
    }
  })

  const remove = async () => {
    setError(null)
    try {
      await del(`/api/v1/routes/${encodeURIComponent(draft.original!.name)}`)
      onRemoved(draft.original!.name)
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  const target = modelFor(models, draft.target)

  // A route for Claude Code should carry the model's own name, so the client
  // shapes its requests for that model rather than for the newest one there is.
  // Asked only where the tool condition names Claude Code and the model is one
  // Claude Code knows; whether other vendors' models may be offered to it at
  // all is not decided yet.
  const wantedName = target && draft.tools.includes(CLAUDE_CODE) ? claudeCodeName(target) : undefined
  const misnamed = wantedName !== undefined && draft.name.trim() !== wantedName

  return (
    <Drawer
      open
      title={existing ? draft.original!.name : 'New route'}
      // No sub on an existing route: the field help below says the same thing
      // about the name, and a panel that opens with two sentences about what a
      // route is delays the one thing somebody came here to change.
      sub={existing ? undefined : 'A name a client may ask for, and the model it becomes.'}
      onClose={onClose}
      onSubmit={save.act}
      footer={
        <>
          {/* A new route has nothing to delete, and the slot is left empty
              rather than filled with a sentence: Create route is the only thing
              in this footer, and it belongs against the right edge like every
              other primary action in the console. */}
          {existing ? (
            <ActionButton onAct={remove} label="Delete route" settled="Deleted" danger />
          ) : (
            <span />
          )}
          <SubmitButton
            state={save.state}
            disabled={
              draft.name.trim() === '' || draft.target === '' || reservedRouteName(draft.name)
            }
            label={existing ? 'Save route' : 'Create route'}
            settled="Saved"
            primary
          />
        </>
      }
    >
      {/* One wrapper, because the panel's own rhythm is 18px and a route's four
          blocks are read at 26px - the sections are long enough that the
          tighter step ran them together. */}
      <div className="rbody">
      <ErrorBanner error={error} />

      <div className="field">
        <label htmlFor="route-name">Route name · required</label>
        <input
          id="route-name"
          className={
            !existing && draft.name === '' ? 'input rinput name focus' : 'input rinput name'
          }
          value={draft.name}
          spellCheck={false}
          disabled={existing}
          placeholder="e.g. deep-work"
          onChange={(e) => onEdit({ name: e.target.value.replace(/[^a-zA-Z0-9-_]/g, '') })}
        />
        <span className="rhelp">
          {existing
            ? 'A name cannot be changed.'
            : 'The model name clients ask for. Letters, digits, hyphens and underscores only.'}
        </span>
        {reservedRouteName(draft.name) && (
          <div className="notice warn" style={{ marginTop: 8 }}>
            <div className="notice-title">
              Claude Code never asks for <span className="mono">{draft.name.trim()}</span>
            </div>
            <div className="notice-body">
              It resolves that name itself before a request is made and sends a model id instead,
              which this deployment refuses because nobody routed it. The engineer sees a model
              they never typed refused, and this route listed as one they can use.{' '}
              {existing
                ? 'This route was created before that was checked, and a name cannot be changed. Create the replacement - “standard” is the usual one - point the laptops at it, then delete this one.'
                : 'Pick another name - “standard” is the usual one.'}
            </div>
          </div>
        )}
        {misnamed && (
          <div className="notice warn" style={{ marginTop: 8 }}>
            <div className="notice-title">
              For Claude Code, name the route <span className="mono">{wantedName}</span>
            </div>
            <div className="notice-body">
              Claude Code prepares each request for the model it thinks it is talking to. A name
              it does not recognise is treated as its newest model, and an older model refuses
              parts of what it sends for that one.{' '}
              {existing
                ? 'A name cannot be changed: create a route with this name, point the laptops at it, then delete this one.'
                : null}
            </div>
            {!existing && (
              <button
                type="button"
                className="rlink"
                style={{ marginTop: 8 }}
                onClick={() => onEdit({ name: wantedName })}
              >
                Use this name
              </button>
            )}
          </div>
        )}
      </div>

      <div className="field">
        <label htmlFor="route-desc">Description · optional</label>
        <input
          id="route-desc"
          className="input rinput desc"
          value={draft.description}
          placeholder="One line, for the next administrator"
          onChange={(e) => onEdit({ description: e.target.value })}
        />
      </div>

      <section className="rsection">
        <div className="rsection-head">Model · required</div>

        {target || draft.target ? (
          <div className="rblock">
            <span className="rblock-text">
              <span className="rblock-title">{target ? target.name : draft.target}</span>
              <span className="rblock-meta">
                {target ? modelMeta(target) : 'Not in the catalogue - the provider is the authority'}
              </span>
            </span>
            <button
              type="button"
              className="rlink"
              onClick={() => onOverlay({ kind: 'catalogue', picking: true })}
            >
              Change
            </button>
          </div>
        ) : (
          <div className="rblock wanting">
            <span className="rblock-text">
              <span className="rblock-title wanting">Choose a model</span>
              <span className="rblock-meta">Required - a route with no model serves nothing</span>
            </span>
            <button
              type="button"
              className="rlink"
              onClick={() => onOverlay({ kind: 'catalogue', picking: true })}
            >
              Browse models
            </button>
          </div>
        )}

        {/* Which endpoint the route answers on follows from the model, and it
            is said here because it decides which tools can use the name at
            all: a route on a model that speaks only Chat Completions is not
            one Claude Code can ask for. */}
        {target && (
          <div className="rblock tall">
            <span className="rblock-text">
              <span className="rblock-title">Served on · {apiLabel(target.apis)}</span>
              <span className="rblock-why">
                {target.apis.map((api) => API_ENDPOINTS[api]).join(', ')}. A request for this
                route at any other endpoint is refused, naming the one to use.
              </span>
            </span>
          </div>
        )}
      </section>

      <section className="rsection">
        <div className="rsection-head">Budget config</div>
        <div className="rblock tall">
          <span className="rblock-text">
            <span className="rblock-title">Past a soft budget limit</span>
            <span className="rblock-why">
              {draft.servesOverSoft
                ? 'Requests keep being served and carry a warning. A hard limit still blocks.'
                : 'Requests on this route are refused past a soft limit. The engineer is told which routes still work and picks one - the model never changes under them.'}
            </span>
          </span>
          <Segmented
            value={draft.servesOverSoft ? 'serves' : 'stops'}
            options={STANCES}
            label="Past a soft budget limit"
            onChange={(id) => onEdit({ servesOverSoft: id === 'serves' })}
          />
        </div>
      </section>

      <section className="rsection wide">
        <div className="rsection-head">Conditions · optional</div>

        <div className="rcond">
          <div className="rcond-head">
            <span className="rcond-name">People</span>
            <button
              type="button"
              className="rcond-action"
              onClick={() => onOverlay({ kind: 'people' })}
            >
              {draft.users.length > 0 ? 'Edit people' : 'Add people'}
            </button>
          </div>
          <span className="rcond-state">
            {draft.users.length === 0
              ? 'Everyone - no person condition'
              : draft.users
                  .map((id) => {
                    const person = people.find((p) => p.id === id)
                    return person?.display_name || person?.email || id
                  })
                  .join(', ')}
          </span>
        </div>

        <div className="rcond">
          <div className="rcond-head">
            <span className="rcond-name">Teams</span>
            <button
              type="button"
              className="rcond-action"
              onClick={() => onOverlay({ kind: 'teams' })}
            >
              {draft.teams.length > 0 ? 'Edit teams' : 'Add teams'}
            </button>
          </div>
          <span className="rcond-state">
            {draft.teams.length === 0
              ? 'Every team - no team condition'
              : draft.teams.map((id) => teams.find((t) => t.id === id)?.name ?? id).join(', ')}
          </span>
        </div>

        {/* Tools are picked from the list the data plane derives from a user
            agent, and typing a name is deliberately not offered: the server
            refuses a tool it has never seen, and a condition naming one would
            be a route that matches nothing. */}
        <div className="rcond">
          <span className="rcond-name">Tools</span>
          <span className="rcond-state">
            {draft.tools.length === 0
              ? 'Every tool - no tool condition'
              : draft.tools.join(', ')}
          </span>
          <div className="rchips">
            {tools.map((tool) => {
              const on = draft.tools.includes(tool)
              return (
                <button
                  key={tool}
                  type="button"
                  className={on ? 'rchip on' : 'rchip'}
                  aria-pressed={on}
                  onClick={() =>
                    onEdit({
                      tools: on ? draft.tools.filter((t) => t !== tool) : [...draft.tools, tool],
                    })
                  }
                >
                  {tool}
                </button>
              )
            })}
          </div>
        </div>
      </section>
      </div>
    </Drawer>
  )
}
