// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useEffect, useState } from 'react'
import type { CSSProperties, ReactNode } from 'react'
import { count } from '../format'
import { Icon } from '../icons'
import { patch, post, put } from '../api'
import { useSession } from '../session'
import type {
  Identity,
  PersonRow,
  ProviderSummary,
  Role,
  Settings as SettingsData,
  SsoFinding,
  TeamRow,
} from '../types'
import {
  ActionButton,
  Avatar,
  Card,
  Choice,
  Drawer,
  Empty,
  ErrorBanner,
  Loading,
  Segmented,
  Select,
  SubmitButton,
  useAction,
  useApi,
  useToast,
} from '../ui'
import { PeoplePicker } from './Pickers'
import { Findings, SsoConnect } from './SsoConnect'
import { CostCenters } from './CostCenters'
import { Integrations } from './Integrations'
import { ModelProviders } from './ModelProviders'
import { WebSearch } from './WebSearch'
import { useRouter } from '../router'

// Deployment-level configuration, visited rarely.
//
// A stack of bands rather than a page of cards, because everything on this
// screen is read as a document: what this deployment calls itself, what it is
// configured to do, and how a directory's groups become roles here. Two of
// those three answer at a glance and are half a page wide, so the first band is
// split down the middle - the settings that can be changed on the left, the
// ones that cannot on the right, with the rule between them saying which is
// which without a word.

/** The roles a group can be mapped to, and what each one can see. `none` is
 *  not among them: a mapping that grants nothing is a group left unmapped. */
const ROLES: { id: Role; label: string; help: string }[] = [
  { id: 'admin', label: 'Admin', help: 'Sees every team aggregate, mints tokens, sets budgets and routes.' },
  { id: 'manager', label: 'Manager', help: 'Sees the aggregates of teams they lead, and sets caps inside them.' },
  { id: 'developer', label: 'Developer', help: 'Sees their own usage and the budget that governs them, nothing else.' },
]

/** The command-line sign-in switch, in the words an administrator reads it in.
 *
 *  Two options rather than a checkbox, and the same pair the model providers
 *  use, because this console says on and off with a Segmented everywhere it
 *  says it at all. */
const CLI_SWITCH: { id: 'on' | 'off'; label: string }[] = [
  { id: 'on', label: 'On' },
  { id: 'off', label: 'Off' },
]

/**
 * The mapping table's tracks, written once.
 *
 * The head takes all three. A row splits them, because the first two open the
 * panel and the third removes the row, and a button inside a button is not
 * markup - so the row is an outer grid of [everything else, action] holding an
 * inner grid of [group, role]. Same gap in all three is what keeps the two
 * halves landing under the head's own columns rather than near them.
 */
const MAP_TRACKS = ['minmax(0, 1fr)', '150px', '32px']
const MAP_GAP = 16
const MAP_HEAD = { gridTemplateColumns: MAP_TRACKS.join(' '), gap: MAP_GAP }
const MAP_ROW = { gridTemplateColumns: `minmax(0, 1fr) ${MAP_TRACKS[2]}`, gap: MAP_GAP }
const MAP_CELLS = { gridTemplateColumns: MAP_TRACKS.slice(0, 2).join(' '), gap: MAP_GAP }

/**
 * Settings is six documents behind tabs.
 *
 * Everything here is configuration an administrator sets once and then lives
 * with: what the deployment calls itself, how people sign in, where it invokes
 * models, which accounting lines it reports against, and which sources it
 * confirms work against. That is one shape, so it is one screen.
 *
 * A region and a switch per model provider is a decision made after a
 * deployment is stood up, not configuration-file material, which is why
 * providers sit here beside integrations. Web search is the same kind of
 * decision about a different backend, so it sits beside them.
 *
 * Integrations is one-off administrator configuration in exactly the sense
 * single sign-on is. A failing source is not buried by living here: it raises
 * an item on Analytics' Attention tab, which is where a reader is when they
 * would want to know.
 *
 * The Organization tab is a document - read rather than operated, which is why
 * it carries no card.
 */
type Tab = 'organization' | 'sso' | 'providers' | 'web-search' | 'cost-centers' | 'integrations'

const TABS: { id: Tab; label: string }[] = [
  { id: 'organization', label: 'Organization' },
  { id: 'sso', label: 'Single sign-on' },
  { id: 'providers', label: 'Model providers' },
  { id: 'web-search', label: 'Web search' },
  { id: 'cost-centers', label: 'Cost centers' },
  { id: 'integrations', label: 'Integrations' },
]

export function Settings() {
  const me = useSession().session!.user
  const { search, setParam } = useRouter()
  const settings = useApi<SettingsData>('/api/v1/settings')

  const asked = (search.get('tab') as Tab) || 'organization'
  // Five of the six are an administrator's. A manager reads the deployment's
  // own facts and nothing else - the server refuses the rest either way, so
  // this decides what is worth offering.
  const visible = TABS.filter((t) => me.is_admin || t.id === 'organization')
  const tab = visible.some((t) => t.id === asked) ? asked : 'organization'

  return (
    <>
      <nav className="screen-tabs" aria-label="Settings sections">
        {visible.map((spec) => (
          <button
            key={spec.id}
            type="button"
            className={spec.id === tab ? 'screen-tab active' : 'screen-tab'}
            aria-current={spec.id === tab ? 'page' : undefined}
            onClick={() => setParam('tab', spec.id === 'organization' ? null : spec.id)}
          >
            {spec.label}
          </button>
        ))}
      </nav>

      <main className="bands">
        {tab === 'providers' && <ModelProviders />}
        {tab === 'web-search' && <WebSearch />}
        {tab === 'cost-centers' && <CostCenters />}
        {tab === 'integrations' && <Integrations nested />}

        {tab === 'sso' && (
          <>
            <IdentitySettings />
            <DirectAdmins />
          </>
        )}

        {tab === 'organization' && (
          <>
            {settings.error ? (
              <section className="band bar">
                <ErrorBanner error={settings.error} />
              </section>
            ) : null}

            {settings.loading ? (
              <section className="band">
                <Loading rows={4} />
              </section>
            ) : settings.data ? (
              // The two halves of "what is this deployment": the part an
              // administrator owns, and the part the manifest does. This tab
              // is the whole of the document now, so it always closes here.
              <section
                className="band split last"
                style={{ '--split-left': '1.15fr' } as CSSProperties}
              >
                <Organization
                  data={settings.data}
                  canEdit={me.is_admin}
                  onSaved={settings.reload}
                />
                <Deployment data={settings.data} />
              </section>
            ) : null}
          </>
        )}
      </main>
    </>
  )
}

function Organization({
  data,
  canEdit,
  onSaved,
}: {
  data: SettingsData
  canEdit: boolean
  onSaved: () => void
}) {
  const [values, setValues] = useState<Record<string, string>>({})

  useEffect(() => {
    setValues({
      'org.name': data.settings['org.name'] ?? '',
      'retention.request_days': data.settings['retention.request_days'] || '400',
      // Absent is off, which is what this ships with. Read the same way the
      // server reads it, so the screen and the gateway agree about a row
      // nobody has written yet.
      'cli.sign_in': data.settings['cli.sign_in'] === 'true' ? 'true' : 'false',
    })
  }, [data])

  const save = useCallback(async () => {
    // The CLI switch is left out of the write unless this build lets it be
    // changed. It is rendered from the stored row either way, and a deployment
    // whose row says on - a database somebody wrote themselves, which is how
    // the end-to-end suite runs - would otherwise have it switched off by
    // somebody saving an organization name.
    const body = { ...values }
    if (!data.deployment.cli_sign_in_released) delete body['cli.sign_in']
    await patch('/api/v1/settings', body)
    onSaved()
  }, [values, data.deployment.cli_sign_in_released, onSaved])

  const { state, error, act } = useAction(save)

  const set = (key: string, value: string) => setValues((current) => ({ ...current, [key]: value }))

  return (
    <div>
      <div className="band-head lead">
        <span className="band-title">Organization</span>
        <span className="band-sub">
          What this deployment calls itself, what it keeps, and how an engineer&rsquo;s tool gets a
          credential.
        </span>
      </div>

      <form
        className="setform"
        onSubmit={(event) => {
          event.preventDefault()
          act()
        }}
      >
        <ErrorBanner error={error} />

        <div className="field">
          <label htmlFor="org-name">Organization name</label>
          <input
            id="org-name"
            className="input"
            value={values['org.name'] ?? ''}
            disabled={!canEdit}
            onChange={(e) => set('org.name', e.target.value)}
            placeholder="Shown beside every screen title"
          />
        </div>

        <div className="field">
          <label htmlFor="retention">Audit trail retention, in days</label>
          {/* Digits only, as they are typed. The field is a number of days and
              the server refuses anything else, so accepting "400 days" here
              would be collecting a keystroke in order to reject it. */}
          <input
            id="retention"
            className="input"
            inputMode="numeric"
            value={values['retention.request_days'] ?? ''}
            disabled={!canEdit}
            onChange={(e) => set('retention.request_days', e.target.value.replace(/\D/g, ''))}
          />
          <span className="setnote">
            Metadata only. There is nothing else to retain - bodies are pattern-matched in flight
            and discarded, so no setting here can make Panorama keep one.
          </span>
        </div>

        <CommandLineSignIn
          value={values['cli.sign_in'] === 'true' ? 'on' : 'off'}
          released={data.deployment.cli_sign_in_released}
          canEdit={canEdit}
          onChange={(next) => set('cli.sign_in', next === 'on' ? 'true' : 'false')}
        />

        {canEdit && (
          <div className="setsave">
            <SubmitButton state={state} label="Save" settled="Saved" primary />
          </div>
        )}
      </form>
    </div>
  )
}

/**
 * The command-line sign-in switch.
 *
 * `panorama login` signs a machine in and leaves a credential that rotates
 * itself, which is the better of the two ways an engineer's tool can hold a
 * token - and it is off, because this release has not handed the CLI to
 * engineers yet. Until it does, a tool holds a gateway token minted on My
 * setup, which the person re-mints when it runs out.
 *
 * SHOWN WHILE IT CANNOT BE MOVED, rather than hidden until it can. A setting
 * that appears on the day it starts working is one nobody was told about; a
 * switch sitting at Off with four words beside it is the same fact stated
 * ahead of time. Four words and not a paragraph: what an administrator needs
 * here is whether the control will move, and the credential that stands in for
 * it is explained on the screen where somebody actually mints one.
 *
 * The console does not decide this. `cli_sign_in_released` comes off the
 * deployment, and the server refuses the write regardless of what renders here
 * - a screen that greyed a control out and called that the rule would be the
 * control this console is not.
 */
function CommandLineSignIn({
  value,
  released,
  canEdit,
  onChange,
}: {
  value: 'on' | 'off'
  released: boolean
  canEdit: boolean
  onChange: (next: 'on' | 'off') => void
}) {
  return (
    <div className="field">
      <span className="fieldlabel">Command-line sign-in</span>
      <Segmented
        value={value}
        options={CLI_SWITCH}
        label="Command-line sign-in"
        disabled={!released || !canEdit}
        onChange={onChange}
      />
      <span className="setnote">
        {released
          ? 'On, an engineer runs panorama login once per machine and their tool holds a short-lived token that rotates on its own. Off, the only credential is a gateway token minted on My setup.'
          : 'Not available in this release.'}
      </span>
    </div>
  )
}

function Deployment({ data }: { data: SettingsData }) {
  return (
    <div className="setcol">
      <div className="band-head lead">
        <span className="band-title">This deployment</span>
        <span className="band-sub">
          Set in the configuration file and the environment, except the model providers, which are
          configured on their own tab.
        </span>
      </div>

      <div className="facts">
        <Fact label="Endpoint" value={data.deployment.base_url} />
        <Fact label="Model providers" value={providersLine(data.deployment.providers)} />
        <Fact label="Database" value={data.deployment.driver} />
        <Fact
          label="Single sign-on"
          value={data.deployment.sso_configured ? 'configured' : 'not configured'}
        />
      </div>

      <span className="setnote">
        Secrets live in the configuration and the environment, and are never written to the
        database. That is why there is no form anywhere for a client secret or an AWS credential.
      </span>
    </div>
  )
}

/** Which providers serve, and where, as one line. A provider that is
 *  configured and not serving says so, because "Bedrock, us-east-1" beside one
 *  that failed to build would read as working. */
function providersLine(providers: ProviderSummary[]): string {
  const configured = providers.filter((p) => p.configured)
  if (configured.length === 0) return 'not configured'
  return configured
    .map((p) => `${p.label}, ${p.region}${p.serving ? '' : p.enabled ? ' (failing)' : ' (off)'}`)
    .join(' · ')
}

/** One thing this deployment is configured to do. A row rather than a stacked
 *  pair, because the label is a question everybody knows the shape of and the
 *  answer is the only part worth scanning down. */
function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div className="fact">
      <span className="fact-label">{label}</span>
      <span className="fact-value" title={value}>
        {value}
      </span>
    </div>
  )
}

/**
 * The frame around single sign-on, and why there are two of them.
 *
 * This block is on two screens with two shapes. On Settings it is a band, which
 * is what every section of that screen is. In a setup session it sits in a
 * column of cards beside the model provider step, where a full-bleed band would be the
 * one element on the page reaching past the gutter. One component, because the
 * mapping is the same mapping; two frames, because the pages are not the same
 * page.
 */
function SsoFrame({
  setup,
  sub,
  actions,
  children,
}: {
  setup: boolean
  sub: string
  actions?: ReactNode
  children: ReactNode
}) {
  if (setup) {
    return (
      <Card title="Single sign-on" sub={sub} actions={actions}>
        {children}
      </Card>
    )
  }
  return (
    <section className="band">
      <div className="ssoblock">
        <div className="band-head-row top">
          <div className="band-head lead">
            <span className="band-title">Single sign-on</span>
            <span className="band-sub">{sub}</span>
          </div>
          {actions}
        </div>
        {children}
      </div>
    </section>
  )
}

/**
 * IdentitySettings edits the half of SSO that is organizational data.
 *
 * In `setup` mode the group mapping is hidden until a sign-in has actually
 * carried groups. Mapping is an operational job, not a setup step - it changes
 * as teams do - and before the first sign-in it cannot be done at all, because
 * nothing yet knows what the provider calls its groups. An empty form there
 * invites somebody to type a guess, and a guessed group name grants nothing and
 * fails silently.
 *
 * It reappears the moment groups have been seen, which is exactly the situation
 * a setup code is issued for the second time: a mapping that admits nobody.
 */
export function IdentitySettings({ setup = false }: { setup?: boolean } = {}) {
  const identity = useApi<Identity>('/api/v1/identity')
  const [groups, setGroups] = useState<{ group: string; role: Role }[]>([])
  const [defaultRole, setDefaultRole] = useState('developer')
  const [error, setError] = useState<unknown>(null)
  const [doctor, setDoctor] = useState<{ level: string; findings: SsoFinding[] } | null>(null)
  /** The chip somebody has picked, which is what "Add a mapping" opens with.
   *  One at a time: a mapping is one group and one role. */
  const [picked, setPicked] = useState<string | null>(null)
  const [editing, setEditing] = useState<{ at: number | null; group: string; role: Role } | null>(
    null,
  )

  useEffect(() => {
    if (!identity.data) return
    setGroups(identity.data.groups)
    setDefaultRole(identity.data.default_role)
  }, [identity.data])

  if (identity.loading) {
    return (
      <SsoFrame setup={setup} sub="The connection is configuration. The group mapping is yours, and lives here.">
        <Loading rows={3} />
      </SsoFrame>
    )
  }

  if (!identity.data?.configured) {
    return (
      <SsoFrame
        setup={setup}
        sub="Generic OIDC. Microsoft Entra ID and Okta are the documented first-class paths."
      >
        <div className="setnote wide" style={{ marginBottom: 22 }}>
          The connection belongs in the configuration or the environment, beside every other secret,
          and Panorama will not store it - a client secret in a database is a client secret in a
          backup. What it can do is prove an issuer works before you restart anything, and hand you
          the exact configuration to apply. The same check runs from a shell as{' '}
          <span className="mono">panorama sso probe</span>.
        </div>
        <SsoConnect />
      </SsoFrame>
    )
  }

  const data = identity.data

  // Errors surface through the banner rather than being swallowed by the
  // button: the button reports that the save failed, the banner reports why.
  const save = async () => {
    setError(null)
    try {
      await put('/api/v1/identity/groups', {
        groups,
        default_role: defaultRole,
        // What this screen loaded. The server refuses the save if the mapping
        // has moved since, because this document is replaced whole and a stale
        // save would delete rows nobody here ever saw.
        mapping_version: data.mapping_version,
      })
      identity.reload()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  // Doctor rather than the discovery-only check: discovery proves the issuer is
  // real, which is the one thing that was never in doubt by the time somebody
  // presses this. What breaks is the redirect URI and the groups claim, and
  // both need what the deployment has actually seen to answer.
  const verify = async () => {
    setError(null)
    try {
      setDoctor(await post('/api/v1/identity/doctor'))
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  const isMapped = (group: string) =>
    groups.some((g) => g.group.toLowerCase() === group.toLowerCase())

  const commit = (draft: { at: number | null; group: string; role: Role }) => {
    const group = draft.group.trim()
    setGroups((current) =>
      draft.at === null
        ? [...current, { group, role: draft.role }]
        : current.map((g, i) => (i === draft.at ? { group, role: draft.role } : g)),
    )
    setPicked(null)
    setEditing(null)
  }

  // Hidden only in a setup session, and only while there is nothing to map.
  const mappingHidden = setup && (data.observed_groups ?? []).length === 0

  return (
    <SsoFrame
      setup={setup}
      sub="The connection is configuration. The group mapping is yours, and lives here."
      actions={<ActionButton onAct={verify} label="Verify" settled="Reachable" small />}
    >
      <ErrorBanner error={error} />

      {doctor && (
        <div className="ssofindings">
          <Findings findings={doctor.findings} />
        </div>
      )}

      <div className="facts">
        <Fact label="Issuer" value={data.issuer} />
        <Fact label="Client id" value={data.client_id} />
        <Fact label="Redirect URI" value={data.redirect_url} />
        <Fact label="Groups claim" value={data.groups_claim} />
      </div>

      {mappingHidden ? (
        <div className="callout" style={{ marginTop: 30 }}>
          <div className="icon-tile">
            <Icon name="identity" />
          </div>
          <div className="prose tight">
            Group-to-role mapping comes after the first sign-in, not here. Nothing yet knows what
            your provider calls its groups - they are only knowable from a token - so the mapping
            screen fills itself in once somebody has signed in. The first person through owns this
            deployment and can do it from Settings.
          </div>
        </div>
      ) : (
        <>
          <ObservedGroups
            identity={data}
            picked={picked}
            isMapped={isMapped}
            onPick={(group) => setPicked((current) => (current === group ? null : group))}
          />

          <div className="ssoblock-part">
            {/* The control sits on the table's own label line rather than
                under the rows, so that it reads as belonging to the table in
                both states - with nothing mapped there are no rows for it to
                sit under, and a button floating under an empty frame is a
                button about nothing in particular. */}
            <div className="maps-head">
              <span className="fieldlabel">Group to role</span>
              <button
                type="button"
                className="btn"
                onClick={() => setEditing({ at: null, group: picked ?? '', role: 'developer' })}
              >
                Add a mapping
              </button>
            </div>

            {/* THE HEAD STAYS UP WHEN THE TABLE IS EMPTY. What the columns are
                is part of the answer to "what would a mapping even be", which
                is exactly the question somebody looking at an empty one has. */}
            <div className="maptable">
              <div className="dtable-head" style={MAP_HEAD}>
                <span>Group</span>
                <span>Role</span>
                <span />
              </div>

              {groups.length === 0 ? (
                <Empty title="No group is mapped">
                  Everybody who signs in takes the role below, whatever groups they are in. Map one
                  to give its members a different one.
                </Empty>
              ) : (
                groups.map((entry, index) => (
                  <div key={index} className="maprow" style={MAP_ROW}>
                    <button
                      type="button"
                      className="maprow-open"
                      style={MAP_CELLS}
                      onClick={() => setEditing({ at: index, group: entry.group, role: entry.role })}
                    >
                      <span className="maprow-group">{entry.group}</span>
                      <span className="maprow-role">{entry.role}</span>
                    </button>
                    <button
                      type="button"
                      className="maprow-remove"
                      aria-label={`Remove ${entry.group}`}
                      onClick={() => setGroups((current) => current.filter((_, i) => i !== index))}
                    >
                      ×
                    </button>
                  </div>
                ))
              )}
            </div>
          </div>

          <div className="field ssofallback">
            <label htmlFor="default-role">Someone in no mapped group</label>
            <Select
              id="default-role"
              value={defaultRole}
              label="Someone in no mapped group"
              onChange={setDefaultRole}
              options={data.default_role_options.map((option) => ({
                value: option,
                label: fallbackLabel(option),
              }))}
            />
            <span className="setnote">
              The strongest matching group wins, so a role never depends on the order your provider
              happened to list someone&rsquo;s groups. Choosing &ldquo;cannot sign in&rdquo; makes
              the mapping a strict allowlist.
            </span>
          </div>

          <div className="ssosave">
            <ActionButton onAct={save} label="Save mapping" settled="Saved" primary />
            {/* Standing, not a confirmation. When the mapping takes effect is
                true before it is saved as well as after, and it is the part an
                administrator most needs before pressing the button - somebody
                watching for a colleague's access to change in the next few
                seconds is watching for something that will not happen. */}
            <span className="setnote">
              Roles are recomputed at each person&rsquo;s next sign-in, not now.
            </span>
          </div>
        </>
      )}

      {editing && (
        <MappingDrawer
          draft={editing}
          observed={(data.observed_groups ?? []).map((g) => g.group)}
          taken={groups
            .filter((_, i) => i !== editing.at)
            .map((g) => g.group.trim().toLowerCase())}
          onEdit={(patchDraft) => setEditing((current) => (current ? { ...current, ...patchDraft } : current))}
          onCommit={() => commit(editing)}
          onClose={() => setEditing(null)}
        />
      )}
    </SsoFrame>
  )
}

/** fallbackLabel reads an option as the sentence it completes. The role names
 *  are the product's own - a `manager` here and a "Manager" on People & teams,
 *  rather than two words for one grade. */
function fallbackLabel(option: string): string {
  switch (option) {
    case 'none':
      return 'Cannot sign in'
    case 'admin':
      return 'Becomes an admin'
    default:
      return `Becomes a ${option}`
  }
}

/**
 * One group, one role, staged in a panel.
 *
 * Not two controls on every row of the list (a text box and a select), which
 * would make the list a form and put a free-text field in front of somebody
 * whose provider's group names they have no way to spell. The list states what
 * is mapped; this is where one is decided.
 */
function MappingDrawer({
  draft,
  observed,
  taken,
  onEdit,
  onCommit,
  onClose,
}: {
  draft: { at: number | null; group: string; role: Role }
  observed: string[]
  /** The groups the other rows already hold, lower-cased. Mapping one group
   *  twice is two answers to one question, and the strongest would silently
   *  win. */
  taken: string[]
  onEdit: (patch: Partial<{ group: string; role: Role }>) => void
  onCommit: () => void
  onClose: () => void
}) {
  const group = draft.group.trim()
  const duplicate = group !== '' && taken.includes(group.toLowerCase())

  return (
    <Drawer
      open
      title={draft.at === null ? 'Add a mapping' : 'Group mapping'}
      sub="A group at your identity provider, and what it grants here."
      onClose={onClose}
      onSubmit={onCommit}
      footer={
        <>
          <span />
          <button type="button" className="btn primary" disabled={group === '' || duplicate} onClick={onCommit}>
            {draft.at === null ? 'Add mapping' : 'Save mapping'}
          </button>
        </>
      }
    >
      <div className="field">
        <label htmlFor="map-group">Group</label>
        <input
          id="map-group"
          className="input"
          value={draft.group}
          spellCheck={false}
          list="observed-groups"
          placeholder="the name, path or id your provider sends"
          onChange={(e) => onEdit({ group: e.target.value })}
        />
        {/* Typing is still allowed, because a group nobody has signed in from
            yet is a legitimate thing to map ahead of time. */}
        <datalist id="observed-groups">
          {observed.map((g) => (
            <option key={g} value={g} />
          ))}
        </datalist>
        {duplicate && (
          <span className="setnote warn">
            That group is already mapped. Change the existing row instead.
          </span>
        )}
      </div>

      <div className="field">
        <label>Role</label>
        <Choice
          value={draft.role}
          options={ROLES}
          label="Role this group grants"
          onChange={(role) => onEdit({ role })}
        />
        <span className="setnote">{ROLES.find((r) => r.id === draft.role)?.help}</span>
      </div>
    </Drawer>
  )
}

/**
 * DirectAdmins is the exception to the group mapping, kept beside the mapping.
 *
 * Every role comes from the provider's groups, recomputed at each sign-in -
 * that is the mechanism by which removing somebody from a group removes their
 * access, and it has to apply to everybody or it applies to nobody. But it
 * cannot grant the first administrator of a fresh deployment, because the
 * mapping is written in terms of group names nothing has seen yet; and it
 * cannot help a deployment whose provider emits no groups at all.
 *
 * So an account may be pinned to administrator by hand, and the mapping leaves
 * it alone. THE LIST LIVES HERE RATHER THAN ON EACH PERSON, which is the whole
 * of this screen's argument: a rule with invisible exceptions is a rule nobody
 * can audit. An administrator reading the mapping above could not otherwise
 * tell that four accounts bypass all of it without opening four drawers on a
 * different screen.
 *
 * Administrator only. Pinning any other role would let the console hold a
 * person at a grade the provider has revoked, which is a silent divergence for
 * everyone except the account that owns the deployment - and that one is worth
 * it, because the alternative is nobody who can fix anything.
 */
function DirectAdmins() {
  const me = useSession().session!.user
  const identity = useApi<Identity>('/api/v1/identity')
  const people = useApi<{ users: PersonRow[] }>('/api/v1/users')
  const teams = useApi<{ teams: TeamRow[] }>('/api/v1/teams')
  const { say, toast } = useToast()

  const [picking, setPicking] = useState(false)
  const [error, setError] = useState<unknown>(null)

  const users = people.data?.users ?? []
  // Everyone whose role the mapping will not revise, whatever that role is.
  // Only administrator can be pinned now, but a deployment upgraded from when
  // any role could be is not a reason to hide the ones it already holds - an
  // exception nobody can see is the thing this panel exists to prevent.
  const pinned = users.filter((person) => person.role_source === 'manual')
  const mapsAdmin =
    (identity.data?.groups ?? []).some((entry) => entry.role === 'admin') ||
    identity.data?.default_role === 'admin'

  const unpin = async (person: PersonRow) => {
    setError(null)
    try {
      await patch(`/api/v1/users/${person.id}`, { role_source: 'sso' })
      people.reload()
      say(
        `${person.display_name || person.email} follows the group mapping again, from their next sign-in.`,
      )
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  const apply = async (next: string[]) => {
    setError(null)
    const before = pinned.map((person) => person.id)
    try {
      for (const id of next.filter((id) => !before.includes(id))) {
        await patch(`/api/v1/users/${id}`, { role: 'admin' })
      }
      for (const id of before.filter((id) => !next.includes(id))) {
        await patch(`/api/v1/users/${id}`, { role_source: 'sso' })
      }
      people.reload()
      say(
        `${count(next.length)} ${next.length === 1 ? 'account is' : 'accounts are'} administrators regardless of their groups.`,
      )
    } catch (cause) {
      setError(cause)
    } finally {
      setPicking(false)
    }
  }

  return (
    <section className="band last">
      <div className="ssoblock">
        <div className="band-head-row top">
          <div className="band-head lead">
            <span className="band-title">Direct administrators</span>
            <span className="band-sub">
              Accounts that hold their role here, not from a group. Everyone else is decided by the
              mapping above.
            </span>
          </div>
          <button type="button" className="btn" onClick={() => setPicking(true)}>
            Choose
          </button>
        </div>

        <ErrorBanner error={error} />

        {/* The failure this panel is most likely to be looking at: a mapping
            that grants admin to nobody, which makes this list the only thing
            standing between the deployment and having no administrator at all.
            Stated rather than enforced - it is a legitimate configuration, and
            it is also what somebody who has not written the mapping yet has. */}
        {!identity.loading && !mapsAdmin && (
          <div className="notice warn" style={{ marginBottom: 24 }}>
            <div className="notice-title">No group grants administrator</div>
            <div className="notice-body">
              This list is the only thing granting administrator access. Handing back the last
              account on it is refused, because the next sign-in would leave nobody who can
              administer this deployment.
            </div>
          </div>
        )}

        {people.loading ? (
          <Loading rows={2} />
        ) : pinned.length === 0 ? (
          <div className="check-empty">
            Nobody. Every role comes from the group mapping, which is how it should read once the
            mapping grants administrator to a group.
          </div>
        ) : (
          <div className="pinlist">
            {pinned.map((person) => (
              <div key={person.id} className="drawer-row">
                <Avatar name={person.display_name || person.email} size="sm" />
                <span className="drawer-row-text">
                  <span className="name truncate">{person.display_name || person.email}</span>
                  <span className="sub">
                    {person.role}
                    {person.id === me.id ? ' · you' : ''}
                  </span>
                </span>
                <ActionButton
                  small
                  onAct={() => unpin(person)}
                  label="Hand back to SSO"
                  settled="Handed back"
                />
              </div>
            ))}
          </div>
        )}

        {/* Standing, not a confirmation, for the same reason the mapping's is:
            when a hand-back takes effect is true before the button as well as
            after, and somebody watching for a colleague's access to drop in the
            next few seconds is watching for something that will not happen. */}
        <span className="setnote pinnote">
          Handing an account back does not change its role now - group membership is your
          provider&rsquo;s record and Panorama keeps no copy. The next sign-in decides.
        </span>
      </div>

      {picking && (
        <PeoplePicker
          title="Direct administrators"
          sub="Checked accounts are administrators regardless of their groups. Unchecking hands them back to the mapping."
          users={users}
          teams={teams.data?.teams ?? []}
          selected={pinned.map((person) => person.id)}
          confirmLabel="Save"
          onClose={() => setPicking(false)}
          onApply={apply}
        />
      )}

      {toast}
    </section>
  )
}

/**
 * ObservedGroups shows what the identity provider has actually sent.
 *
 * The mapping asks for a group name, and until this existed nothing on the
 * screen could tell an administrator what theirs are called. Providers
 * disagree: Entra emits object IDs rather than names, Keycloak can emit a full
 * group path, Auth0 emits nothing at all unless an Action adds a namespaced
 * claim. A wrong guess does not fail - it grants nothing, quietly.
 *
 * Only group names are held, never who was in one. That membership is the
 * provider's record, and a second copy of it is not something Panorama needs.
 */
function ObservedGroups({
  identity,
  picked,
  isMapped,
  onPick,
}: {
  identity: Identity
  picked: string | null
  isMapped: (group: string) => boolean
  onPick: (group: string) => void
}) {
  const seen = identity.observed_groups ?? []

  if (identity.sign_ins === 0) {
    return (
      <div className="callout ssocallout">
        <div className="icon-tile">
          <Icon name="identity" />
        </div>
        <div className="prose tight">
          Nobody has signed in through single sign-on yet, so nothing here knows what your provider
          calls its groups. Sign in once and they appear - a name typed before then is a guess, and
          a wrong one grants nothing rather than failing.
        </div>
      </div>
    )
  }

  if (seen.length === 0) {
    return (
      <div className="callout warn ssocallout">
        <div className="icon-tile">
          <Icon name="alert" />
        </div>
        <div className="prose tight">
          {identity.sign_ins} sign-in{identity.sign_ins === 1 ? '' : 's'} completed and not one
          carried a <span className="mono">{identity.groups_claim}</span> claim, so every one of
          them took the default role. Either that is not what your provider calls the claim, or it
          is not emitting it for this application - most require it to be switched on per
          application, and requesting the scope is not enough on its own.
        </div>
      </div>
    )
  }

  return (
    <div className="ssoblock-part">
      <div className="ssolabelled">
        <span className="fieldlabel">Groups your provider has sent</span>
        <span className="band-sub">
          From {identity.sign_ins} sign-in{identity.sign_ins === 1 ? '' : 's'} through{' '}
          <span className="mono">{identity.groups_claim}</span>
          {identity.sign_ins_without_groups > 0 &&
            `, ${identity.sign_ins_without_groups} of which carried none`}
          . Click one to map it.
        </span>
      </div>
      <div className="gchips">
        {seen.map((g) => {
          const mapped = isMapped(g.group)
          return (
            <button
              key={g.group}
              type="button"
              className={mapped ? 'gchip mapped' : picked === g.group ? 'gchip on' : 'gchip'}
              aria-pressed={picked === g.group}
              disabled={mapped}
              title={
                `${g.group} - seen in ${g.count} sign-in${g.count === 1 ? '' : 's'}` +
                (mapped ? ', already in the mapping below' : '')
              }
              onClick={() => onPick(g.group)}
            >
              {mapped && (
                <span className="inline-check" aria-hidden>
                  <Icon name="check" />
                </span>
              )}
              {g.group}
            </button>
          )
        })}
      </div>
    </div>
  )
}
