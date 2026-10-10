// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from 'react'
import { del, post, put } from '../api'
import { usePageChrome } from '../chrome'
import { ago } from '../format'
import { Icon } from '../icons'
import { useSession } from '../session'
import type { IntegrationRow, Integrations as IntegrationsData } from '../types'
import {
  ActionButton,
  ErrorBanner,
  Loading,
  Pill,
  Segmented,
  SubmitButton,
  useAction,
  useApi,
} from '../ui'

// The systems Panorama asks about the work it has already seen.
//
// A band per integration, and each band is one sentence and one decision: the
// rail says what this source is and what it is worth, the wide column is the
// thing you do about it. GitHub is connectable and gets a form; Jira is a line.
//
// The rail is deliberately the narrow half. What an administrator is doing here
// is filling in a credential, and the argument for doing so belongs beside that
// work rather than above it - which is also why the rail sticks: the reason a
// scope is only ever read is still on screen when the private key is pasted.

/** The two credential shapes GitHub accepts, in the order they should be
 *  reached for. An App is what a deployment runs on; a token is what somebody
 *  tries it with. */
const METHODS: { id: 'app' | 'token'; label: string }[] = [
  { id: 'app', label: 'GitHub App' },
  { id: 'token', label: 'Token' },
]

const NOTES: Record<'app' | 'token', string> = {
  app: 'The path for a real deployment: installed on the organisation with a stated scope, so its access does not belong to whoever set it up. It needs read access to pull requests and contents on the repositories your engineers touch - Panorama never enumerates a repository, so nothing beyond that.',
  token:
    'A personal access token belongs to the person who made it. Access ends when their account does, and every read is recorded as theirs. Use this to try the integration, not to run it.',
}

/**
 * Connecting a source.
 *
 * A Settings tab rather than a sidebar screen: connecting GitHub is a one-off
 * administrator decision made months after a deployment is stood up, which is
 * the same shape as an identity connection and the same shape as a chart of
 * accounts.
 *
 * It keeps its own header count when nested. "3 of 4 connected" is what somebody
 * opening this tab came to read, and the tab strip above it says nothing about
 * whether anything is working.
 */
export function Integrations({ nested = false }: { nested?: boolean } = {}) {
  const me = useSession().session!.user
  const data = useApi<IntegrationsData>('/api/v1/integrations')
  const [error, setError] = useState<unknown>(null)

  const rows = data.data?.integrations ?? []
  const connected = rows.filter((row) => row.connected).length
  const banner = error ?? data.error

  const check = async () => {
    setError(null)
    try {
      await post('/api/v1/integrations/run')
      data.reload()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  // How many of the sources this deployment could confirm against actually
  // answer. Against the header's right edge, where every screen's standing
  // count goes - and absent while the answer is unknown, because "0 of 0
  // connected" is a claim rather than a placeholder.
  usePageChrome(
    {
      actions: data.loading ? undefined : (
        <>
          {me.is_admin && data.data?.any_connected && (
            <ActionButton onAct={check} label="Check now" settled="Checked" />
          )}
          <span className={connected > 0 ? 'iconn on' : 'iconn'}>
            {connected} of {rows.length} connected
          </span>
        </>
      ),
    },
    [connected, rows.length, me.is_admin, data.data?.any_connected, data.loading],
  )

  const bands = (
    <>
      {banner ? (
        <section className="band bar">
          <ErrorBanner error={banner} />
        </section>
      ) : null}

      {data.loading ? (
        <section className="band last">
          <Loading rows={4} />
        </section>
      ) : (
        rows.map((integration, at) => {
          const last = at === rows.length - 1
          return integration.kind === 'github' ? (
            <GitHubBand
              key={integration.kind}
              integration={integration}
              canStoreSecrets={data.data?.can_store_secrets ?? false}
              isAdmin={me.is_admin}
              last={last}
              onChanged={() => data.reload()}
            />
          ) : (
            <JiraBand key={integration.kind} integration={integration} last={last} />
          )
        })
      )}
    </>
  )

  return nested ? bands : <main className="bands">{bands}</main>
}

function GitHubBand({
  integration,
  canStoreSecrets,
  isAdmin,
  last,
  onChanged,
}: {
  integration: IntegrationRow
  canStoreSecrets: boolean
  isAdmin: boolean
  last: boolean
  onChanged: () => void
}) {
  return (
    <section className={last ? 'band iband last' : 'band iband'}>
      <div className="irail">
        <div className="irail-head">
          <span className="irail-name">GitHub</span>
          <StatusPill integration={integration} />
        </div>
        <span className="irail-rule" />
        <p className="irail-unlocks">{integration.unlocks}</p>
        {/* What is lost without it, and only while it is lost. Left up once a
            source answers, it would read as a warning about a working
            deployment. */}
        {!integration.connected && <p className="irail-degraded">{integration.degraded}</p>}
        {/* Said in both states, because what it qualifies is the numbers a
            connected source produces, and the manual says the same. */}
        <p className="irail-degraded">
          Experimental. It has not yet been tested against a real GitHub organization.
        </p>
      </div>

      <div className="imain">
        {integration.connected ? (
          <Connected integration={integration} isAdmin={isAdmin} onChanged={onChanged} />
        ) : (
          <ConnectGitHub
            integration={integration}
            canStoreSecrets={canStoreSecrets}
            isAdmin={isAdmin}
            onConnected={onChanged}
          />
        )}
      </div>
    </section>
  )
}

/**
 * Jira, which is one line tall on purpose.
 *
 * It is out of v1 and naming it as a provider is refused at startup, so there
 * is nothing here to configure and no form to draw. What it is still worth
 * saying is what connecting it would buy, in the same words the rail uses for
 * GitHub - a reader comparing the two is comparing exactly that.
 */
function JiraBand({ integration, last }: { integration: IntegrationRow; last: boolean }) {
  return (
    <section className={last ? 'band iband line last' : 'band iband line'}>
      <div className="irail">
        <div className="irail-head">
          <span className="irail-name off">Jira</span>
          <Pill tone="off">coming soon</Pill>
        </div>
      </div>
      <p className="irail-unlocks">{integration.unlocks}</p>
    </section>
  )
}

/** The word beside the name. Four states rather than two: a connection that has
 *  never run and one that is failing are both "not working", and an operator
 *  reaching for the logs needs to know which. */
function StatusPill({ integration }: { integration: IntegrationRow }) {
  if (!integration.connected) return <Pill tone="off">not connected</Pill>
  if (integration.status === 'error') return <Pill tone="warn">failing</Pill>
  if (integration.status === 'ok') return <Pill tone="good">connected</Pill>
  return <Pill>never run</Pill>
}

/**
 * A source that answers, read as the facts the form left behind.
 *
 * Same grid and the same register labels as the fields it replaces, because it
 * is the same information from the other side: what was typed in, and what came
 * back. The credential is not among it - no endpoint returns one, and a masked
 * value here would imply it could be revealed.
 */
function Connected({
  integration,
  isAdmin,
  onChanged,
}: {
  integration: IntegrationRow
  isAdmin: boolean
  onChanged: () => void
}) {
  return (
    <div className="istack">
      {integration.is_mock && (
        <div className="callout warn">
          <div className="icon-tile" aria-hidden>
            <Icon name="alert" />
          </div>
          <div className="prose" style={{ color: 'var(--text-primary)' }}>
            These numbers come from a fixtures file, not from GitHub.{' '}
            <span className="mono">{integration.detail}</span>
          </div>
        </div>
      )}

      <div className="ifields">
        <Fact label="Provider" value={integration.provider ?? '-'} />
        {/* Which credential is behind the numbers. A personal access token
            carries one person's access and expires with their employment, so a
            deployment running on one should be able to see that it is - the
            same reason a mock says so. */}
        {integration.detail && !integration.is_mock && (
          <Fact label="Connection" value={integration.detail} />
        )}
        <Fact label="Last checked" value={ago(integration.last_checked_at)} />
        <Fact label="Last sync" value={ago(integration.last_synced_at)} />
        <Fact
          label="Last run"
          value={`${integration.last_confirmed_items ?? 0} confirmed of ${integration.last_checked_items ?? 0} asked`}
        />
      </div>

      {integration.last_error && (
        <div className="notice warn">
          <div className="notice-title">Last failure: {integration.last_error}</div>
          <div className="notice-body">
            Kept after it clears, because an integration that fails intermittently is the one worth
            chasing.
          </div>
        </div>
      )}

      {isAdmin && (
        <div className="iact">
          <ActionButton
            label="Disconnect"
            settled="Disconnected"
            onAct={async () => {
              await del(`/api/v1/integrations/${integration.kind}`)
              onChanged()
            }}
          />
        </div>
      )}
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
 * ConnectGitHub is the form that connects GitHub, rather than a block of YAML.
 *
 * It exists because connecting GitHub is not initial setup. It is a decision an
 * administrator makes later, from a screen, often long after the deployment was
 * stood up and by somebody who has no access to its manifest - so requiring a
 * redeploy would put a release cycle in front of a checkbox.
 *
 * The private key is a textarea rather than a file picker because that is how
 * it arrives: GitHub hands you a .pem, and people paste its contents. It is
 * write-only in both directions - sent once, never returned, and shown as
 * "stored" rather than as a masked value that implies it could be revealed.
 */
function ConnectGitHub({
  integration,
  canStoreSecrets,
  isAdmin,
  onConnected,
}: {
  integration: IntegrationRow
  canStoreSecrets: boolean
  isAdmin: boolean
  onConnected: () => void
}) {
  // Opens on the shape already stored, so an administrator replacing an
  // expiring credential lands on the fields they filled in last time.
  const [shape, setShape] = useState<'app' | 'token'>(
    integration.secret_kind === 'token' ? 'token' : 'app',
  )
  const [appID, setAppID] = useState(integration.app_id ?? '')
  const [installationID, setInstallationID] = useState(integration.installation_id ?? '')
  const [apiBaseURL, setAPIBaseURL] = useState(integration.api_base_url ?? '')
  const [privateKey, setPrivateKey] = useState('')
  const [token, setToken] = useState('')
  const [result, setResult] = useState('')
  const [error, setError] = useState<unknown>(null)

  const save = useAction(async () => {
    setError(null)
    setResult('')
    try {
      const out = await put<{ checked: boolean; ok?: boolean; error?: string; detail?: string }>(
        '/api/v1/integrations/github',
        {
          provider: 'github',
          app_id: shape === 'app' ? appID : '',
          installation_id: shape === 'app' ? installationID : '',
          api_base_url: apiBaseURL,
          private_key: shape === 'app' ? privateKey : '',
          token: shape === 'token' ? token : '',
        },
      )
      // Cleared on success: the credential has been stored and the field should
      // not keep a copy of it sitting in the page.
      setPrivateKey('')
      setToken('')
      setResult(
        !out.checked
          ? 'Saved. Not yet checked.'
          : out.ok
            ? `Connected - ${out.detail ?? 'reached GitHub'}.`
            : `Saved, but GitHub refused it: ${out.error}`,
      )
      onConnected()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  })

  if (!isAdmin) {
    return (
      <p className="inote">
        An administrator connects this. What it adds is pull request titles, authors, and merge
        status - and with them, cost per merged pull request.
      </p>
    )
  }

  if (!canStoreSecrets) {
    // Said before the form rather than after a failed save: without a key there
    // is nowhere safe to put the credential, and no amount of correct typing
    // will change that.
    return (
      <div className="callout warn">
        <div className="icon-tile" aria-hidden>
          <Icon name="alert" />
        </div>
        <div className="prose" style={{ color: 'var(--text-primary)' }}>
          This deployment has no encryption key, so it cannot store a credential. Set{' '}
          <span className="mono">PANORAMA_ENCRYPTION_KEY</span> to any passphrase of sixteen
          characters or more - or to the output of{' '}
          <span className="mono">panorama admin generate-key</span> - and restart. The key stays in
          your configuration while what it protects lives in the database, so a backup carries
          nothing usable.
        </div>
      </div>
    )
  }

  const stored = (kind: 'app_key' | 'token') =>
    integration.has_secret && integration.secret_kind === kind ? (
      <span className="ifield-note">One is stored. Paste a new one to replace it.</span>
    ) : null

  return (
    <form
      className="iform"
      onSubmit={(e) => {
        e.preventDefault()
        void save.act()
      }}
    >
      <ErrorBanner error={error} />

      {/* Which credential, before anything about it. The two shapes share no
          field, so choosing second would mean typing into a form that is about
          to be replaced. */}
      <Segmented value={shape} options={METHODS} label="Credential shape" onChange={setShape} />

      <p className="inote">{NOTES[shape]}</p>

      <div className="ifields">
        {shape === 'app' ? (
          <>
            <div className="field">
              <label htmlFor="gh-app-id">App ID</label>
              <input
                id="gh-app-id"
                className="input"
                value={appID}
                spellCheck={false}
                placeholder="123456"
                onChange={(e) => setAppID(e.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="gh-install-id">Installation ID</label>
              <input
                id="gh-install-id"
                className="input"
                value={installationID}
                spellCheck={false}
                placeholder="discovered automatically when installed in one place"
                onChange={(e) => setInstallationID(e.target.value)}
              />
            </div>
            {/* The one field that holds pasted terminal content, so the one
                field set in the code face rather than the structural one. */}
            <div className="field wide">
              <label htmlFor="gh-key">Private key</label>
              <textarea
                id="gh-key"
                className="input"
                rows={4}
                value={privateKey}
                spellCheck={false}
                placeholder="-----BEGIN RSA PRIVATE KEY-----"
                onChange={(e) => setPrivateKey(e.target.value)}
              />
              {stored('app_key')}
            </div>
          </>
        ) : (
          <div className="field">
            <label htmlFor="gh-token">Personal access token</label>
            <input
              id="gh-token"
              className="input"
              type="password"
              value={token}
              spellCheck={false}
              placeholder="ghp_ - read scopes only"
              onChange={(e) => setToken(e.target.value)}
            />
            {stored('token')}
          </div>
        )}

        <div className="field">
          <label htmlFor="gh-api">API base URL</label>
          <input
            id="gh-api"
            className="input"
            value={apiBaseURL}
            spellCheck={false}
            placeholder="github.com - set this only for GitHub Enterprise Server"
            onChange={(e) => setAPIBaseURL(e.target.value)}
          />
        </div>
      </div>

      <div className="iact">
        <SubmitButton state={save.state} label="Connect" settled="Connected" primary />
        {/* Beside the button rather than under it: the credential was stored
            either way, and what GitHub then said about it is the second half of
            the same sentence the button started. */}
        {result && <span className="iresult">{result}</span>}
      </div>
    </form>
  )
}
