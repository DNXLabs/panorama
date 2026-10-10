// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState } from 'react'
import { Icon } from '../icons'

import { post, get } from '../api'
import { ActionButton, CopyBlock, ErrorBanner, Select } from '../ui'
import type { SsoFinding, SsoPreflight, SsoPreset } from '../types'

/**
 * SsoConnect validates an issuer and renders the configuration that would use
 * it. It saves nothing, and says so.
 *
 * The connection lives in the environment, so getting it wrong costs a redeploy
 * to discover - and the error surfaces at the identity provider, in the
 * provider's words, a long way from the setting that caused it. This screen
 * exists to move that discovery before the restart rather than after it.
 *
 * It is the same check `panorama sso probe` runs, rendered by the same code on
 * the server, because two generators of one artifact drift.
 */
export function SsoConnect({ compact = false }: { compact?: boolean }) {
  const [presets, setPresets] = useState<SsoPreset[]>([])
  const [presetId, setPresetId] = useState('entra-id')
  const [issuer, setIssuer] = useState('')
  const [clientId, setClientId] = useState('')
  const [result, setResult] = useState<SsoPreflight | null>(null)
  const [error, setError] = useState<unknown>(null)

  const [redirectUri, setRedirectUri] = useState('')

  useEffect(() => {
    get<{ presets: SsoPreset[]; redirect_uri: string }>('/api/v1/identity/presets')
      .then((data) => {
        setPresets(data.presets)
        setRedirectUri(data.redirect_uri)
      })
      .catch(setError)
  }, [])

  const preset = presets.find((p) => p.id === presetId)

  // A probe that reaches the issuer and finds it misconfigured is a completed
  // check, not a failed one - the findings below say what is wrong. Only an
  // unreachable issuer is a failure of the check itself.
  const check = async () => {
    setError(null)
    try {
      setResult(
        await post<SsoPreflight>('/api/v1/identity/preflight', {
          issuer: issuer.trim(),
          client_id: clientId.trim(),
          preset: presetId,
        }),
      )
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  return (
    <div className="stack">
      <ErrorBanner error={error} />

      {/* Shown before anything is probed, because it is knowable before anything
          is probed - it comes from server.base_url and nothing about it depends
          on the provider. Registering it wrongly is the most common way a first
          sign-in fails, and it fails at the provider, in the provider's words. */}
      {redirectUri && (
        <div className="callout">
          <div className="icon-tile"><Icon name="redirect" /></div>
          <div>
            <div className="prose tight" style={{ marginBottom: 8 }}>
              Register this redirect URI at your provider, exactly as written - a trailing slash is
              a different URI. It comes from <span className="mono">server.base_url</span>, so if
              that is not the URL a browser reaches this console at, fix that too.
            </div>
            <CopyBlock label="redirect uri" text={redirectUri} />
          </div>
        </div>
      )}

      <div className="field">
        <label htmlFor="sso-preset">Your identity provider</label>
        <Select
          id="sso-preset"
          value={presetId}
          label="Your identity provider"
          onChange={(next) => {
            setPresetId(next)
            setResult(null)
          }}
          options={presets.map((p) => ({ value: p.id, label: p.label }))}
        />
      </div>

      {preset && preset.notes.length > 0 && (
        <div className="callout">
          <div className="icon-tile"><Icon name="info" /></div>
          <div>
            <div className="prose tight" style={{ marginBottom: 8 }}>
              At {preset.label}, before this will work:
            </div>
            <ul className="prose tight" style={{ paddingLeft: 18 }}>
              {preset.notes.map((note) => (
                <li key={note} style={{ marginBottom: 4 }}>
                  {note}
                </li>
              ))}
            </ul>
            {preset.docs_url && (
              <div className="prose tight" style={{ marginTop: 8 }}>
                <a href={preset.docs_url} target="_blank" rel="noreferrer noopener">
                  {preset.label} documentation
                </a>
              </div>
            )}
          </div>
        </div>
      )}

      <div className="field">
        <label htmlFor="sso-issuer">Issuer URL</label>
        <input
          id="sso-issuer"
          className="input"
          value={issuer}
          placeholder={preset?.issuer_hint ?? 'https://your-idp.example.com'}
          onChange={(e) => setIssuer(e.target.value)}
        />
        <span className="prose tight">
          The URL that serves <span className="mono">/.well-known/openid-configuration</span>.
          Panorama appends that path itself.
        </span>
      </div>

      <div className="field">
        <label htmlFor="sso-client-id">Client id</label>
        <input
          id="sso-client-id"
          className="input"
          value={clientId}
          placeholder="the application you registered"
          onChange={(e) => setClientId(e.target.value)}
        />
      </div>

      <div>
        <ActionButton
          onAct={check}
          label="Check this connection"
          settled="Checked"
          primary
          disabled={!issuer.trim()}
        />
      </div>

      {result && (
        <>
          <Findings findings={result.result.findings} />

          {result.result.ok && (
            <div>
              <div className="kpi-label" style={{ marginBottom: 10 }}>
                Configuration to apply
              </div>
              <div className="prose tight" style={{ marginBottom: 14 }}>
                Nothing here has been saved. The connection lives in a secret this deployment
                already reads, so there is no task definition to edit - and editing one would be
                undone by the next stack update anyway.
              </div>

              <ol className="sso-steps">
                <li>
                  In <strong>Secrets Manager</strong>, open the secret named in your stack&rsquo;s{' '}
                  <span className="mono">OidcSecretArn</span> output, then{' '}
                  <em>Retrieve secret value</em> &rarr; <em>Edit</em> &rarr; <em>Plaintext</em>.
                </li>
                <li>
                  Replace what is there with this, putting your client secret in place of{' '}
                  <span className="mono">REPLACE_WITH_CLIENT_SECRET</span>. Leave a field empty to
                  keep the built-in default.
                  <div style={{ marginTop: 10 }}>
                    <CopyBlock label="secret value" text={result.artifacts['ecs'] ?? ''} />
                  </div>
                </li>
                <li>
                  In <strong>ECS</strong>, open the <span className="mono">panorama</span> service,
                  choose <em>Update</em>, tick <em>Force new deployment</em>, and update. The secret
                  is read once when a task starts, so the running tasks keep the old connection
                  until they are replaced.
                </li>
              </ol>

              {!compact && (
                <div className="prose tight" style={{ marginTop: 12 }}>
                  Then sign in once. Group membership only becomes knowable after that first round
                  trip - until somebody has signed in, nothing here knows what your provider calls
                  its groups.
                </div>
              )}
            </div>
          )}
        </>
      )}
    </div>
  )
}

/**
 * Findings renders a check result at the severity that says what to do.
 *
 * The palette carries one alarm colour and no separate danger tone, which is
 * the mock's decision rather than an omission. So only a failure takes it; a
 * warning is distinguished by its word and by being dimmer. Three hues for
 * three levels would have meant inventing one.
 */
export function Findings({ findings }: { findings: SsoFinding[] }) {
  if (findings.length === 0) return null

  return (
    <div className="list" style={{ gap: 12 }}>
      {findings.map((f, index) => (
        <div key={index} className="inline" style={{ alignItems: 'flex-start', gap: 12 }}>
          <span
            className="mono"
            style={{
              fontSize: 'var(--size-label-sm)',
              letterSpacing: 'var(--track-micro)',
              color: levelColor(f.level),
              flexShrink: 0,
              width: 34,
              paddingTop: 2,
            }}
          >
            {f.level === 'fail' ? 'FAIL' : f.level === 'warn' ? 'WARN' : 'OK'}
          </span>
          <div>
            <div style={{ fontSize: 'var(--size-body)', marginBottom: 3 }}>{f.title}</div>
            {f.detail && (
              <div className="prose tight" style={{ marginBottom: f.remedy ? 5 : 0 }}>
                {f.detail}
              </div>
            )}
            {f.remedy && (
              <div className="prose tight" style={{ color: 'var(--text-muted)' }}>
                → {f.remedy}
              </div>
            )}
          </div>
        </div>
      ))}
    </div>
  )
}


function levelColor(level: SsoFinding['level']): string {
  switch (level) {
    case 'fail':
      return 'var(--state-warn)'
    case 'warn':
      return 'var(--text-tertiary)'
    default:
      return 'var(--accent)'
  }
}
