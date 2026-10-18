// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// The shapes the API actually returns.
//
// Written by hand rather than generated, and deliberately partial: only what
// the console reads is declared, so a field the server adds does not need a
// change here, and a field the console reads is one the compiler has checked
// exists in this file at least.
//
// Money crosses the wire twice - exact integer nanodollars to compute with, and
// a decimal string to display - and never as a JSON number. A float64 cannot
// hold a nanodollar total exactly, and a cost report that changes when it is
// parsed is not a cost report. Nothing here types a cost as `number` except the
// nanos, which are integers small enough to be exact.

export type Role = 'admin' | 'manager' | 'developer'

export type User = {
  id: string
  email: string
  display_name: string
  role: Role
  can_see_everyone: boolean
  is_admin: boolean
  /**
   * "user" for everyone the identity provider owns, "setup" for a redeemed
   * one-time code - which is not an account and has no id or email. The console
   * renders a different, much smaller application for the latter.
   */
  kind: 'user' | 'setup'
}

export type Session = {
  user: User
  sso_configured: boolean
  csrf_token?: string
  session_expires_at?: string
  /** Which build is running. The shell's footer names it on every screen, and
   *  it rides on this response because every screen already has it - the
   *  alternative was /settings, which a developer is refused. */
  version?: string
  /** The running build's source, at its commit. The footer links it: AGPL-3.0
   *  section 13 offers the source to everybody using a deployment over the
   *  network, and this is where that offer is made. */
  source?: string
  /** Whether this deployment serves command-line sign-in - `panorama login`,
   *  the approval screen, and the machines it signs in.
   *
   *  Here for the same reason the version is: My setup and the approval screen
   *  both need it, both are a developer's, and /settings is closed to one. One
   *  fact on one response, so no two screens can disagree about whether the
   *  CLI path works. Off in this release; the switch is on Settings. */
  cli_sign_in?: boolean
}

export type AuthState = {
  setup_required: boolean
  sso_configured: boolean
  base_url: string
  /** Whether this deployment has the development sign-in switched on. True only
   *  on a stack reachable from nowhere but the machine running it - the server
   *  refuses to start otherwise - so the gate may offer it plainly. */
  dev_sign_in?: boolean
  /** Whether a gateway token may be exchanged for a console session. True
   *  exactly while single sign-on is not configured: the server registers the
   *  route only then. */
  token_sign_in?: boolean
}

export type Cost = {
  total_nanos: number
  total_usd: string
  by_class: Record<string, { nanos: number; usd: string }>
}

export type Totals = {
  requests: number
  error_requests: number
  unpriced_requests: number
  unpriced_billable: number
  tokens: {
    input: number
    output: number
    cache_read: number
    cache_write: number
    cache_write_1h: number
  }
  /** Searches made through the gateway's MCP tool, billed per search. */
  web_searches: number
  cost: Cost
}

export type Bucket = Totals & {
  key: string
  name?: string
  members?: number
  /** How much of this bucket's spend reached a work item. Served only on the
   *  team rollup today, and from the same query as the total it is a share of
   *  - so a table never divides one request's numerator by another's
   *  denominator. */
  attributed_cost_nanos?: number
}

export type Usage = {
  since: string
  until: string
  scope: 'organization' | 'self'
  totals: Totals
  by_model: Bucket[]
  by_tool: Bucket[]
  by_day: Bucket[]
  by_team?: Bucket[]
  teams_overlap?: boolean
  teams_note?: string
}

export type WorkBucket = Totals & {
  key: string
  kind: string
  repo: string
  rung?: string
  sessions: number
  attributed: boolean
  label?: string
  /** What a system of record said, and absent when none has been asked. The
   *  absence is the point: "GitHub says this is open" and "nothing has looked
   *  this up" are different states and a screen has to be able to tell them
   *  apart. `status` is the source's own word and is for display; `outcome` is
   *  the three-way value anything counting reads. */
  title?: string
  status?: string
  outcome?: 'open' | 'landed' | 'abandoned'
  url?: string
  last_seen_at?: string
  /** The attributed half of this group's spend. Equal to the total on a work
   *  item; the share worth reading on a repository or a project. */
  attributed_cost_nanos: number
  /** Merged pull requests this item is worth, one hop away - the pull request a
   *  branch was opened as, the pull requests implementing a ticket, or itself.
   *  Absent when there are none, which is not the same as zero being measured:
   *  nothing has been confirmed unless an integration is connected. */
  merged_prs?: number
}

export type Coverage = {
  requests: number
  attributed_requests: number
  unattributed_requests: number
  cost_nanos: number
  attributed_cost_nanos: number
  unpriced_requests: number
  attributed_cost_percent?: number
  attributed_requests_percent?: number
}

/** UnplacedRow is one reason spend could not be tied to a work item.
 *
 *  A partition of the coverage gap rather than a list of things that might be
 *  true of it: every unattributed request is in exactly one row, and the costs
 *  sum to `coverage.cost_nanos - coverage.attributed_cost_nanos`. The label and
 *  the detail are the server's words, because naming a reason is an
 *  interpretation and it belongs on the side that can defend it. */
export type UnplacedRow = {
  reason: 'no_repository' | 'no_branch' | 'no_work_item'
  label: string
  detail: string
  requests: number
  sessions: number
  cost_nanos: number
}

export type Work = {
  since: string
  until: string
  scope: string
  by_work_item: WorkBucket[]
  by_repository: WorkBucket[]
  coverage: Coverage
  unplaced: UnplacedRow[]
}

export type WorkItemRef = {
  key: string
  kind: string
  rung: string
  confidence: string
  evidence: string
}

export type SessionRow = {
  id: string
  user_email: string
  tool: string
  method: string
  started_at: string
  last_seen_at: string
  requests: number
  repo: string
  branch: string
  attributed: boolean
  describe: string
  work_item?: WorkItemRef
  /** What the whole conversation cost - not the slice of it inside the window
   *  being viewed. A session is a conversation, not a time slice. */
  cost: Money
}

/** One row of the audit trail: the calls one conversation made on one route
 *  with one outcome, with no more than thirty idle minutes between them. Every
 *  figure is over the calls inside the filter and the window, so a row's total
 *  and the calls it expands to are the same set. */
export type RequestGroupRow = Totals & {
  id: string
  first_at: string
  last_at: string
  user_email: string
  tool: string
  /** The routes the sitting's model calls named: one, or both ends of
   *  several (a main model and Claude Code's fast model, say). Empty for a
   *  sitting of tool calls alone. */
  routes: string[]
  route_count: number
  /** How many of the sitting's calls were tool calls, such as web searches. */
  tool_calls: number
  provider: string
  /** The outcome class every call in the group belongs to. */
  outcome: 'routed' | 'blocked' | 'error'
  /** One model, or two when the route was re-pointed during the sitting. */
  models: string[]
  attempts: number
  duration_ms: number
  /** What the class is made of: a refusal and a hard limit are both
   *  "blocked" and mean different things to whoever reads them. */
  decisions: { refused: number; blocked_hard: number; blocked_soft: number; warned: number }
  error_kind?: string
  status_code?: number
  session_id?: string
  repo?: string
  branch?: string
  work_item?: string
  rung?: string
}

export type RequestRow = {
  id: string
  group_id: string
  request_id: string
  started_at: string
  duration_ms: number
  surface: string
  provider: string
  requested_model: string
  resolved_model: string
  streaming: boolean
  status_code: number
  decision: string
  error_kind: string
  tool: string
  user_email: string
  tokens: {
    input: number
    output: number
    cache_read: number
    cache_write: number
    cache_write_1h: number
  }
  priced: boolean
  ttft_ms?: number
  session_id?: string
  /** What the request's session was working on, read through the session by
   *  the same query that returned the row - so a repository filter and the
   *  repository shown against a row can never be two different answers.
   *  `work_item` is the key alone here, not the SessionRow reference: a row of
   *  the audit trail shows which item, and the session is where the provenance
   *  behind it is set out. */
  repo?: string
  branch?: string
  work_item?: string
  rung?: string
  budget_state?: string
  attempts?: number
  /** Present on a search made through the gateway's MCP tool. */
  web_searches?: number
  cost?: Cost
  cost_source?: string
}

export type BudgetRow = {
  id: string
  name: string
  scope: string
  period: string
  enabled: boolean
  spend_visible: boolean
  limits: {
    soft?: number | null
    hard?: number | null
    soft_usd?: string | null
    hard_usd?: string | null
  }
  state?: string
  spent_usd?: string
  spent_nanos?: number
  requests?: number
  /** Beside every figure, as everywhere else: what is missing from it. */
  unpriced_requests?: number
  percent_of_hard_limit?: number
  window_start?: string
  window_end?: string
  /** Who the budget governs, served only to a caller entitled to administer
   *  it. A name beside a limit, never beside a spend figure. */
  scope_id?: string
  scope_label?: string
}

export type Policy = {
  budgets: BudgetRow[]
  scope: string
  /** Administrators only. Budgets and routing are one screen and one
   *  permission: both decide what a deployment spends. */
  can_edit: boolean
}

/** The request shapes a model can be served in, and the endpoint each arrives
 *  at. A route is served only on the ones its model speaks. */
export type ModelAPI = 'anthropic-messages' | 'openai-chat' | 'openai-responses'

/** The model providers this build can serve. */
export type ProviderKind = 'bedrock' | 'bedrock-mantle'

/** ModelRow is one invocable target as the provider described it.
 *
 *  A model reachable in two geographies is two rows, not one with a flag: they
 *  route to different places and bill about ten per cent apart, so allowing one
 *  and refusing the other has to be expressible. The same model on two
 *  providers is two rows for the same reason. */
export type ModelRow = {
  /** `provider/id`, which is what a route stores and a picker returns. */
  ref: string
  id: string
  model: string
  name: string
  /** Who made the model: Anthropic, OpenAI, Qwen. */
  vendor: string
  provider: ProviderKind
  provider_label: string
  /** The region the provider invokes it from. */
  region: string
  /** Empty for a model this build knows no API for: listed, and not routable. */
  apis: ModelAPI[]
  /** 'us', 'au', 'global', or '' for a bare foundation-model id or a provider
   *  with no inference profiles. */
  geography: string
  global: boolean
  regions?: string[] | null
  /** Where a request to this target may be processed, in words. The compliance
   *  answer, and the reason geography is a row. */
  residency: string
  streaming: boolean
  lifecycle: string
  legacy: boolean
  requires_prof: boolean
  /** False when the provider lists the model and says it cannot be invoked. */
  available: boolean
  status?: string
  status_reason?: string
  /** The data-retention modes the provider allows for this model. */
  retention?: string[]
  /** How many tokens this target accepts, absent when this build has no entry
   *  for the model. Bedrock publishes no context length, so an absence here is
   *  "not recorded" and is rendered as nothing rather than as a zero. */
  context_window?: number
  /** How many tokens it produces by default. Same provenance, same absence. */
  max_output?: number
  priced: boolean
  rate_source?: string
  rates?: Record<string, string>
}

/** RouteRow is a name a client may ask for. It is the only thing a client may
 *  ask for: an engineer points their tool's model setting at one of these and
 *  never names a model. */
export type RouteRow = {
  name: string
  description: string
  /** The model this route becomes, as `provider/id`. */
  target: string
  provider: ProviderKind
  provider_label: string
  /** The APIs the route can be served on. A request on any other is refused
   *  with the endpoint it should have used. */
  apis: ModelAPI[]
  /** Whether the route is still served once a budget governing the caller has
   *  passed its soft limit. False is the deliberate choice that makes an
   *  engineer switch to another route rather than have their model changed
   *  underneath them. */
  serves_over_soft: boolean
  /** The catalogue entry for the target, when discovery has seen it. */
  model?: ModelRow
  /** The target is not in the catalogue. It still works - the provider is the
   *  authority, not this cache - but it is worth surfacing. */
  unrecognised?: boolean
  /** False when the route has no model, which means a request it admits has
   *  nowhere to go. */
  complete: boolean

  /** The conditions, served only to administrators and managers: they name
   *  people, and a developer is served the routes they can use rather than the
   *  reasons somebody else cannot. */
  users?: string[]
  teams?: string[]
  tools?: string[]
  restricted?: boolean
  enabled_by?: string
  updated_at?: string
}

/** A provider as a screen that names providers without configuring them sees
 *  it. */
export type ProviderSummary = {
  kind: ProviderKind
  label: string
  configured: boolean
  region?: string
  enabled?: boolean
  /** Built and switched on, so a route on it can be served. */
  serving?: boolean
}

export type Routing = {
  routes: RouteRow[]
  models: ModelRow[]
  providers: ProviderSummary[]
  discovered: boolean
  discovered_at?: string
  /** When each provider's catalogue was last read. */
  discovered_by: Partial<Record<ProviderKind, string>>
  /** Administrators only. Routes are the allowlist, and changing one moves
   *  spend and possibly the country a prompt is processed in. */
  can_edit: boolean
  /** The tools a condition may name - the same list the data plane derives
   *  from a user agent. */
  tools: string[]
}

export type IntegrationRow = {
  kind: 'github' | 'jira'
  connected: boolean
  unlocks: string
  degraded: string
  /** The stored connection, so the form re-opens with what is there. The
   *  credential is never among these - only whether one is stored. */
  app_id?: string
  installation_id?: string
  api_base_url?: string
  fixtures?: string
  has_secret?: boolean
  secret_kind?: '' | 'app_key' | 'token'
  configured_at?: string
  provider?: string
  detail?: string
  status?: string
  last_error?: string
  last_checked_at?: string
  last_synced_at?: string
  last_checked_items?: number
  last_confirmed_items?: number
  is_mock?: boolean
}

/** Seats is the population's economics: how many seats are paid for, how many
 *  are used, and how unevenly. It names nobody, which is why every role reads
 *  it - the table beside it is a different question with a different gate. */
export type Seats = {
  since: string
  until: string
  seats: {
    provisioned: number
    active: number
    idle: number
    cost_per_active: Money
    p50: Money
    p90: Money
    max: Money
    total: Money
  }
  cache_hygiene: CacheHygieneRow[]
  /** Whether this caller may see the per-person table at all. Said by the
   *  server rather than inferred from an empty list, so the screen can tell
   *  "nobody spent anything" from "this is not yours to see". */
  can_see_people: boolean
  people: UserSpendRow[]
  people_note?: string
}

/** Money crosses the wire as exact nanodollars to compute with and a decimal
 *  string to display, never as a JSON number. */
export type Money = { nanos: number; usd: string }

export type UserSpendRow = {
  user_id: string
  email: string
  display_name: string
  role: Role
  requests: number
  sessions: number
  cost: Money
  /** The share of `cost` that reached a work item. Whether this account's
   *  traffic is landing in the attribution - not a measure of the person. */
  attributed_cost: Money
  unpriced: number
}

export type CacheHygieneRow = {
  tool: string
  requests: number
  input_tokens: number
  cache_read_tokens: number
  cost: Money
  /** Read nothing from cache at all across the window - the flag that says a
   *  harness is misconfigured rather than merely inefficient. */
  uncached: boolean
  cache_read_share?: number
}

export type Integrations = {
  integrations: IntegrationRow[]
  any_connected: boolean
  /** Whether a credential can be stored at all. False when the deployment has
   *  no encryption key, which the connect form has to say rather than
   *  discovering on save. */
  can_store_secrets: boolean
}

export type OutcomeBucket = Totals & { outcome: string; work_items: number; label: string }

export type Outcomes = {
  by_outcome: OutcomeBucket[]
  by_project: WorkBucket[]
  confirmed: boolean
  integration: boolean
  cost_per_merged_pr?: {
    merged_pull_requests: number
    per_pr_nanos: number
    total_nanos: number
    per_pr_usd: string
    total_usd: string
    definition: string
  }
}

export type SetupStep = {
  id: string
  title: string
  summary: string
  required: boolean
  state: 'done' | 'todo' | 'skipped'
  detail: Record<string, unknown>
}

export type Setup = {
  steps: SetupStep[]
  remaining: number
  complete: boolean
  dismissed: boolean
  endpoint: string
  can_configure: boolean
}

export type ToolRow = {
  id: string
  name: string
  supported: boolean
  surface: string
  note: string
  env?: { name: string; value: string }[]
  shell?: string
  /** The routes this person may ask for with this tool. A route name is what
   *  the model variables hold; a model id is not something a client can send. */
  routes?: string[]
  /** opencode's whole provider block, or Codex's config.toml, rendered by the
   *  deployment. Present only for tools configured by a file rather than by
   *  variables. */
  config?: string
  /** The same file rendered once per route, for a tool whose file carries one
   *  route's numbers: choosing another route swaps the whole block. */
  configs?: Record<string, string>
  /** Where that file goes, in the words a sentence uses. Served for both
   *  supported tools, because it is also what `panorama configure` writes to
   *  and what a generated block is labelled with. */
  path?: string
  cli?: {
    recommended: boolean
    why: string
    /** Signing the machine in, which happens once per machine. */
    login: string
    /** Writing this tool's configuration, which happens once per tool - and
     *  for opencode also exports the variable its provider block reads. */
    configure: string
    /** The same block with the credential read from the environment. This is
     *  what `panorama configure opencode` fetches and writes; the console does
     *  not render it, and its presence is what says the CLI path will work. */
    config?: string
    settings?: { apiKeyHelper: string; env: Record<string, string> }
  }
  stamping?: {
    optional: boolean
    why: string
    headers?: string[]
    shell?: string
  }
  /** Claude Code's half of web search: the command that adds the gateway's
   *  MCP server, and the settings that deny its own WebSearch. opencode and
   *  Codex carry theirs inside `config`. Present only while search is on. */
  mcp?: {
    command: string
    settings: { permissions: { deny: string[] } }
  }
}

export type ToolSetup = {
  tools: ToolRow[]
  endpoint: string
  user: { email: string; display_name: string }
  token_placeholder: string
  /** Whether the blocks configure the gateway's MCP server for web search. */
  web_search?: boolean
}

/** Settings, Web search. */
export type WebSearchData = {
  configured: boolean
  serving: boolean
  state: 'not configured' | 'off' | 'failing' | 'never checked' | 'connected'
  label: string
  mcp_url: string
  published_price: string
  role_policy: string
  kind?: string
  /** The gateway is the deployment's own: shown, not edited. */
  deployed?: boolean
  enabled?: boolean
  gateway_url?: string
  gateway_id?: string
  region?: string
  price_per_search?: string
  price_source?: 'aws-published' | 'config'
  verified_at?: string
  configured_at?: string
  error?: string
  /** Set on the answer to a check. */
  ok?: boolean
}

export type Onboarding = {
  has_token: boolean
  has_request: boolean
  at?: string
  detected?: {
    tool: string
    /** The route name the client asked for. */
    route?: string
    /** The provider-native model that route became. */
    model: string
    repo?: string
    branch?: string
    attributed?: boolean
    describe?: string
    work_item?: WorkItemRef
  }
}

/** GrantRow is one machine `panorama login` signed in.
 *
 *  The same shape as a token because it is the same idea one level up - a
 *  credential with a name, a life and a revocation - but they are listed apart,
 *  because signing a machine out and revoking a pasted token are different
 *  actions with different consequences. */
export type GrantRow = {
  id: string
  name: string
  prefix: string
  created_at: string
  revoked: boolean
  used: boolean
  last_used_at?: string
  expires_at?: string
}

export type TokenRow = {
  id: string
  name: string
  prefix: string
  created_at: string
  revoked: boolean
  used: boolean
  last_used_at?: string
  expires_at?: string
}

export type TeamRef = { id: string; slug: string; name: string; role: string }

/** Where a role came from: the group mapping, or a person who assigned it.
 *  Only "sso" is recomputed at sign-in - that is what makes a change at the
 *  identity provider take effect, and what makes an assigned role stick. */
export type RoleSource = 'sso' | 'manual'

export type PersonRow = {
  id: string
  email: string
  display_name: string
  role: Role
  role_source: RoleSource
  status: string
  created_at: string
  teams: TeamRef[]
  tools: string[] | null
  tokens: number
  onboarded: boolean
  sso_linked: boolean
  budget_state?: string
  first_request_at?: string
  last_request_at?: string
  last_token_used_at?: string
}

export type TeamRow = {
  id: string
  slug: string
  name: string
  description: string
  members: number
  created_at: string
}

export type Identity = {
  configured: boolean
  issuer: string
  client_id: string
  scopes: string[]
  groups_claim: string
  redirect_url: string
  groups: { group: string; role: Role }[]
  /** What the mapping looked like when this was read. Sent back on save so a
   *  tab that has gone stale is refused rather than allowed to delete rows it
   *  never saw. */
  mapping_version: string
  /**
   * What the provider has actually sent, most-used first. The mapping above
   * asks for group names, and nothing else on this screen could tell an admin
   * what theirs are called - providers disagree about whether that is a name,
   * a path, or a GUID.
   */
  observed_groups: { group: string; count: number; first_seen: string; last_seen: string }[]
  sign_ins: number
  sign_ins_without_groups: number
  default_role: string
  default_role_options: string[]
}

/** One check result from a preflight or a doctor run. */
export type SsoFinding = {
  level: 'ok' | 'warn' | 'fail'
  title: string
  detail: string
  remedy?: string
}

export type SsoPreset = {
  id: string
  label: string
  issuer_hint: string
  scopes: string[]
  groups_claim: string
  notes: string[]
  docs_url?: string
}

export type SsoPreflight = {
  result: {
    issuer: string
    preset_id: string
    ok: boolean
    scopes: string[]
    groups_claim: string
    findings: SsoFinding[]
    notes?: string[]
    docs_url?: string
  }
  redirect_uri: string
  /** Rendered configuration per packaging shape. Nothing here is stored. */
  artifacts: Record<string, string>
  formats: string[]
  stored: false
}

export type Settings = {
  settings: Record<string, string>
  defaults: Record<string, string>
  deployment: {
    base_url: string
    providers: ProviderSummary[]
    driver: string
    sso_configured: boolean
    /** Whether this build offers command-line sign-in at all, which is what
     *  decides whether the switch beside it can be moved. Served rather than
     *  compiled in here, so the screen is not a second place it is decided. */
    cli_sign_in_released: boolean
  }
}

/** The five words for a provider's state. "never checked" and "failing" are
 *  both "not working", and an operator reaching for the logs needs to know
 *  which. */
export type ProviderState = 'not configured' | 'disabled' | 'failing' | 'never checked' | 'connected'

export type ProviderOptions = {
  unsupported_betas?: string[] | null
  context_windows?: Record<string, number> | null
  apis?: Record<string, ModelAPI[]> | null
}

/** One provider as Settings, Model providers configures it. */
export type ProviderRow = {
  kind: ProviderKind
  label: string
  summary: string
  /** The regions this build knows the provider in, for the select. */
  regions: string[]
  configured: boolean
  state: ProviderState
  enabled?: boolean
  region?: string
  options?: ProviderOptions
  endpoint?: string
  configured_at?: string
  configured_by?: string
  verified_at?: string
  verified_model?: string
  /** Why it could not be built, and what usually fixes that. */
  error?: string
  hint?: string
  catalogue: { models: number; serveable: number; discovered_at?: string }
  /** The routes whose model is on this provider. */
  routes: string[]
  /** The latest request the provider refused to serve, in its own words. */
  last_refusal?: ProviderRefusal
}

/** A request the provider refused - not found, or not permitted - kept for an
 *  administrator because the engineer who met it was told only to look here. */
export type ProviderRefusal = {
  at: string
  region: string
  model: string
  kind: string
  error: string
  hint: string
  request_id: string
}

/** What Bedrock reports about a refused model's availability. Evidence beside
 *  the refusal, never a verdict: the agreement reads NOT_AVAILABLE for every
 *  model nobody has invoked yet. */
export type ModelAccess = {
  model: string
  agreement: string
  agreement_error?: string
  authorization: string
  entitlement: string
  region: string
}

/** A model a check tried and the provider refused, for a reason of its own. */
export type ProviderCheckAttempt = {
  target: string
  route?: string
  error: string
  hint: string
  access?: ModelAccess
}

export type ProvidersData = {
  providers: ProviderRow[]
  /** The role policy every provider here needs, as the CloudFormation template
   *  grants it. */
  iam_policy: string
  can_configure: boolean
}

export type ProviderPreflight = {
  kind: ProviderKind
  region: string
  ok: boolean
  error?: string
  hint?: string
  models?: number
  serveable?: number
  stranded?: string[]
}

export type ProviderSaved = {
  kind: ProviderKind
  enabled: boolean
  region: string
  region_changed: boolean
  discovered: number
  stranded: string[]
  discovery_error?: string
  hint?: string
}

export type ProviderCheck = {
  ok: boolean
  kind: ProviderKind
  target: string
  /** The route the checked model was chosen from, when it was a route's. */
  route?: string
  region: string
  duration_ms?: number
  error?: string
  hint?: string
  access?: ModelAccess
  /** Models tried before target and refused, in the order they were tried. */
  refused: ProviderCheckAttempt[]
}

/** Who administers the deployment, as a setup session sees them. */
export type SetupAdmins = {
  users: {
    id: string
    email: string
    display_name: string
    role: Role
    role_source: RoleSource
    status: string
    sso_linked: boolean
  }[]
  assigned_roles_survive_sign_in: boolean
}

/* ------------------------------------------------------------ allocation -- */

/**
 * One accounting line.
 *
 * `plan_nanos` is null where nobody has set one, and null is not zero: zero is
 * a line meant to spend nothing. Nothing here is enforced - a plan warns, and
 * what actually stops an account is a budget on a person or a team.
 */
export type CostCenterRow = {
  id: string
  code: string
  name: string
  owner: string
  description: string
  plan_nanos: number | null
  warn_at_percent: number
  /** Whether x-panorama-cost-center may name this line. A stamp is the
   *  spender's claim about where their own cost belongs, so a line whose
   *  integrity matters more than its coverage turns this off. */
  accepts_stamp: boolean
  archived: boolean
}

/** One line of the ordered allocation list. First match wins. */
export type AllocationRuleRow = {
  id: string
  cost_center_id: string
  position: number
  match_on: 'ticket' | 'repo'
  pattern: string
  /** True when an earlier rule claims everything this one could. Reported
   *  rather than refused: a stranded rule is legal, saves cleanly and does
   *  nothing, which is exactly what somebody needs told about. */
  unreachable: boolean
  /** True when a repository catch-all above it claims nearly everything this
   *  rule could. Weaker than `unreachable` and worded that way on the screen:
   *  a ticket carrying no repository would still reach it. */
  shadowed: boolean
}

export type CostCentersData = {
  cost_centers: CostCenterRow[]
  rules: AllocationRuleRow[]
  enforced: boolean
}

/** One line's share of a window, with the plan comparison always month to
 *  date - a plan is a monthly figure, and a week shown as a share of one reads
 *  as coming in five times under budget. */
export type AllocationLine = {
  id: string
  code: string
  name: string
  owner: string
  archived: boolean
  accepts_stamp: boolean
  cost_nanos: number
  requests: number
  sessions: number
  /** The share of cost_nanos a client claimed with a header rather than a rule
   *  deriving from evidence. The two answer to different standards. */
  stamped_nanos: number
  plan_nanos: number | null
  month_to_date: number
  warn_at_percent: number
  plan_used_percent?: number
  over_warn?: boolean
}

/** One reason a slice of the window reached no line. The rows sum to exactly
 *  the unallocated remainder. */
export type UnallocatedRow = {
  reason: 'no_rule' | 'no_work_item'
  label: string
  detail: string
  repo: string
  requests: number
  sessions: number
  cost_nanos: number
  /** Only the first kind is something anybody can act on here. */
  actionable: boolean
}

/** A cost center code a client sent that named no line. Counted over the whole
 *  window rather than over the gap: a misspelt stamp on work a rule does claim
 *  is allocated correctly and never appears in the remainder. */
export type RejectedStamp = {
  stamp: string
  requests: number
  cost_nanos: number
}

export type Allocation = {
  lines: AllocationLine[]
  totals: {
    cost_nanos: number
    allocated_nanos: number
    unallocated_nanos: number
  }
  unallocated: UnallocatedRow[]
  rejected_stamps: RejectedStamp[]
  enforced: boolean
}
