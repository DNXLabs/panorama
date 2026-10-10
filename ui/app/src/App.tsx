// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { patch } from './api'
import { ChromeProvider, useChrome } from './chrome'
import { Icon } from './icons'
import { Link, useRouter } from './router'
import { ORGANIZATION, PERSONAL, screenAt } from './screens'
import type { Screen } from './screens'
import { useSession } from './session'
import { SignIn } from './screens/SignIn'
import { IdentitySettings } from './screens/Settings'
import { ProviderStep } from './screens/Welcome'
import { ActionButton, Avatar, Card, ErrorBanner, Pill, useApi, useTheme } from './ui'
import type { Theme } from './ui'
import type { ProviderSummary, Setup, SetupAdmins } from './types'

export function App() {
  const { session, loading } = useSession()
  const { path } = useRouter()
  // Applied here rather than in the header that switches it, because the gate
  // and the setup console have no header and a reader who chose paper should
  // not be handed a dark sign-in page on the way to it.
  const theme = useTheme()

  if (loading) {
    // Withheld for --delay-wait, like every skeleton in the console and for the
    // same reason. Resolving the session is one request against the deployment
    // the page was served from, so on most loads it lands well inside that and
    // this line is never painted - and a sentence that appears for two frames
    // on the way to a screen is a flicker, not a status.
    return (
      <div className="gate">
        <div className="prose wait">Loading Panorama…</div>
      </div>
    )
  }

  if (!session) {
    return <SignIn />
  }

  // A setup session gets a different, much smaller application: no sidebar, no
  // screens, nothing but what it exists to configure. The server refuses
  // everything else regardless - a console that merely hid the screens would be
  // one bypass away from being the control it is not - so this is about not
  // offering what would only 403.
  if (session.user.kind === 'setup') {
    return <SetupConsole theme={theme} />
  }

  const current = screenAt(path)
  const title = current?.title ?? 'Not found'

  return (
    <ChromeProvider>
      <div className="shell">
        <Sidebar path={path} theme={theme} />
        <Main title={title}>
          {current ? current.element() : <NotFound />}
        </Main>
      </div>
    </ChromeProvider>
  )
}

/**
 * SetupConsole is what a redeemed setup code sees.
 *
 * Only what the session can actually do: the identity connection and the
 * model providers. Not the first-run wizard - its later steps link to Routing,
 * People, Budgets and Integrations, which a setup session is refused from, so
 * rendering it here produced a page of controls that navigated nowhere.
 *
 * Ending it revokes the session rather than leaving it to expire, because a
 * live setup session in a closed tab is the failure mode this whole mechanism
 * exists not to hand anyone.
 */
function SetupConsole({ theme }: { theme: Theme }) {
  const { signOut } = useSession()
  const setup = useApi<Setup>('/api/v1/setup')
  const provider = setup.data?.steps.find((step) => step.id === 'provider')

  useEffect(() => {
    document.title = 'Set up Panorama'
  }, [])

  return (
    <div className="main" style={{ maxWidth: 860, margin: '0 auto' }}>
      <header className="topbar ruled">
        <div className="topbar-title">
          <div className="topbar-kicker">setup session</div>
          <h1>Set up Panorama</h1>
        </div>
        <div className="topbar-actions">
          <button
            type="button"
            className="theme-switch"
            onClick={theme.toggle}
            aria-label={theme.current === 'light' ? 'Switch to the dark theme' : 'Switch to the light theme'}
          >
            {theme.current === 'light' ? 'Dark' : 'Light'}
          </button>
          <button type="button" className="btn small" onClick={() => void signOut()}>
            End session
          </button>
        </div>
      </header>

      <main className="page">
        <div className="callout" style={{ marginBottom: 'var(--band-gap)' }}>
          <div className="icon-tile"><Icon name="info" /></div>
          <div className="prose tight">
            This session was opened with a one-time code. It is not an account, it expires in an
            hour, and it configures the two things a deployment cannot work without: how people
            sign in, and where models are invoked. The first is environment in your deployment; the
            second is a region per provider, saved here. Everything else - which models engineers
            may use, teams, budgets, integrations - is an administrator's decision afterwards,
            stored in the database and changeable without a redeploy. Set single sign-on up, end the session, and sign in through your identity
            provider; the first account is created by that sign-in, not here.
          </div>
        </div>

        <div className="stack">
          <IdentitySettings setup />

          {provider && (
            <Card
              title="Model providers"
              sub="Where this gateway invokes models, and proof that it can."
            >
              <ProviderStep detail={provider.detail} />
            </Card>
          )}

          <SetupAdministrators />
        </div>
      </main>
    </div>
  )
}

/**
 * SetupAdministrators is who administers the deployment, from a setup session.
 *
 * Two states, and the empty one carries the more important message. Before
 * anybody has signed in there is nobody to promote, and what a reader needs to
 * know is that the first person through will own the deployment - because that
 * is a consequence they can still control, by being that person.
 *
 * Once there are accounts, this exists because "first sign-in owns it" can seat
 * the wrong person. A setup session could already grant admin by mapping a
 * group to it; this is the same power by a more direct route.
 */
function SetupAdministrators() {
  const { data, loading, error, reload } = useApi<SetupAdmins>('/api/v1/setup/administrators')
  const [failure, setFailure] = useState<unknown>(null)

  const setRole = useCallback(
    async ({ userID, role }: { userID: string; role: string }) => {
      setFailure(null)
      try {
        await patch('/api/v1/setup/administrators', { user_id: userID, role })
        reload()
      } catch (cause) {
        setFailure(cause)
        throw cause
      }
    },
    [reload],
  )

  if (loading) return null

  const users = data?.users ?? []

  if (users.length === 0) {
    return (
      <Card title="Who will administer this" sub="Nobody has signed in yet.">
        <div className="prose">
          The first person to sign in through your identity provider owns this deployment - they
          become an administrator regardless of the group mapping, because otherwise a freshly
          configured deployment would have nobody who could finish setting it up. That window closes
          the moment it is used.
        </div>
        <div className="prose" style={{ marginTop: 12 }}>
          So sign in yourself first. If somebody else gets there before you, a fresh{' '}
          <span className="mono">panorama admin setup</span> code can put it right from this screen.
        </div>
      </Card>
    )
  }

  return (
    <Card
      title="Administrators"
      sub="Who may configure this deployment from the console."
    >
      <ErrorBanner error={failure ?? error} />
      <div className="list" style={{ gap: 10 }}>
        {users.map((u) => (
          <div key={u.id} className="list-item-head">
            <div style={{ minWidth: 0 }}>
              <div className="truncate">{u.display_name || u.email}</div>
              <div className="prose tight mono">{u.email}</div>
            </div>
            <div className="inline">
              <Pill tone={u.role === 'admin' ? 'good' : 'off'}>{u.role}</Pill>
              <ActionButton
                onAct={async () =>
                  setRole({ userID: u.id, role: u.role === 'admin' ? 'developer' : 'admin' })
                }
                label={u.role === 'admin' ? 'Remove admin' : 'Make admin'}
                settled={u.role === 'admin' ? 'Removed' : 'Granted'}
                small
              />
            </div>
          </div>
        ))}
      </div>
      <div className="card-foot">
        <span>
          A role set here is recorded as your decision, so their next sign-in does not recompute it
          from the group mapping - this is how you get an administrator back when nothing maps to
          one yet. Once your groups are arriving, map one to admin and hand the account back from
          People; a mapping is what keeps working when somebody changes team.
        </span>
      </div>
    </Card>
  )
}

function Sidebar({ path, theme }: { path: string; theme: Theme }) {
  const { session, signOut } = useSession()
  const user = session!.user

  const visible = (screen: Screen) =>
    (!screen.leaders || user.can_see_everyone) && (!screen.admin || user.is_admin)

  return (
    <aside className="sidebar">
      {/* The brand is inside the scroll container because it is the top of the
          navigation rather than a header above it. What stays pinned is the
          block below - who is signed in, and how to stop being. */}
      <nav className="nav">
        <div className="brand">
          <span className="brand-name">Panorama</span>
        </div>

        <div className="nav-group">
          <div className="nav-label">Organization</div>
          {ORGANIZATION.filter(visible).map((screen) => (
            <NavItem key={screen.path} screen={screen} active={screen.path === path} />
          ))}
        </div>

        <div className="nav-group">
          {/* The grouping is the trust model made visible: the personal screens
              are the only place an individual's numbers appear, and no
              organization screen ranks people against each other. */}
          <div className="nav-label">Personal</div>
          {PERSONAL.map((screen) => (
            <NavItem key={screen.path} screen={screen} active={screen.path === path} />
          ))}
        </div>
      </nav>

      <div className="sidebar-foot">
        <SetupNudge />
        {/* WHICH THEME IS RUNNING IS A PREFERENCE, NOT A PAGE ACTION. In the
            header it sat beside the range picker and the export button - two
            controls that change what the screen is showing - and read as a
            third one. Down here it is next to the account it actually belongs
            to, and the header is left holding only things that change the
            data. */}
        <button
          type="button"
          className="theme-switch"
          onClick={theme.toggle}
          aria-label={
            theme.current === 'light' ? 'Switch to the dark theme' : 'Switch to the light theme'
          }
        >
          <span className="dot" aria-hidden />
          {theme.current === 'light' ? 'Dark' : 'Light'}
        </button>
        <div className="sidebar-user">
          <Avatar name={user.display_name || user.email} />
          <div style={{ minWidth: 0, flex: 1 }}>
            <div className="truncate sidebar-user-name">{user.display_name || user.email}</div>
            <div className="sidebar-user-role">{user.role}</div>
          </div>
          <button type="button" className="btn small" onClick={() => void signOut()}>
            Sign out
          </button>
        </div>
      </div>
    </aside>
  )
}

function NavItem({ screen, active }: { screen: Screen; active: boolean }) {
  return (
    <Link to={screen.path} className={active ? 'nav-item active' : 'nav-item'}>
      {screen.label}
    </Link>
  )
}

/** SetupNudge is the sidebar's one piece of state: an administrator with
 *  unfinished setup should not have to remember where the wizard was. */
function SetupNudge() {
  const { session } = useSession()
  const { data } = useApi<Setup>(session?.user.is_admin ? '/api/v1/setup' : null)

  if (!data || data.complete || data.dismissed) return null

  return (
    <Link to="/welcome" className="deployment-chip">
      <span className="dot accent" />
      <span>
        setup · {data.remaining} step{data.remaining === 1 ? '' : 's'} left
      </span>
    </Link>
  )
}

/**
 * Main is the chrome around whichever screen is showing: the header above it
 * and the footer below it.
 *
 * The deployment is asked for once here rather than by each of them - `useApi`
 * holds no cache, so two callers would be two requests for the same two strings
 * on every navigation. It is also the endpoint a developer is refused, which is
 * why nothing but the kicker depends on it: the console still renders, with one
 * small line missing.
 */
function Main({ title, children }: { title: string; children: ReactNode }) {
  const { data } = useApi<{
    settings: Record<string, string>
    deployment: { providers: ProviderSummary[]; driver: string }
  }>('/api/v1/settings')
  const org = data?.settings['org.name']
  // Where this deployment's models are invoked. One region when every serving
  // provider shares it, and each region once when they do not - the kicker is
  // a fact about the deployment, not a list of its configuration.
  const region = [
    ...new Set((data?.deployment.providers ?? []).filter((p) => p.serving).map((p) => p.region)),
  ].join(' · ')

  return (
    <div className="main">
      <Topbar title={title} org={org} region={region} />
      {children}
      <Footer />
    </div>
  )
}

/**
 * Topbar opens a screen rather than labelling it.
 *
 * The deployment is a kicker above the title instead of a breadcrumb beside it,
 * and the title is a real heading at the size a heading is set - which is the
 * whole of the change. A screen supplies its own sub-line and controls through
 * the chrome context; everything here is the same on all ten.
 */
function Topbar({ title, org, region }: { title: string; org?: string; region?: string }) {
  const { sub, actions, flush } = useChrome()
  const kicker = [org, region].filter(Boolean).join(' · ')

  useEffect(() => {
    document.title = org ? `${title} · ${org} · Panorama` : `${title} · Panorama`
  }, [title, org])

  return (
    <header className={flush ? 'topbar' : 'topbar ruled'}>
      <div className="topbar-title">
        {kicker && <div className="topbar-kicker">{kicker}</div>}
        <h1>{title}</h1>
        {sub && <div className="topbar-sub">{sub}</div>}
      </div>
      <div className="topbar-actions">{actions}</div>
    </header>
  )
}

/**
 * Footer closes every screen with the two things only it can say.
 *
 * The screen's name, the organization and the region are in the header at the
 * other end of the page, so they are not repeated here. What is left is who
 * made this and which build is running, and the version comes off
 * the session so a developer sees the same one their administrator does.
 *
 * The wordmark is the project's home and is written as one, which is why it is
 * dot-separated rather than a name with an attribution in brackets: it reads as
 * the address it goes to.
 *
 * Source is the running build's source at its commit, from the session. It is
 * here, on every screen and for every role, because AGPL-3.0 section 13 offers
 * the source to everybody who uses a deployment over the network, and a fork
 * running modified code points it at its own repository at build time rather
 * than editing a screen.
 *
 * THE ONLY TWO OUTBOUND LINKS IN THE CONSOLE, and they load nothing - an
 * air-gapped deployment renders them exactly like any other, and a reader who
 * clicks one there gets their browser's own failure rather than a screen that
 * waited on it. New tab, so a click never costs somebody the filters they had
 * set.
 */
function Footer() {
  const { session } = useSession()
  return (
    <footer className="register">
      <a href="https://panorama.dnx.ai" target="_blank" rel="noreferrer">
        Panorama·DNX·AI
      </a>
      <span className="register-build">
        {session?.source && (
          <a href={session.source} target="_blank" rel="noreferrer">
            Source
          </a>
        )}
        <span>{session?.version ?? ''}</span>
      </span>
    </footer>
  )
}

function NotFound() {
  return (
    <main className="page">
      <div className="card">
        <div className="card-title">There is no such screen</div>
        <div className="card-sub">
          <Link to="/">Back to analytics</Link>
        </div>
      </div>
    </main>
  )
}
