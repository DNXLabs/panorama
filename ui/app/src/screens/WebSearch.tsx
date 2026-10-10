// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from 'react'
import { post, put } from '../api'
import { amountForEdit, dateTime, rate } from '../format'
import type { WebSearchData } from '../types'
import {
  ActionButton,
  CopyBlock,
  ErrorBanner,
  Loading,
  Pill,
  Segmented,
  SubmitButton,
  useAction,
  useApi,
} from '../ui'

// Web search, offered to every coding tool by this gateway.
//
// The model providers' shape, because it is the same kind of thing: a
// backend this deployment reaches with its own role, a switch, and a check
// that proves it. The rail says what it is and whether it works; the wide
// column is what an administrator does about it. There is no credential field,
// because the gateway is called with the deployment's role.

const SWITCH: { id: 'on' | 'off'; label: string }[] = [
  { id: 'on', label: 'Serving' },
  { id: 'off', label: 'Off' },
]

export function WebSearch() {
  const data = useApi<WebSearchData>('/api/v1/web-search')

  if (data.loading) {
    return (
      <section className="band last">
        <Loading rows={4} />
      </section>
    )
  }
  if (!data.data) {
    return (
      <section className="band bar last">
        <ErrorBanner error={data.error} />
      </section>
    )
  }
  const search = data.data

  return (
    <>
      <section className="band iband">
        <div className="irail">
          <div className="irail-head">
            <span className="irail-name">Web search</span>
            <StatePill state={search.state} />
          </div>
          <span className="irail-rule" />
          <p className="irail-unlocks">
            A web_search tool for every coding tool and every route, served by this gateway over MCP.
            Searches run on Amazon&rsquo;s own web index through an AgentCore gateway, so a query
            stays inside AWS. Queries and results are not stored; each search is recorded with its
            cost against the person who made it.
          </p>
          {search.state === 'never checked' && (
            <p className="irail-degraded">
              Configured and not yet proven. A check asks the gateway for its tools, which is what
              tells a working role from one that looks right.
            </p>
          )}
        </div>
        <div className="imain">
          <div className="istack">
            {search.state === 'failing' && search.error && (
              <div className="notice warn">
                <div className="notice-title">The gateway could not be reached: {search.error}</div>
              </div>
            )}
            {search.deployed ? (
              <DeployedSwitch search={search} onSaved={data.reload} />
            ) : (
              <SearchForm search={search} onSaved={data.reload} />
            )}
            {search.configured && <SearchFacts search={search} />}
            {search.configured && search.enabled && <SearchCheckRow onChecked={data.reload} />}
          </div>
        </div>
      </section>

      <section className="band iband last">
        <div className="irail">
          <div className="irail-head">
            <span className="irail-name">Role policy</span>
          </div>
          <span className="irail-rule" />
          <p className="irail-unlocks">
            What the deployment&rsquo;s role needs to call a gateway. The search itself is allowed by
            the gateway&rsquo;s own service role, which is created with the gateway.
          </p>
        </div>
        <div className="imain">
          <RolePolicy policy={search.role_policy} />
        </div>
      </section>
    </>
  )
}

/** Five words rather than two, for the reason a model provider has five. */
function StatePill({ state }: { state: WebSearchData['state'] }) {
  switch (state) {
    case 'connected':
      return <Pill tone="good">connected</Pill>
    case 'failing':
      return <Pill tone="warn">failing</Pill>
    case 'never checked':
      return <Pill>never checked</Pill>
    default:
      return <Pill tone="off">{state}</Pill>
  }
}

/**
 * The gateway, whether coding tools may use it, and what a search costs.
 *
 * The price is empty unless somebody has overridden it, and the placeholder is
 * the rate AWS published, so an administrator who changes nothing is charging
 * the published rate and can see which one that is.
 */
function SearchForm({ search, onSaved }: { search: WebSearchData; onSaved: () => void }) {
  const [gateway, setGateway] = useState(search.gateway_url ?? '')
  const [serving, setServing] = useState<'on' | 'off'>(search.enabled === false ? 'off' : 'on')
  const storedPrice = search.price_source === 'config' ? amountForEdit(search.price_per_search) : ''
  const [price, setPrice] = useState(storedPrice)
  const [error, setError] = useState<unknown>(null)
  const [saved, setSaved] = useState<WebSearchData | null>(null)

  const save = useAction(async () => {
    setError(null)
    setSaved(null)
    try {
      const outcome = await put<WebSearchData>('/api/v1/web-search', {
        gateway_url: gateway,
        enabled: search.configured ? serving === 'on' : true,
        price_per_search: price,
      })
      setSaved(outcome)
      onSaved()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  })

  const unchanged =
    gateway.trim() === (search.gateway_url ?? '') &&
    (!search.configured || (serving === 'on') === search.enabled) &&
    price.trim() === storedPrice

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
        <div className="field wide">
          <label htmlFor="web-search-gateway">Gateway URL</label>
          <input
            id="web-search-gateway"
            className="input"
            value={gateway}
            spellCheck={false}
            placeholder="https://<gateway-id>.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp"
            onChange={(e) => setGateway(e.target.value)}
          />
          <span className="ifield-note">
            An AgentCore gateway with IAM inbound authorization and a Web Search Tool target, as the
            AgentCore console or the CloudFormation stack&rsquo;s outputs show it.
          </span>
        </div>

        {search.configured && (
          <div className="field">
            <span className="fieldlabel">Coding tools may use it</span>
            <Segmented value={serving} options={SWITCH} label="Coding tools may use it" onChange={setServing} />
          </div>
        )}

        <div className="field">
          <label htmlFor="web-search-price">Price per search</label>
          <input
            id="web-search-price"
            className="input"
            value={price}
            inputMode="decimal"
            spellCheck={false}
            placeholder={amountForEdit(search.published_price)}
            onChange={(e) => setPrice(e.target.value)}
          />
          <span className="ifield-note">
            Empty charges the rate AWS published, {rate(search.published_price)} a search.
          </span>
        </div>
      </div>

      <div className="iact">
        <SubmitButton
          state={save.state}
          disabled={gateway.trim() === '' || unchanged}
          label={search.configured ? 'Save' : 'Turn on'}
          settled="Saved"
          primary
        />
        {saved && (
          <span className="iresult">
            {!saved.enabled
              ? 'Saved. Off - no coding tool is offered web search.'
              : saved.serving
                ? `Saved. Serving from gateway ${saved.gateway_id} in ${saved.region}.`
                : 'Saved.'}
          </span>
        )}
      </div>
    </form>
  )
}

/**
 * The switch, alone, for a gateway the deployment created. The stack made it
 * and will delete it, so the address and the price are shown below and not
 * offered for editing; turning search off is the one decision left here.
 */
function DeployedSwitch({ search, onSaved }: { search: WebSearchData; onSaved: () => void }) {
  const [serving, setServing] = useState<'on' | 'off'>(search.enabled === false ? 'off' : 'on')
  const [error, setError] = useState<unknown>(null)
  const [saved, setSaved] = useState<WebSearchData | null>(null)

  const save = useAction(async () => {
    setError(null)
    setSaved(null)
    try {
      const outcome = await put<WebSearchData>('/api/v1/web-search', { enabled: serving === 'on' })
      setSaved(outcome)
      onSaved()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  })

  return (
    <form
      className="iform"
      onSubmit={(e) => {
        e.preventDefault()
        void save.act()
      }}
    >
      <ErrorBanner error={error} />
      <p className="inote">
        This deployment created its own search gateway, so it is on from the start and its address
        and price are set by the deployment.
      </p>
      <div className="ifields">
        <div className="field">
          <span className="fieldlabel">Coding tools may use it</span>
          <Segmented value={serving} options={SWITCH} label="Coding tools may use it" onChange={setServing} />
        </div>
      </div>
      <div className="iact">
        <SubmitButton
          state={save.state}
          disabled={(serving === 'on') === search.enabled}
          label="Save"
          settled="Saved"
          primary
        />
        {saved && (
          <span className="iresult">
            {saved.enabled ? 'Saved. Coding tools are offered web search.' : 'Saved. Off - no coding tool is offered web search.'}
          </span>
        )}
      </div>
    </form>
  )
}

function SearchFacts({ search }: { search: WebSearchData }) {
  return (
    <div className="ifields">
      <Fact label="Last verified" value={search.verified_at ? dateTime(search.verified_at) : 'never, for this gateway'} />
      <Fact
        label="Gateway"
        value={
          search.gateway_id
            ? `${search.gateway_id} · ${search.region}${search.deployed ? ' · set by the deployment' : ''}`
            : '-'
        }
      />
      {search.deployed && search.gateway_url && <Fact label="Gateway URL" value={search.gateway_url} />}
      <Fact
        label="Price"
        value={`${rate(search.price_per_search)} a search · ${search.price_source === 'config' ? 'set here' : 'published by AWS'}`}
      />
      <Fact label="MCP endpoint" value={search.mcp_url} />
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
 * The check. A failure arrives as a 200 carrying ok:false, so the request
 * succeeding is not the check succeeding, and throwing on ok:false is what
 * stops the button reporting "Verified" over a line saying it failed.
 */
function SearchCheckRow({ onChecked }: { onChecked: () => void }) {
  const [result, setResult] = useState<WebSearchData | null>(null)
  const [error, setError] = useState<unknown>(null)

  const check = async () => {
    setError(null)
    setResult(null)
    let outcome: WebSearchData
    try {
      outcome = await post<WebSearchData>('/api/v1/web-search/check', {})
    } catch (cause) {
      setError(cause)
      throw cause
    }
    setResult(outcome)
    onChecked()
    if (!outcome.ok) throw new Error(outcome.error ?? 'check failed')
  }

  return (
    <div>
      <ErrorBanner error={error} />
      {result && !result.ok && (
        <div className="notice warn" style={{ marginBottom: 18 }}>
          <div className="notice-title">{result.error}</div>
        </div>
      )}
      <div className="iact">
        <ActionButton onAct={check} label="Check" settled="Verified" />
        <span className="iresult">
          {result?.ok
            ? `Gateway ${result.gateway_id} answered and offers web search.`
            : "Asks the gateway for its tools. No search is run, so it costs nothing."}
        </span>
      </div>
    </div>
  )
}

/** Folded, for the reason the model providers' is: read once, while attaching. */
function RolePolicy({ policy }: { policy: string }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="stack" style={{ gap: 14 }}>
      <p className="inote" style={{ marginBottom: 0 }}>
        The CloudFormation template grants this already; anywhere else, attach it to the role the
        deployment runs as.
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
