# Panorama: working agreement

Panorama is an open-source (AGPL-3.0-only) AI gateway, sponsored and maintained by DNX
Solutions, deployed into a company's own
infrastructure. It governs coding-AI usage: SSO, per-user and per-team quotas, policy
routing over Amazon Bedrock, and — the headline — cost attribution that connects AI
spend to real work (pull requests, tickets) with zero client configuration.

The source of truth for the product's principles and scope is the README's
**Principles** section; changing one takes a decision in an issue first.

## Non-negotiables

These are product commitments. A change that violates one does not get merged, however
convenient it is.

1. **Bodies are never stored.** Prompt and response bodies are pattern-matched in flight
   and discarded. Only extracted metadata persists. No prompt or response storage
   anywhere in the open-source edition — not in the audit log, not in a debug mode, not
   behind a flag. The extraction rules are open source so the claim is auditable.
2. **A developer sees only their own numbers.** No UI, API, export, or dashboard may
   show one developer another developer's usage. Named per-person spend exists, and is
   visible to administrators and to managers of that person's teams: a FinOps buyer
   needs per-seat cost (README, Principles).
   `filterFrom` in `internal/controlplane/api.go` is the single chokepoint that enforces
   the developer half, and it stays that way: a second place to decide visibility is a
   second place to get it wrong.
3. **Unattributed spend is always visible** next to attributed numbers, everywhere they
   appear. An honest 70% beats a false 100%.
4. **Streaming is sacred.** Nothing on the hot path may buffer a stream, block, or add
   meaningful latency. Target: sub-10ms added latency to time-to-first-token. Inline
   work on the request path is regex and structural matching only; everything else runs
   asynchronously against already-extracted metadata.

## Conventions

- **No fallbacks that mask errors.** Log the error and return a structured failure.
  Falling back is sanctioned only for genuine service interruption, and the product
  has no place that does it: a throttle reaches the client as a 429 (see "A route
  becomes one model" below). Never use a fallback to avoid handling an error.
- **Allowlists over denylists** for anything crossing a trust boundary: models, routes,
  OIDC claims, header passthrough.
- **Authenticate every endpoint.** The only unauthenticated routes are `/healthz` and
  `/readyz`, and they expose no org data. Anything new that is unauthenticated needs an
  explicit reason in review.
- **Structured context on every metric and log line**: `userId`, `teamId`, `sessionId`,
  `model`, `provider`, `tool`, wherever they are known.
- **Fix root causes.** Trace the actual execution path before hypothesising; do not
  patch around a bug in documentation or tool descriptions.
- **Change the manual with the code, in the same pull request, screenshots
  included.** A change to what somebody sees, types or is told is not finished
  until `docs/` says the same thing, and a changed screen is not finished until
  its image is retaken in both themes. What to search, which page answers for
  which package, and the exact size and state of every screenshot are under
  *The documentation site*: *When you change code, change the manual in the
  same pull request* and *Screenshots are of a real deployment, in both themes*.
- **Never `git push --force`.** If something wrong is committed, revert it. An imperfect
  history is fine.
- **Every new file carries its licence.** A source file starts with the two SPDX lines
  (`SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors`,
  `SPDX-License-Identifier: AGPL-3.0-only`) in its own comment syntax; a file that
  cannot hold a comment gets a path in `REUSE.toml`. The manual's prose and images are
  CC-BY-4.0. `make license-check` (`reuse lint`) fails on a file with neither.

## Architecture

One Go binary containing the data plane (proxy), the control plane (API + jobs), and
the embedded React UI. Internally separable — the future SaaS shape is a hosted control
plane with a customer-run data plane — but it ships as a single artifact.

| Concern | Choice |
| ------- | ------ |
| Language | Go, single static binary (pure-Go SQLite, no cgo) |
| Storage | SQLite by default, Postgres for production; same schema via migrations |
| Human auth | Generic OIDC; Microsoft Entra ID and Okta are the documented first-class providers |
| Tool auth | Per-user gateway tokens, revocable and rotatable |
| Jobs | Interval workers in the same process, on SQLite and Postgres alike. River (in-process, Postgres-backed) is where retries, scheduling and fan-out belong once a job needs them, and is not a dependency yet |
| Provider | `Provider` interface; two v1 implementations, bedrock-runtime and bedrock-mantle, each configured in the console with its own region |
| UI | React + TypeScript, built by Vite, embedded with `go:embed` |
| Packaging | Container-first: docker-compose for evaluation, one CloudFormation template for AWS |

Model IDs are namespaced by provider (`bedrock/us.anthropic.claude-...`,
`bedrock-mantle/openai.gpt-oss-120b`), split on the first `/`. A route, a policy
file, the price table's overrides and the audit trail's model filter all hold
the reference, and a bare id in a policy file is refused rather than guessed
onto a provider. Nothing in core imports AWS SDKs outside the Bedrock provider.
The AWS packaging is a CloudFormation template and imports nothing.

### Model providers

A provider is **a kind, a region and a switch**, stored in `providers` and
configured in Settings, Model providers (or `panorama providers`). One row per
kind, never the same kind in two regions: each provider has its own region, and
that region may differ from another provider's. `bedrock.region` in the file
**seeds** the bedrock row on first boot and is not read again; when the two
disagree the startup log says so at WARN, because two owners for one setting
means nobody can say which one is in force.

`providers.Registry` builds an `Instance` (provider, catalogue, price source,
the APIs a model speaks) from a row, cached by the row's fingerprint, and the
policy engine syncs the registry into **its own snapshot** before publishing it,
so `Decision` carries the provider to invoke and the data plane looks nothing
up. A provider that is off or failed to build is in the snapshot as a state with
an error, which is how a route on it becomes a 502 naming the deployment rather
than a nil dereference. Adding a kind is a `Descriptor`: label, regions, how to
build, how to hint at a failure, the IAM policy.

**The role grants both endpoints in every region, so a region is never an IAM
change.** `bedrock.TaskRolePolicy` is the one statement list, the CloudFormation
template's TaskRole is tested against it statement by statement, and the console
serves the same JSON. No condition keys and no region in any ARN: the point is
that an administrator choosing `eu-west-1` for Mantle on a Tuesday needs nobody
with IAM access.

**bedrock-mantle is plain HTTP with SigV4, not an SDK.** Service
`bedrock-mantle`, host `bedrock-mantle.<region>.api.aws`, and three request
shapes by model family: `/anthropic/v1/messages` for Claude,
`/v1/chat/completions` and `/v1/responses` (or the `/openai/v1` base, by family)
for everything else. `AWS_ENDPOINT_URL_BEDROCK_MANTLE` redirects it, for the
same test-double reason the SDK's own variables exist, and is logged at WARN
whenever it is set because no production deployment has a reason to. A stream
ends at `[DONE]`, `message_stop` or the terminal `response.*` event and not at
the connection closing; a body that ends before its end marker is an upstream
error, never a clean finish with a smaller bill.

**GPT-5.x on Chat Completions reads its cap as `max_completion_tokens` and
refuses `max_tokens`**, which is the only spelling `@ai-sdk/openai-compatible`
has, so without help every opencode request on a GPT-5.6 route is refused. The
Mantle provider renames the field for the families marked `completionTokens` in
`bedrock/apis.go`. One number with one meaning, so it is a spelling like the beta flags and not a decision made for
the engineer; a body carrying both keeps `max_completion_tokens`, and a
differing `max_tokens` is dropped at WARN. The fake refuses `max_tokens` for
those families in Mantle's own words, and function tools on their Chat
Completions too, which is what makes the opencode e2e leg on the `gpt` route a
test of both.

**Which API a model speaks is a table, like its window** (`bedrock/apis.go`),
because `/v1/models` says nothing about it. A model the table has not heard of
is listed and **browse-only**: in the catalogue, not routable, and named once
per discovery pass at INFO. Both tables are overridable per provider from the
console, for the release in which AWS ships something this build does not know.

**A route is served only on the APIs its model speaks.** A request for it at
another endpoint is refused with a 400 naming the endpoint to use, from
`policy.RouteAPIs`, which the console reads too. There is no translation between
the Messages API and the OpenAI shapes: two dialects rendered into each other is
two sets of edge cases nobody tests.

### Bedrock is a dialect, not a mirror

The first-party Anthropic API and Bedrock's Anthropic surface are not the same
API, and the gaps are load-bearing rather than cosmetic. Three are handled in
the provider, and each was found by a request failing, not by reading a doc:

1. **Inference profiles.** Current Anthropic models reject invocation by their
   bare foundation-model id and require a `us.`/`global.` prefixed profile id.
   That is why a route carries a target and is not just a permitted name.
2. **Beta flags travel in the body.** Clients send `anthropic-beta` as an HTTP
   header; Bedrock reads `anthropic_beta` from the request body. Without the
   translation, every parameter a beta enables is rejected as unrecognised.
3. **Bedrock refuses unknown beta flags outright**, failing the whole request.
   The flags Bedrock refuses are dropped and each drop is logged: a translation
   between dialects, made visible, not a fallback that hides a failure. **No
   count is stated here, on purpose**: it moves whenever either side ships.
   The list is `bedrock.DefaultUnsupportedBetas`,
   it is found by probing Bedrock and not by reading a document, and it is
   overridable per provider for the release in which it has gone stale.

When a client works against api.anthropic.com and fails here, look at these
first.

**A fourth gap is not between the two APIs, and is closed by naming.**
Claude Code prepares each request for the model it believes it is talking to,
and a name it does not recognise is treated as the newest model there is: it
sends `thinking: {type: adaptive}`, `output_config.effort` and a `system` role
mid-conversation, and Bedrock refuses the whole request when the model behind
the route lacks any one of them (Haiku 4.5 refuses all three). Asked for by its
own name, Claude Code sends a model only what it accepts.

So **a route for Claude Code is named with its model's Anthropic ID**:
`claude-sonnet-5`, `claude-haiku-4-5-20251001`. The route editor asks for it
when the tool condition names `claude-code` and the model is Anthropic's, and
offers the name (`claudeCodeName` in `routes.ts`, read from the
foundation-model id). Haiku 4.5 behind a route called `fast` fails every
subagent request with `role 'system' is not supported on this model`, and
behind `claude-haiku-4-5-20251001` serves all of them.

The gateway does not translate instead, dropping what each model refuses: each
drop exposes the next refusal, and every entry in such a table repairs a request
the client built for the wrong model. Naming removes the cause. It is a
warning, not a refusal, and other vendors' models may still be offered to Claude
Code: whether to restrict it to Anthropic's is undecided. Two
refusals remain whatever the name: structured output (`output_config.format`)
on Sonnet 5 and Opus 5, and Anthropic's server-side web search tool, which
Bedrock does not offer. Search is served instead by the gateway itself (*Web
search*, below), and My setup denies Claude Code's own WebSearch so the model
never reaches for the refused one.

**v1 is single-organization.** One deployment is one org. Multi-org and multi-account
are enterprise edition; do not add org scoping to v1 tables speculatively.

### Models are discovered, and geography is the choice

Nobody types a model mapping. A discovery job asks every serving provider what it
can invoke on an interval (`providers.discovery_interval`) and caches the result
in `provider_models`, keyed by provider and id, replaced per provider so one
region's rows leave in the transaction the next region's arrive in. On
bedrock-runtime it joins `ListFoundationModels` with `ListInferenceProfiles`,
because three things an operator otherwise learns by a request failing are
published there:

- `inferenceTypesSupported` says whether the bare model id is refused and a
  profile required. It almost always is: **14 of the 15 Anthropic models in
  us-east-1 are profile-only**, and the one that still accepts its bare id is
  `LEGACY`. A bare-id row is emitted only where AWS reports `ON_DEMAND`, and it
  sorts last and is labelled "no profile": leading with it would lead with the
  target almost nobody should pick.
- `modelLifecycle` marks a model `LEGACY`. A route pointing at one is a
  scheduled outage nobody has noticed.
- a profile's model ARNs enumerate **the regions it may route to**, which is the
  data-residency answer and cannot be derived from anything else. Read from the
  ARN's region field rather than the profile description, which says the same
  thing in prose that AWS may reword.

**A context window is not discovered, because Bedrock does not publish one.**
Checked field by field against the API rather than assumed:
`ListFoundationModels` and `GetFoundationModel` return modalities, inference
types, streaming and lifecycle; inference profiles return regions; the only
`MaxTokens` anywhere in the service belongs to imported models, fine-tuning and
evaluation jobs. The figure the Bedrock console shows is editorial content in
its frontend. So `internal/providers/bedrock/windows.go` is a hand-maintained
table, keyed by model **family** — a geography prefix and a revision date are
stripped before lookup, so one entry covers `us.`/`au.`/`global.` and survives
AWS revising a model in place - and overridable per provider in Settings, Model
providers, for the same reason the unsupported beta list is.
A model it has not heard of is recorded as **unrecorded, never as a default**:
the route check declines to compare and the client is told nothing. A wrong
window is worse than an absent one, because a route refusal and an engineer's
token accounting are both derived from it. The output cap sits in the same
table because opencode's `limit` is one object requiring both numbers.

**A geography is a row, not a flag.** `au.anthropic.claude-sonnet-5` and
`global.anthropic.claude-sonnet-5` are two catalogue entries, because they route
to different countries and bill about 10% apart, and allowing one while refusing
the other is the entire point. Checked live: Haiku 4.5 is $1.00/M global against
$1.10/M us — but Sonnet 4 is $3.00 either way, so **the 10% delta is a common
pattern, not a rule**, and each scope's published rate is looked up rather than
computed from the other.

**bedrock-mantle has no profiles and so no geography.** One call, `GET
/v1/models`, returns an id, an owner, a status and the retention modes a model
allows; a model is invoked in the region the call is made to, and that region is
the residency answer on every row. A model the endpoint lists as not available
is kept, with its status, so "why can I not pick it" has an answer on the
screen. Its prices come from `-mantle-` usage types; a Claude model with none
published is priced at the runtime regional rate for its family, and left
unpriced when that is ambiguous.

**Everything a client may ask for is a route, and a route is the whole of the
decision.** `model_routes` is the allowlist; a name absent from it is refused.
An engineer sets their tool's model (`ANTHROPIC_MODEL`, opencode's `model`,
Codex's `model`) to a route name and never names a
model — a model id sent by a client is a name nobody routed, and is refused like
any other. There is no "allow any model" flag to leave switched on anywhere in
the product: the console refuses to create a `*` route, and there is no second
place — no file, no environment variable, no evaluation shortcut — that could
create one.

A route carries three different kinds of decision and they must not be
confused:

- **Conditions** — users, teams, tools; empty means everybody — say who the
  route exists for. Every condition set must match, so a route naming a person
  and a team admits that person when they are in that team and nobody else; that
  is the reading that makes a route narrower as an administrator adds to it, and
  the one that fails closed when they meant the other. A request from outside
  them is refused **exactly as though the name had never been created**, and the
  refusal lists only the routes that caller can use. A route somebody may not
  use is not one they can see.
- **Target** says which model it becomes, as a provider reference.
- **`serves_over_soft`** says whether it is still served once a budget governing
  the caller has passed its soft limit. True by default — a soft limit warns, a
  hard limit blocks, and nothing in between stops anybody — and false is the
  deliberate mark on the expensive route.

**Past a soft limit a route stops; it does not change model.** The refusal names
the budget, how much of it is used, when it resets, and **the routes that are
still served** — filtered through `Set.AvailableAt` to what that caller can
actually use in that budget state, so the list is never a guess. The engineer
switches (`/model` in Claude Code, the model list in opencode) and carries on.
When there is nothing to switch to the message says so instead of offering an
empty list.

The gateway never swaps in a cheaper model at the soft limit, because a harness
is told a route's context window and output cap once, when it is configured, and
nothing renegotiates them mid-session. Changing what an engineer is working with
without telling them would need a fence around every smaller target; telling
them needs nothing, because every route somebody may use is already declared in
their own configuration with its own numbers beside it.

The route is refused with `policy.BlockedOverSoft`, which is a `BlockReason`
rather than a second decision value: `requests.decision` stays `blocked` and
`budget_state` already says which line was crossed, so no `CHECK` constraint on
the audit trail's largest table widens, and a third column saying the same thing
would be a third column to keep in step.

**Whether there is anywhere to switch to is warned about, not enforced.** A
route marked `serves_over_soft: false` with no serving route available to the
same people leaves everybody it admits refused with nowhere to go. That is a
legitimate configuration — it is how "past the warning line, stop" is written —
and it is also what somebody who forgot the second route has just built, so the
save returns a warning naming the problem rather than refusing. Whether an
alternative exists is asked *per person* (`coversAtLeast`), because conditions
make "there is another route" and "there is another route for you" different
questions: a deployment can have five economy routes and still strand the one
team whose route names them and nobody else.

**The route list is the only table deciding whether a request may proceed.** A
policy table beside it (matching on user, tool or budget state, able to allow,
block or exempt) would be a second answer to that question, and the day the two
disagreed the permissive one would win silently. An exemption from a hard limit
is a raised budget, and a block is a name nobody routed.

Routes load into the **policy engine's existing snapshot**, not a second one.
Two snapshots refreshed independently would spend part of every cycle
disagreeing about which models exist, and a request could be admitted by one and
have no target in the other. There is no resolver beside it: `Evaluate` returns
the provider-native model to invoke, so the data plane has nothing left to look
up.

**A route with no model is refused at the write path**, because it would admit a
request and then have nowhere to send it. If one is reached anyway (a
hand-edited database, say) the engineer gets a 502 and the
operator an error log, because that failure is the deployment's and saying
otherwise would send somebody looking at their own configuration.

**Conditions are JSON lists on the route, not a junction table.** Rows
referencing `users` would have to cascade, and a cascade here *widens* a route:
delete the one person a route names and the condition disappears, leaving a
route that admits everybody. A list of ids that no longer resolve simply never
matches, which fails closed.

**A route becomes one model.** There is no fallback chain: a second model per
route would be a second model to discover, price, window-check and explain, and
it would change an engineer's model under load without telling them. A throttle
is a 429 in the client's own error shape, which every coding tool already
retries, and a route that should survive a throttle is a second route the
engineer can switch to. `requests.attempts` is the budget predicate (0 or 1).

`policy.RouteMinimum` is what reaches a laptop: the route's model's limit, zero
when it is unrecorded.

**A route's target is a provider reference.** It is checked against the
discovered catalogue and against a provider that exists when an administrator
saves it (`controlplane.RouteTargetProblem`, which `panorama policy apply` calls
too), so by the time a request is being served there is nothing left to resolve.

**The database owns the route list, and the configuration file cannot create a
route.** No file key and no environment variable names a model. Two owners
would put a branch for which one is speaking in every screen, every handler and
every startup path, and the GitOps argument for a file is answered by a route
being one row an administrator can change in ten seconds without a deploy.
Choosing models is an operational decision made months after the deployment is
stood up, which is also why integrations and provider configuration live in the
database: `bedrock.region` seeds the bedrock row once, and a region is changed in
the console from then on.

**Reaching a provider and choosing models are two setup steps, not one.** The
first is a region per provider and the deployment's credentials and egress, and
is the only one a setup session can act on. The second is `model_routes`, and it is an
administrator's, made once there is an administrator. Folding them together
would ask somebody holding a one-time code to decide which models a company pays
for and which country its prompts are processed in, and would make the provider
check impossible to run until a route existed, at the exact moment an operator
most needs to know whether their credentials work. The check therefore invokes a
target from that provider's discovered catalogue (servable, streaming,
available, not `LEGACY`, Anthropic first, cheapest first) rather than a route,
and records the verification on the provider row. A region change clears it in
the same statement, because a check that passed in one region says nothing about
another.

**A check with nothing to pick from reads the catalogue itself, and a catalogue
it cannot read is its finding.** A laptop with expired credentials fails
discovery at startup and so has an empty catalogue; a check that refused with
"refresh the catalogue first" would say nothing about credentials, from the one
control that claims to prove them. `credentialHint` is asked before either
provider's hint table for the same reason: otherwise "no credentials" and
"expired credentials" read as a wrong identifier and as a missing IAM action.

**Model access is not a gate, and there is no API that is.** Access to every
foundation model is enabled by default; Bedrock auto-subscribes on first
invocation. `GetFoundationModelAvailability` reporting `NOT_AVAILABLE` therefore
means "nobody has invoked this here yet", not "you are blocked" — and an SCP
denying `bedrock:InvokeModel`, which is how AWS documents actually blocking a
model, is **invisible** to it. So the live invocation in the setup step is not a
nicety: an SCP, a missing `aws-marketplace:Subscribe`, and a wrong identifier all
arrive as the same `AccessDenied`, and one real request is what tells them apart.

## The console

Seven screens plus a first-run wizard, in `ui/app`. How they look and behave
(the design system, type, layout, panels and modals, and the decisions behind
each screen) is in `ui/CLAUDE.md`, which is read when working under `ui/`.

## The console API

What the console is served, filtered and refused. These rules live on the
server side of the wire, so they bind a change to `internal/controlplane` as
much as a change to a screen.

**The coverage gap is broken down, and the breakdown is a partition.**
`/api/v1/work` returns `unplaced` beside `coverage`: every unattributed request
falls in exactly one reason — no repository was open, a repository but no
branch, a branch whose name matched no work item — and the costs sum to
`cost_nanos - attributed_cost_nanos`. A breakdown whose parts overlap or fall
short would be a second number to reconcile against the first, which is the
opposite of what "the gap is stated rather than absorbed" is for. The store
reports what it observed; the label ("Exploratory sessions") is chosen in the
handler, because naming a reason is an interpretation and it belongs on the side
of the wire that has to defend it.

**A filter over a paginated list is applied by the server — the outcome one
included.** The audit trail holds weeks and loads fifty rows at a time; a
control that searched only what was loaded would report "nothing matches" about
a page while appearing to report it about the window, which on this screen is
the one wrong answer that matters. The typed filters — user, repository, branch,
model — and the outcome are query parameters, matched in one place
(`requestConditions`), so the table, its next page and the CSV export are
narrowed by the same string. They only ever narrow: `filterFrom` reads them
before it pins the scope and ANDs them with it, so no combination of them is a
way to see somebody else's traffic. Every one of them lives in the URL, which is
what makes a view of the trail a link.

**Four decisions fold into three outcomes, through one table read in both
directions.** `allowed` is served, under the outcome called `routed`;
`blocked` and `refused` are both "no provider was invoked"; `error` is its
own. `outcomeDecisions` is what the filter turns into
SQL and what `CountOutcomes` folds through, because a decision the filter admits
but the count bins elsewhere makes an option say 4 and return 3 rows — and
nothing about that looks wrong from either side of the wire. The counts are
computed with the outcome cleared and every other filter applied, so each option
carries the size it would select and the three sum to All; they travel with the
first page only, since they describe the window rather than the page.

**A row of the audit trail carries the work it paid for**, joined through its
session by the query that returned it, not by a second, separately limited
call over sessions: that would let a request be returned by a repository filter
and show none, and an export of fifty thousand rows resolve the repository for
fifty of them.

**The audit trail reads in sittings, and membership is decided at write time.**
A group is principal, conversation (`session_id`), tool and outcome class,
with no more than `store.GroupIdleGap` (thirty minutes) between one request and
the next. `requests.group_id` is the id of the group's first request, set by
`groupOf` in `RecordRequest`. It is stored rather than computed when the trail is read because an
idle gap compares neighbours, and every filter and page would otherwise
recompute it over weeks of the largest table. The totals are not stored:
`/api/v1/requests/groups` aggregates the requests `requestConditions` admits,
grouped by `group_id`, so a row's total and the requests it expands to
(`/api/v1/requests?group=`) are one set. The outcome is in the key so a refusal
never hides inside a served total, and the export stays per request.

**The route is not in the key, because most route changes inside a
conversation are nobody's choice.** Claude Code sends titles, summaries and
command checks to its fast model on every turn, so a key with the route split
each sitting into two rows, one of them a stream of small calls on a route
nobody picked. The row names the route and model of the group's first request
(the opening turn, so the main model) and counts the others, which keeps a
deliberate switch visible.

**A tool call joins the sitting it was made from, by inference.** No client
sends its conversation on an MCP call (checked: Claude Code 2.1.291 and opencode
1.18.30 send only their user agent), so a search is recorded with
`SessionFromSitting` and takes the session of the latest request its principal's
same tool made in the `GroupIdleGap` before it. That puts it in the right row
and attributes it to the work the conversation was on. Two conversations at once
in one tool can put a search in the wrong one; the cost of that is one search's,
and the alternative is every search unattributed. On the trail a tool call is a
row of the opened sitting with a `tool call` label, its searches where tokens
would be, and its duration.

**A developer is served the routes they can use, and not the conditions.** The
conditions name colleagues, so the console filters the list the same way the
gateway filters a refusal — the identity half of it, at least: the tool
condition is not applied in the console, because a browser is not a coding tool
and applying it would hide a Claude-Code-only route from the person whose Claude
Code uses it.

**A harness is told what a route is, in whatever shape it accepts.** Three
clients are configured here and the setup screen lists **opencode first**, which
is a recommendation rather than a ranking. Each is offered only the routes whose
model speaks an API it does: Claude Code the Messages routes, Codex the
Responses routes, and opencode all three, under three provider blocks
(`panorama` on `@ai-sdk/anthropic`, `panorama-openai` on `@ai-sdk/openai`,
`panorama-chat` on `@ai-sdk/openai-compatible`) so each route is sent to the
endpoint its model speaks. A route on a model speaking more than one is
declared once, Messages first, then **Responses before Chat Completions**,
because GPT-5.x refuses function tools on Chat Completions while it is
reasoning and a coding agent sends tools on every request. Switching reasoning
off would make it work with a weaker model than the engineer chose, so nothing is
rewritten; opencode sets `store: false` for `@ai-sdk/openai` and strips the
stored-item ids from what it resends, so its Responses traffic is stateless
without being told. Codex gets a `config.toml` provider entry with `wire_api = "responses"`,
rendered on the server once per route, because the window and output cap in it
belong to the route chosen. A route name may be a model this
deployment invented, which is in no client's registry: opencode's provider block
declares the model outright, window and output cap beside the name. Claude Code
is told no numbers, because its routes carry the model's own name (the fourth
gap, above) and it knows the window of a model it recognises. So the two clients
are configured in different shapes and the server owns both: Claude Code gets
environment rows and a settings file assembled from them, opencode gets its
provider block **rendered whole on the server**, because the numbers in it are
`RouteMinimum` over the route's reachable models and the browser has no way to
compute one.

**The window is not optional for a client that cannot know it.** A client
that does not recognise a model name assumes a window and manages the session
against it, so a route on a million-token model can auto-compact at a fifth of
its context, silently, and most expensively on the models somebody chose
precisely for their context. Claude Code avoids this by recognising its routes'
names. opencode and Codex need the numbers, and both are **omitted rather than
guessed** when
`RouteMinimum` returns zero: a client left to its own assumption is at least
conservative, while a figure invented here is one an engineer's session is
managed against.

**The block also chooses a model.** Declaring every route a person may use and
selecting none of them would leave opencode on whatever its picker had last,
which on a first run is nothing at all: the provider configured and the tool
still with no model. `model` and
`small_model` are the pair that fixes it, they are the same pair `routesFor`
picks for Claude Code's two variables, and they are the only two keys the
console rewrites when a reader changes their routes. Everything else in that
block is arithmetic the browser cannot redo, so it is edited there and never
assembled there.

The credential is where opencode is weaker, and the block says so rather than
implying otherwise: there is no credential-helper command, so
`{env:PANORAMA_TOKEN}` and a token minted per shell is the best available, and a
session outliving its token needs a new shell. Claude Code's `apiKeyHelper`
rotates in place; pretending both do would be found out by an expiry mid-session.
There is also **no environment-variable form for opencode**, and the step says
so rather than offering one: variables pointed at the gateway would carry none
of what the provider block declares, which is the whole reason opencode arrives
intact.

**The unsupported tools are served and not drawn.** `/api/v1/setup/tools`
returns Cline, Continue, Gemini CLI, Copilot and Cursor with `supported: false`
and the reason, and My setup does not render them: the
screen configures the tool somebody has, and five tools they are not using
would be the longest thing on it. The claim is auditable at the endpoint.

**The console is not a control.** The sidebar hides screens a role has no use
for, and `requireRole` on the server is what actually refuses. A console that
hid a button and called it access control would be one bypass away from a
leaderboard.

**Deleting a team is refused while a service token bills it.**
`service_tokens.team_id` cascades, so the delete would revoke every pipeline
credential on that team — silently, at whatever hour the pipeline next runs.
Revoking the token first is one click and is the point. A budget scoped to the
team is deliberately left behind instead: `budgets.scope_id` has no foreign key,
and an orphan an operator can see beats one that vanished with the thing it
governed.

**A roster is written in one request.** `POST /api/v1/teams/{slug}` with a
`members` list replaces the membership wholesale, in a transaction. Translating a
screen full of checkboxes into a stream of add and remove calls leaves a
half-applied roster on the first failure, with nothing on the screen saying which
half landed. An empty list is a real instruction and is why the field is a
pointer.

**The people list will carry spend, for those entitled to see it, and does not
yet.** Per-user spend in that table is permitted (README, Principles) and not yet
built: the screen and the endpoint both say plainly that they carry no per-user
spend.

When it is built, who sees it is decided on the server, once: an administrator
sees everyone, a manager sees members of teams they lead, and a developer sees
nobody — including themselves in that table, since their own numbers live on
the personal screens. The sidebar hiding a screen is not what enforces this.

## Signing in

Five credentials, and they are not interchangeable. The prefix is what makes
them tellable apart before any query runs, which is what keeps authentication on
the request path to one indexed lookup in one table — and what lets a credential
be refused at a door it has no business at, rather than looked up and rejected
later. `auth.Parse` admits exactly two of these; everything else is refused on
shape.

- **Gateway tokens** (`pnt_`) are what a coding tool holds. They also
  authenticate the console API, for scripts and for a deployment before SSO.
  Minted by hand in the console, or — the recommended path — by a CLI grant,
  short-lived and rotating.

  **A minted one lives thirty days and there is no way to ask for longer.**
  `auth.GatewayTokenTTL` is the number, `auth.GatewayTokenExpiry` is the only
  function that applies it, and both mint paths (the console and `panorama
  token create`) call it, because two places deciding when a credential dies
  is two answers and the longer one wins silently. `auth.max_token_ttl`
  binds and binds **downward only**: an operator who said a week said something
  stricter, and a ceiling above thirty days changes nothing. The request body
  has no expiry field at all rather than one the server ignores.

  The reason it is fixed rather than configurable is that a pasted token is the
  credential this product can least protect: it does not rotate, it lives in a
  dotfile, and nothing about a laptop leaving the building tells the deployment
  to revoke it. The grant path does not need this — those tokens live
  `auth.token_ttl`, an hour by default, and rotate on their own. A service
  token keeps its own `expires_in_days`, because a pipeline's lifetime is a
  real decision made by somebody who knows the pipeline.
- **CLI grants** (`png_`) are what `panorama login` leaves on a laptop. Long
  lived, and deliberately **not** spendable: the gateway refuses one outright,
  so a grant that leaks cannot buy model calls. It exchanges for a gateway token
  at one endpoint, which mints a new one and retires the last, so a grant holds
  at most two working tokens and revoking it stops the machine within one token
  lifetime instead of whenever somebody notices.

  Approval is a console screen. The two device-flow endpoints are
  unauthenticated because the caller having no credential is the problem a
  device flow solves; neither grants anything, names an account, or says whether
  one exists, and the decision itself happens on an authenticated route that
  carries CSRF like every other write.

  **All of it is switched off in this release, and the switch is a setting.**
  `cli.sign_in` is off by default; `requireCLISignIn` refuses the device flow,
  the grant poll, the token exchange and the approval endpoints; `/setup/tools`
  serves no `cli` block, so My setup offers one credential rather than a
  choice; and `/cli` resolves and says the flow is off, because the
  address is in documentation and "there is no such screen" would send somebody
  looking for a broken deployment. An engineer's tool therefore holds a gateway
  token minted on My setup, which lives `auth.GatewayTokenTTL` and is re-minted
  there by hand when it runs out.

  **Two switches, because they answer different questions.** The setting is
  what the server reads at request time. `cliSignInReleased` is a build
  constant, and all it does is refuse the write that would turn the setting on
  — so the release that hands the CLI over flips one declaration rather than
  touching the request path. The seam is deliberate: a deployment that writes
  the row itself gets the flow, and the end-to-end suite is what does, because
  a switch nothing can ever turn on is a switch nothing tests. The refusal is
  on the server and not in the console, for the same reason every other one is:
  a screen that hid a button and called that the rule would be the control this
  console is not.
- **Service tokens** (`pnk_`) are what a pipeline holds. They belong to a team
  rather than a person, bill that team's budget, and survive the engineer who
  created them — `created_by` is provenance and is nulled when that account
  goes, while `team_id` is the principal and cascades.

  `store.RoleService` follows the `RoleSetup` pattern: absent from `ValidRole`
  and from the `CHECK` constraints, so it cannot be written to a person or
  mapped to a group. Where it differs is which door it opens — none. It is
  refused in `authenticatedAllowing`, the one place every console route passes
  through, rather than by a role list, because a role list only refuses what it
  does not name and there is no console route a pipeline should ever reach. A
  handler added tomorrow is closed by default. `authenticated` checks no roles
  at all, so without that refusal a service token would reach `/api/v1/me`,
  `/usage`, `/tokens` and `/requests`.
- **Console sessions** (`pns_`) are what a browser holds. Cookie-borne, so every
  write carries a CSRF header as well; the token comes from
  `GET /api/v1/auth/session` rather than a readable cookie, because Panorama
  sends no CORS headers and that makes the response something only a same-origin
  page can read.
- **Setup codes** (`pnb_`) are the one principal Panorama manages itself.
  Redeeming one opens a **setup session**: not an account — no row in `users`,
  no email, no name, no password — that may configure this deployment (identity
  connection, group mapping, model providers) and nothing else, for an hour.
  Minted and logged at startup while nobody can sign in, and by `panorama admin
  setup` at any time.

  It is not an account because Panorama does not own identities: an
  administrator created from a code would need an email to be adopted by its
  owner's SSO sign-in, and `provision` recomputes the role from groups at every
  sign-in, so it would demote itself the first time its owner used SSO. The
  first account is created by the first sign-in, and a deployment with no SSO
  is reached with a gateway token.

  No password, because the command that would reset one is already proof of
  access to the deployment — a password would be a second credential guarding
  the same door.

**Service tokens are a table of their own, not a nullable `user_id`.** The
alternative is destructive on SQLite, which cannot drop `NOT NULL`: relaxing
that column means rebuilding `gateway_tokens`, and `requests.token_id`
references it `ON DELETE SET NULL`. SQLite's `DROP TABLE` performs an implicit
`DELETE` that runs foreign-key actions, so the rebuild would silently erase
which token served every request ever recorded.

**A service token's spend reaches its team by a union, not a join.** Team spend
is otherwise derived by joining `team_members` on `requests.user_id`, and a
pipeline has no user id, so without the second arm its cost would be counted
at the organization level and nowhere else. The arms are made disjoint in the
query (`user_id IS NULL` on the service side) rather than assumed disjoint
because of what the data plane writes: trusting the writer is how a union
becomes a double count the first time some other path fills in both columns.

**Two intervals govern token rotation and they come from one number.**
`panorama token` refreshes at half a token's life, so anything it returns is
good for at least half a lifetime; the generated
`CLAUDE_CODE_API_KEY_HELPER_TTL_MS` is two fifths, so the helper always asks
before that runs out. Setting the helper above the refresh point would have
Claude Code use an expired token for the remainder of its own interval — the
tool asks on its own schedule and then uses whatever it got until the next ask.
That arithmetic has a test rather than a comment.

Every one of them is stored as a SHA-256 and never in the clear.

`store.RoleSetup` is an authorization label on a live principal and nothing
more. It is absent from `ValidRole` and from the `CHECK` constraints on
`users.role` and `oidc_group_roles.role`, so it cannot be written to a person or
mapped to a group by any code path, and the database would refuse it if one
tried. `authenticated` refuses a setup principal outright; only a route whose
`requireRole` names `RoleSetup` admits it, so a handler added later is closed by
default rather than quietly reachable.

**The OIDC connection is configuration; the group mapping is data.** Issuer,
client id and client secret live in the file and the environment beside every
other secret — a client secret in a database is a client secret in a backup.
The group-to-role mapping lives in the database, because the names of your
groups are organizational data an admin edits from a screen. The strongest
matching group wins, so a role never depends on the order a provider listed
someone's groups, and a role change revokes that person's other sessions.

**A role a person assigned is not the mapping's to revise.** Every sign-in
recomputes the role from group membership — that is the whole mechanism by
which removing somebody from a group at the provider removes their access — but
applied to every account it makes a directly assigned role impossible to hold.
An administrator promotes somebody, that person signs in, matches nothing, falls
to the default role, and the promotion is gone with nothing admitting it
happened. The grant the product depends on most would be undone silently: the
account that owns a fresh deployment would be demoted by its own second sign-in.

So `users.role_source` says who decided. `sso` is recomputed at every sign-in;
`manual` is left alone until somebody hands the account back. The first
sign-in's admin grant is `manual`, because it exists precisely where the mapping
cannot yet produce an administrator. Handing an account back does not recompute
anything on the spot: group membership is the provider's record and Panorama
keeps no copy — it lets the next sign-in decide.

**Administrator is the only role assigned by hand, and the list of who holds one
that way lives beside the mapping it is an exception to.** Settings carries
*Direct administrators*; People shows a person's role and says where it is
decided, and nothing else. A control on the person would make each exception
easy to create and impossible to audit: an administrator reading the mapping
could not tell that four accounts bypassed all of it without opening four
drawers on another screen. A rule whose exceptions are invisible from the
rule is not a rule anybody can check.

Narrowing it to administrator is the other half. A pinned role is a role the
provider can no longer revoke — worth it for the account that owns the
deployment, and a silent divergence for anybody else — so a demotion is an
unpin rather than an assignment, and the mapping decides at the next sign-in.

**Assigning a role is what pins it, whether or not the role moves.** The common
entry on that list is somebody the mapping already made an administrator, whom
an administrator wants to *keep* one when their groups change. Acting only on a
change would make exactly that case a write that silently does nothing.

**A hand-back that no group could undo is refused.** Unpinning is not a role
change, so `refuseLastAdminChange` never sees it; what it is is a demotion that
arrives at the person's next sign-in, hours or days later, with a database edit
as the only way back. `refuseStrandingAdmins` asks whether the mapping *could*
grant admin — a group mapped to it, or a default role of admin — because whether
it *will* depends on group membership Panorama does not keep. The last pinned
administrator on a deployment where the answer is no cannot be handed back, and
the panel says so before anybody tries.

**The claim names a token carried are recorded; no claim value ever is.** "The
groups claim is empty" has two opposite causes — the claim is called something
else, or the provider is not emitting it. Without a record nothing in a
deployment tells them apart, and administrators alternate between the two fixes
a sign-in at a time. `oidc_observed_claims` holds names and two counts, and `panorama sso
claims` turns the disjunction into an answer. Names are schema; values are
somebody's email, subject and group membership, and there is nowhere in the
schema to put one.

**An issuer that names no tenant needs a domain, and Google's is the one that
names none.** Every other provider Panorama documents has an issuer
that identifies the organization - an Entra tenant id, an Okta org, a Keycloak
realm, a Cognito pool - so reaching it at all means being one of their people.
`accounts.google.com` is one URL for every Google account there is: a verified
token proves who somebody is and nothing about whether they belong here, and
the first account to sign in owns the deployment. So `oidc.allowed_domains`
exists, is empty by default, and **is required when `config.SharedIssuer` says
so**: Panorama refuses to start on Google's issuer without it, for the reason
it refuses the development sign-in off loopback - the failure is an open door
nothing on a running system would ever complain about. Google's consent screen
set to Internal closes it too, and Panorama cannot see that setting.

Which claim carries the domain depends on the issuer, and at Google it is `hd`
and never the address. Anybody may open a personal Google account on a work
address and it outlives the mailbox, so somebody who left last year still holds
a verified `them@company.com`; `hd` is present only on an account the Workspace
manages, which is why Google's own documentation says it "must" be checked.
Elsewhere the list narrows a tenant by the domain of an email the provider has
not marked unverified, and `hd` is not read at all, since any provider can be
configured to emit a claim by that name. The refusal happens **before the
sign-in is recorded and before `provision`**, which is where the first-account
grant lives. `domainRefusal` is one pure function so that it has a table of
cases and not a fake identity provider.

On the CloudFormation stack it is a **parameter, not a key of the OIDC secret**,
against that secret's own comment about not splitting a connection. The secret's
value is written once at create, so an existing stack's secret may have no
`allowed_domains` key, and ECS refuses to start a task that references a JSON
key its secret lacks. The rendered configuration carries the key for a shared
issuer with a placeholder validation refuses, so a block pasted unread fails at
startup naming the key.

**AWS IAM Identity Center is not an OIDC provider**, and so cannot be a
first-class one. Identity Center's OAuth 2.0 support is trusted
identity propagation — exchanging tokens another provider already issued — and
sign-in for customer managed applications is SAML 2.0 only. Worse for this
product, the attributes it can put in an assertion are name, email, subject and
a few directory fields, with **no groups attribute at all**, so group-to-role
mapping cannot work through it even via a Cognito bridge. It is therefore not a
preset. Its absence is explained in `panorama sso presets` and in the README
rather than left to be rediscovered, because a Bedrock gateway is an AWS
deployment and this is the first place anyone looks.

**SAML is not enterprise-gated.** For providers like Identity Center it is the
only way to sign in at all, so gating SAML would gate SSO, and SSO is not gated.
Unimplemented, but it belongs in the open-source edition when it lands.

**Nothing validates the connection by storing it.** `internal/ssosetup` probes
an issuer, renders the configuration for each packaging shape, and writes
nothing; `panorama sso probe` and the console's preflight are two doors onto the
same functions, because two generators of one artifact drift. In compose, Helm
and ECS the config inside the container is not the source of truth, so a command
that "saved" the connection would be lost on the next deploy — or worse, leave
the running deployment disagreeing with its manifest.

**The groups a provider sends are recorded; who was in them is not.** The role
mapping asks for group names, and nothing else could tell an admin what theirs
are called: Entra emits object IDs, Keycloak can emit a full path, Identity
Center emits either depending on an attribute mapping. A wrong guess grants
nothing and fails silently, so the strings that arrive are stored and offered as
a picklist. Only the names — membership stays the provider's record.

**A group is matched without regard to case, so it may only be mapped once.**
`ResolveRole` lowercases both sides, which is what makes `Platform` and `platform` one
group — and two rows for one group are two answers, with the one that binds
decided by the order rows came back in. The write path refuses the pair, in the
handler rather than only in the console, because the mapping is also written by
scripts holding a gateway token; a pair that exists anyway resolves to the
stronger role, the same way every other ambiguity here does. Casing is stored as
it was typed rather than folded away, because a group name is now often a
display name and rendering somebody's `Platform Engineering` as
`platform engineering` would be the console inventing its own spelling.

**What a provider calls a group is per-provider, and the notes travel with the
preset.** `internal/ssosetup/presets.go` carries scopes, the claim name and the
ordered notes for eight providers, rendered by both the console and `panorama
sso presets` from one source. Two of those notes recur and are the ones worth
writing carefully: almost nothing emits group membership by default, and several
emit identifiers rather than names once it is on. Entra is the sharp case — the
source options in its dialog only cover groups synced from on-premises AD, and
cloud-only display names need `cloud_displayname` added to `additionalProperties`
in the app registration's manifest, which the dialog never mentions. **None of
this is load-bearing**: `oidc_observed_groups` records what actually arrived, so
a provider with no preset is a supported path rather than a dead end, and a note
shortens the loop from two sign-ins to one rather than being the mechanism.

**A display name is mutable and an object id is not**, and the presets say so
where somebody is about to choose. Renaming a group at the provider stops the
mapping matching, silently, granting nothing — the same failure a typo makes.
Names are easier to read; ids are the thing that keeps working.

The failure this exists to catch: **a connection that authenticates and carries
no groups looks like complete success** and puts everyone on the default role.
`panorama sso doctor` reports that as a failure, along with a `base_url` that
would hand out an unreachable redirect URI, and a mapped group nobody has ever
been seen in. Config validation still owns "an issuer requires a secret" — the
doctor does not re-derive it, because two derivations of one rule drift.

**Setup state is derived, not remembered.** A stored "identity: done" flag still
says done after somebody removes the issuer from the configuration. Only two
things are recorded: that a step was skipped on purpose, and that a live
invocation succeeded against a provider at a moment, on that provider's row and
cleared when its region changes - which is the one thing that distinguishes a
working provider from a plausible one.

## Integrations

**Connected in the console, stored in the database, encrypted.** Connecting
GitHub is not initial setup — it is an operational decision made months after a
deployment is stood up, often by somebody with no access to its manifest, and
requiring a redeploy would put a release cycle in front of a checkbox.

The backup argument for keeping credentials in the file is answered rather than
dismissed: the credential is sealed with AES-GCM before it is written and the
key stays in the configuration, so a dump, a replica or a stolen volume carries
nothing usable. See `internal/secrets`. The key is any passphrase of sixteen
characters or more — derived with HKDF — or the 32 bytes `generate-key` mints,
used as given, which keeps credentials stored under a generated key readable. Two consequences worth knowing: a
deployment with no `encryption_key` cannot store a credential and says so at the
moment somebody tries, and losing the key means re-entering the credential
rather than a recovery problem.

**The credential is write-only.** No endpoint returns it, including the one that
accepts it — the response says what was stored, never echoes what it was given.
`IntegrationSecret` is a separate store function from `IntegrationConfigs` so
that the ordinary path of rendering the screen cannot pick one up by accident.

**The registry is resolved, not held.** It is rebuilt from the database each
confirmation pass, because a connection can change at any time without a
restart: one made at ten past the hour is used by the pass at quarter past, and
`Confirmer.Run` deliberately does not exit when nothing is connected, because
something may be ten minutes later.

**The configuration file holds what a deployment cannot start without**: how to
reach the identity provider, the key that protects everything else, and a region
to seed the first model provider with. Anything an administrator turns on
afterwards - model providers and their regions included - belongs in the
database, behind a screen.

Confirmation, not synchronisation. A source is asked about the work items
Panorama has already seen and about nothing else, so read scope covers only what
engineers touched and the call count is bounded by conversations rather than by
the size of the organization.

**Confirming a work item does not move the attribution that found it.** GitHub
agreeing that a branch exists does not make the observation better, and GitHub
never having heard of it does not make it wrong — the branch may never have been
pushed. What confirmation adds is a relation, recorded as one: branch, opened as
pull request, implementing ticket. The outcome rollup follows exactly one hop,
because a branch never merges and its pull request does; two hops would carry a
branch's spend to a ticket, which is not what cost per merged pull request
counts.

**`status` and `outcome` are separate columns.** "Merged", "Closed", "Done",
"Released" and "Won't Do" are five words and three outcomes, and a metric
grouped on the source's own word gains a sixth category the day somebody renames
a Jira workflow state. `status` is for display; `outcome` is the only thing the
metrics read.

**An unreachable source confirms nothing.** Carrying on past a failed health
check would write "not found" against every reference and turn one broken API
into a deployment-wide claim that none of the work exists.

**The mock never invents anything.** A key not in its fixtures comes back not
found, exactly as an unknown key would from the real API, and the console labels
a mock-backed integration as a mock wherever it appears. Naming a provider that
is not implemented is refused at startup rather than accepted and quietly doing
nothing.

## Web search

**Search is a tool the harness calls, served by the gateway over MCP, not a
server-side tool emulated inside a model request.** Claude Code's WebSearch is
Anthropic's `web_search_20250305`, run by Anthropic's API mid-request; Codex's is
OpenAI's hosted `web_search` on Responses. Bedrock refuses the first and offers
the second only for GPT-5.x on Mantle. Emulating either in the proxy means
holding a stream while the gateway runs a search and invokes the model again,
and rendering one vendor's tool blocks into another's: a tool loop on the path
that promises sub-10ms, and the translation the API surfaces refuse. So `/mcp`
is a stateless Streamable HTTP MCP server offering `web_search`, and to the
model it is one more client tool, which is why it works on every route.

**It is authenticated, budgeted and recorded like a request, and is one.**
Same gateway token, same `authenticated` wrapper, same principal. The engine
decides it with `EvaluateTool`, which reads budgets only: a search names no
route, so a hard limit stops it and a soft limit warns, since
`serves_over_soft` is a property of routes. Each call is a row of `requests` on
the surface `mcp` with `web_searches` and `web_search_cost_nanos`, which is
what puts it in every total, budget, export and model breakdown through the
queries that already exist. A table of its own would be a second place for each
of those to forget, and the unpriced predicate counts a search as billable the
way it counts a token. **No query and no result is stored**: a query is written
by a model from a conversation, so it is a prompt body by another name.

**The backend is the AgentCore Web Search Tool, reached with the deployment's
role.** An AgentCore gateway with IAM inbound authorization and a `web-search`
connector target, called with SigV4 as `bedrock-agentcore`, plain HTTP like
Mantle. Checked against a live gateway rather than read from a document: no
`initialize` is needed, the reply is JSON, results are a JSON document inside
the call's one text block, dates arrive as `"05:00PM, Monday, August 31 2026,
PDT"` or `"unknown"`, and each extract is about two kilobytes, which is why the
default is five results and extracts are trimmed. Amazon's index means a query
never goes to a third-party search engine, and the role means there is no key.
`bedrock-agentcore:InvokeGateway` on any gateway is in `TaskRolePolicy`, for
the reason regions are unscoped; the search itself is authorised against each
gateway's own service role, so the task role searches through nothing an
administrator did not create.

**Only a gateway-shaped host is accepted**, because the request is signed with
the deployment's credentials: a field that took any URL would send signed
requests wherever somebody typed. `AWS_ENDPOINT_URL_BEDROCK_AGENTCORE` redirects
it for tests, environment only, logged at WARN, never a console field.

**Configured in the console, off until an administrator turns it on, unless
the deployment made its own gateway.** A row of `web_search` holds the kind,
the gateway URL, the switch and an optional price override; null prices at
`websearch.PublishedPrice` ($7 per 1,000), because the Price List API does not
carry it. Administrators only, not a setup session: where prompt-derived text
is sent is the same class of decision as a route.

**A gateway the stack created is the deployment's, and the console keeps only
its switch.** `CreateWebSearchGateway` creates the gateway and hands its URL to
the task as `web_search.gateway_url`; setting the parameter is the decision to
offer search, so search is on from the first start and nobody pastes the same
URL a second time. The console shows that gateway and the published price and
does not offer to change either: the stack deletes the gateway with the stack,
so a URL pasted over it would point somewhere nobody manages, and a price
override would be a second owner of a number the stack's gateway is billed
at. The switch stays in the row so an administrator can still turn search off.
This is not the `bedrock.region` seed: a seed is read once and then the
console owns the value, whereas here the deployment owns the gateway for as
long as it names one. `store.EffectiveWebSearch` lays the deployment's gateway
over the row, and is the one read both the backend's loader and every console
handler use, so the screen and the endpoint cannot disagree about what is
configured; a URL or price saved before the stack made its own gateway is
ignored, and a verification counts only if it was of this gateway. A value
that is not a gateway URL is refused at startup, like any malformed key.
`websearch.Service` holds the built backend and is reloaded on
the policy interval and after a save, so the tool call reads an atomic pointer
and nothing queries between the call and the search. The console reaches the
backend through `websearch.Descriptor`, the way it reaches model providers.

**My setup configures it for each tool, and only while it is serving.** Claude
Code gets a `claude mcp add --transport http --scope user` command and a
`permissions.deny` for `WebSearch`; opencode an `mcp` block in the same file as
its providers, carrying the same credential; Codex an `[mcp_servers.panorama]`
table reading `PANORAMA_TOKEN`. `tools/list` offers `web_search` only while
search is serving, and the setup screen tests the same thing, so neither
advertises a tool that answers "switched off".

**Every search failure is a tool result, not a protocol error**, because that
is what reaches the model and it can rephrase or carry on. The backend's words
reach the model only for a query it refused; a permission or gateway fault is
the deployment's, logged at ERROR, and the model is told to continue without
search and given the request id.

## Cost centers

**Spend is allocated by what it bought, not by who spent it.** A rule maps a
repository or a ticket prefix to an accounting line, and a request reaches its
line through the session that carries it. Allocating on the principal is the
obvious design and is wrong twice: a person is on several teams, so "who pays"
has two answers and no ordering to break the tie, and charging AI spend to
headcount is something finance already does through payroll. What a CFO cannot
get anywhere else is spend against a product, and this deployment already knows
it.

**A cost center is not a budget, and conflating them is the trap.** A budget
enforces on a principal because the principal is the only thing the gateway
knows when a request needs a decision — the token identifies a person or a
pipeline before a byte of the body is read. A cost center accounts for work, and
the work is not known then: attribution is per session and inherited across
turns, so the opening turn has no repository yet and a session's work item can
still be superseded. A cost center limit that blocked would admit every new
session until it was attributed and then stop somebody mid-conversation over a
repository they may not own, with nothing to switch to, which is the failure a
route's soft-limit behaviour exists to avoid. So a cost center carries a **plan**
that warns, hard limits stay on People & teams, and `/api/v1/allocation` says
`enforced: false` on the wire rather than leaving each client to assume.

**There is no fallback rung.** Ticket prefix, then repository, then unallocated.
Falling back to the spender's team is refused: it would produce a
fully allocated report in which an unknown share of every line was an assumption,
and — deciding the question — it would remove the only thing that ever gets a
missing rule written. A repository with real spend and no rule has to read as a
worklist item; absorbed into a team's line, nobody writes the rule because
nothing ever looks wrong. That is why the gap is an **Attention item** rather
than a figure on a tab.

**The allocation is a cache, and that is the point.** `work_items.cost_center_id`
is derived, never authored, and recomputed in the same transaction as any rule
write — a rollup reading between the two would see a half-applied mapping.
Rules change mid-month, and when they do **every period is restated, closed ones
included**: a mis-assignment caught in November was a mis-assignment in October,
and a report that cannot be corrected is one nobody trusts. `Reallocate` rebuilds
the whole thing from the rules at any time, which is possible only because
allocation lives on the work item and a work item is permanent.

On the work item rather than on `requests`: one pattern match per conversation
instead of one per request on the largest table, and a session upgraded to better
evidence carries its whole cost to the new work item's line, which is correct
because the work genuinely changed.

**Ticket rules are what rescue a monorepo**, and they need no integration. One
repository would otherwise be one cost center and the feature would be useless to
a monorepo shop; a branch named `feat/PAY-1183-x` is attributed to the ticket
`PAY-1183` structurally, so the work item carries the key long before anybody
connects a tracker.

**An ordered rule list is allowed here, and is not allowed beside the route
list.** A policy table there would decide an authorization question
`model_routes` already decides: two answers to "may this request proceed", and
the permissive one would win silently. These rules decide a question nothing else decides, resolve to at most
one line, and being wrong misfiles money rather than opening a door. What an
ordered list costs is predictability, and that is paid for rather than argued
away: `POST /api/v1/cost-centers/test` answers "which rule claims this
repository?" from the same resolver the recompute uses.

**A stamp is a claim, not a measurement.** `x-panorama-cost-center` overrides the
rules and is the top rung, because a pipeline building one product or a
contractor billing an engagement is not expressible as a repository pattern. It
is recorded on `requests`, never on the work item — work items are shared across
people, so a caller's override written there would move a colleague's spend to a
line they never named. `accepts_stamp` is how a line refuses claims, and reports
separate stamped from rule-derived spend because the two answer to different
standards under challenge.

A stamp naming nothing **does not refuse the request** — breaking a coding
session over an accounting label would be absurd — and **is not dropped**, which
would be a fallback masking an error. It falls through to the rules and the raw
code is kept in `requests.cost_center_stamp`, counted over the whole window
rather than over the gap: a misspelt stamp on work a rule does claim is allocated
correctly and would never appear in the remainder, so a deployment can have every
stamp wrong and a fully allocated report.

**Deleting a line with recorded spend is refused; archiving is the operation
that exists.** An archived line is hidden from pickers and takes no new rules,
and its existing rules keep matching — if archiving stopped them, closing an
account would silently move a year of recorded spend into the unallocated bucket.

**The ladder is one SQL expression.** `costCenterOf` in
`internal/store/allocation.go` is a correlated subquery even in the queries that
already join `work_items`, because a second form would be a second derivation,
and a filter that admits a row the total counts elsewhere looks wrong from
neither side of the wire.

Cost centers are configured in **Settings → Cost centers** and read on
**Analytics → Cost centers**, which is the same split the rest of the console
makes: what the deployment is set to do, and what it did.


## The documentation site

`docs/` is the public manual, at **panorama.dnx.ai/docs**. Astro + Starlight,
built here, published to the landing page's CloudFront distribution as a second
origin under `/docs/*`. The landing page stays in its own private repository
and owns the domain; nothing in `docs/` runs CloudFormation.

**It lives in this repository because a page and the code it describes change
in the same pull request, or they drift.** That is the whole argument for the
split, and it is the one thing to protect when changing any of this.

**A behaviour change is not finished until its page changes with it.** That is
a judgement nobody can automate, so it is stated here rather than pretended
into a check. What *can* be mechanical is, and none of it depends on anybody
remembering:

- **The configuration and policy references are generated**, from
  `config.example.yaml` and `policy.example.yaml`, by `docs/scripts/generate-reference.mjs`
  on every build. There is no committed copy of either page, so there is
  nothing to go stale and no check that could pass while it had.
- **The configuration table is derived from the code, not from the example
  file.** `internal/config/configdoc` reads four things that live in four
  places - the yaml tag, the environment variable in `applyEnv`, the literal in
  `Default()`, and the field's doc comment - and emits them as
  `docs/src/data/config-keys.json`. A hand-written table would be four chances
  per key across forty-four keys to be wrong.

  Three guards, because a derivation that quietly misses something is worse
  than none. `allVariablesAccountedFor` refuses to emit while any `PANORAMA_`
  variable in the package reaches no documented field, so a key parsed by hand
  fails the build rather than appearing as "file only".
  `TestTheConfigurationReferenceIsCurrent` asserts the committed JSON matches
  the source - it is committed so `make docs` needs no Go toolchain, the same
  trade `docs/csp-hashes.json` makes. And `TestEveryVariableNameFollowsTheRule`
  holds the page's claim that the variable is the key mechanically, since the
  page states that rule once instead of listing exceptions.
- **Every CLI command is asserted to appear in the reference.**
  `TestEveryCommandIsDocumented` walks the command dispatch in `cmd/panorama`
  and fails when one is missing, so the page cannot quietly fall behind the
  binary. It checks coverage and not prose, because *when to reach for a
  command* is written by somebody who understands it.
- **A dead cross-reference fails the build**, through
  `starlight-links-validator`. This matters more here than on most sites: the
  distribution's 404 serves the landing page, so a broken docs link looks like
  a marketing detour rather than a mistake.
  **It is the unversioned build that checks them**, and the workflow runs one
  on every push for that reason. Every hand-written link is `/docs/...`, which
  the validator can only check at that base, so a versioned build skips them
  and then rewrites them into its own version (`keepLinksInVersion`): Astro
  compiles the base into the URLs it generates and leaves a typed link as
  typed, so without the rewrite the first link out of the prose on
  `/docs/next` lands on the latest release without saying so. A local build
  leaves `DOCS_VERSION` unset, so a versioned build that fails is noticed only
  in CI; run `DOCS_VERSION=next npm run build` before pushing a change to the
  site's configuration.
- **The sidebar cannot name a page that does not exist**; Starlight refuses to
  build.

**The docs are absent from `make check` and from every Go build target**,
because a typo in a heading should not fail the gateway's CI. The command
coverage test is the exception and is an ordinary Go test, because "the product
and its manual disagree" is worth failing on.

### When you change code, change the manual in the same pull request

**This is an instruction to whoever is making the change, an agent included, and
it is part of the change and not a follow-up.** A pull request that alters what
somebody sees, types or is told, and leaves the manual as it was, is
unfinished, however green it is. Do it before you say the work is done, and say
in the pull request which pages you changed, or that you looked and none
needed it.

**Find the pages by searching for the words, not by guessing the page.** The
manual quotes the product: labels as `ui/app` spells them, messages as the
server sends them. So the string you just changed is the search term.

```sh
grep -rn "the old label or message" docs/src/content/docs README.md
```

Then check the places a change usually lands:

| You changed | Look at |
| --- | --- |
| A label, tab, button or screen in `ui/app/src/screens` | Every page that quotes the label, and **the screenshots that show it** (below) |
| The sidebar, the header, `ui/design-tokens.css` or `styles.css` | **All of the screenshots.** The chrome is in every one |
| A refusal or error in `internal/policy/message.go` or `internal/dataplane` | `engineers/refusals.md`, which is indexed by the first words of the message |
| A sign-in message in `internal/controlplane/oidc.go`, or a finding in `internal/ssosetup/doctor.go` or `probe.go` | The two tables in `configure/sso-troubleshooting.md` |
| A provider note in `internal/ssosetup/presets.go` | `configure/sso-providers.md`, which explains what the note only instructs |
| A failure hint in `internal/providers/bedrock/descriptor.go` | `troubleshooting.md`, *The provider check fails* |
| A configuration key | The reference table regenerates itself (`go run ./internal/config/configdoc/generate -o docs/src/data/config-keys.json`, and a test fails until you do). The **prose** that tells somebody when to set it does not: `config.example.yaml`, `deploy/compose/.env.example`, and the task page |
| A `panorama` command or flag | `reference/cli.md`. `TestEveryCommandIsDocumented` catches a missing command and nothing catches a changed flag |
| `deploy/compose/*` or the `Dockerfile` | `deploy/server.md`, `deploy/laptop.mdx`, and the README's *Deploying on a server instead* |
| A parameter or output of `deploy/cloudformation/panorama.yaml` | `start/try-it.mdx` (the Quickstart deploys it), `deploy/aws.mdx`, `deploy/cloudformation/README.md`, and the README's short form of the Quickstart |
| What My setup generates for a tool (`internal/controlplane/setup.go`) | `engineers/setup.md` and that tool's page |
| An extractor or the provenance ladder | `start/attribution.mdx` and `internals/extraction.md` |
| A decision this file argues | `internals/decisions.md`, which is where the manual keeps the why |

**Then build it, both ways.** `cd docs && npm run build` checks every link and
the CSP hash list, and `DOCS_VERSION=next npm run build` is what CI runs. A
page that gains an image or a component becomes `.mdx`; before renaming one,
search its prose for `{` and for `<` outside code, which MDX reads as an
expression and a tag. The slug does not change with the extension.

**The README carries a short form of the Quickstart and of Requirements.** They
are the two places the repository says the same thing twice, so a change to
either page is a change to both.

### A page tells somebody what to do. This file tells somebody why.

**The manual is not this file re-cut into pages.** Every section above argues a
decision to somebody who already knows the product. A reader of the manual knows
nothing about it and came to do one thing, and a page can be entirely accurate
and still never say which button to press.

So a page under **Deploy**, **Administer** or **For engineers** has one shape:
what the thing is in two sentences, *before you start*, numbered steps that use
the console's own labels, *check it worked*, *what goes wrong*, and a link to
the reasoning. Four rules keep it that way:

- **A heading names a task or a thing, never an aphorism.** "Change a region",
  not "A geography is a row, not a flag". Nobody scans or searches for the
  second, and the right-hand table of contents is made of headings.
- **Nothing a reader cannot act on.** No table names, no Go identifiers, no
  "this has a test". `model_routes` is this file's word; the manual says "the
  route list".
- **The present tense.** Why a design is the way it is belongs on **How it
  works, Design decisions**, which exists so that moving the rationale out of a
  task page is a move and not a deletion.
- **A feature that is switched off gets one note, once**, not a third of a page
  describing something the reader cannot reach.

**Every label and every message on a page is quoted from the source.** The
steps name buttons as `ui/app` spells them, and **When a request is refused**
is indexed by the first words of the messages `internal/policy/message.go`
actually sends, because that is what an engineer has in front of them. A page
that paraphrases a label is a page somebody follows to a button that is not
there.

**The console first, the CLI second.** Without the
console's analytics, cost centers and routing, this product is no different
from pointing a tool straight at Bedrock, so that is where the manual sends
people. Setting up a deployment and setting up an engineer are both done in the
console: the first-run checklist, Routing, People & teams, My setup. The CLI is
for what it is good at, which is getting in when nobody can sign in
(`admin setup`, `token create`), checks that belong in a pipeline
(`sso doctor`, `providers check`, `policy check`), and repair. Where a page
shows a command it names the screen that does the same thing, and the CLI
reference opens with that mapping. A Quickstart that created the first route
from a policy file and read the cost back with `curl` would show somebody the
gateway without the product.

**Two facts the manual and the README both depend on.** Without single sign-on, a setup code reaches the
identity connection, the model providers and the administrators list, which are
the routes wrapped in `setup(...)` in `admin.go`, and nothing else. A gateway
token authenticates `/api/v1` and not a browser. The Quickstart therefore
connects single sign-on before anything else, and nothing seeds a route, so its
route step says plainly that every request is refused until one exists. And service tokens have no screen; they are created through
`POST /api/v1/service-tokens`, which is the one task the manual sends an
administrator to `curl` for.

**The name is Panorama.** DNX Solutions sponsors and maintains it, and is named
as that where it matters (the README's header, the CLA, the image registry, the
address these pages live at) and nowhere inside the product's own name. The
legal entity is **DNX Technology Pty Ltd, trading as DNX Solutions**: legal text
(the SPDX lines, `NOTICE`, `EDITIONS.md` and the CLA) names the company, and
everything else names the sponsor. The
refusals an engineer reads, the provider name written into their tool's config
and the console's titles all say Panorama.

**The Quickstart deploys on AWS, and that order is deliberate.** One
CloudFormation template is the recommended deployment, so it is the first path
in the README, in the Quickstart and in *Deploy*; Docker Compose on a server
is the second. Compose on a laptop with the development sign-in is for working
on Panorama or a first look at the console, has its own page
(`deploy/laptop.mdx`), and is never offered as the way to start: a deployment
nobody else can sign in to teaches a team nothing about running it.

**The manual deploys the template a release ships, never the repository's
copy.** `release.yml` renders it with `deploy/cloudformation/release-template.sh`,
which sets the `Release` mapping to the release's version, attaches it to the
GitHub release and copies it to the public bucket the Launch Stack button reads
(CloudFormation's quick-create page takes a template from S3 and nowhere
else). An empty `ImageTag`, the default, runs the mapping's version: a
parameter's default would be stored with the stack and survive the next
template, and a mapping is not. Every `aws cloudformation deploy` on an
existing stack names `DesiredCount` explicitly, because a parameter left out
keeps its current value.

**Sections are grouped by who is reading**: deciding, deploying, administering,
using a coding tool, then reference and how it works. The *Administer* pages
live under `configure/` slugs, and stay there, because a renamed path is a
broken link in somebody's README.

### Diagrams are drawn from source, in both themes

`docs/diagrams/*.py` draws them with mingrammer's `diagrams`, and `make
docs-diagrams` renders them in a container. The PNGs are committed, which is
the same trade `csp-hashes.json` makes: `make docs` needs neither Python nor
Graphviz.

**Each is rendered twice, because the site is dark by default and the ground is
transparent.** Ink chosen for paper is invisible on `#1a1b1e`, and an SVG
following `prefers-color-scheme` would follow the operating system and not the
switch in the header. `ThemeImage.astro` emits both and Starlight's utility
classes hide one. Two things about that are load-bearing: no rule may set
`display` on a diagram's `img`, because it out-ranks the class that hides the
other theme's copy and both are drawn; and the component takes a class and not
a `style` attribute, because the hash-list policy drops an inline style without
a word.

**A diagram is sized for the column it is read in.** A figure is held to
58rem (`--measure-figure` in `docs.css`) however wide the pane is, so a graph
much wider than twelve inches has its 14pt labels scaled under 12px.
`preview.py` prints the size a label will
be on the page, and composites on the page colour, since a transparent PNG
opens on white and makes the dark theme look broken when it is not.

### Screenshots are of a real deployment, in both themes

`docs/src/assets/console/` holds the console as a reader meets it, 14 screens,
each in both themes. **A change to a screen that appears in one is not finished
until the image is retaken**, and a change to the chrome or the tokens retakes
all of them. A screenshot of a label the product no longer has is the manual
pointing at a button that is not there, which is the failure this whole section
exists to prevent.

**Every image is made the same way. These are not preferences.**

| | |
| --- | --- |
| Browser window | **1440 x 900** CSS pixels. Three exceptions, below, each for a reason |
| Density | **`deviceScaleFactor: 2`**, so a 1440 window is captured at 2880 |
| Themes | **Both, always, as a pair.** Dark is the console's default; light is `data-theme="light"` |
| How the theme is set | `localStorage['panorama.theme'] = 'dark'` or `'light'`, from an init script, **before** the page loads. Do not click the switch: it is in the picture |
| Wait before shooting | 1.5 s after navigation, 2.5 to 3 s on Analytics and My setup, which load after they paint |
| What is captured | The window, not the full page. A reader should see what they will see |
| Stored as | **WebP, 2000 px wide**, height in proportion, Lanczos, quality 86, method 6 |
| Named | `<screen>-dark.webp` and `<screen>-light.webp`, in `docs/src/assets/console/` |
| Weight | 40 to 110 KB each, about 2 MB for all 28. One over 150 KB has something wrong with it |
| Raw captures | Under `.playwright-mcp/`, which is ignored. Never commit a PNG |

The conversion, so that nobody chooses a different quality:

```sh
uv run --with pillow python - <<'PY'
from PIL import Image
for theme in ("dark", "light"):
    im = Image.open(f".playwright-mcp/shots/routing-{theme}.png").convert("RGB")
    im = im.resize((2000, round(im.height * 2000 / im.width)), Image.LANCZOS)
    im.save(f"docs/src/assets/console/routing-{theme}.webp", "WEBP", quality=86, method=6)
PY
```

**Which screen, from where, in what state.** The state matters as much as the
size: a roster of one person or an empty chart teaches nothing, and the
Quickstart's images have to show exactly what its steps produce.

| File | Address | Window | State | Used on |
| --- | --- | --- | --- | --- |
| `sign-in` | `/`, signed out | 1440 x 900 | **Empty database**, development sign-in on | On your laptop |
| `model-providers` | `/settings?tab=providers` | 1440 x 900 | Quickstart: Bedrock `connected`, Mantle `not configured` | Quickstart, step 4 |
| `route-editor` | `/routing`, **New route** | 1440 x 900 | **Empty database**. Name `claude-sonnet-5`, model Claude Sonnet 5 `global` chosen | Quickstart, step 5 |
| `model-catalogue` | The editor's **Browse models**, searched `claude sonnet` | 1440 x 900 | A discovered catalogue | Routes |
| `my-setup` | `/setup` | **1440 x 1000** | Quickstart, after one request from Claude Code, so Verify reads `receiving` | Quickstart, step 6 |
| `audit-trail` | `/requests` | **1600 x 900** | Seeded, with the Quickstart's request as the first row | Quickstart, step 7 |
| `analytics-spend` | `/` | **1440 x 1180** | Seeded | What Panorama is |
| `analytics-work` | `/?tab=work` | 1440 x 900 | Seeded | How attribution works |
| `analytics-attribution` | `/?tab=attribution` | 1440 x 900, **cropped to the top 800 of 1250** after resizing | Seeded | How attribution works |
| `analytics-cost-centers` | `/?tab=cost-centers` | 1440 x 900 | Seeded | Cost centers |
| `routing` | `/routing` | 1440 x 900 | Seeded | Routes |
| `people-teams` | `/people` | 1440 x 900 | Seeded | Budgets |
| `single-sign-on` | `/settings?tab=sso` | 1440 x 900 | No identity provider connected | Single sign-on |
| `web-search` | `/settings?tab=web-search` | 1440 x 900 | A gateway saved and checked, so the state reads `connected` | Web search |

The three windows that are not 1440 x 900: My setup is 1000 tall so the Verify
panel and the first instruction are both in it; Analytics is 1180 tall so the
daily chart is; and the audit trail is 1600 wide because its table has a floor
of about 1180 px and at 1440 the Outcome column is cut off. The attribution tab
is cropped because its lower third is empty.

**The three states, and how to reach each.**

- **Empty database.** The first-run card is gone the moment an account exists,
  and the route editor's backdrop should be an empty Routing screen. Start the
  shipped compose stack on a new volume (`down -v`, then `up -d`), and take
  `sign-in` in *both* themes before signing in once.
- **Quickstart.** Follow `deploy/laptop.mdx` and then the Quickstart's console
  steps (`start/try-it.mdx`, from *Check that Panorama can reach Bedrock*)
  exactly, on the shipped image. The console steps are the same screens on a
  laptop as on the CloudFormation stack, and a laptop puts no hostname or
  account in the picture: development sign-in as `you@example.com`, **Check**, a route called
  `claude-sonnet-5` on Claude Sonnet 5, a minted token, and one request from Claude
  Code started inside a git repository on a branch named like
  `feat/PAY-1183-refund-webhook`, so that Verify shows a ticket read from the
  branch. That needs real AWS credentials and costs about half a cent; the
  repository's fake Bedrock (`make fake-bedrock`) gives the same screens for
  nothing when the change is to the console and not to what Bedrock says.
- **Seeded.** `scripts/seed` on top of the Quickstart state, which writes 45 to
  60 days of coherent traffic: `make dev-seed` on the development stack, or the
  built binary with `-driver sqlite -dsn ...` against the compose volume.
  `-reset-only` removes it again, except the bedrock-mantle provider row, which
  it leaves behind and has to be deleted by hand before `model-providers` is
  retaken.

For a change that is only to the console, the development stack is enough
(`make dev`, then the console on `:5173`; see *Looking at the console*). Use the
shipped image when the picture has to be what a reader following the Quickstart
gets.

**What must never be in one.** A whole token (the list shows eight characters
and that is fine), a setup code, an AWS account id or access key, or a real
person's name or address. Sign in as `you@example.com`. Anything captured while
diagnosing, failed probes included, is deleted from the scratch database before
the audit trail is shot. Credentials used for the run are removed with the
stack: `down -v`, and delete the `.env`.

**Putting one on a page.** Through `ThemeImage` with `frame`, never a bare
Markdown image, which would show one theme to everybody:

```mdx
import ThemeImage from '../../../components/ThemeImage.astro';
import routingDark from '../../../assets/console/routing-dark.webp';
import routingLight from '../../../assets/console/routing-light.webp';

<ThemeImage
  frame
  dark={routingDark}
  light={routingLight}
  alt="The Routing screen. One card per route: its name and who it is for on the left, the model it becomes and its price on the right."
  caption="Each card is a route. Click one to change it."
/>
```

`frame` draws the hairline: the console and this site share a token set, so a
screenshot is a rectangle in the page's own colour and the hairline is the only
thing saying where it ends. The **alt text says what is on the screen**, in
full sentences, for somebody who cannot see it; the **caption says what to
notice**, in one. An image goes directly after the step it shows.

**Then look at it.** Build, serve `docs/dist` from a static server under
`/docs` (the file protocol is blocked, and the base path is `/docs`), and open
the page in both themes. Exactly one image may be visible in each figure: two
means a rule has set `display` on a figure's `img`, which out-ranks the class
hiding the other theme's copy.

**There is no script that retakes them, and that is a debt.** They are driven
by hand through Playwright, which is why every number above is written down.
The fix is the one the configuration table has: a script in `docs/scripts` that
takes this table as its input, so the specification and the pictures cannot
disagree.

**The request is real.** The one in `my-setup` and in the first row of
`audit-trail` comes from the shipped image on a fresh clone with real AWS
credentials, not from the fake.

### Versions, because a reader may be running last quarter's binary

Three paths, and `DOCS_VERSION` decides which one a build writes: `/docs` is
the latest released series, `/docs/vX.Y` is that series frozen, `/docs/next` is
main. A patch does not get a path of its own - v1.2.0 and v1.2.3 are the same
documentation, so a patch overwrites its series.

**A frozen series is never rebuilt.** `/docs/v0.9` is what the documentation
said when v0.9 shipped, not what today's prose would say about a release from
six months ago.

**Which release moves `/docs` is decided exactly as `release.yml` decides which
release moves the `stable` image tag**, and for the same reason: a backported
v1.3.5 published after v1.4.0 must not drag the unversioned path backwards. A
prerelease never moves it. Two copies of that rule would disagree the first
time somebody backported.

**The version selector fetches its list at runtime** from
`/docs/versions.json`, rewritten on every deploy, rather than baking one in. A
frozen build cannot know about versions published after it, so a baked-in list
would be correct only on the newest version and quietly wrong on every older
one - which is exactly where somebody is looking for the way forward. The list
is derived from the bucket's own prefixes, not from a manifest somebody
maintains, because a version in a manifest but not the bucket is a 404 the
selector offered.

### The CSP is a hash list, and that is why it has a check

The landing page sets `script-src 'self'` and carries no inline script.
Starlight cannot hold that line: it writes the reader's theme onto the document
before first paint, and doing that without a flash of the wrong theme requires
an inline script. So `/docs/*` carries its own response headers policy naming
the SHA-256 hashes of exactly what Starlight inlines, rather than relaxing to
`'unsafe-inline'`.

A hash list is only safe if it fails loudly, because this failure is silent: an
inline script missing from the policy is blocked, the page still renders, and
the theme toggle stops working for everybody. `scripts/csp-hashes.mjs --check`
runs as a postbuild step, so the build fails before the deploy. After a
Starlight upgrade, `--write` prints the replacement policy, and **the website
stack is deployed before the docs are**.

### Two things an S3 origin does not do for you

Both are in the website repository's `template.yaml`, in one viewer-request
function, and both are the reason a docs site cannot simply be dropped into a
bucket:

- **A REST origin serves no directory index.** `DefaultRootObject` covers the
  distribution root and nothing else, so `/docs/start/` would reach S3 as the
  key `start/` and come back 403. The S3 *website* endpoint would resolve it,
  and is public HTTP only, which would undo the private-bucket posture.
- **`/docs/*` does not match a bare `/docs`**, which is what somebody types. It
  has a cache behaviour of its own.

**The docs have their own bucket rather than a prefix of the site bucket**,
because the landing page's `make sync` uploads with `--delete`: docs under a
prefix would be erased by the next marketing deploy, silently.


## Layout

```
cmd/panorama/          single binary entrypoint
cmd/fakebedrock/       Bedrock's wire protocol, for a stack with no AWS
cmd/pricefinder/       the Prices workflow's tool; not in the image
internal/
  dataplane/           proxy, streaming, API surfaces (anthropic, openai-compat)
  providers/           Provider interface, the registry, + bedrock/ (runtime and mantle)
  attribution/         provenance ladder, extractors, session grouping
  pricing/             price table, cache-aware cost computation, long-context tiers
  pricefinder/         finds prices the binary lacks: Price List snapshots, model cards read and checked
  policy/              routes, quotas, budgets
  costcenters/         allocation rules, the glob, the stamp lookup
  controlplane/        REST API for the UI, OIDC, token minting
  ssosetup/            OIDC presets, issuer probe, config rendering, doctor
  tools/               which coding tool a request came from, as one list
  integrations/        Source interface, mock/, and github/ + jira/ to come
  websearch/           the web search backend interface, its service, the MCP tool's limits
  jobs/                interval workers: work-item confirmation, session sweep
  store/               sqlite/postgres, migrations
  auth/                gateway token minting and verification
  config/              configuration loading and validation
  fakebedrock/         Bedrock's wire protocol as a test double
  logging/             slog setup
  version/             build metadata
ui/app/                React + TypeScript console (built to ui/dist, not committed)
docs/                  the public manual (Astro + Starlight, built to docs/dist)
test/e2e/              the end-to-end suite, behind the `e2e` build tag
deploy/                compose/, cloudformation/
```

## Cost model

Coding-agent economics live in prompt caching. Account for **all four token classes** —
input, output, cache read, cache write — with separate per-model rates. Ignoring cache
classes overstates coding-agent cost several-fold and is the fastest way to discredit
the headline feature. Rates are seeded from the AWS Pricing API into a local price
table and refreshed by a job.

**Money is integer nanodollars, never a float.** `pricing.Money` and `pricing.Rate` are
int64; parsing goes through `ParseUSD*`, which reads decimal text digit by digit. A
column of costs summed as float64 disagrees with itself depending on summation order,
and nanodollars are small enough that one cheap request is not zero.

**Unpriced is null, not zero.** A request whose model has no rate — or whose token class
has none — is recorded with a null cost and counted separately, and that count appears
beside every total. Pricing an unknown at zero would understate spend and look like a
discount. This is the same commitment as unattributed spend, applied to pricing.

**A price can change with the prompt's size, and a tier replaces the whole price.**
GPT-5.x on Bedrock costs more above 272K input tokens, and the higher rate "applies to all
input and output tokens, not just tokens above 272K". `pricing.Entry.Tiers` holds it, the
threshold counts the whole prompt (`Usage.PromptTokens`: uncached, cache read and cache
write), and `Table.Price` prices at `RatesFor`. Tiers travel everywhere a rate does - the
seed, `pricing.models` (YAML `tiers:`, env `...@272000=...`), the `tiers` column - because
a surface that dropped them would bill long sessions at a fraction of their cost. The Price
List API has no tiers and a fetched rate outranks a compiled one, so `Service.keepTiers`
keeps a tier when the fetched base rate matches and otherwise keeps the threshold with **no
rate above it**: a long request goes unpriced rather than under-priced, with a WARN. That
empty tier is the table's alone; `ValidateCard` refuses one from a file or an operator.

The unpriced log line names which of two things happened (`ErrNoPrice`, which also
`errors.Is` `ErrRateMissing`): a model with no row, or a class with no rate. One message
for both would send somebody looking for a missing cache-class rate when nobody has priced
the model at all.

**A price the Price List API does not publish is found by a workflow and compiled in.**
GPT-5.x is billed through AWS Marketplace and has no Price List row on either endpoint; its
only published price is its model card. `cmd/pricefinder` (`internal/pricefinder`), run by
`.github/workflows/prices.yml` after every merge and weekly, lists every servable model in
the configured regions and, for each one neither `seed.json` nor the table prices: copies
the Price List rate if there is one (source `aws-pricing-api`, which ranks as the seed), or
else has Claude read the card (source `aws-model-card`, ranked below the seed). **The model
only proposes.** `Verify` keeps a card's price only when every figure is a cell of a row
quoted verbatim, in the column whose header names that class, under a named heading that is
not GovCloud or China, in a section that states per million tokens; one bad row fails the
card. The model is invoked through the Bedrock provider, so no new AWS client exists.
The reader is Haiku 4.5; move it to Sonnet 5 if the run summaries show it misreading
cards; a misread fails Verify and lands under "Still unpriced" with the
reason, never in the table. It reads **one region by default** (`REGIONS: us-east-1`): regions decide which models are
looked for and nothing about price, because every region lists both `us.` and `global.`
profiles and publishes both rate sets, and the table has no region in its key.

The table lives in S3 (the bucket in the `PRICES_BUCKET` repository variable, under
`pricing/`, versioned, written with conditional puts by the role in `PRICES_ROLE_ARN`),
not in the repository, so a beta cut
from a PR branch has it and nothing expires. It **only grows** - a priced model is never
read again; a stale row is deleted from the object by hand. `release.yml` reads it with the
same role before assuming the ECR one, checks its shape, overwrites
`internal/pricing/found_prices.json`, records the S3 version and digest in `image.json`, and
attaches the table. **The repository's copy stays empty** (a test says so), so a table
generated on a laptop cannot ship by being committed. **A source build fetches a
release's table instead** (`FOUND_PRICES` in the Dockerfile: `latest`, a tag, or `none`,
checked with `pricefinder -check` before it is embedded), so a compose deployment prices
what a published image prices; a fetch that fails stops the build rather than embedding
nothing. CI and the release build pass `none`, the release because the table it already
placed is the one it ships. The binary logs the embedded table's
digest at startup as `found_table`. What nothing could price is a warning on the run and one
issue, "Models with no price", rewritten each run and closed when empty.

### Bedrock pricing facts

Three things, each established against the live API rather than from documentation:

1. **Current model prices are not under the `AmazonBedrock` service code.** That
   catalogue holds five Anthropic products, all retired models. Everything anyone runs a
   coding agent against is published under **`AmazonBedrockFoundationModels`**, in a
   different shape with different attributes.
2. **The `us.` / `global.` inference-profile prefix is a price input.** AWS publishes two
   rate sets per model; global is ~10% cheaper. Sonnet 5 input is $2.20 per million
   through `us.`, $2.00 through `global.`. The prefix is therefore part of the price-table
   key and must never be normalised away.
3. **Prompt cache writes have two TTLs at different prices** — five minutes and one hour,
   roughly 1.6x apart. Bedrock reports the split in `cache_creation`; a single
   cache-write number cannot be costed correctly.

The pricing catalogue also identifies models only by display name, so the refresher joins
`pricing:GetProducts` against `bedrock:ListFoundationModels` to reach a model id. Usage
types are matched against an **allowlist**: batch rates are half the on-demand ones, and
pattern-matching would eventually halve a bill.

## Attribution

Every request is attributed to a work item via a **provenance ladder**; higher rungs
always win.

1. **Explicit stamp** — client-injected headers, generated per-user. Precision tier.
2. **Structural inference** — extracted in flight from the request body's structural
   ground truth: the environment block, `git`/`gh` output inside tool results, file
   paths, remotes. Regex and structure matching only.
3. **Content signals** — ticket or PR keys mentioned in conversation text. Low
   confidence; used only with session corroboration. Structural always beats content.
4. **Unattributed** — surfaced honestly, everywhere.

A session is recognised by `x-panorama-session` when a client stamps one, then, from
opencode only, by the `x-session-id` it sends unasked (`x-parent-session-id` first, so a
subagent's turns join the conversation that started it), from Claude Code only, by the
`X-Claude-Code-Session-Id` it sends unasked, and otherwise by a hash of the working
directory and the opening message. Claude Code's header is what keeps its fast model's
background calls in their conversation: they open with their own short prompt and no
environment block, so by inference each was a session of its own with no repository. A header is read only from the tool whose
meaning it has, and every one is hashed with the principal, so naming a session never
joins somebody else's.

Attribute once per session and inherit across turns; do not re-guess per request. Record
provenance on every attribution and display it: "PAY-1183 — branch name (structural,
high)."

### Reading request bodies

Four things, each of which made an extractor that looked correct find nothing:

1. **The body is JSON, so the text inside it is escaped.** A newline is the two
   characters `\` and `n`. A pattern written against the text a human sees —
   `Current branch: main\nMain branch:` — matches nothing. Captures terminate at
   the next quote or backslash instead, which reads one field out of escaped
   JSON without unescaping the document.
2. **A cheap literal anchor is usually in the middle of what the pattern
   matches** — `/pull/` inside a pull request URL. Starting the match window at
   the anchor puts the start of the pattern outside it. Windows begin a fixed
   distance *before* the anchor.
3. **`"text"` matches inside `"type":"text"`.** A content block is
   `{"type":"text","text":"…"}`, so searching for the characters `"text"` finds
   the value of `type` and returns the word `text` — identically for every
   request from every conversation, which merges the whole deployment into one
   session. Keys are matched as keys: name, colon, then a string.
4. **The opening message has to be cut at its own end.** Taking a fixed number of
   bytes spills into the messages after it, and those grow every turn, so the
   fingerprint changes on each request and every turn becomes its own session.

The extraction rules are open source so the privacy claim is auditable. They are
also the part most likely to rot: the environment block's spelling differs between
Claude Code versions, and an extractor pinned to one spelling does not fail
loudly — it silently stops attributing. Support both.

## Policy

Where a request goes is decided by its route (above). Budgets exist at org, team,
and user level; soft limits warn and close the routes marked as not serving past
one, hard limits block everything.

**A route reads budgets and cannot set one.** Otherwise "what may I spend" would
have two answers that disagree the first time somebody edited only one of them.
The screens say in words where the other half is, for the same reason.

**Nothing on the policy path touches the database.** Routes, budgets, team membership and
spend all live in an in-memory snapshot, reloaded on an interval and incremented as
requests complete. A `SELECT` between a keystroke and the first token is exactly what
this gateway promises not to do. The consequence is stated rather than hidden: a request
is priced only after it finishes, so enforcement is always against spend as of the last
completed request.

**A blocked request must never count against the budget that blocked it.** The predicate
is `attempts > 0` — did a provider actually get invoked — and it is applied identically
in the ledger and in the reconcile query. Two derivations of the same rule drift.

A route names a model directly, and **the only models it can name are ones
discovery has seen**: both the console and `panorama policy apply` refuse a
target absent from `provider_models`, or on a provider that is not configured,
because the time to find that out is at the desk of the person who wrote it, not
at 3am.
There is no allowlist for a route to escape — the route *is* the allowlist, and
by the time its model is being invoked the request has already been admitted.

## Building

No host toolchain is required; every command runs in a container via the
`Makefile`. `make check` runs what CI runs. If you have Go and Node locally and
prefer them, `make <target> HOST=1`.

**The console's build output is not committed, and every Go target depends on
it.** `make ui` builds `ui/app` into `ui/dist`; `make build`, `make test`, and
`make lint` all run it first, because `go:embed` will not compile against an
empty directory. `go build ./...` on its own therefore does not work from a
fresh clone. That is deliberate: the alternative is committing a minified
bundle, which would put generated code in every diff that touches a screen.

`make ui-dev` runs Vite against a local `make run`, so iteration is against the
real API rather than a mock layer that drifts from it.

**`make dev` is the working loop**, and it is not the shipped artifact. It runs
the Go toolchain against a bind mount of the tree: console on `:5173` with hot
reload, gateway on `:8088` rebuilt and restarted by `air` on a `.go` change,
Postgres behind both, and a shell in every container. Postgres because it is
what a production deployment runs; the evaluation stack is where SQLite is
exercised. `internal/jobs` runs interval workers in the gateway's own process,
on either database, so choosing SQLite gives up no background job.

Two seams are worth knowing before changing them. `go:embed` will not compile
against an empty `ui/dist`, and no browser in that stack loads it, so the
entrypoint writes a placeholder page rather than putting a Vite build in front
of every `up` — an existing `make ui` output is left alone. And both watchers
poll: file change events do not cross a bind mount reliably on macOS, and a
reload that quietly stops firing costs more than the polling does.

### Looking at the console

**A change to a screen is not finished until somebody has looked at it, and on
this stack single sign-on is bypassed so that somebody can be an agent.**

```
make dev                     # console :5173, gateway :8088, Postgres behind both
open http://localhost:5173   # the sign-in gate
```

The gate offers **"Sign in as the development account"** under the usual SSO
button. One click, no identity provider, an administrator session for
`dev@localhost`, enough to reach every screen, both themes (the `Light` /
`Dark` switch is in the header), and every drawer and modal.

For working on the console, use **this** compose stack
(`deploy/compose/docker-compose.dev.yml`, which is what `make dev` runs). It is
the one that sets `PANORAMA_DEV_SIGN_IN_AS` for you.

**The same variable is what the manual's *On your laptop* page uses, on the
shipped image.** Somebody trying Panorama on a laptop has no identity
provider, and without a sign-in the console is reachable only as far as a setup
code reaches, which is single sign-on and the model providers. Routing,
Analytics, cost centers and My setup are the product; a quickstart that drove
the CLI instead would show a gateway no different from pointing a tool straight at
Bedrock. So `.env.example` offers the variable commented out, that page sets
it, and both say it is for a laptop. What keeps that safe is the server
and not the wording:

- The route is **not registered at all** unless that variable names an account,
  so on every other deployment `POST /api/v1/auth/dev` is a 404 from the mux:
  no handler to reach and no check that could be wrong.
- With it set, **Panorama refuses to start unless `server.base_url` is
  loopback** (`localhost`, `127.0.0.1` or `::1`, matched as names and never
  resolved). A Helm values file, the CloudFormation template or a server's
  `.env` that carries it produces a deployment that does not boot, which is
  the point: the failure this guards against is an unauthenticated
  administrator door that nothing on a running system would ever complain
  about. The shipped compose file on a laptop *does* boot with it, because its
  default base URL is `http://localhost:8088`.
- It is logged at WARN at startup and again on every use.

The stack reads the compose file once, so after changing it: `make dev` again.
Hot reload covers `ui/app/**` in the browser and rebuilds the gateway on a `.go`
change; a `docker-compose.dev.yml` change does not.

**The workflows that reach AWS run where `vars.PUBLISH` is `'true'`, each in a GitHub
environment** (`release`, `docs`, `aws-tests`), and every account-specific value (role ARNs,
the registry, the prices bucket) is a repository variable, so the tree names no AWS
account. A fork has no `PUBLISH`, so those jobs are skipped and its runs stay green. The
variable is not the security boundary: each role's trust admits only its environment's
subject (`repo:<owner>@<owner-id>/<repo>@<repo-id>:environment:<name>`), and each
environment admits only its refs: `release` the `v*` tags, `docs` `main` and `v*`,
`aws-tests` `main`. The docs role is its own and not the role that deploys the
landing page: that one owns the website stack, and a role this public repository can
assume must not be able to rewrite somebody else's site. `docs.yml` lists the four
actions it needs.

## Testing what a tool actually does

`make check` is the fast gate and does not change. Beside it:

```
make e2e        # the built binary, a real database, a fake Bedrock, real coding tools
make e2e-live   # the handful of checks that need real AWS credentials
```

**The fake sits at the wire, not at `providers.Provider`.** Every double in the
unit tests implements the provider interface, which means the whole of
`internal/providers/bedrock` (SigV4 signing, event-stream frame decoding,
`classify`'s error mapping, `prepareBody`'s beta translation) would never
execute in a test. `internal/fakebedrock` speaks the protocol instead, so all
of it runs.

**Nothing in the product exists to allow that, and nothing should.** The
provider builds its clients with stock `awsconfig.LoadDefaultConfig`, which
populates `cfg.ConfigSources` from the environment, so the SDK's own
service-specific endpoint override applies:
`AWS_ENDPOINT_URL_BEDROCK_RUNTIME` redirects invocations,
`AWS_ENDPOINT_URL_BEDROCK` redirects discovery. bedrock-mantle is not an SDK
client, so it reads the same convention itself, `AWS_ENDPOINT_URL_BEDROCK_MANTLE`,
and warns whenever it is set. An endpoint field on a provider's settings would be
a production surface with no production reason to exist, and an operator who
found it could point a deployment's traffic anywhere - which is why the variable
is environment only and never a console field.

**The gateway runs as a subprocess of the built binary.** In-process would test
the packages; this tests the artifact — its own configuration loading, its
embedded console, its real path to a provider. Bootstrap is `panorama token
create`, which makes the account and its credential in one step with no identity
provider: `panorama login` is a device flow that waits for a human, and the
suite drives that too, by parsing the code and calling the endpoint the console
calls.

**A fault on bedrock-runtime must be armed with `fakebedrock.Always`.** The AWS
SDK retries a throttle or a 5xx three times before the provider sees an error,
so a fault armed once is consumed by the first attempt and the call succeeds.
Count is attempts at the wire, not calls to `Invoke`. bedrock-mantle has no retry
loop, so a fault armed once is the one its request meets.

**`cmd.Dir` is not enough; `PWD` travels with it.** Setting a child's working
directory leaves `PWD` naming the parent's, and a tool that reads the variable
rather than calling `getcwd` then reports the wrong repository. It looks exactly
like an attribution bug; the tell is a recorded request body naming this suite's
own package directory.

**The two supported tools do not carry the same context, and the tests say so
rather than averaging them.** Claude Code's environment block contains `Current
branch:`, so a work item is attributed structurally with no client configuration
at all — that assertion is the product's headline, end to end. opencode (1.18.29)
carries its working directory and **no branch**, so its traffic reaches a
repository and never a ticket. That is asserted rather than logged, so opencode
gaining a branch arrives as a failing test with an obvious fix instead of as a
silent improvement in a number nobody watches.

**The live suite is gated by a build tag, not a skip**, because it spends money;
`make e2e-live` runs it. It covers only what a fake cannot settle — whether these
credentials can invoke anything, what the catalogue and the price list say today,
and which beta flags Bedrock still refuses, checked in both directions. Every
request it makes asks for one output token.

## Scope

**v1 is what a FinOps buyer would miss in their first hour** (README, Principles).
Jira, the Grafana pack, anonymous telemetry and Helm are out. Each change lands
as a reviewed pull request with tests.

The OpenAI-compatible surfaces are in, because bedrock-mantle serves open-weight
and other non-Claude models over nothing else, so a Mantle provider without them
would be Claude by a second road. `/v1/chat/completions` and `/v1/responses` are
relayed natively, never translated, and each has its extractor
(`attribution.ShapeChat`, `ShapeResponses`). Three rules bind them. Responses are
**stateless**: `store` is forced false and `store: true`,
`previous_response_id` and `background` are refused, because a stored response
is a prompt and its answer kept on this deployment's account. A `service_tier`
other than the default is refused, because the price table holds the standard
rate. And a Chat Completions stream is asked for its usage chunk on the client's
behalf and the chunk withheld from a client that did not ask, because an
unmetered stream is spend nobody can see. **Do not add a surface without an
extractor to match** - the traffic would land in the unattributed bucket, which
is the one number this product promises to be honest about.

The **GitHub source** is a GitHub App, with a personal access token accepted for
evaluation and labelled as one. **It is experimental** until it has been run
against a real organization, and says so in the console's Integrations rail, the
README and the manual; dropping the word is a change to all three. Two rules it exists to hold: a reference GitHub
does not have comes back absent, because a branch nobody pushed is not a broken
integration; and anything that stopped it from asking (rate limit, revoked
installation, network) is an error, because reporting "not found" without having
asked would stamp that claim across every work item in the deployment. The
`merged_at` field decides the outcome, never `state`: a merged pull request
reports state `closed`, so grouping on state would file every merge as abandoned
and zero the headline number.

**Jira is out of v1**, and naming it is refused at startup.
