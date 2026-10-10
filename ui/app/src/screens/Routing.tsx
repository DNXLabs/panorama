// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useMemo, useState } from 'react'
import { post } from '../api'
import { usePageChrome } from '../chrome'
import { count } from '../format'
import { Icon } from '../icons'
import { Link } from '../router'
import { apiLabel, matchedPeople, modelFor, nameOf, priceLine, profileLabel, scopeOf } from '../routes'
import type { Draft, Overlay } from '../routes'
import { useSession } from '../session'
import type {
  ModelRow,
  PersonRow,
  ProviderSummary,
  RouteRow,
  Routing as RoutingData,
  TeamRow,
} from '../types'
import { Empty, ErrorBanner, Loading, Pill, Segmented, Select, useApi, useToast } from '../ui'
import { Catalogue } from './ModelCatalogue'
import { PeoplePicker, TeamPicker } from './Pickers'
import { RouteEditor } from './RouteEditor'

// The names a client may ask for, and what serves them.
//
// An engineer points their tool's model setting at a route name and never at a
// model.
// Which model that name becomes - and which country its traffic is processed
// in - is decided here, and can change under a running deployment without
// anybody touching a laptop.
//
// A card is the whole of a route: what it is called and who it exists for on
// the left, what it becomes and what happens to it at a soft limit on the
// right. The whole card is the click target, because there is nothing on a
// route that is not editable and an "Edit" link would have been a second thing
// to aim at inside a thing you can already aim at.
//
// Budgets are not on this screen. A route reads a budget and cannot set one,
// so a limit here would look like a property of routing; it is a property of a
// person or a team, and it lives with them on People & teams. What is here is
// the half that is routing's: whether a route keeps serving once a soft limit
// is crossed.

type Scope = 'all' | 'open' | 'narrowed'

const SCOPES: { id: Scope; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'open', label: 'Everyone' },
  { id: 'narrowed', label: 'Narrowed' },
]

export function Routing() {
  const me = useSession().session!.user
  const routing = useApi<RoutingData>('/api/v1/models')

  // The people and teams a condition names. Served to leaders only, which is
  // also who may edit: a developer reading this screen is served the routes
  // they can use and no list of colleagues.
  const people = useApi<{ users: PersonRow[] }>(me.can_see_everyone ? '/api/v1/users' : null)
  const teams = useApi<{ teams: TeamRow[] }>('/api/v1/teams')

  const [draft, setDraft] = useState<Draft | null>(null)
  const [overlay, setOverlay] = useState<Overlay>(null)
  const [query, setQuery] = useState('')
  const [scope, setScope] = useState<Scope>('all')
  const [model, setModel] = useState('all')
  const { say, toast } = useToast()

  const canEdit = routing.data?.can_edit ?? false
  const models = routing.data?.models ?? []
  const routes = useMemo(() => routing.data?.routes ?? [], [routing.data])
  const roster = people.data?.users ?? []
  const teamList = teams.data?.teams ?? []

  // A developer is served the routes they can use and not the conditions that
  // decided it, so there is nothing for a scope filter to read. Offered to the
  // reader who has the data rather than left as a control that does nothing.
  const conditioned = routes.some((route) => route.users !== undefined)

  /** The models these routes actually point at, which is the only list worth
   *  filtering by - the catalogue has thirty and this deployment uses three. */
  const inUse = useMemo(() => {
    const ids = [...new Set(routes.map((route) => route.target).filter(Boolean))]
    return ids
      .map((id) => ({ id, label: nameOf(models, id) }))
      .sort((a, b) => a.label.localeCompare(b.label))
  }, [routes, models])

  const shown = useMemo(() => {
    const q = query.trim().toLowerCase()
    return routes.filter((route) => {
      if (q !== '' && !route.name.toLowerCase().includes(q)) return false
      if (scope !== 'all') {
        const narrowed = scopeOf(route) !== 'everyone'
        if (scope === 'open' ? narrowed : !narrowed) return false
      }
      if (model !== 'all' && route.target !== model) return false
      return true
    })
  }, [routes, query, scope, model])

  const edit = (patch: Partial<Draft>) => setDraft((d) => (d === null ? d : { ...d, ...patch }))

  const open = (route: RouteRow | null) => {
    setDraft({
      original: route,
      name: route?.name ?? '',
      description: route?.description ?? '',
      target: route?.target ?? '',
      // A new route serves past a soft limit. Defaulting the other way would
      // turn a warning into a block for everybody the route admits.
      servesOverSoft: route?.serves_over_soft ?? true,
      users: route?.users ?? [],
      teams: route?.teams ?? [],
      tools: route?.tools ?? [],
    })
    setOverlay(null)
  }

  // Throws what went wrong and sets nothing on this page: the refresh is
  // pressed inside the catalogue, which is a modal over this screen, so an
  // error shown here would be shown behind the scrim - the button would read
  // "failed" and the reason would sit under the panel that had asked for it. The catalogue shows
  // what this throws.
  const discover = async () => {
    const result = await post<{ ok: boolean; providers: DiscoveryResult[] }>(
      '/api/v1/models/discover',
    )
    // Each provider is asked on its own and one failing does not stop the
    // others, so the catalogue is reloaded either way and the failures are
    // named by provider.
    routing.reload()
    if (!result.ok) {
      const failed = result.providers
        .filter((p) => !p.ok)
        .map((p) => [`${p.provider} in ${p.region}: ${p.error}`, p.hint].filter(Boolean).join(' - '))
      throw new Error(failed.join('\n'))
    }
  }

  usePageChrome(
    {
      sub: routing.loading
        ? undefined
        : `${count(routes.length)} route ${routes.length === 1 ? 'name serves' : 'names serve'} this account.`,
      actions: (
        <>
          {me.can_see_everyone && (
            <button
              type="button"
              className="btn"
              onClick={() => setOverlay({ kind: 'catalogue', picking: false })}
            >
              Model catalogue
              <span className="btn-count">{count(models.length)}</span>
            </button>
          )}
          {canEdit && (
            <button type="button" className="btn primary" onClick={() => open(null)}>
              New route
            </button>
          )}
        </>
      ),
    },
    [routes.length, models.length, canEdit, me.can_see_everyone, routing.loading],
  )

  return (
    <main className="bands">
      {/* One band, not two. The filters and the cards they narrow are one
          thing, and a band boundary between them would say the section had
          ended. The rule under the controls is the filter row's own. */}
      <section className="band rows last">
        <div className="rfilters">
          <label className="search">
            <Icon name="search" />
            <input
              value={query}
              placeholder="Search route name"
              aria-label="Search route names"
              onChange={(e) => setQuery(e.target.value)}
            />
          </label>

          {conditioned && (
            <Segmented
              value={scope}
              dense
              label="Who a route is for"
              options={SCOPES}
              onChange={setScope}
            />
          )}

          <Select
            value={model}
            label="Model"
            onChange={setModel}
            options={[
              { value: 'all', label: 'Any model' },
              ...inUse.map((entry) => ({ value: entry.id, label: entry.label })),
            ]}
          />

          <span className="filter-count">
            {shown.length === routes.length
              ? `${count(routes.length)} ${routes.length === 1 ? 'route' : 'routes'}`
              : `${count(shown.length)} of ${count(routes.length)} routes`}
          </span>
        </div>

        <ErrorBanner error={routing.error} />

        {routing.loading ? (
          <Loading rows={3} />
        ) : routes.length === 0 ? (
          <Empty title="No routes yet">
            {canEdit
              ? 'Every request is refused until a route exists - there is no "allow any model" setting anywhere, so this list is the whole of what this deployment permits.'
              : 'There is no route you can use yet. An administrator creates routes and decides which model each one runs on.'}
          </Empty>
        ) : shown.length === 0 ? (
          <Empty title="No route matches these filters" />
        ) : (
          <div className="rcards">
            {shown.map((route) => (
              <RouteCard
                key={route.name}
                route={route}
                models={models}
                providers={routing.data?.providers ?? []}
                people={roster}
                teams={teamList}
                canEdit={canEdit}
                onEdit={() => open(route)}
              />
            ))}
          </div>
        )}

        {/* One span, because `.footnote` is a flex row: left as loose children
            the sentence and its link become separate flex items and the link
            wraps into a column of its own with the full stop stranded beside
            it. */}
        <p className="footnote accent">
          <span>
            Budgets are not set here. A route only reacts to them: past a soft limit a route
            either keeps serving or stops. Limits live on the person or the team, over on{' '}
            <Link to="/people">People &amp; teams</Link>.
          </span>
        </p>
      </section>

      {draft !== null && (
        <RouteEditor
          draft={draft}
          models={models}
          tools={routing.data?.tools ?? []}
          people={roster}
          teams={teamList}
          onEdit={edit}
          onOverlay={setOverlay}
          onClose={() => {
            setDraft(null)
            setOverlay(null)
          }}
          onSaved={(name, created, warning) => {
            setDraft(null)
            setOverlay(null)
            routing.reload()
            // The route saved. A warning says it stops at the soft limit with
            // nowhere to send the people it stops - which may be exactly what
            // was meant, so it is said instead of the confirmation rather than
            // in place of the save.
            say(
              warning ??
                (created
                  ? `${name} created. Point a tool at it by name, from My setup.`
                  : `${name} saved. Takes effect on the next request.`),
            )
          }}
          onRemoved={(name) => {
            setDraft(null)
            setOverlay(null)
            routing.reload()
            say(`${name} removed. Clients still asking for it are refused.`)
          }}
        />
      )}

      {overlay?.kind === 'catalogue' && (
        <Catalogue
          data={routing.data ?? undefined}
          loading={routing.loading}
          picking={overlay.picking}
          chosen={overlay.picking ? (draft?.target ?? '') : ''}
          canRefresh={me.is_admin}
          onDiscover={discover}
          onChoose={(ref) => {
            // The catalogue only offers a row to click when it is picking, so
            // a read cannot get here - and a draft cannot either, since the
            // editor is what opened it.
            if (!overlay.picking || draft === null) return
            edit({ target: ref })
            setOverlay(null)
          }}
          onClose={() => setOverlay(null)}
        />
      )}

      {overlay?.kind === 'people' && draft !== null && (
        <PeoplePicker
          title="People this route exists for"
          sub="Leave it empty and the route is open to everyone. Naming people narrows it to exactly them."
          users={roster}
          teams={teamList}
          selected={draft.users}
          clearLabel="Clear condition - everyone"
          onApply={(next) => {
            edit({ users: next })
            setOverlay(null)
          }}
          onClose={() => setOverlay(null)}
        />
      )}

      {overlay?.kind === 'teams' && draft !== null && (
        <TeamPicker
          title="Teams this route exists for"
          sub="Leave it empty and every team qualifies. Naming teams narrows it to their members."
          teams={teamList}
          users={roster}
          selected={draft.teams}
          clearLabel="Clear condition - every team"
          onApply={(next) => {
            edit({ teams: next })
            setOverlay(null)
          }}
          onClose={() => setOverlay(null)}
        />
      )}

      {toast}
    </main>
  )
}

/** One provider's answer to a catalogue refresh. */
type DiscoveryResult = {
  provider: string
  region: string
  ok: boolean
  targets?: number
  error?: string
  hint?: string
}

/* ------------------------------------------------------------------- card -- */

function RouteCard({
  route,
  models,
  providers,
  people,
  teams,
  canEdit,
  onEdit,
}: {
  route: RouteRow
  models: ModelRow[]
  providers: ProviderSummary[]
  people: PersonRow[]
  teams: TeamRow[]
  canEdit: boolean
  onEdit: () => void
}) {
  // Conditions are served to leaders only. A developer is shown the routes
  // they can use, not the names of the colleagues a route was narrowed to.
  const conditioned = route.users !== undefined
  const model = modelFor(models, route.target)
  // A route on a provider that is off or failed to build admits a request and
  // has nowhere to send it. Said on the card, because the engineer on the other
  // end is told it is the deployment's fault and this is where it is fixed.
  const provider = providers.find((p) => p.kind === route.provider)
  const stranded = route.complete && !provider?.serving

  const body = (
    <>
      <span className="rcard-main">
        <span className="rcard-id">
          <span className="rcard-name">{route.name}</span>
          {!route.complete && <Pill tone="warn">no model</Pill>}
          {/* Said only when it is true, and said as what happens rather than as
              a flag name: a route that keeps serving is the ordinary one and
              needs no label. */}
          {!route.serves_over_soft && <Pill tone="warn">stops at soft limit</Pill>}
          {conditioned && <Pill tone="off">{scopeOf(route, matchedPeople(route, people).length)}</Pill>}
        </span>

        <span className="rcard-desc">{route.description || 'No description.'}</span>

        {conditioned && (
          <span className="rcard-conds">
            <span className="rcard-cond-label">People</span>
            <span className="rcard-cond-value">
              {named(route.users, 'everyone', (id) => {
                const person = people.find((p) => p.id === id)
                return person?.display_name || person?.email || 'someone who has left'
              })}
            </span>
            <span className="rcard-cond-label">Teams</span>
            <span className="rcard-cond-value">
              {named(route.teams, 'every team', (id) => teams.find((t) => t.id === id)?.name ?? 'a team that is gone')}
            </span>
            <span className="rcard-cond-label">Tools</span>
            <span className="rcard-cond-value">{named(route.tools, 'every tool', (t) => t)}</span>
          </span>
        )}
      </span>

      <span className="rcard-serves">
        <span className="rcard-model">
          <span className="rcard-cond-label">Serves</span>
          <span className="rcard-model-name">
            <span className={model || route.target ? 'name' : 'name unset'}>
              {model ? model.name : route.target || 'No model set'}
            </span>
            {stranded && <Pill tone="warn">provider not serving</Pill>}
            {model && <Pill tone={model.global ? undefined : 'off'}>{profileLabel(model)}</Pill>}
            {route.unrecognised && <Pill tone="warn">not in catalogue</Pill>}
            {model?.legacy && <Pill tone="warn">legacy</Pill>}
          </span>
          <span className="rcard-rate">
            {route.target ? priceLine(model) : 'This route cannot serve a request yet'}
          </span>
        </span>

        <span className="rcard-pair">
          {/* Where it runs and what it answers on, because both decide who can
              use the name: a route on a Chat Completions model is not one
              Claude Code can ask for, whatever its conditions say. */}
          <span className="rcard-fact">
            <span className="key">{route.provider_label || 'Provider'}</span>
            <span className={route.apis.length > 0 ? 'value' : 'value warn'}>
              {apiLabel(route.apis)}
            </span>
          </span>
          <span className="rcard-fact">
            <span className="key">Past soft limit</span>
            <span className={route.serves_over_soft ? 'value' : 'value warn'}>
              {route.serves_over_soft ? 'keeps serving' : 'stops'}
            </span>
          </span>
        </span>
      </span>
    </>
  )

  if (!canEdit) return <div className="rcard flat">{body}</div>
  return (
    <button type="button" className="rcard" onClick={onEdit}>
      {body}
    </button>
  )
}

/** named reads a condition as the words it narrows to, or as what it lets
 *  through when it is empty. An empty condition is not "none": it is everyone,
 *  and writing it as an absence is how a reader concludes the opposite. */
function named(ids: string[] | null | undefined, none: string, label: (id: string) => string): string {
  const list = ids ?? []
  if (list.length === 0) return none
  return list.map(label).join(', ')
}
