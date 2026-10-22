// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState } from 'react'
import { post, put } from '../api'
import { ago, dateTime } from '../format'
import type {
  ModelAccess,
  ModelAPI,
  ProviderCheck,
  ProviderCheckAttempt,
  ProviderRefusal,
  ProviderOptions,
  ProviderPreflight,
  ProviderRow,
  ProviderSaved,
  ProvidersData,
} from '../types'
import {
  ActionButton,
  CopyBlock,
  ErrorBanner,
  Loading,
  Pill,
  Segmented,
  Select,
  SubmitButton,
  useAction,
  useApi,
} from '../ui'

// Where this deployment invokes models.
//
// A band per provider, in the shape Integrations already taught this screen:
// the rail says what the provider is and whether it works, the wide column is
// what an administrator does about it. A provider is a region and a switch,
// stored in the database - the credentials are the deployment's own and never
// cross this screen, which is why there is no field for one.
//
// The same blocks render inside the setup step as well as on Settings, framed
// as a card's contents there, because a setup session is exactly who needs to
// connect a provider and it may not open Settings.

const SWITCH: { id: 'on' | 'off'; label: string }[] = [
  { id: 'on', label: 'Serving' },
  { id: 'off', label: 'Off' },
]

const BETA_SOURCES: { id: 'built-in' | 'mine'; label: string }[] = [
  { id: 'built-in', label: "This build's list" },
  { id: 'mine', label: 'My list' },
]

const API_NAMES: ModelAPI[] = ['anthropic-messages', 'openai-chat', 'openai-responses']

export function ModelProviders({ setup = false }: { setup?: boolean } = {}) {
  const data = useApi<ProvidersData>('/api/v1/providers')

  if (data.loading) {
    return setup ? (
      <Loading rows={3} />
    ) : (
      <section className="band last">
        <Loading rows={4} />
      </section>
    )
  }

  if (!data.data) {
    return setup ? (
      <ErrorBanner error={data.error} />
    ) : (
      <section className="band bar last">
        <ErrorBanner error={data.error} />
      </section>
    )
  }

  const { providers, iam_policy: policy, can_configure: canConfigure } = data.data

  if (setup) {
    return (
      <div className="pstack">
        {providers.map((provider) => (
          <div key={provider.kind} className="pblock">
            <div className="pblock-head">
              <span className="irail-name">{provider.label}</span>
              <StatePill provider={provider} />
            </div>
            <p className="irail-unlocks">{provider.summary}</p>
            <ProviderBody provider={provider} canConfigure={canConfigure} onChanged={data.reload} />
          </div>
        ))}
        <div className="pblock">
          <div className="pblock-head">
            <span className="irail-name">Role policy</span>
          </div>
          <RolePolicy policy={policy} />
        </div>
      </div>
    )
  }

  return (
    <>
      {providers.map((provider) => (
        <section key={provider.kind} className="band iband">
          <div className="irail">
            <div className="irail-head">
              <span className="irail-name">{provider.label}</span>
              <StatePill provider={provider} />
            </div>
            <span className="irail-rule" />
            <p className="irail-unlocks">{provider.summary}</p>
            {provider.state === 'never checked' && (
              <p className="irail-degraded">
                Configured and not yet proven. A check is one real request, and it is the only
                thing that tells a working role from one that looks right.
              </p>
            )}
          </div>
          <div className="imain">
            <ProviderBody provider={provider} canConfigure={canConfigure} onChanged={data.reload} />
          </div>
        </section>
      ))}

      <section className="band iband last">
        <div className="irail">
          <div className="irail-head">
            <span className="irail-name">Role policy</span>
          </div>
          <span className="irail-rule" />
          <p className="irail-unlocks">
            One policy for every provider on this screen, in every region, so choosing or changing a
            region never needs an IAM change.
          </p>
        </div>
        <div className="imain">
          <RolePolicy policy={policy} />
        </div>
      </section>
    </>
  )
}

/** The word beside the name. Five rather than two, for the reason the
 *  integrations have four: "failing" and "never checked" are both not working,
 *  and an operator reaching for the logs needs to know which. */
function StatePill({ provider }: { provider: ProviderRow }) {
  switch (provider.state) {
    case 'connected':
      return <Pill tone="good">connected</Pill>
    case 'failing':
      return <Pill tone="warn">failing</Pill>
    case 'never checked':
      return <Pill>never checked</Pill>
    default:
      return <Pill tone="off">{provider.state}</Pill>
  }
}

function ProviderBody({
  provider,
  canConfigure,
  onChanged,
}: {
  provider: ProviderRow
  canConfigure: boolean
  onChanged: () => void
}) {
  return (
    <div className="istack">
      {provider.state === 'failing' && (
        <div className="notice warn">
          <div className="notice-title">This provider could not be built: {provider.error}</div>
          {provider.hint && <div className="notice-body">{provider.hint}</div>}
        </div>
      )}

      {provider.last_refusal && <LastRefusal refusal={provider.last_refusal} />}

      {canConfigure ? (
        <ProviderForm provider={provider} onSaved={onChanged} />
      ) : (
        <p className="inote">An administrator configures model providers.</p>
      )}

      {provider.configured && <ProviderFacts provider={provider} />}

      {provider.configured && provider.enabled && canConfigure && (
        <ProviderCheckRow provider={provider} onChecked={onChanged} />
      )}
    </div>
  )
}

/**
 * The configuration an administrator owns: a region, whether it serves, and the
 * three tables this build keeps about models that an operator may need to
 * correct before a release does.
 *
 * Choosing a region asks it what it offers before anything is saved, because
 * the answer decides whether the save is safe: a region that does not list a
 * route's model leaves that route admitting requests it cannot serve.
 */
function ProviderForm({ provider, onSaved }: { provider: ProviderRow; onSaved: () => void }) {
  const [region, setRegion] = useState(provider.region ?? '')
  const [serving, setServing] = useState<'on' | 'off'>(provider.enabled === false ? 'off' : 'on')
  const [preflight, setPreflight] = useState<ProviderPreflight | null>(null)
  const [preflighting, setPreflighting] = useState(false)
  const [saved, setSaved] = useState<ProviderSaved | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [advanced, setAdvanced] = useState(false)
  const [options, setOptions] = useState(() => draftOptions(provider.options))
  const [optionsTouched, setOptionsTouched] = useState(false)

  const regionChanged = region !== '' && region !== (provider.region ?? '')

  // Asked when the region changes and not on every render, and a stale answer
  // is dropped rather than shown against a region nobody is looking at.
  useEffect(() => {
    if (!regionChanged) {
      setPreflight(null)
      return
    }
    let live = true
    setPreflighting(true)
    setPreflight(null)
    post<ProviderPreflight>(`/api/v1/providers/${provider.kind}/preflight`, { region })
      .then((answer) => {
        if (live) setPreflight(answer)
      })
      .catch((cause: unknown) => {
        if (live) setError(cause)
      })
      .finally(() => {
        if (live) setPreflighting(false)
      })
    return () => {
      live = false
    }
  }, [provider.kind, region, regionChanged])

  const save = useAction(async () => {
    setError(null)
    setSaved(null)
    const body: { region: string; enabled?: boolean; options?: ProviderOptions } = { region }
    if (provider.configured) body.enabled = serving === 'on'
    if (optionsTouched) {
      const parsed = parseOptions(options)
      if (typeof parsed === 'string') {
        const refusal = new Error(parsed)
        setError(refusal)
        throw refusal
      }
      body.options = parsed
    }
    try {
      const outcome = await put<ProviderSaved>(`/api/v1/providers/${provider.kind}`, body)
      setSaved(outcome)
      setOptionsTouched(false)
      onSaved()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  })

  const editOptions = (patch: Partial<OptionsDraft>) => {
    setOptions((current) => ({ ...current, ...patch }))
    setOptionsTouched(true)
  }

  const unchanged =
    region === (provider.region ?? '') &&
    (!provider.configured || (serving === 'on') === provider.enabled) &&
    !optionsTouched

  return (
    <form
      className="iform"
      onSubmit={(e) => {
        e.preventDefault()
        void save.act()
      }}
    >
      <ErrorBanner error={error} />

      <div className="ifields">
        <div className="field">
          <label htmlFor={`${provider.kind}-region`}>Region</label>
          <Select
            id={`${provider.kind}-region`}
            value={region}
            label="Region"
            onChange={setRegion}
            options={[
              ...(region === '' ? [{ value: '', label: 'Choose a region' }] : []),
              // A stored region this build does not list is still the stored
              // region, and a select that could not show it would silently
              // offer to change it.
              ...(provider.region && !provider.regions.includes(provider.region)
                ? [{ value: provider.region, label: provider.region }]
                : []),
              ...provider.regions.map((r) => ({ value: r, label: r })),
            ]}
          />
        </div>

        {provider.configured && (
          <div className="field">
            <span className="fieldlabel">Routes may use it</span>
            <Segmented value={serving} options={SWITCH} label="Routes may use it" onChange={setServing} />
          </div>
        )}

        {regionChanged && (
          <div className="notice wide">
            {preflighting ? (
              <div className="notice-title">Asking {region} what it offers…</div>
            ) : preflight && !preflight.ok ? (
              <>
                <div className="notice-title">{region} could not be read: {preflight.error}</div>
                {preflight.hint && <div className="notice-body">{preflight.hint}</div>}
              </>
            ) : preflight ? (
              <>
                <div className="notice-title">
                  {region} lists {preflight.models} models, {preflight.serveable} of which this gateway
                  can serve.
                </div>
                <div className="notice-body">
                  {(preflight.stranded ?? []).length > 0
                    ? `Saving leaves ${(preflight.stranded ?? []).join(', ')} pointing at a model this region does not list. Those routes refuse every request until they are pointed at one it does.`
                    : provider.routes.length > 0
                      ? 'Every route on this provider points at a model this region lists. Nothing has been saved yet.'
                      : 'No route uses this provider yet. Nothing has been saved.'}
                </div>
              </>
            ) : null}
          </div>
        )}

        {/* A row of the form with its own label rather than a loose button:
            the tables are a correction made rarely, and a bare control between
            the region and Save read as a step somebody had to take. */}
        <div className="field wide">
          <span className="fieldlabel">Model tables</span>
          <span className="ifield-note">
            Beta flags to drop, context windows and APIs, for when this build&rsquo;s tables are
            behind the provider.{' '}
            <button
              type="button"
              className="btn link"
              aria-expanded={advanced}
              onClick={() => setAdvanced((open) => !open)}
            >
              {advanced ? 'Hide' : optionsTouched || hasOptions(provider.options) ? 'Edit' : 'Correct one'}
            </button>
          </span>
        </div>
      </div>

      {advanced && (
        <OptionsFields
          kind={provider.kind}
          draft={options}
          onEdit={editOptions}
        />
      )}

      <div className="iact">
        <SubmitButton
          state={save.state}
          disabled={region === '' || unchanged}
          label={provider.configured ? 'Save' : 'Turn on'}
          settled="Saved"
          primary
        />
        {saved && <span className="iresult">{savedLine(saved)}</span>}
      </div>

      {saved?.discovery_error && (
        <div className="notice warn" style={{ marginTop: 22 }}>
          <div className="notice-title">Saved, and the catalogue could not be read: {saved.discovery_error}</div>
          {saved.hint && <div className="notice-body">{saved.hint}</div>}
        </div>
      )}
      {saved && saved.stranded.length > 0 && (
        <div className="notice warn" style={{ marginTop: 22 }}>
          <div className="notice-title">
            {saved.stranded.join(', ')} {saved.stranded.length === 1 ? 'points' : 'point'} at a model{' '}
            {saved.region} does not list
          </div>
          <div className="notice-body">
            Every request for {saved.stranded.length === 1 ? 'it' : 'them'} is refused until it is
            pointed at a model this region offers, on Routing.
          </div>
        </div>
      )}
    </form>
  )
}

function savedLine(saved: ProviderSaved): string {
  if (!saved.enabled) return `Saved. Off - no route may use it.`
  if (saved.discovery_error) return 'Saved.'
  return `Saved. ${saved.discovered} models discovered in ${saved.region}.`
}

/** What is stored, read as facts. The verification is the one worth reading
 *  first: it says when a real request last worked, and against which model. */
function ProviderFacts({ provider }: { provider: ProviderRow }) {
  return (
    <div className="ifields">
      <Fact
        label="Last verified"
        value={
          provider.verified_at
            ? `${dateTime(provider.verified_at)}${provider.verified_model ? ` · ${provider.verified_model}` : ''}`
            : 'never, in this region'
        }
      />
      <Fact
        label="Catalogue"
        value={
          provider.catalogue.discovered_at
            ? `${provider.catalogue.models} models, ${provider.catalogue.serveable} servable · read ${ago(provider.catalogue.discovered_at)}`
            : 'not read yet'
        }
      />
      <Fact
        label="Routes on it"
        value={provider.routes.length > 0 ? provider.routes.join(', ') : 'none'}
      />
      {provider.endpoint && <Fact label="Endpoint" value={provider.endpoint} />}
    </div>
  )
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div className="ifact">
      <span className="ifact-label">{label}</span>
      <span className="ifact-value">{value}</span>
    </div>
  )
}

/**
 * The live check, which performs a real invocation and says so.
 *
 * A refused invocation comes back as a 200 carrying ok:false, so the request
 * succeeding is not the verification succeeding. Throwing on ok:false is what
 * stops the button reporting "Verified" over a line saying the model could not
 * be invoked.
 */
function ProviderCheckRow({ provider, onChecked }: { provider: ProviderRow; onChecked: () => void }) {
  const [result, setResult] = useState<ProviderCheck | null>(null)
  const [error, setError] = useState<unknown>(null)

  const check = async () => {
    setError(null)
    setResult(null)
    let outcome: ProviderCheck
    try {
      outcome = await post<ProviderCheck>(`/api/v1/providers/${provider.kind}/check`, {})
    } catch (cause) {
      setError(cause)
      throw cause
    }
    setResult(outcome)
    onChecked()
    if (!outcome.ok) throw new Error(outcome.error ?? 'invocation failed')
  }

  return (
    <div>
      <ErrorBanner error={error} />
      {result?.refused?.map((attempt) => (
        <CheckRefusal key={attempt.target} attempt={attempt} />
      ))}
      {result && !result.ok && (
        <CheckRefusal
          attempt={{
            target: result.target,
            route: result.route,
            error: result.error ?? '',
            hint: result.hint ?? '',
            access: result.access,
          }}
        />
      )}
      <div className="iact">
        <ActionButton onAct={check} label="Check" settled="Verified" />
        <span className="iresult">
          {result?.ok
            ? `Invoked ${result.target}${routeNote(result.route)} in ${result.region} in ${result.duration_ms} ms, streamed.`
            : 'One real request, streamed, one token of output: a route’s model first, otherwise the cheapest model this gateway can serve here.'}
        </span>
      </div>
    </div>
  )
}

/** One model a check could not invoke, in the provider's words, with the fix
 *  and what Bedrock reports about the model beside it. */
function CheckRefusal({ attempt }: { attempt: ProviderCheckAttempt }) {
  return (
    <div className="notice warn" style={{ marginBottom: 18 }}>
      <div className="notice-title">
        {attempt.target ? `${attempt.target}${routeNote(attempt.route)} could not be invoked: ` : ''}
        {attempt.error}
      </div>
      {attempt.hint && <div className="notice-body">{attempt.hint}</div>}
      {attempt.access && <div className="notice-body">{accessLine(attempt.access)}</div>}
    </div>
  )
}

/**
 * The latest request this provider refused, which an engineer was told to ask
 * an administrator about. Bedrock's own words are the point: "not found" covers
 * an unsubmitted use case form and a retired model alike, and only the
 * sentence AWS wrote says which.
 */
function LastRefusal({ refusal }: { refusal: ProviderRefusal }) {
  return (
    <div className="notice warn">
      <div className="notice-title">
        Last refused request, {dateTime(refusal.at)} · {refusal.model}: {refusal.error}
      </div>
      {refusal.hint && <div className="notice-body">{refusal.hint}</div>}
      <div className="notice-body">
        Request {refusal.request_id || 'without an id'} in {refusal.region}. A check that invokes this
        model clears this.
      </div>
    </div>
  )
}

function routeNote(route?: string): string {
  return route ? ` (route ${route})` : ''
}

/** Bedrock's availability report, read as a sentence. The agreement reads
 *  NOT_AVAILABLE until a model is first invoked, so it is reported and not
 *  interpreted. */
function accessLine(access: ModelAccess): string {
  const base = `Bedrock reports for ${access.model}: agreement ${access.agreement || '-'}, authorization ${access.authorization || '-'}, entitlement ${access.entitlement || '-'}, region ${access.region || '-'}.`
  return access.agreement_error ? `${base} The agreement says: ${access.agreement_error}` : base
}

/** The role policy, folded. It is sixty lines that somebody reads once, while
 *  attaching it, and left open it would be the longest thing on the screen by a
 *  factor of three. */
function RolePolicy({ policy }: { policy: string }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="stack" style={{ gap: 14 }}>
      <p className="inote" style={{ marginBottom: 0 }}>
        The deployment signs every request with its own credentials - a task role, an instance role,
        or whatever the AWS SDK&rsquo;s default chain finds - so this screen never asks for a key.
        The CloudFormation template grants this already; anywhere else, attach it to that role.{' '}
        <span className="mono">panorama providers policy</span> prints the same document.
      </p>
      <div>
        <button type="button" className="btn" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
          {open ? 'Hide the policy' : 'Show the policy'}
        </button>
      </div>
      {open && <CopyBlock label="iam policy" text={policy} />}
    </div>
  )
}

/* ------------------------------------------------------------ model tables --
 *
 * Three corrections an operator can make without waiting for a release: which
 * beta flags a provider refuses, how much context a model takes, and which API a
 * model speaks. Each is a table in this build, and each has been wrong the day a
 * provider changed something.
 */

type OptionsDraft = {
  betaSource: 'built-in' | 'mine'
  betas: string
  windows: string
  apis: string
}

/** Whether any correction is stored, which is what the row's link offers to
 *  edit rather than to make. */
function hasOptions(options?: ProviderOptions): boolean {
  return (
    (options?.unsupported_betas ?? null) !== null ||
    Object.keys(options?.context_windows ?? {}).length > 0 ||
    Object.keys(options?.apis ?? {}).length > 0
  )
}

function draftOptions(options?: ProviderOptions): OptionsDraft {
  const betas = options?.unsupported_betas
  return {
    betaSource: betas === undefined || betas === null ? 'built-in' : 'mine',
    betas: (betas ?? []).join('\n'),
    windows: Object.entries(options?.context_windows ?? {})
      .map(([family, tokens]) => `${family} = ${tokens}`)
      .join('\n'),
    apis: Object.entries(options?.apis ?? {})
      .map(([family, list]) => `${family} = ${list.join(', ')}`)
      .join('\n'),
  }
}

/** Reads the three fields into the options the server stores, or says which
 *  line it could not read. The server validates again; this is so the answer
 *  arrives before the save rather than as a refusal of it. */
function parseOptions(draft: OptionsDraft): ProviderOptions | string {
  const out: ProviderOptions = {}
  if (draft.betaSource === 'mine') {
    out.unsupported_betas = draft.betas
      .split(/[\s,]+/)
      .map((flag) => flag.trim())
      .filter(Boolean)
  }

  const windows: Record<string, number> = {}
  for (const line of lines(draft.windows)) {
    const [family, value] = pair(line)
    const tokens = Number(value)
    if (!family || !Number.isInteger(tokens) || tokens < 0) {
      return `Context windows: "${line}" is not a model family and a number of tokens.`
    }
    windows[family] = tokens
  }
  if (Object.keys(windows).length > 0) out.context_windows = windows

  const apis: Record<string, ModelAPI[]> = {}
  for (const line of lines(draft.apis)) {
    const [family, value] = pair(line)
    const list = value
      .split(',')
      .map((api) => api.trim())
      .filter(Boolean)
    const unknown = list.find((api) => !API_NAMES.includes(api as ModelAPI))
    if (!family || unknown) {
      return `APIs: "${line}" names ${unknown ? `"${unknown}", which is not one of ${API_NAMES.join(', ')}` : 'no model family'}.`
    }
    apis[family] = list as ModelAPI[]
  }
  if (Object.keys(apis).length > 0) out.apis = apis

  return out
}

function lines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
}

function pair(line: string): [string, string] {
  const at = line.indexOf('=')
  if (at < 0) return [line.trim(), '']
  return [line.slice(0, at).trim(), line.slice(at + 1).trim()]
}

function OptionsFields({
  kind,
  draft,
  onEdit,
}: {
  kind: string
  draft: OptionsDraft
  onEdit: (patch: Partial<OptionsDraft>) => void
}) {
  return (
    <div className="ifields" style={{ marginTop: 22 }}>
      <div className="field wide">
        <span className="fieldlabel">Beta flags to drop</span>
        <Segmented
          value={draft.betaSource}
          options={BETA_SOURCES}
          label="Beta flags to drop"
          onChange={(betaSource) => onEdit({ betaSource })}
        />
        {draft.betaSource === 'mine' ? (
          <textarea
            id={`${kind}-betas`}
            className="input"
            rows={3}
            spellCheck={false}
            value={draft.betas}
            placeholder="one flag per line - empty forwards every flag"
            onChange={(e) => onEdit({ betas: e.target.value })}
          />
        ) : null}
        <span className="ifield-note">
          A Claude request carrying a flag the provider refuses fails whole, so these are removed
          first and each removal is logged. An empty list of your own forwards everything, which is
          how you find out what the provider has started accepting.
        </span>
      </div>

      <div className="field wide">
        <label htmlFor={`${kind}-windows`}>Context windows</label>
        <textarea
          id={`${kind}-windows`}
          className="input"
          rows={3}
          spellCheck={false}
          value={draft.windows}
          placeholder="anthropic.claude-sonnet-5 = 1000000"
          onChange={(e) => onEdit({ windows: e.target.value })}
        />
        <span className="ifield-note">
          A model family or an exact id, and the tokens it takes. Zero records it as unknown, which
          is what a harness is told rather than a guess.
        </span>
      </div>

      <div className="field wide">
        <label htmlFor={`${kind}-apis`}>APIs</label>
        <textarea
          id={`${kind}-apis`}
          className="input"
          rows={3}
          spellCheck={false}
          value={draft.apis}
          placeholder="qwen.qwen3-coder = openai-chat"
          onChange={(e) => onEdit({ apis: e.target.value })}
        />
        <span className="ifield-note">
          Which of {API_NAMES.join(', ')} a model family speaks. Nothing after the equals sign makes
          it browse-only. Saving reads the catalogue again with the new answer.
        </span>
      </div>
    </div>
  )
}
