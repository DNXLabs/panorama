// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState } from 'react'
import { get, post } from '../api'
import { dateTime } from '../format'
import { useSession } from '../session'
import { Link } from '../router'
import { ActionButton, Card, Empty, ErrorBanner, Loading, Trust } from '../ui'

// Approving a sign-in from the terminal.
//
// The one screen in the console that exists to answer a question rather than to
// show a number, and the whole of the device flow's security rests on it: the
// code a CLI displays is inert until somebody signed in here agrees to it.
//
// So the screen is built to make the decision, not to complete a wizard. What
// is shown is what the person needs in order to say no - which machine asked,
// from which address, and when - and the refusing action is as reachable as the
// approving one rather than tucked behind a link. A page that only offers
// "Allow" has not asked a question.
//
// IT STILL RESOLVES WHILE THE FLOW IS SWITCHED OFF, and says so. This release
// does not serve command-line sign-in, so nobody arrives here from a terminal -
// but the address is printed in documentation and pasted into runbooks, and a
// path that answered "there is no such screen" would send somebody looking for
// a broken deployment. What is switched off is said in one sentence, next to
// the thing that does work.

type Pending = {
  user_code: string
  client_name: string
  source_ip: string
  created_at: string
  expires_at: string
}

/** codeFromLocation reads a code the CLI put in the URL, so the common path is
 *  a click rather than transcription. Typing it is still supported, because the
 *  machine showing the code is often not the machine with the browser. */
function codeFromLocation(): string {
  const params = new URLSearchParams(window.location.search)
  return (params.get('code') ?? '').toUpperCase()
}

export function ApproveCLI() {
  const cliSignIn = useSession().session?.cli_sign_in ?? false
  const [code, setCode] = useState(codeFromLocation)
  const [pending, setPending] = useState<Pending | null>(null)
  const [decided, setDecided] = useState<'approved' | 'denied' | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [looking, setLooking] = useState(false)

  const look = async (raw: string) => {
    const wanted = raw.trim()
    if (!wanted) return
    setError(null)
    setLooking(true)
    try {
      setPending(await get<Pending>(`/api/v1/cli/pending?user_code=${encodeURIComponent(wanted)}`))
    } catch (cause) {
      setPending(null)
      setError(cause)
    } finally {
      setLooking(false)
    }
  }

  // A code in the URL is looked up once, on arrival. Typing one is a submit.
  useEffect(() => {
    const fromURL = codeFromLocation()
    if (fromURL) void look(fromURL)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const decide = async (approve: boolean) => {
    await post('/api/v1/cli/decide', { user_code: pending?.user_code ?? code, approve })
    setDecided(approve ? 'approved' : 'denied')
    setPending(null)
  }

  // The form is not drawn at all rather than drawn and refused. Every code it
  // could accept is one the deployment would not have issued, so asking for one
  // is asking a question with no right answer - and the server refuses the
  // lookup regardless of what renders here.
  if (!cliSignIn) {
    return (
      <main className="page">
        <div className="grid">
          <Card
            title="Approve a terminal sign-in"
            sub="Signing a machine in from the command line is switched off on this deployment."
          >
            <div style={{ padding: '18px 22px' }} className="stack">
              <Empty title="Nothing to approve">
                No terminal can ask to sign in as you here, so no code will ever be waiting. Your
                coding tool holds a gateway token instead - mint one on{' '}
                <Link to="/setup">My setup</Link>, and re-mint it there when it expires.
              </Empty>
            </div>
          </Card>

          <Trust>
            If a command line showed you a code and sent you here, it was not talking to this
            deployment. Nothing was issued, and nothing needs to be refused.
          </Trust>
        </div>
      </main>
    )
  }

  return (
    <main className="page">
      <ErrorBanner error={error} />

      <div className="grid">
        <Card
          title="Approve a terminal sign-in"
          sub="A command line asked to sign in as you. It cannot until you say so here."
        >
          <div style={{ padding: '18px 22px' }} className="stack">
            {decided === 'approved' && (
              <Empty title="Approved">
                Your terminal has what it needs. It may take a few seconds to notice.
              </Empty>
            )}
            {decided === 'denied' && (
              <Empty title="Refused">
                That sign-in was refused and the code is spent. If it was not you who
                started it, nothing further is needed - nothing was issued.
              </Empty>
            )}

            {!decided && !pending && (
              <form
                className="stack"
                onSubmit={(event) => {
                  event.preventDefault()
                  void look(code)
                }}
              >
                <div className="field">
                  <label htmlFor="cli-code">The code your terminal is showing</label>
                  <input
                    id="cli-code"
                    className="input mono"
                    value={code}
                    onChange={(event) => setCode(event.target.value.toUpperCase())}
                    placeholder="ABCD-EFGH"
                    autoComplete="off"
                    autoCapitalize="characters"
                    spellCheck={false}
                  />
                </div>
                <div>
                  <button type="submit" className="btn primary" disabled={looking}>
                    {looking ? 'Looking…' : 'Continue'}
                  </button>
                </div>
              </form>
            )}

            {looking && !pending && <Loading rows={2} />}

            {!decided && pending && (
              <div className="stack">
                <div className="rows">
                  <div className="row spread">
                    <span>Machine</span>
                    <span className="mono">{pending.client_name}</span>
                  </div>
                  <div className="row spread">
                    <span>Asked from</span>
                    <span className="mono">{pending.source_ip || 'an unknown address'}</span>
                  </div>
                  <div className="row spread">
                    <span>Asked at</span>
                    <span className="mono">{dateTime(pending.created_at)}</span>
                  </div>
                  <div className="row spread">
                    <span>Expires</span>
                    <span className="mono">{dateTime(pending.expires_at)}</span>
                  </div>
                </div>

                <p className="prose tight">
                  Approving signs that machine in as you. Everything it spends is metered
                  to you, and you can sign it out again from My setup at any time.
                </p>

                <div className="inline">
                  <ActionButton
                    primary
                    label="Allow"
                    settled="allowed"
                    onAct={() => decide(true)}
                  />
                  <ActionButton label="Refuse" settled="refused" onAct={() => decide(false)} />
                </div>
              </div>
            )}
          </div>
        </Card>

        <Trust>
          If you did not start this, refuse it. A code nobody approves expires on its own
          and issues nothing - there is no request here that succeeds by being ignored.
        </Trust>
      </div>
    </main>
  )
}
