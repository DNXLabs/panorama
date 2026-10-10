// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useState } from 'react'
import { Icon } from '../icons'
import { post } from '../api'
import { Link, useRouter } from '../router'
import type { Setup, SetupStep } from '../types'
import { ActionButton, Card, CopyBlock, ErrorBanner, Loading, Pill, Trust, useApi } from '../ui'
import { ModelProviders } from './ModelProviders'
import { SsoConnect } from './SsoConnect'

// The admin first-run wizard.
//
// Sequential but resumable, and every step shows a verified state rather than a
// remembered one - so a step that says done is done right now, not done once.

export function Welcome() {
  const setup = useApi<Setup>('/api/v1/setup')
  const { navigate } = useRouter()
  const [error, setError] = useState<unknown>(null)

  const act = useCallback(
    async (step: string, action: string) => {
      setError(null)
      try {
        await post('/api/v1/setup/step', { step, action })
        setup.reload()
      } catch (cause) {
        setError(cause)
        throw cause
      }
    },
    [setup],
  )

  if (setup.loading) {
    return (
      <main className="page">
        <Card>
          <Loading rows={5} />
        </Card>
      </main>
    )
  }

  const steps = setup.data?.steps ?? []
  const done = steps.filter((s) => s.state === 'done').length

  return (
    <main className="page">
      <ErrorBanner error={error ?? setup.error} />

      <div className="callout">
        <div className="icon-tile" aria-hidden>
          <Icon name="welcome" />
        </div>
        <div style={{ flex: 1, minWidth: 0 }}>
          <div className="card-title">
            {setup.data?.complete
              ? 'Panorama is set up'
              : `${done} of ${steps.length} steps done`}
          </div>
          <div className="card-sub">
            Every state on this page is checked against the deployment right now, not remembered
            from when somebody clicked. A step that says done is done.
          </div>
        </div>
        {setup.data?.complete && (
          <ActionButton
            onAct={async () => {
              await act('', 'dismiss')
              navigate('/')
            }}
            label="Finish"
            settled="Done"
          />
        )}
      </div>

      <Card flush>
        <div className="rows">
          {steps.map((step, index) => (
            <Step key={step.id} step={step} index={index + 1} onAct={act} />
          ))}
        </div>
      </Card>

      <Trust>
        Nothing on this page is a subscription prompt. Single sign-on, the full policy engine, full
        analytics, and full attribution are in the open-source edition, and none of them stop
        working.
      </Trust>
    </main>
  )
}

function Step({
  step,
  index,
  onAct,
}: {
  step: SetupStep
  index: number
  onAct: (step: string, action: string) => Promise<void>
}) {
  return (
    <div className="step">
      <div className={`step-mark ${step.state === 'done' ? 'done' : step.state === 'skipped' ? 'skipped' : ''}`}>
        {step.state === 'done' ? (
          <Icon name="check" />
        ) : step.state === 'skipped' ? (
          <Icon name="skipped" />
        ) : (
          index
        )}
      </div>

      <div style={{ flex: 1, minWidth: 0 }}>
        <div className="spread">
          <div style={{ minWidth: 0 }}>
            <div className="step-title">{step.title}</div>
            <div className="step-summary">{step.summary}</div>
          </div>
          <div className="inline">
            {step.state === 'done' ? (
              <Pill tone="good">verified</Pill>
            ) : step.state === 'skipped' ? (
              <Pill tone="off">skipped</Pill>
            ) : step.required ? (
              <Pill tone="warn">to do</Pill>
            ) : (
              <Pill>optional</Pill>
            )}
            {!step.required && step.state !== 'done' && (
              <ActionButton
                onAct={() => onAct(step.id, step.state === 'skipped' ? 'unskip' : 'skip')}
                label={step.state === 'skipped' ? 'Unskip' : 'Skip'}
                settled={step.state === 'skipped' ? 'Unskipped' : 'Skipped'}
                small
              />
            )}
          </div>
        </div>

        {step.state !== 'done' && (
          <div style={{ marginTop: 14 }}>
            <StepBody step={step} />
          </div>
        )}
      </div>
    </div>
  )
}

function StepBody({ step }: { step: SetupStep }) {
  switch (step.id) {
    case 'identity':
      return <IdentityStep detail={step.detail} />
    case 'provider':
      return <ProviderStep detail={step.detail} />
    case 'models':
      return <ModelsStep detail={step.detail} />
    case 'team':
      return (
        <div className="prose">
          Teams are the smallest unit a leader is shown, and a budget hangs off one.{' '}
          <Link to="/people">Create a team and set its budget</Link> - a soft limit warns, and
          closes any route set to stop past one, a hard one stops them entirely.
        </div>
      )
    case 'developers':
      return <DevelopersStep detail={step.detail} />
    case 'integrations':
      return (
        <div className="prose">
          Optional, and honestly so. Attribution already works - what an integration adds is titles,
          merge status, and cost per merged pull request.{' '}
          <Link to="/settings?tab=integrations">See what each unlocks</Link>.
        </div>
      )
    default:
      return null
  }
}

function IdentityStep({ detail }: { detail: Record<string, unknown> }) {
  const configured = Boolean(detail['configured'])
  const redirect = String(detail['redirect_url'] ?? '')

  return (
    <div className="stack">
      <div className="prose">{String(detail['note'] ?? '')}</div>
      {!configured ? (
        <SsoConnect compact />
      ) : (
        <div className="prose">
          The issuer is configured. Register <span className="mono">{redirect}</span> as a redirect
          URI at your provider, then sign in once through it - this step is not done until somebody
          actually has, because discovery alone does not prove the round trip.
        </div>
      )}
      <div>
        <Link to="/settings" className="btn">
          Map groups to roles
        </Link>
      </div>
    </div>
  )
}

/**
 * ProviderStep is where this deployment invokes models, and proof that it can.
 *
 * Not which models anyone may use. That is a separate step, on a separate
 * screen, made by an administrator once there is one - this step is the part
 * that has to be true before any of it can be, and the part a setup session
 * has standing over. The providers themselves are the same blocks Settings
 * renders, so there is one form for a region and one live check, not a copy
 * that drifts from it.
 */
export function ProviderStep({ detail }: { detail: Record<string, unknown> }) {
  return (
    <div className="stack">
      <div className="prose">{String(detail['note'] ?? '')}</div>
      <ModelProviders setup />
    </div>
  )
}

/**
 * ModelsStep points at the screen that owns the decision rather than making it
 * here. Routes live in the database and change long after setup is over, so a
 * copy of the editor on a first-run page would be a second place to make one
 * decision - and the one people would go on using.
 */
function ModelsStep({ detail }: { detail: Record<string, unknown> }) {
  const discovered = Number(detail['discovered'] ?? 0)
  const providers = (detail['providers'] as { label: string; region: string; enabled: boolean }[] | undefined) ?? []
  const where = providers
    .filter((p) => p.enabled)
    .map((p) => `${p.label} in ${p.region}`)
    .join(', ')
  return (
    <div className="stack">
      <div className="prose">{String(detail['note'] ?? '')}</div>
      <div className="meta">
        <span>{Number(detail['routes'] ?? 0)} routes</span>
        <span>
          {discovered} discovered{where ? ` across ${where}` : ', and no provider is switched on'}
        </span>
      </div>
      <div className="prose tight">
        Until one route exists, every request is refused - there is no "allow any model" setting
        to leave switched on, which is deliberate.
      </div>
      <div>
        <Link to="/routing" className="btn">
          Create a route
        </Link>
      </div>
    </div>
  )
}

function DevelopersStep({ detail }: { detail: Record<string, unknown> }) {

  const url = String(detail['onboarding_url'] ?? '')
  return (
    <div className="stack">
      <div className="prose">
        Send engineers this link. They sign in, mint their own token, and paste one exported
        variable - and their spend starts appearing against real branches and tickets with nothing
        else configured on their side.
      </div>
      <CopyBlock label="share this" text={url} />
      <div className="prose tight">
        {String(detail['onboarded'] ?? 0)} of {String(detail['users'] ?? 0)} accounts have used the
        gateway.
      </div>
    </div>
  )
}
