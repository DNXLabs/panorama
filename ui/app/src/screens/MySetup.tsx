// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useMemo, useState } from 'react'
import { usePageChrome } from '../chrome'
import { del, get, post } from '../api'
import { ago, dateTime } from '../format'
import { useSession } from '../session'
import type { GrantRow, Onboarding, TokenRow, ToolRow, ToolSetup } from '../types'
import {
  ActionButton,
  CopyBlock,
  Empty,
  ErrorBanner,
  Loading,
  Pill,
  Segmented,
  Select,
  useApi,
} from '../ui'

/**
 * My setup is where a developer points a coding tool at this gateway.
 *
 * CONFIGURATION AND INSTRUCTIONS ARE SEPARATE THINGS, and the screen is built
 * on that distinction. The top of the left column is what the account is set
 * to - the routes, the tool, how the credential arrives - and it is the only
 * part anybody changes. Everything under it is derived: the numbered steps are
 * regenerated from those three answers and are never independently editable,
 * which is why there is no "which delivery" control among them. The right-hand
 * column is the third thing, and the only one the reader does not author:
 * proof the setup landed.
 *
 * Not tabs (shell, this machine, this project, sign in): four ways to say one
 * thing make the reader decide which applies to them before they can read any
 * of it, and the answer is always the same two questions - which tool, and how
 * does the credential get there. So the screen asks those two directly and
 * prints one set of steps.
 */

/** How the credential reaches the tool, which decides the *set* of steps rather
 *  than only their contents. */
type Credential = 'signin' | 'manual'

const CREDENTIALS: { id: Credential; label: string }[] = [
  { id: 'signin', label: 'Sign in' },
  { id: 'manual', label: 'Manual token' },
]

/** The manual half of the credential note. The sign-in half is the deployment's
 *  own `cli.why`, because it differs per tool and the difference is the honest
 *  part: Claude Code rotates in place, opencode mints per shell.
 *
 *  The deadline is in the sentence rather than only in the panel on the right.
 *  A minted token expires, nothing warns anybody when it does, and on a
 *  deployment with command-line sign-in switched off this is the only credential
 *  there is - so the day it stops working is the one fact this note owes the
 *  reader. The number comes from the server, which is where the lifetime is
 *  decided. */
function manualNote(lifetimeDays: number): string {
  return (
    'A minted token placed by hand. It expires ' +
    `${lifetimeDays} days after you mint it, and nothing rotates it for you - ` +
    'come back here, mint another, and replace it.'
  )
}

/** Whether a token has run out on its own. Revocation is the other way one
 *  dies and is recorded; this one just happens, so it is worked out here from
 *  the deadline the server served. */
function expired(token: TokenRow): boolean {
  return Boolean(token.expires_at) && new Date(token.expires_at!).getTime() <= Date.now()
}

/** The variables that hold a route name, in the order the blocks show them. */
const PRIMARY_VAR = 'ANTHROPIC_MODEL'
const FAST_VAR = 'ANTHROPIC_DEFAULT_HAIKU_MODEL'
const TOKEN_VAR = 'ANTHROPIC_AUTH_TOKEN'

/** What the deployment writes where a token goes, in every block it serves. */
const TOKEN_PLACEHOLDER = 'pnt_YOUR_TOKEN'

/** What the deployment calls itself inside opencode's configuration: one block
 *  for routes on models that speak the Messages API, one for routes on models
 *  that speak Responses, and one for routes on models that speak only Chat
 *  Completions. The same strings the server renders, and the prefix on both
 *  route-carrying keys. The Messages block is last because it is the answer
 *  when no other block declares a route. */
const OPENCODE_PROVIDERS = ['panorama-openai', 'panorama-chat', 'panorama']

/** Codex, which takes one model and a config.toml rather than a pair and a
 *  JSON block. */
const CODEX = 'codex-cli'

export function MySetup() {
  const setup = useApi<ToolSetup>('/api/v1/setup/tools')
  const tokens = useApi<{ tokens: TokenRow[]; lifetime_days: number }>('/api/v1/tokens')
  const verified = useVerification()
  // Whether this deployment serves `panorama login` at all. Off in this
  // release, and the server has already dropped every tool's `cli` block, so
  // the credential question below answers itself - this is only what decides
  // whether the machines panel is a panel or an empty one explaining a feature
  // nobody here can use.
  const cliSignIn = useSession().session?.cli_sign_in ?? false

  const [minted, setMinted] = useState('')
  const [error, setError] = useState<unknown>(null)

  const supported = useMemo(
    () => (setup.data?.tools ?? []).filter((t) => t.supported),
    [setup.data],
  )
  const [toolId, setToolId] = useState('claude-code')
  const [credential, setCredential] = useState<Credential>('signin')

  const tool = supported.find((t) => t.id === toolId) ?? supported[0]
  const routes = tool?.routes ?? []
  const routeKey = routes.join(',')

  // The two route names, held here rather than per tool: they are what the
  // account is set to, and the same pair configures either client. Seeded from
  // what the deployment already chose, and re-seeded when the answer changes -
  // an administrator adding a route this person may use, or a tool whose
  // conditions admit a different set of them. A name that survives a tool
  // switch is kept, because switching tool is not a decision about routes.
  //
  // The reader's own pick is remembered in this browser, so coming back to the
  // screen shows the routes they configured rather than the deployment's
  // default - the steps below are what they pasted, and a screen that forgot
  // would print a different block from the one on their laptop. A remembered
  // name the reader may no longer use falls back to the served one.
  const [main, setMain] = useState(() => storedRoutes().main)
  const [background, setBackground] = useState(() => storedRoutes().background)
  useEffect(() => {
    if (!tool) return
    const [servedMain, servedBackground] = servedRoutes(tool)
    setMain((current) => (routes.includes(current) ? current : servedMain))
    setBackground((current) => (routes.includes(current) ? current : servedBackground))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tool?.id, routeKey])
  useEffect(() => {
    if (main || background) storeRoutes({ main, background })
  }, [main, background])

  // Sign in is offered only where the deployment says the CLI path works, and
  // is the recommendation everywhere it does - so it is also the default, and
  // is given up only when the tool in hand has no CLI path. Asking instead
  // whether the current choice is still in the list would leave the screen on
  // manual forever: manual is always in it, so nothing would ever move back.
  //
  // `cli` is absent from every tool on a deployment with command-line sign-in
  // switched off, which is this release: the list is then one long, and the
  // control that would offer the choice is not drawn at all.
  const credentials = CREDENTIALS.filter((c) => c.id !== 'signin' || tool?.cli)
  useEffect(() => {
    if (tool && !tool.cli && credential === 'signin') setCredential('manual')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tool?.id])

  const mint = async () => {
    setError(null)
    try {
      const created = await post<{ token: string }>('/api/v1/tokens', {
        name: 'console',
      })
      setMinted(created.token)
      tokens.reload()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  const revoke = async (id: string) => {
    setError(null)
    try {
      await del(`/api/v1/tokens/${id}`)
      tokens.reload()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  const receiving = verified.state?.has_request ?? false

  // The header pill and the Verify panel's pill are the same fact read twice,
  // and they must never disagree - so they are the same state, not two polls.
  usePageChrome(
    {
      actions: (
        <>
          <span className="band-kicker">Gateway</span>
          <Pill tone={receiving ? 'good' : 'warn'}>{receiving ? 'receiving' : 'waiting'}</Pill>
        </>
      ),
    },
    [receiving],
  )

  const token = minted || (setup.data?.token_placeholder ?? TOKEN_PLACEHOLDER)
  const banner = error ?? setup.error

  return (
    <main className="bands">
      <section className="band split setup last" style={{ ['--split-left' as string]: '1.62fr' }}>
        <div className="setup-main">
          {banner ? (
            <div className="setup-lead">
              <ErrorBanner error={banner} />
            </div>
          ) : null}

          {setup.loading || !tool ? (
            <Loading rows={5} />
          ) : (
            <>
              <section className="setup-config">
                <div className="setup-lead">
                  <span className="band-kicker">Configuration</span>
                  <span className="band-sub">
                    What your account is set to. Changing anything here rewrites the instructions
                    below.
                  </span>
                </div>

                <div className="setup-cell">
                  <span className="band-kicker">Routes</span>
                  {routes.length === 0 ? (
                    <span className="setup-note">
                      No route has been created that you may use, so the blocks below carry a
                      placeholder where a route name goes. An administrator creates routes on the
                      Routing screen.
                    </span>
                  ) : (
                    <>
                      <div className="setup-routes">
                        <label htmlFor="route-everyday">Everyday work</label>
                        <Select
                          id="route-everyday"
                          className="input setup-select"
                          value={main}
                          label="Everyday work"
                          onChange={setMain}
                          options={routes.map((route) => ({ value: route, label: route }))}
                        />
                        {/* Codex takes one model. A second select would be a
                            choice that changed nothing in any block below. */}
                        {tool.id !== CODEX && (
                          <>
                            <label htmlFor="route-background">Background and subagents</label>
                            <Select
                              id="route-background"
                              className="input setup-select"
                              value={background}
                              label="Background and subagents"
                              onChange={setBackground}
                              options={routes.map((route) => ({ value: route, label: route }))}
                            />
                          </>
                        )}
                      </div>
                      <span className="setup-note">
                        A route, never a model id. An administrator decides which model each one
                        runs on and can change it without you touching anything.
                        {tool.id === CODEX
                          ? ' Only routes whose model speaks the Responses API are listed, because that is the only API Codex speaks.'
                          : ' Background is not optional - your tool asks for the fast tier on its own.'}
                      </span>
                    </>
                  )}
                </div>

                <div className="setup-cell">
                  <span className="band-kicker">Tool</span>
                  <Segmented
                    value={tool.id}
                    options={supported.map((t) => ({
                      id: t.id,
                      label: t.name,
                    }))}
                    onChange={setToolId}
                    label="Which tool"
                  />
                  <span className="setup-note">{tool.note}</span>
                </div>

                <div className="setup-cell">
                  <span className="band-kicker">Credential</span>
                  {/* A control appears where there is a choice to make. With
                      command-line sign-in switched off there is one way a
                      credential reaches a tool, and a segmented control holding
                      a single pressed option is a question with one answer -
                      so the sentence stands on its own and says which way it
                      is. */}
                  {credentials.length > 1 && (
                    <Segmented
                      value={credential}
                      options={credentials}
                      onChange={setCredential}
                      label="How the credential reaches it"
                    />
                  )}
                  <span className="setup-note">
                    {credential === 'signin' && tool.cli
                      ? `${tool.cli.recommended ? 'Recommended. ' : ''}${tool.cli.why}`
                      : manualNote(tokens.data?.lifetime_days ?? 30)}
                  </span>
                </div>
              </section>

              <Instructions
                tool={tool}
                credential={credential}
                main={main}
                background={background}
                token={token}
              />
            </>
          )}

          <section className="setup-close">
            <p className="footnote accent">
              A token acts as you. Nobody, including an administrator, can mint one on your behalf -
              recovering access is revoke-and-mint-your-own, not mint-for-somebody-else.
            </p>
          </section>
        </div>

        <div className="setup-side">
          <Verify {...verified} />

          <section className="setup-panel">
            <div className="band-head-row">
              <div className="band-head">
                <span className="band-title">Gateway tokens</span>
                <span className="band-sub">
                  Shown once, stored only as a hash, and good for{' '}
                  {tokens.data ? tokens.data.lifetime_days : 30} days. Revoking is permanent - mint
                  a new one rather than un-revoking one that may have leaked.
                </span>
              </div>
              <ActionButton onAct={mint} label="Mint" settled="Minted" small />
            </div>

            {minted && (
              <div>
                <CopyBlock label="copy this now - it is not shown again" text={minted} />
              </div>
            )}

            {tokens.loading ? (
              <Loading rows={2} />
            ) : (tokens.data?.tokens.length ?? 0) === 0 ? (
              <div className="setup-frame">
                <Empty title="No tokens yet">Mint one when the steps ask for it.</Empty>
              </div>
            ) : (
              <div className="list">
                {tokens.data!.tokens.map((row) => (
                  <div key={row.id} className="list-item-head">
                    <div style={{ minWidth: 0 }}>
                      <div className="inline" style={{ gap: 8 }}>
                        <span className="mono cell-soft">{row.prefix}…</span>
                        {row.revoked ? (
                          <Pill tone="off">revoked</Pill>
                        ) : expired(row) ? (
                          <Pill tone="off">expired</Pill>
                        ) : row.used ? (
                          <Pill tone="good">in use</Pill>
                        ) : (
                          <Pill tone="warn">never used</Pill>
                        )}
                      </div>
                      {/* When it dies is the fact this row is for. A token
                          nobody revoked still stops working, and the day it
                          does is the one thing the list can say ahead of
                          time. */}
                      <div className="cell-sub">
                        {row.name} · created {dateTime(row.created_at)}
                        {row.used ? ` · last used ${ago(row.last_used_at)}` : ''}
                        {row.expires_at
                          ? ` · ${expired(row) ? 'expired' : 'expires'} ${dateTime(row.expires_at)}`
                          : ''}
                      </div>
                    </div>
                    {!row.revoked && (
                      <ActionButton
                        onAct={() => revoke(row.id)}
                        label="Revoke"
                        settled="Revoked"
                        small
                      />
                    )}
                  </div>
                ))}
              </div>
            )}
          </section>

          {/* Only where there is such a thing as a signed-in machine. With the
              flow switched off nothing can ever appear in it, and a permanently
              empty panel telling a developer to run a command this deployment
              refuses is the screen's one piece of advice that does not work. */}
          {cliSignIn && <SignedInMachines />}
        </div>
      </section>
    </main>
  )
}

/* ------------------------------------------------------------ instructions --
 *
 * Derived output. Nothing here is a control, and nothing here is stored: what
 * the reader chose above decides which steps exist and what is in them, and
 * every block already carries their routes.
 */

type Block = { heading?: string; label?: string; text: string }
type Step = {
  title: string
  note: string
  optional?: boolean
  blocks: Block[]
}

function Instructions({
  tool,
  credential,
  main,
  background,
  token,
}: {
  tool: ToolRow
  credential: Credential
  main: string
  background: string
  token: string
}) {
  const steps = buildSteps({ tool, credential, main, background, token })

  return (
    <>
      <div className="setup-lead tight">
        <span className="band-kicker">Instructions</span>
        <span className="band-sub">
          For {tool.name} with {credential === 'signin' ? 'sign-in' : 'a minted token'}, on this
          machine. Every block below already carries your routes.
        </span>
      </div>

      {steps.map((step, index) => (
        <section key={step.title} className="setup-step">
          <span className="setup-n" aria-hidden>
            {String(index + 1).padStart(2, '0')}
          </span>
          <div className="setup-body">
            <div className="setup-step-head">
              <span className="setup-title">
                {step.title}
                {step.optional && <span className="setup-tag">Optional</span>}
              </span>
              <span className="band-sub">{step.note}</span>
            </div>
            {step.blocks.map((block, at) => (
              <div key={at} className="setup-block">
                {block.heading && <span className="band-kicker">{block.heading}</span>}
                <CopyBlock label={block.label} text={block.text} />
              </div>
            ))}
          </div>
        </section>
      ))}
    </>
  )
}

function buildSteps({
  tool,
  credential,
  main,
  background,
  token,
}: {
  tool: ToolRow
  credential: Credential
  main: string
  background: string
  token: string
}): Step[] {
  const path = tool.path ?? ''
  const out: Step[] = []
  // Said where the file already carries the MCP server, so the entry is not a
  // surprise to somebody reading what they are about to paste.
  const search = carriesSearch(tool) ? " The file also adds this gateway's web_search tool." : ''

  // Codex reads its token from a variable the file names, and has no command
  // that writes the file - so signing in is a login, the file, and a shell that
  // mints a token, and pasting a token is the file and a shell that exports it.
  if (tool.id === CODEX) {
    const config = codexConfig(tool, main)
    if (credential === 'signin' && tool.cli) {
      out.push({
        title: 'Sign this machine in',
        note: 'Opens a browser once. The credential lands in your keychain; each shell below mints a short-lived token from it.',
        blocks: [{ label: 'Shell', text: tool.cli.login }],
      })
      out.push({
        title: 'Add the gateway to your Codex config',
        note: `Goes in ${path}, beside anything already there. Nothing in it is a credential - Codex reads the token from the variable it names.${search}`,
        blocks: [{ label: path, text: config }],
      })
      out.push({
        title: 'Start Codex with a fresh token',
        note: 'Run in each new shell. The token is minted on the spot, so nothing long-lived is written to disk.',
        blocks: [{ label: 'Shell', text: tool.cli.configure }],
      })
    } else {
      out.push({
        title: 'Mint a token',
        note: 'Use Mint on the right. It is shown once - copy it before you leave the page, and treat it as a password from then on.',
        blocks: [],
      })
      out.push({
        title: 'Point Codex at the gateway',
        note: `Codex is configured by a file, and reads the token from the variable that file names. The file goes in ${path}.${search}`,
        blocks: [
          { label: path, text: config },
          { label: 'Shell', text: withToken(tool.shell ?? '', token) },
        ],
      })
    }
    return out
  }

  if (credential === 'signin' && tool.cli) {
    out.push({
      title: 'Sign this machine in',
      note: 'Opens a browser once. The credential lands in your keychain and rotates on its own; nothing is written into a file you have to protect.',
      blocks: [{ label: 'Shell', text: tool.cli.login }],
    })
    out.push({
      title: `Write the ${tool.name} config`,
      note: `Generated from the configuration above and written to ${path}. Nothing in it is a credential - the token is fetched at request time.${search}`,
      blocks: [
        { label: 'Shell', text: tool.cli.configure },
        {
          heading: 'Result',
          label: path,
          text: signedInConfig(tool, main, background),
        },
      ],
    })
  } else {
    out.push({
      title: 'Mint a token',
      note: 'Use Mint on the right. It is shown once - copy it before you leave the page, and treat it as a password from then on.',
      blocks: [],
    })
    out.push({
      title: `Point ${tool.name} at the gateway`,
      // opencode has no environment-variable form, so it is told what it has
      // rather than offered a choice it does not get. The provider block is
      // what declares each route as a model with its context window beside it;
      // variables pointed at the gateway would carry none of that.
      note: tool.env
        ? `Either form works. Environment variables live with your shell session; the config file applies to every ${tool.name} run on this machine.`
        : `${tool.name} is configured by a file. There is no environment-variable form - the provider block is what declares each route as a model, with its context window and output cap beside it.${search}`,
      blocks: tool.env
        ? [
            {
              heading: 'Either - environment variables',
              label: 'Shell',
              text: exports(tool, main, background, token),
            },
            {
              heading: 'Or - config file',
              label: path,
              text: manualConfig(tool, main, background, token),
            },
          ]
        : [{ label: path, text: manualConfig(tool, main, background, token) }],
    })
  }

  // Web search, where the deployment offers it. opencode and Codex carry the
  // MCP server inside the file the step above already wrote; Claude Code adds
  // it with a command, and has its own WebSearch denied, because that tool is
  // Anthropic's server-side one and the gateway's providers refuse it.
  if (tool.mcp) {
    out.push({
      title: 'Add web search',
      note: `Registers this gateway's web_search tool with ${tool.name} for every project, and turns off the built-in WebSearch, which cannot work through the gateway. The deny rule goes in ${path}.`,
      blocks: [
        { label: 'Shell', text: withToken(tool.mcp.command, token) },
        { heading: 'And', label: path, text: JSON.stringify(tool.mcp.settings, null, 2) },
      ],
    })
  }

  // Offered where the deployment offers it, which is Claude Code alone.
  // opencode's headers are static strings in a file, so a repository captured
  // when the file was written is wrong at the next checkout - and a step that
  // stamped the wrong branch would be worse than the structural attribution
  // that already covers it.
  if (tool.stamping?.shell) {
    out.push({
      title: 'Stamp the precision tier',
      optional: true,
      note: tool.stamping.why,
      blocks: [{ label: 'Shell', text: tool.stamping.shell }],
    })
  }

  return out
}

/** The route names the deployment picked, before the reader changes them.
 *
 *  Read out of whichever shape this tool is configured in - the environment
 *  rows for Claude Code, the rendered provider block for opencode - rather
 *  than defaulted to the first route. Which route is the everyday one and
 *  which is the fast tier is the server's judgement, made by looking at the
 *  model behind each name, and the console is not in a position to repeat it. */
function servedRoutes(tool: ToolRow): [string, string] {
  const env = tool.env ?? []
  const value = (name: string) => env.find((e) => e.name === name)?.value ?? ''
  if (value(PRIMARY_VAR)) return [value(PRIMARY_VAR), value(FAST_VAR)]

  if (tool.id === CODEX) {
    const chosen = /^model = "([^"]*)"$/m.exec(tool.config ?? '')
    return [chosen?.[1] ?? '', '']
  }

  const parsed = parseConfig(tool.config)
  if (parsed) return [routeOf(parsed.model), routeOf(parsed.small_model)]
  return ['', '']
}

/** Where this browser keeps the reader's two routes. Per browser rather than
 *  per account, which is right for a choice about the laptop in front of them;
 *  an absent or unreadable entry is just the deployment's default. */
const ROUTES_KEY = 'panorama.setup.routes'

function storedRoutes(): { main: string; background: string } {
  try {
    const parsed: unknown = JSON.parse(localStorage.getItem(ROUTES_KEY) ?? 'null')
    if (parsed && typeof parsed === 'object') {
      const { main, background } = parsed as Record<string, unknown>
      return {
        main: typeof main === 'string' ? main : '',
        background: typeof background === 'string' ? background : '',
      }
    }
  } catch {
    // Storage blocked, or an entry this build did not write.
  }
  return { main: '', background: '' }
}

function storeRoutes(routes: { main: string; background: string }) {
  try {
    localStorage.setItem(ROUTES_KEY, JSON.stringify(routes))
  } catch {
    // Storage blocked: the choice lasts as long as the page.
  }
}

/** Codex's config.toml for the route chosen. The server renders one per route,
 *  because the window and output cap in it are that route's and the browser has
 *  no way to compute them; the default rendering is what is shown before a
 *  route exists. */
function codexConfig(tool: ToolRow, main: string): string {
  return (main && tool.configs?.[main]) || tool.config || ''
}

/** A served shell line with the reader's token where the placeholder was. Split
 *  and joined rather than replaced, because a token is an opaque string and a
 *  replacement pattern reads `$&` in one as an instruction. */
function withToken(shell: string, token: string): string {
  return shell.split(TOKEN_PLACEHOLDER).join(token)
}

type OpenCodeConfig = {
  model?: string
  small_model?: string
  provider?: Record<string, { options?: { apiKey?: string }; models?: Record<string, unknown> }>
  /** The gateway's MCP server, present while web search is on. */
  mcp?: Record<string, { headers?: Record<string, string> }>
}

/** Whether a file-configured tool's rendering declares the gateway's MCP
 *  server. Claude Code's arrives as its own step instead. */
function carriesSearch(tool: ToolRow): boolean {
  if (!tool.config) return false
  if (tool.id === CODEX) return tool.config.includes('[mcp_servers.')
  return Boolean(parseConfig(tool.config)?.mcp)
}

/** Reads the deployment's own rendering of a file-configured tool.
 *
 *  It is JSON the server built and this is the only thing that reads it, so a
 *  parse failure is a bug in the renderer rather than a state to design for:
 *  it is logged, and the block is still shown exactly as it arrived, because
 *  what the server sent is the one honest thing left to show. */
function parseConfig(config?: string): OpenCodeConfig | null {
  if (!config) return null
  try {
    return JSON.parse(config) as OpenCodeConfig
  } catch (cause) {
    console.error('the deployment sent a configuration that is not valid JSON', cause)
    return null
  }
}

function routeOf(id?: string): string {
  const value = id ?? ''
  for (const prefix of OPENCODE_PROVIDERS) {
    if (value.startsWith(`${prefix}/`)) return value.slice(prefix.length + 1)
  }
  return value
}

/** A route as opencode names it: under the provider block that declares it,
 *  which the server chose by the API the route's model speaks. */
function qualified(parsed: OpenCodeConfig, route: string): string {
  const block =
    OPENCODE_PROVIDERS.find((name) => route in (parsed.provider?.[name]?.models ?? {})) ??
    OPENCODE_PROVIDERS[OPENCODE_PROVIDERS.length - 1]
  return `${block}/${route}`
}

/** The reader's routes - and, where they pasted one, their token - put into a
 *  block the server rendered.
 *
 *  Only the keys that name a route and the one that holds the credential are
 *  touched. Everything else in there - every route declared as a model, each
 *  with the smallest context window and output cap across the models it can
 *  reach - is arithmetic only the server can do, which is why the block is
 *  rendered there and edited here rather than assembled in the browser.
 *
 *  Edited through the parsed object rather than by pattern, because a token is
 *  an opaque string and a string replacement reads `$&` in one as an
 *  instruction. */
function personalise(config: string, main: string, background: string, token?: string): string {
  const parsed = parseConfig(config)
  if (!parsed) return config
  if (main) parsed.model = qualified(parsed, main)
  if (background) parsed.small_model = qualified(parsed, background)
  if (token) {
    for (const block of Object.values(parsed.provider ?? {})) {
      if (block.options) block.options.apiKey = token
    }
    // The MCP server carries the same credential, as a header.
    for (const server of Object.values(parsed.mcp ?? {})) {
      if (server.headers?.Authorization) server.headers.Authorization = `Bearer ${token}`
    }
  }
  return JSON.stringify(parsed, null, 2)
}

/** What `panorama configure` is about to write. Shown as well as run, because
 *  an engineer wants to see what a command will put in their settings file. */
function signedInConfig(tool: ToolRow, main: string, background: string): string {
  if (tool.cli?.config) return personalise(tool.cli.config, main, background)

  const settings = tool.cli?.settings
  if (!settings) return ''
  return JSON.stringify(
    {
      apiKeyHelper: settings.apiKeyHelper,
      env: Object.fromEntries(
        Object.entries(settings.env).map(([name, served]) => [
          name,
          chosen(name, served, main, background, ''),
        ]),
      ),
    },
    null,
    2,
  )
}

/** The same file with the credential in it, for somebody who pasted a token. */
function manualConfig(tool: ToolRow, main: string, background: string, token: string): string {
  if (tool.config) return personalise(tool.config, main, background, token)

  const env = tool.env ?? []
  return JSON.stringify(
    {
      env: Object.fromEntries(
        env.map((e) => [e.name, chosen(e.name, e.value, main, background, token)]),
      ),
    },
    null,
    2,
  )
}

function exports(tool: ToolRow, main: string, background: string, token: string): string {
  return (tool.env ?? [])
    .map((e) => `export ${e.name}=${chosen(e.name, e.value, main, background, token)}`)
    .join('\n')
}

/** One variable's value: the reader's route where a route goes, their token
 *  where the credential goes, and the deployment's answer everywhere else. */
function chosen(
  name: string,
  served: string,
  main: string,
  background: string,
  token: string,
): string {
  if (name === PRIMARY_VAR) return main || served
  if (name === FAST_VAR) return background || served
  if (name === TOKEN_VAR) return token || served
  return served
}

/* -------------------------------------------------------------- the proof --
 *
 * The right column, and the only part of the screen the reader does not write.
 */

/** useVerification polls "has it worked yet" for the person asking.
 *
 *  One poll for the whole screen: the header pill and the Verify panel are the
 *  same fact, and two subscriptions would eventually show it two ways. */
function useVerification() {
  const [state, setState] = useState<Onboarding | null>(null)
  const [error, setError] = useState<unknown>(null)

  useEffect(() => {
    let live = true
    const poll = async () => {
      try {
        const next = await get<Onboarding>('/api/v1/setup/onboarding')
        if (live) {
          setState(next)
          setError(null)
        }
      } catch (cause) {
        if (live) setError(cause)
      }
    }
    void poll()
    // Polled rather than pushed: the console is a page somebody leaves open
    // while they run one command in a terminal, and ten seconds is the right
    // amount of patience for that.
    const timer = window.setInterval(() => void poll(), 10_000)
    return () => {
      live = false
      window.clearInterval(timer)
    }
  }, [])

  return { state, error }
}

/**
 * Verify reports what Panorama read out of the latest request.
 *
 * The latest and not the first: the endpoint takes one row, newest first, so
 * "first" would be true for exactly one request per account. The latest is
 * also the better answer
 * to what the panel is asked - "is it working now" - on the day somebody
 * comes back to it after changing a route or minting a new token.
 *
 * Three figures rather than a list of detected fields, because they are the
 * three claims the setup makes and each one is either true or it is not: a
 * request arrived, the name the client asked for became a model, and the spend
 * was attached to work. Provenance is the one that carries the accent, because
 * it is the claim the product is actually for - and it says "not yet" rather
 * than nothing when there is no work item, since an unattributed request is a
 * fact about the session and not a gap in the screen.
 */
function Verify({ state, error }: { state: Onboarding | null; error: unknown }) {
  const receiving = state?.has_request ?? false
  const detected = state?.detected

  return (
    <section className="setup-panel">
      <div className="band-head-row">
        <div className="band-head">
          <span className="band-title">Verify</span>
          <span className="band-sub">Run your tool once. This updates on its own.</span>
        </div>
        <Pill tone={receiving ? 'good' : 'warn'}>{receiving ? 'receiving' : 'waiting'}</Pill>
      </div>

      <ErrorBanner error={error} />

      {!state ? (
        <Loading rows={2} />
      ) : !receiving ? (
        <div className="setup-frame">
          <Empty title="No request from you has reached the gateway yet">
            Work through the steps, start your tool, and ask it anything. The first request lands
            here within a second or two.
          </Empty>
        </div>
      ) : (
        <div className="setup-kpis">
          <Figure
            label="Latest request"
            value={ago(state.at)}
            note={detected?.tool || 'tool unidentified'}
          />
          <Figure
            label="Route resolved"
            value={detected?.route || '-'}
            note={detected?.model ? `to ${detected.model}` : 'no model recorded'}
          />
          <Figure
            label="Provenance"
            value={detected?.work_item?.rung ?? 'not yet'}
            note={provenanceNote(detected)}
            accent={Boolean(detected?.attributed)}
          />
        </div>
      )}
    </section>
  )
}

/** What the rung was read from, in the words the reader can act on. Stated
 *  rather than absorbed: a request nothing could be attributed to says so, and
 *  says what it did see. */
function provenanceNote(detected: Onboarding['detected']): string {
  if (detected?.attributed) return detected.describe || 'work item read from your session'
  const seen = [detected?.repo, detected?.branch].filter(Boolean).join(' · ')
  if (seen) return `${seen} - no work item in that branch name yet`
  return 'No repository was open when that request was made.'
}

function Figure({
  label,
  value,
  note,
  accent,
}: {
  label: string
  value: string
  note: string
  accent?: boolean
}) {
  return (
    <div>
      <div className="kpi-label">{label}</div>
      <div className={accent ? 'kpi-figure accent' : 'kpi-figure'}>{value}</div>
      <div className="kpi-note">{note}</div>
    </div>
  )
}

/**
 * SignedInMachines lists what `panorama login` has signed in.
 *
 * Separate from the token list, and deliberately so. A pasted token is a string
 * somebody is holding; a signed-in machine is a laptop, and the question asked
 * about it - "is that one still mine?" - is answered by a name and a last-used
 * time rather than by a prefix.
 *
 * Signing one out revokes the tokens it minted as well as the sign-in itself,
 * which is what makes this the control to reach for when a machine is lost. The
 * panel says so, because a button whose blast radius is larger than it looks is
 * a button people use too late.
 */
function SignedInMachines() {
  const grants = useApi<{ grants: GrantRow[] }>('/api/v1/grants')
  const [error, setError] = useState<unknown>(null)

  const signOut = async (id: string) => {
    setError(null)
    try {
      await del(`/api/v1/grants/${id}`)
      grants.reload()
    } catch (cause) {
      setError(cause)
      throw cause
    }
  }

  return (
    <section className="setup-panel">
      <div className="band-head">
        <span className="band-title">Signed-in machines</span>
        <span className="band-sub">
          What panorama login signed in. Signing one out also stops the token it is currently using,
          within the hour.
        </span>
      </div>

      <ErrorBanner error={error ?? grants.error} />

      {grants.loading ? (
        <Loading rows={2} />
      ) : (grants.data?.grants.length ?? 0) === 0 ? (
        <div className="setup-frame">
          <Empty title="No machines signed in">
            Run <span className="mono">panorama login</span> on a machine to sign it in. Your tools
            then get their credential from <span className="mono">panorama token</span>, and it
            rotates on its own.
          </Empty>
        </div>
      ) : (
        <div className="list">
          {grants.data!.grants.map((grant) => (
            <div key={grant.id} className="list-item-head">
              <div style={{ minWidth: 0 }}>
                <div className="inline" style={{ gap: 8 }}>
                  <span className="cell-strong truncate">{grant.name || 'a command line'}</span>
                  {grant.revoked ? (
                    <Pill tone="off">signed out</Pill>
                  ) : grant.used ? (
                    <Pill tone="good">in use</Pill>
                  ) : (
                    <Pill tone="warn">never used</Pill>
                  )}
                </div>
                <div className="cell-sub">
                  signed in {dateTime(grant.created_at)}
                  {grant.used ? ` · last refreshed ${ago(grant.last_used_at)}` : ''}
                  {grant.expires_at ? ` · expires ${dateTime(grant.expires_at)}` : ''}
                </div>
              </div>
              {!grant.revoked && (
                <ActionButton
                  onAct={() => signOut(grant.id)}
                  label="Sign out"
                  settled="Signed out"
                  small
                />
              )}
            </div>
          ))}
        </div>
      )}
    </section>
  )
}
