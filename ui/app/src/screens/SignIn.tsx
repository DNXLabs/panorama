// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState } from 'react'
import { post, setCsrfToken } from '../api'
import { landingPath } from '../screens'
import { useSession } from '../session'
import { ActionButton, ErrorBanner, SubmitButton, useAction } from '../ui'

// The sign-in gate.
//
// Two doors, and which one leads is decided by the server: a deployment nobody
// has signed in to yet opens on the setup code, and every other one opens on
// single sign-on. The code door stays reachable from the second, because the
// situation it answers - a group mapping that admits nobody, a provider
// reconfigured out from under the deployment - arises long after first run.
//
// A third door exists only while single sign-on does not: a gateway token,
// minted with `panorama token create` where the deployment runs, exchanged for
// an ordinary console session. It is how a deployment evaluated without an
// identity provider is used at all, and the server stops registering it the
// moment an issuer is configured, so the gate offers it only when told to.

export function SignIn() {
  const { auth, reload } = useSession()
  const [failure, setFailure] = useState('')
  const [enteringCode, setEnteringCode] = useState(false)
  const [enteringToken, setEnteringToken] = useState(false)

  // A failed SSO round trip comes back in the fragment, so the reason never
  // reaches a server log or a referrer header.
  useEffect(() => {
    const hash = window.location.hash
    const marker = '#sign-in-failed='
    if (hash.startsWith(marker)) {
      setFailure(decodeURIComponent(hash.slice(marker.length)))
      window.history.replaceState(null, '', window.location.pathname + window.location.search)
    }
  }, [])

  // The code form leads only when there is genuinely no other way in. A
  // deployment with single sign-on configured and nobody signed in yet is the
  // normal state a setup session leaves behind, and leading with the code there
  // would hide the button that creates the first account.
  const noWayIn = Boolean(auth && auth.setup_required && !auth.sso_configured)

  if (!auth) {
    return (
      <div className="gate">
        <div className="gate-card">
          <Brand />
          <div className="prose">
            Panorama is not answering. If you have just started it, give it a moment and reload.
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="gate">
      <div className="gate-card">
        <Brand />
        {failure && (
          <div style={{ marginBottom: 16 }}>
            <ErrorBanner error={new Error(failure)} />
          </div>
        )}
        {enteringToken && auth.token_sign_in ? (
          <div className="stack">
            <div>
              <div className="card-title">Sign in with a gateway token</div>
              <div className="card-sub">Single sign-on is not configured on this deployment yet.</div>
            </div>
            <TokenSignIn onDone={reload} onCancel={() => setEnteringToken(false)} />
          </div>
        ) : noWayIn || enteringCode ? (
          <EnterSetupCode
            onDone={reload}
            onCancel={noWayIn ? undefined : () => setEnteringCode(false)}
            onToken={auth.token_sign_in ? () => setEnteringToken(true) : undefined}
          />
        ) : (
          <Existing
            ssoConfigured={auth.sso_configured}
            tokenSignIn={Boolean(auth.token_sign_in)}
            nobodyYet={auth.setup_required}
            onSetUp={() => setEnteringCode(true)}
            onSignedIn={reload}
          />
        )}
        {auth.dev_sign_in && <DevSignIn onDone={reload} />}
      </div>
    </div>
  )
}

function Brand() {
  return (
    <div className="gate-brand">
      <span className="brand-name">Panorama</span>
    </div>
  )
}

/**
 * EnterSetupCode redeems the one-time code from the log, or from
 * `panorama admin setup`.
 *
 * It asks for the code and nothing else. No email, no name, no password,
 * because there is no account being made: what the code opens is a session that
 * configures this deployment, and the first person to hold an account arrives
 * from the identity provider that session exists to configure.
 */
function EnterSetupCode({
  onDone,
  onCancel,
  onToken,
}: {
  onDone: () => Promise<void>
  onCancel?: () => void
  onToken?: () => void
}) {
  const [code, setCode] = useState('')
  const [error, setError] = useState<unknown>(null)

  // No settled state is ever seen here: onDone swaps the whole gate for the
  // console. The pending state is the whole point - exchanging a code involves
  // a round trip and a session fetch, which is long enough to press twice.
  const submit = async () => {
    setError(null)
    try {
      const result = await post<{ csrf_token?: string }>('/api/v1/auth/setup', {
        code: code.trim(),
      })
      setCsrfToken(result.csrf_token ?? '')
      await onDone()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  const { state, act } = useAction(submit)

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault()
        act()
      }}
      className="stack"
    >
      <div>
        <div className="card-title">Set up this deployment</div>
        <div className="card-sub">
          Panorama printed a one-time code to its own log when it started - that is the proof of
          access that stands in for a sign-in there is nobody to make yet.
        </div>
      </div>

      <ErrorBanner error={error} />

      <div className="code">docker compose logs panorama | grep &quot;one-time code&quot;</div>

      <div className="field">
        <label htmlFor="setup-code">One-time code</label>
        <input
          id="setup-code"
          className="input mono"
          value={code}
          placeholder="pnb_…"
          autoComplete="off"
          onChange={(e) => setCode(e.target.value)}
        />
      </div>

      <SubmitButton
        state={state}
        label="Open a setup session"
        disabled={!code.trim()}
        primary
      />

      <div className="prose tight">
        It works once, lasts an hour, and creates no account. The session configures single sign-on
        and the model providers - the things a deployment cannot work without - and then you sign
        in through your identity provider. If you have lost the code,{' '}
        <span className="mono">panorama admin setup</span> issues a fresh one.
        {onToken && (
          <>
            {' '}
            No identity provider to connect yet?{' '}
            <button type="button" className="btn link" onClick={onToken}>
              Sign in with a gateway token
            </button>
            .
          </>
        )}
        {onCancel && (
          <>
            {' '}
            <button type="button" className="btn link" onClick={onCancel}>
              Back to sign in
            </button>
            .
          </>
        )}
      </div>
    </form>
  )
}

/**
 * DevSignIn is the door a development stack opens, and it says what it is.
 *
 * It appears only where the server reported it, and the server only reports it
 * where the route exists - which is a deployment that named an account in
 * `dev.sign_in_as` and is reachable from nowhere but the machine running it, a
 * condition it refuses to start without. So this is not a control: it is the
 * visible half of a decision already made and already enforced.
 *
 * Drawn as a warning rather than as a third option, because somebody looking at
 * a screenshot of this gate should be able to tell instantly that they are not
 * looking at a production deployment.
 */
function DevSignIn({ onDone }: { onDone: () => Promise<void> }) {
  const [error, setError] = useState<unknown>(null)

  const signIn = async () => {
    setError(null)
    try {
      const result = await post<{ csrf_token?: string }>('/api/v1/auth/dev', {})
      setCsrfToken(result.csrf_token ?? '')
      await onDone()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  return (
    <div className="notice warn" style={{ marginTop: 22 }}>
      <div className="notice-title">Development sign-in is enabled</div>
      <div className="notice-body">
        This deployment will open an administrator session for the account in{' '}
        <span className="mono">dev.sign_in_as</span>, with no identity provider. It exists so the
        console can be reviewed without somebody’s real credentials, and a deployment anything but
        this machine can reach refuses to start with it set.
      </div>
      <ErrorBanner error={error} />
      <div style={{ marginTop: 12 }}>
        <ActionButton onAct={signIn} label="Sign in as the development account" />
      </div>
    </div>
  )
}

/**
 * TokenSignIn exchanges a gateway token for a console session.
 *
 * Shown only where the server said the route exists, which is a deployment
 * with no identity provider. The token is the owner's own credential and
 * already reaches every console endpoint from a script, so the session it opens
 * is that person's and nothing more; the field says where one comes from,
 * because somebody looking at this has by construction not been given one by a
 * screen.
 */
function TokenSignIn({ onDone, onCancel }: { onDone: () => Promise<void>; onCancel?: () => void }) {
  const [token, setToken] = useState('')
  const [error, setError] = useState<unknown>(null)

  // As with the setup code, onDone replaces the gate with the console, so the
  // pending state is the only one anybody sees.
  const submit = async () => {
    setError(null)
    try {
      const result = await post<{ csrf_token?: string }>('/api/v1/auth/token', {
        token: token.trim(),
      })
      setCsrfToken(result.csrf_token ?? '')
      await onDone()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  const { state, act } = useAction(submit)

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault()
        act()
      }}
      className="stack"
    >
      <ErrorBanner error={error} />

      <div className="field">
        <label htmlFor="gateway-token">Gateway token</label>
        <input
          id="gateway-token"
          className="input mono"
          type="password"
          value={token}
          placeholder="pnt_…"
          autoComplete="off"
          onChange={(e) => setToken(e.target.value)}
        />
      </div>

      <SubmitButton state={state} label="Sign in" disabled={!token.trim()} primary />

      <div className="prose tight">
        Mint one where Panorama runs, with{' '}
        <span className="mono">panorama token create -email you@example.com -role admin</span> - on
        AWS, as a one-off task on the stack’s cluster. The session is that account’s, and
        this way in closes once single sign-on is configured.
        {onCancel && (
          <>
            {' '}
            <button type="button" className="btn link" onClick={onCancel}>
              Back
            </button>
            .
          </>
        )}
      </div>
    </form>
  )
}

/** Existing is the ordinary sign-in. */
function Existing({
  ssoConfigured,
  tokenSignIn,
  nobodyYet,
  onSetUp,
  onSignedIn,
}: {
  ssoConfigured: boolean
  tokenSignIn: boolean
  nobodyYet: boolean
  onSetUp: () => void
  onSignedIn: () => Promise<void>
}) {
  // Only a path the console can actually render is worth coming back to. The
  // gate appears at whatever URL the visitor arrived at - it has to, since every
  // path serves index.html - so without this check a guessed URL survives the
  // round trip and the first thing a new sign-in sees is "there is no such
  // screen". Asking for nothing lets the server choose, and for the account that
  // owns a fresh deployment it chooses the setup screen.
  const destination = landingPath(window.location.pathname, window.location.search)

  return (
    <div className="stack">
      <div>
        <div className="card-title">Sign in</div>
        <div className="card-sub">
          {!ssoConfigured
            ? 'Single sign-on is not configured on this deployment yet.'
            : nobodyYet
              ? 'Single sign-on is configured and nobody has signed in yet. The first person through owns this deployment.'
              : 'Panorama uses your organization’s identity provider. There is no Panorama password to forget.'}
        </div>
      </div>

      {ssoConfigured ? (
        <a
          className="btn primary"
          style={{ display: 'inline-flex', alignItems: 'center', justifyContent: 'center' }}
          href={`/auth/oidc/start${destination ? `?redirect_to=${encodeURIComponent(destination)}` : ''}`}
        >
          Continue with single sign-on
        </a>
      ) : tokenSignIn ? (
        <TokenSignIn onDone={onSignedIn} />
      ) : null}

      <div className="prose tight" style={{ marginTop: 4 }}>
        {ssoConfigured
          ? 'Refused by the group mapping, or your provider changed? '
          : 'Configuring single sign-on? '}
        <button type="button" className="btn link" onClick={onSetUp}>
          Enter a setup code
        </button>
        .
      </div>
    </div>
  )
}
