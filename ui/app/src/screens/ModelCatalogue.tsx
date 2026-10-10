// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useMemo, useState } from 'react'
import { ago, rate } from '../format'
import { Icon } from '../icons'
import { apiLabel, byNameThenProfile, profileLabel, routable } from '../routes'
import type { ModelRow, Routing as RoutingData } from '../types'
import { ActionButton, Empty, ErrorBanner, Loading, Modal, Pill, Select } from '../ui'

// What the providers say this account can invoke, and deliberately not filtered
// down to what looks tidy: a model shown as legacy, unpriced or unavailable is
// more useful than one quietly missing, because "why is Opus not in this list"
// is a question nobody can answer from an absence.
//
// One table with two callers. Browsing it is reading; choosing a route's model
// is the same rows with an action column on the end, because the thing that
// matters most - which models are eligible at all - must not differ between the
// list somebody reads and the list they pick from.
export function Catalogue({
  data,
  loading,
  picking,
  chosen,
  canRefresh,
  onDiscover,
  onChoose,
  onClose,
}: {
  data?: RoutingData
  loading: boolean
  /** Choosing a route's model rather than reading the catalogue. */
  picking: boolean
  /** The reference the route holds now, marked "in use". */
  chosen: string
  canRefresh: boolean
  onDiscover: () => Promise<void>
  onChoose: (ref: string) => void
  onClose: () => void
}) {
  const all = data?.models ?? []
  const [query, setQuery] = useState('')
  const [provider, setProvider] = useState('all')
  const [vendor, setVendor] = useState('all')
  const [api, setApi] = useState('all')
  const [profile, setProfile] = useState('all')
  const [status, setStatus] = useState('all')
  const [sort, setSort] = useState<'name' | 'cheap' | 'dear'>('name')
  const [refreshError, setRefreshError] = useState<unknown>(null)

  // The failure is this panel's to show, because this panel is where the
  // button is. Rethrown so the button still settles on "failed".
  const refresh = async () => {
    setRefreshError(null)
    try {
      await onDiscover()
    } catch (cause) {
      setRefreshError(cause)
      throw cause
    }
  }

  const providers = useMemo(
    () => (data?.providers ?? []).filter((p) => all.some((m) => m.provider === p.kind)),
    [data, all],
  )
  const vendors = useMemo(() => [...new Set(all.map((m) => m.vendor).filter(Boolean))].sort(), [all])
  // Geography is a bedrock-runtime idea. Offered only where a row carries one,
  // so a deployment on a provider without inference profiles is not shown a
  // filter every option of which matches nothing.
  const profiles = useMemo(
    () => [...new Set(all.map((m) => m.geography).filter(Boolean))].sort(),
    [all],
  )

  // A model a route cannot point at is not on offer while one is being picked,
  // only while the catalogue is being read: a model that cannot stream cannot
  // serve a coding agent, one the provider says is unavailable cannot serve
  // anybody, and one that speaks no API this gateway serves has no endpoint to
  // arrive at.
  const source = picking ? all.filter(routable) : all

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase()
    const filtered = source.filter((m) => {
      if (q && !m.name.toLowerCase().includes(q) && !m.id.toLowerCase().includes(q)) return false
      if (provider !== 'all' && m.provider !== provider) return false
      if (vendor !== 'all' && m.vendor !== vendor) return false
      if (api !== 'all' && !m.apis.includes(api as ModelRow['apis'][number])) return false
      if (profile !== 'all' && (profile === 'none' ? m.geography !== '' : m.geography !== profile))
        return false
      if (status === 'current' && m.legacy) return false
      if (status === 'legacy' && !m.legacy) return false
      return true
    })
    if (sort === 'cheap' || sort === 'dear') {
      const price = (m: ModelRow) => Number(m.rates?.['input'] ?? (sort === 'cheap' ? 1e9 : -1))
      return filtered
        .slice()
        .sort((a, b) => (sort === 'cheap' ? price(a) - price(b) : price(b) - price(a)))
    }
    return filtered.slice().sort(byNameThenProfile)
  }, [source, query, provider, vendor, api, profile, status, sort])

  return (
    <Modal
      open
      size="wide"
      title={picking ? 'Model this route points to' : 'Model catalogue'}
      sub={
        picking
          ? 'Pick one. Engineers never see this name - they only ever ask for the route.'
          : data?.discovered
            ? `${all.length} targets this account can invoke, ${discoveredLine(data)}.`
            : 'Not discovered yet.'
      }
      onClose={onClose}
      // The picker has no footer: it is chosen from and closed by choosing, and
      // a bar under it would offer a second way out of a modal whose only
      // action is on the row. Refreshing is the catalogue's, because it is the
      // catalogue somebody is reading when they notice a model is missing.
      footer={
        !picking && canRefresh ? (
          <>
            <span />
            <ActionButton onAct={refresh} label="Refresh catalogue" settled="Refreshed" />
          </>
        ) : undefined
      }
    >
      <ErrorBanner error={refreshError} />

      <div className="cat-filters">
        <label className="search grow">
          <Icon name="search" />
          <input
            value={query}
            placeholder="Search model or id"
            aria-label="Search the catalogue"
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        {providers.length > 1 && (
          <Select
            value={provider}
            label="Provider"
            onChange={setProvider}
            options={[
              { value: 'all', label: 'All providers' },
              ...providers.map((p) => ({ value: p.kind, label: p.label })),
            ]}
          />
        )}
        <Select
          value={vendor}
          label="Vendor"
          onChange={setVendor}
          options={[
            { value: 'all', label: 'All vendors' },
            ...vendors.map((v) => ({ value: v, label: v })),
          ]}
        />
        <Select
          value={api}
          label="API"
          onChange={setApi}
          options={[
            { value: 'all', label: 'Any API' },
            { value: 'anthropic-messages', label: 'Messages' },
            { value: 'openai-chat', label: 'Chat Completions' },
            { value: 'openai-responses', label: 'Responses' },
          ]}
        />
        {profiles.length > 0 && (
          <Select
            value={profile}
            label="Inference profile"
            onChange={setProfile}
            options={[
              { value: 'all', label: 'Any inference profile' },
              ...profiles.map((p) => ({ value: p, label: p })),
              { value: 'none', label: 'no profile' },
            ]}
          />
        )}
        <Select
          value={status}
          label="Lifecycle"
          onChange={setStatus}
          options={[
            { value: 'all', label: 'Current and legacy' },
            { value: 'current', label: 'Current only' },
            { value: 'legacy', label: 'Legacy only' },
          ]}
        />
        <Select
          value={sort}
          label="Sort"
          onChange={(next) => setSort(next as 'name' | 'cheap' | 'dear')}
          options={[
            { value: 'name', label: 'Sort by name' },
            { value: 'cheap', label: 'Cheapest input first' },
            { value: 'dear', label: 'Priciest input first' },
          ]}
        />
      </div>

      <div className="cat-count">
        {rows.length} of {source.length}
      </div>

      {loading ? (
        <Loading rows={5} />
      ) : !data?.discovered ? (
        <Empty title="Nothing discovered yet">
          Panorama has not asked a provider what this account can invoke. Use “Refresh
          catalogue”, or configure a provider in Settings, Model providers.
        </Empty>
      ) : rows.length === 0 ? (
        <Empty title="No model matches these filters" />
      ) : (
        <div className={picking ? 'cat-table picking' : 'cat-table'}>
          <div className="cat-row head">
            <span>Model</span>
            <span>Processed in</span>
            <span className="num">In /M</span>
            <span className="num">Out /M</span>
            <span className="num">Cache</span>
            {picking && <span />}
          </div>
          <div className="cat-scroll">
            {rows.map((model) => {
              const inUse = model.ref === chosen
              const tag = !model.streaming
                ? 'no streaming'
                : model.context_window
                  ? formatContext(model.context_window)
                  : null
              return (
                <div key={model.ref} className={inUse ? 'cat-row chosen' : 'cat-row'}>
                  <span style={{ minWidth: 0 }}>
                    <span className="cat-name">
                      <span className="cell-strong">{model.name}</span>
                      <Pill tone={model.global ? undefined : 'off'}>{profileLabel(model)}</Pill>
                      {model.legacy && <Pill tone="warn">legacy</Pill>}
                      {/* The provider's own word, and its reason on hover: a
                          listed model that cannot be invoked is kept in the
                          list so "why is it missing" has an answer here. */}
                      {!model.available && (
                        <span title={model.status_reason || undefined}>
                          <Pill tone="warn">{model.status || 'unavailable'}</Pill>
                        </span>
                      )}
                      {/* A bordered tag rather than a filled one: the window is
                          a fact about the model, not a state of it, and a third
                          fill beside two pills would read as a third status. */}
                      {tag && <span className="cat-tag">{tag}</span>}
                      {model.apis.length === 0 && <span className="cat-tag">browse only</span>}
                    </span>
                    <span className="cat-id truncate">{model.id}</span>
                  </span>
                  <span style={{ minWidth: 0 }}>
                    <span className="truncate" style={{ display: 'block' }}>
                      {model.residency}
                    </span>
                    <span className="cat-provider truncate" title={apiLabel(model.apis)}>
                      {model.provider_label} · {apiLabel(model.apis)}
                    </span>
                  </span>
                  <Rate value={model.rates?.['input']} />
                  <Rate value={model.rates?.['output']} />
                  <Rate value={model.rates?.['cache_read']} />
                  {picking && (
                    <span className="cat-action">
                      {inUse ? (
                        <span className="cat-in-use">In use</span>
                      ) : (
                        <button
                          type="button"
                          className="btn small"
                          onClick={() => onChoose(model.ref)}
                        >
                          Select
                        </button>
                      )}
                    </span>
                  )}
                </div>
              )
            })}
          </div>
        </div>
      )}

      <p className="cat-note">
        Rates come from the AWS Price List API. A model with no published rate is recorded
        unpriced, never as free. On Amazon Bedrock geography is part of the price, not a routing
        detail - a local profile costs about ten per cent more than global, and that is what keeping
        prompts in one country buys. Bedrock Mantle has no profiles: a model runs in the region its
        provider is set to. A model marked browse only speaks no API this gateway serves yet.
      </p>
    </Modal>
  )
}

/** When each provider's catalogue was read, as the modal's sub-line says it. */
function discoveredLine(data: RoutingData): string {
  const parts = data.providers
    .filter((p) => data.discovered_by[p.kind])
    .map((p) => `${p.label}${p.region ? ` in ${p.region}` : ''} ${ago(data.discovered_by[p.kind])}`)
  return parts.length > 0 ? `read from ${parts.join(', ')}` : `discovered ${ago(data.discovered_at)}`
}

/** One published rate, or the absence of one. An em dash in the ghost ink
 *  rather than $0.00: a model nobody has priced is not a free one, and the
 *  column has to be able to say so without looking like a discount. */
function Rate({ value }: { value?: string }) {
  const text = rate(value)
  return <span className={text === '-' ? 'num none' : 'num'}>{text}</span>
}

/**
 * How much context a model takes, as the tag reads it.
 *
 * Absent rather than zero when nothing is recorded - the caller checks before
 * calling - because Bedrock publishes no context length and Panorama's table
 * has not heard of every model AWS ships. A "0 context" tag would say something
 * false about a model that works.
 */
function formatContext(tokens: number): string {
  if (tokens >= 1_000_000 && tokens % 1_000_000 === 0) return `${tokens / 1_000_000}M context`
  if (tokens >= 1_000) return `${Math.round(tokens / 1_000)}K context`
  return `${tokens} context`
}
