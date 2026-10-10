# Architecture

For someone changing the code. What the pieces are, and the decisions that are load
bearing — the ones where the obvious alternative is wrong for a reason that is not
obvious.

[`CLAUDE.md`](CLAUDE.md) is the full conventions list and the review rules.
[`README.md`](README.md) is the product and how to deploy it.

## The shape

One Go binary containing the data plane (proxy), the control plane (API and jobs),
and the embedded React console. Internally separable — the future SaaS shape is a
hosted control plane with a customer-run data plane — but it ships as one artifact.

| Concern | Choice |
| ------- | ------ |
| Language | Go, single static binary (pure-Go SQLite, no cgo) |
| Storage | SQLite by default, Postgres for production; same schema via migrations |
| Human auth | Generic OIDC |
| Tool auth | Per-user gateway tokens, revocable and rotatable |
| Provider | `Provider` interface; bedrock-runtime and bedrock-mantle in v1, each with its own region |
| UI | React + TypeScript, built by Vite, embedded with `go:embed` |

```
cmd/panorama/          single binary entrypoint
internal/
  dataplane/           proxy, streaming, API surfaces (Messages, Chat Completions, Responses)
  providers/           Provider interface, the registry, + bedrock/ (runtime and mantle)
  attribution/         provenance ladder, extractors, session grouping
  pricing/             price table, cache-aware cost computation
  policy/              routes, quotas, budgets
  controlplane/        REST API for the UI, OIDC, token minting
  ssosetup/            OIDC presets, issuer probe, config rendering, doctor
  tools/               which coding tool a request came from
  integrations/        Source interface, mock/, github/
  jobs/                interval workers
  store/               sqlite/postgres, migrations
  auth/                gateway token minting and verification
ui/app/                React console (built to ui/dist, not committed)
deploy/                compose/, cloudformation/
```

Model IDs are namespaced by provider (`bedrock/us.anthropic.claude-...`,
`bedrock-mantle/openai.gpt-oss-120b`). A provider is a kind, a region and a
switch, stored in the database and configured in the console; `bedrock.region`
in the file seeds the first one. Nothing in core imports AWS SDKs outside the
Bedrock provider. The AWS packaging is a
CloudFormation template and imports nothing.

**v1 is single-organization.** One deployment is one org. Multi-org is enterprise;
do not add org scoping to v1 tables speculatively.

## The hot path

**Streaming is sacred.** Nothing on the request path may buffer a stream, block, or
add meaningful latency. Target is sub-10ms added time-to-first-token.

**Nothing on the policy path touches the database.** Routes, budgets, team
membership and spend live in an in-memory snapshot, reloaded on an interval and
incremented as requests complete. A `SELECT` between a keystroke and the first token
is exactly what this gateway promises not to do.

The consequence is stated rather than hidden: a request is priced only after it
finishes, so enforcement is always against spend as of the last completed request.

Inline work on the request path is regex and structural matching only. Everything
else runs asynchronously against already-extracted metadata — never against a body,
because no body was kept.

**A blocked request must never count against the budget that blocked it.** The
predicate is `attempts > 0` — did a provider actually get invoked — applied
identically in the ledger and in the reconcile query. Two derivations of one rule
drift.

## Bedrock is a dialect, not a mirror

The first-party Anthropic API and Bedrock's Anthropic surface are not the same API,
and the gaps are load bearing. Each was found by a request failing, not by reading a
doc:

1. **Inference profiles.** Current Anthropic models reject their bare
   foundation-model id and require a `us.`/`global.` prefixed profile id. That is why
   a route carries a target and is not just a permitted name.
2. **Beta flags travel in the body.** Clients send `anthropic-beta` as an HTTP
   header; Bedrock reads `anthropic_beta` from the request body.
3. **Bedrock refuses unknown beta flags outright**, failing the whole request.
   The flags it refuses are dropped, and each drop is logged. The list is
   `bedrock.DefaultUnsupportedBetas`, and no count is given here because both
   sides move it: it is found by probing Bedrock and is overridable per
   provider.

When a client works against api.anthropic.com and fails here, look at these first.

## Models are discovered; geography is a row

Nobody types a model mapping. A discovery job asks each serving model provider
what it can invoke and caches the result in `provider_models`, keyed by provider
and id. On bedrock-runtime it joins `ListFoundationModels` with
`ListInferenceProfiles`, because three things an operator otherwise learns from a
failing request are published there:

- `inferenceTypesSupported` says whether a profile is required. It almost always is:
  14 of the 15 Anthropic models in us-east-1 are profile-only, and the one that
  accepts its bare id is `LEGACY`.
- `modelLifecycle` marks a model `LEGACY`. A route pointing at one is a scheduled
  outage nobody has noticed.
- a profile's model ARNs enumerate **the regions it may route to** — the
  data-residency answer, derivable from nothing else. Read it from the ARN's region
  field, not the profile description, which AWS may reword.

**A geography is a row, not a flag.** `au.anthropic.claude-sonnet-5` and
`global.anthropic.claude-sonnet-5` are two catalogue entries: they route to different
countries and bill about 10% apart, and allowing one while refusing the other is the
entire point. The 10% delta is a common pattern, not a rule — Sonnet 4 is $3.00
either way — so each scope's published rate is looked up, never computed from the
other.

**A context window is not discovered, because Bedrock does not publish one.**
Checked field by field: `ListFoundationModels` and `GetFoundationModel` return
modalities, inference types, streaming and lifecycle; inference profiles return
regions; the only `MaxTokens` in the service belongs to imported models,
fine-tuning and evaluation jobs. The figure the Bedrock console shows is
editorial content in its frontend. So `internal/providers/bedrock/windows.go`
is a hand-maintained table, keyed by model **family** so that a geography prefix
and a revision date do not each need their own row, and overridable per provider
in the console. A model it has not heard of is **unrecorded, never
defaulted**: a wrong window is worse than an absent one, because a route
refusal and an engineer's token accounting are both derived from it. The output
cap lives beside it because opencode's `limit` requires both numbers.

**Model access is not a gate, and there is no API that is.** Access to every
foundation model is on by default; Bedrock auto-subscribes on first invocation. So
`GetFoundationModelAvailability` reporting `NOT_AVAILABLE` means "nobody has invoked
this here yet", not "you are blocked" — and an SCP denying `bedrock:InvokeModel` is
invisible to it. That is why the setup step makes a live invocation: an SCP, a
missing `aws-marketplace:Subscribe`, and a wrong identifier all arrive as the same
`AccessDenied`, and one real request is what tells them apart.

## Routes

**Everything a client may ask for is a route, and a route is the whole decision.**
`model_routes` is the allowlist; a name absent from it is refused. There is no "allow
any model" flag anywhere in the product — the console refuses to create a `*` route,
and no file or environment variable can create one.

A route carries three kinds of decision, and they must not be confused:

- **Conditions** — users, teams, tools; empty means everybody — say who the route
  exists for. Every condition set must match, so a route naming a person *and* a team
  admits that person when they are in that team and nobody else. That is the reading
  that makes a route narrower as an administrator adds to it, and the one that fails
  closed when they meant the other. A request from outside is refused **exactly as
  though the name had never been created**, and the refusal lists only routes that
  caller can use.
- **Target** says which model it becomes, as a provider reference.
- **`serves_over_soft`** says whether it is still served once a budget governing the
  caller has passed its soft limit. True by default, which is the ordinary route.

**A route with no model is refused at the write path**, because it would admit
a request and have nowhere to send it. If one is reached anyway — a hand-edited
database, a bad migration — the engineer gets a 502 and the operator an error log.

**Past a soft limit a route stops; it does not change model.** A route marked
`serves_over_soft: false` is refused with the budget named, how much of it is used,
when it resets, and **the routes that are still served** — filtered to what that
caller can actually use right now, so the list is never a guess. The engineer
switches (`/model` in Claude Code, the model list in opencode) and carries on.

The route does not swap in a cheaper model at the soft limit, because a harness is
told a route's context window once, when it is configured, and nothing renegotiates
it mid-session. The engineer acts, not the gateway: every route they may use is
already declared in their own configuration with its own window and output cap.

**A route becomes one model.** There is no fallback chain: a throttle reaches the
client as a 429 in its own error shape, which every coding tool retries, and a route
that should survive a throttle is a second route the engineer can switch to. Nothing
moves an engineer's model without telling them.

**Conditions are JSON lists on the route, not a junction table.** Rows referencing
`users` would have to cascade, and a cascade here *widens* a route: delete the one
person a route names and the condition disappears, leaving a route that admits
everybody. A list of ids that no longer resolve simply never matches.

**Routes load into the policy engine's existing snapshot**, not a second one. Two
snapshots refreshed independently would spend part of every cycle disagreeing about
which models exist. `Evaluate` returns the provider-native model to invoke, so the
data plane has nothing left to look up.

**There is no rules table beside the routes.** A policy language sitting beside the
allowlist and pointing into it means every question is asked of both, and the day they
disagree the permissive one wins silently.

**A route reads budgets and cannot set one.** Otherwise "what may I spend" has two
answers that disagree the first time somebody edits one of them.

## Money

**Integer nanodollars, never a float.** `pricing.Money` and `pricing.Rate` are int64;
parsing goes through `ParseUSD*`, which reads decimal text digit by digit. A column of
costs summed as float64 disagrees with itself depending on summation order, and
nanodollars are small enough that one cheap request is not zero.

**Unpriced is null, not zero.** A request whose model has no rate — or whose token
class has none — is recorded with a null cost and counted separately, and that count
appears beside every total. Pricing an unknown at zero would understate spend and look
like a discount.

Account for **all four token classes** — input, output, cache read, cache write —
with separate per-model rates. Coding-agent economics live in prompt caching; ignoring
cache classes overstates cost several-fold.

Three things about Bedrock pricing, each checked against the live API:

1. **Current model prices are not under the `AmazonBedrock` service code.** That
   catalogue holds five retired Anthropic products. Everything anyone runs a coding
   agent against is under **`AmazonBedrockFoundationModels`**, in a different shape.
2. **The `us.`/`global.` prefix is a price input.** AWS publishes two rate sets per
   model. It is part of the price-table key and must never be normalised away.
3. **Prompt cache writes have two TTLs at different prices** — five minutes and one
   hour, roughly 1.6x apart. A single cache-write number cannot be costed correctly.

The pricing catalogue identifies models only by display name, so the refresher joins
`pricing:GetProducts` against `bedrock:ListFoundationModels` to reach a model id.
Usage types are matched against an **allowlist**: batch rates are half the on-demand
ones, and pattern-matching would eventually halve a bill.

## Attribution

Every request is attributed via a **provenance ladder**; higher rungs always win.

1. **Explicit stamp** — client-injected headers. Precision tier.
2. **Structural inference** — extracted in flight from the body's structural ground
   truth: the environment block, `git`/`gh` output inside tool results, file paths,
   remotes.
3. **Content signals** — ticket or PR keys in conversation text. Low confidence, used
   only with session corroboration. Structural always beats content.
4. **Unattributed** — surfaced honestly, everywhere.

Attribute once per session and inherit across turns; do not re-guess per request.
Record provenance on every attribution and display it.

Four things, each of which makes an extractor that looks correct find nothing:

1. **The body is JSON, so the text inside it is escaped.** A newline is the two
   characters `\` and `n`. A pattern written against the text a human sees matches
   nothing.
2. **A cheap literal anchor is usually in the middle of what the pattern matches** —
   `/pull/` inside a PR URL. Windows must begin a fixed distance *before* the anchor.
3. **`"text"` matches inside `"type":"text"`.** Searching for the characters `"text"`
   returns the word `text` identically for every request from every conversation,
   merging the whole deployment into one session. Match keys as keys.
4. **The opening message has to be cut at its own end.** A fixed byte count spills
   into later messages, which grow every turn, so the fingerprint changes on each
   request and every turn becomes its own session.

The extraction rules are open source so the privacy claim is auditable. They are also
the part most likely to rot: the environment block's spelling changed between Claude
Code versions, and an extractor pinned to one spelling does not fail loudly — it
silently stops attributing. Support both.

## Credentials

Five prefixes, and they are not interchangeable. The prefix is what makes them
tellable apart before any query runs, which keeps request-path authentication to one
indexed lookup in one table. `auth.Parse` admits exactly two; everything else is
refused on shape. All are stored as SHA-256, never in the clear.

| Prefix | What | Notes |
| --- | --- | --- |
| `pnt_` | Gateway token | What a coding tool holds. Also authenticates the console API. |
| `png_` | CLI grant | What `panorama login` leaves on a laptop. Deliberately **not** spendable — the gateway refuses one outright. Exchanges for a gateway token. **Switched off in this release**: `cli.sign_in` is off by default and cannot be turned on yet, so the whole flow is refused. |
| `pnk_` | Service token | Belongs to a team, not a person. Bills the team, survives its creator. Reaches **no** console route. |
| `pns_` | Console session | Cookie-borne, so every write carries a CSRF header too. |
| `pnb_` | Setup code | Opens a setup session: not an account, one hour, configures this deployment only. |

**A grant holds at most two working tokens**, because exchanging mints a new one and
retires the last. Revoking a grant stops the machine within one token lifetime.

**Two rotation intervals come from one number.** `panorama token` refreshes at half a
token's life; the generated `CLAUDE_CODE_API_KEY_HELPER_TTL_MS` is two fifths. Setting
the helper above the refresh point would have Claude Code use an expired token for the
rest of its own interval. That arithmetic has a test, not a comment.

**Service tokens are a table of their own, not a nullable `user_id`.** Relaxing that
column means rebuilding `gateway_tokens` on SQLite, and `requests.token_id` references
it `ON DELETE SET NULL`. SQLite's `DROP TABLE` performs an implicit `DELETE` that runs
foreign-key actions, so the rebuild would silently erase which token served every
request ever recorded.

**A service token's spend reaches its team by a union, not a join.** Team spend is
otherwise derived by joining `team_members` on `requests.user_id`, and a pipeline has
no user id. The arms are made disjoint in the query (`user_id IS NULL` on the service
side) rather than assumed disjoint, because trusting the writer is how a union becomes
a double count.

**`RoleService` and `RoleSetup` are authorization labels on live principals**, absent
from `ValidRole` and from the `CHECK` constraints, so neither can be written to a
person or mapped to a group. `RoleService` is refused in `authenticatedAllowing` — the
one place every console route passes through — rather than by a role list, because a
role list only refuses what it names. A handler added tomorrow is closed by default.

## Identity

**The OIDC connection is configuration; the group mapping is data.** Issuer, client id
and client secret live in the file and the environment — a client secret in a database
is a client secret in a backup. The group-to-role mapping lives in the database,
because the names of your groups are organizational data an admin edits from a screen.
The strongest matching group wins, so a role never depends on claim ordering.

**A role a person assigned is not the mapping's to revise.** Every sign-in recomputes
the role from groups — that is how removing somebody from a group removes their access
— but applied to every account it makes a directly assigned role impossible to hold.
So `users.role_source` says who decided: `sso` is recomputed at every sign-in, `manual`
is left alone. The first sign-in's admin grant is `manual`, because it exists precisely
where the mapping cannot yet produce an administrator.

**The claim names a token carried are recorded; no claim value ever is.** "The groups
claim is empty" has two opposite causes — wrong claim name, or the provider is not
emitting it — and nothing could tell them apart, so administrators alternated between
the two fixes a sign-in at a time. `oidc_observed_claims` holds names and two counts.
Names are schema; values are somebody's email and group membership, and there is
nowhere in the schema to put one.

**Setup state is derived, not remembered.** A stored "identity: done" flag still says
done after somebody removes the issuer. Only two things are recorded: that a step was
skipped on purpose, and that a live invocation succeeded against a provider at a
moment, on that provider's row, cleared when its region changes.

**Nothing validates the connection by storing it.** `internal/ssosetup` probes an
issuer, renders configuration for each packaging shape, and writes nothing. In compose,
Helm and ECS the config inside the container is not the source of truth, so a command
that "saved" the connection would be lost on the next deploy.

## Integrations

**Connected in the console, stored in the database, encrypted.** Connecting GitHub is
an operational decision made months after a deployment is stood up, often by somebody
with no access to its manifest, and requiring a redeploy would put a release cycle in
front of a checkbox.

The credential is sealed with AES-GCM before it is written and the key stays in
configuration, so a dump or a stolen volume carries nothing usable. The key is any
passphrase of sixteen characters or more — derived with HKDF — or the 32 bytes
`generate-key` mints, used as given. Two consequences: a
deployment with no `encryption_key` cannot store a credential and says so at the moment
somebody tries, and losing the key means re-entering credentials.

**The credential is write-only.** No endpoint returns it, including the one that
accepts it. `IntegrationSecret` is a separate store function from `IntegrationConfigs`
so the ordinary path of rendering the screen cannot pick one up by accident.

**The registry is resolved, not held.** Rebuilt from the database each confirmation
pass, so a connection made at ten past the hour is used by the pass at quarter past.

**Confirmation, not synchronisation.** A source is asked about work items Panorama has
already seen and nothing else, so read scope covers only what engineers touched and the
call count is bounded by conversations rather than by the size of the organization.

**Confirming a work item does not move the attribution that found it.** GitHub agreeing
that a branch exists does not make the observation better, and GitHub never having
heard of it does not make it wrong — the branch may never have been pushed. What
confirmation adds is a relation. The outcome rollup follows exactly one hop, because a
branch never merges and its pull request does.

**`status` and `outcome` are separate columns.** "Merged", "Closed", "Done",
"Released" and "Won't Do" are five words and three outcomes. `status` is for display;
`outcome` is the only thing the metrics read. And **`merged_at` decides the outcome,
never `state`**: a merged PR reports state `closed`, so grouping on state would file
every merge as abandoned.

**An unreachable source confirms nothing.** Carrying on past a failed health check
would write "not found" against every reference and turn one broken API into a
deployment-wide claim that none of the work exists.

## Jobs

Interval goroutines, not a queue. `internal/jobs` runs the work-item confirmer, the
model discoverer, the price refresher and the session sweeper.

River (an in-process, Postgres-backed queue) is where retries, scheduling and fan-out belong once there is any. It is not a
dependency today: the confirmer is a single idempotent pass over a bounded list, and a
queue would add a Postgres requirement to a deployment that is otherwise happy on
SQLite for no behaviour it needs. **Background work therefore runs on SQLite too.**

## The console

Seven screens plus a first-run wizard. [`ui/README.md`](ui/README.md) has the design
decisions; the code-review rules are in [`CLAUDE.md`](CLAUDE.md). The short version:

- **`filterFrom` in `internal/controlplane/api.go` is the single chokepoint** that
  enforces "a developer sees only their own numbers". A second place to decide
  visibility is a second place to get it wrong.
- **The console is not a control.** The sidebar hides screens a role has no use for;
  `requireRole` on the server is what actually refuses.
- **A panel is where something is edited; a modal is where something is picked for
  it.** One `Drawer` and one `Modal` in `ui.tsx`, three picker call sites.
- **"What actually stops them" is one derivation, in `people.ts`.** Personal cap, then
  the tightest team hard limit, then the organization.
- **A harness is told what a route is, in the shape it accepts.** The setup screen
  lists opencode first. A route for Claude Code is named with its model's Anthropic
  ID, so Claude Code recognises the model and is told no numbers; opencode's provider
  block declares each route's window and output cap, rendered whole on the server
  because the browser has no way to compute them.
- **Money is rendered from integer nanodollars.** `format.ts` is the only place
  rounding happens.
- **Lists that can hold an organisation are virtualised.** Three thousand accounts is
  a normal deployment.
- Nothing loads from a third-party origin. Fonts are bundled; the CSP names no
  external host. Air-gapped is a supported deployment.

## Building

Every command runs in a container via the `Makefile`; `make check` is what CI runs. If
you have Go and Node locally, `make <target> HOST=1`.

**The console's build output is not committed, and every Go target depends on it.**
`make ui` builds `ui/app` into `ui/dist`; `make build`, `make test` and `make lint` all
run it first, because `go:embed` will not compile against an empty directory. `go build
./...` on its own therefore does not work from a fresh clone. The alternative was
committing a minified bundle into every diff that touches a screen.

`make dev` is the working loop and is not the shipped artifact: the Go toolchain
against a bind mount of the tree, console on `:5173`, gateway on `:8088` restarted by
`air`, Postgres behind both. Two seams worth knowing — `go:embed` needs a non-empty
`ui/dist`, so the entrypoint writes a placeholder rather than putting a Vite build in
front of every `up`; and both watchers poll, because file events do not cross a bind
mount reliably on macOS.

## Scope

v1 is what a FinOps buyer would miss in their first hour (see [Principles](README.md#principles)). Jira, the
Grafana pack, anonymous telemetry and Helm are not in it.

The OpenAI-compatible surfaces are, because bedrock-mantle serves non-Claude models
over nothing else. They relay natively, with no translation to or from the Messages
API, and each has its extractor. Responses are kept stateless (`store` forced false;
stored or background responses refused), and a service tier other than the default is
refused.

**Do not add a surface without an extractor to match** — the traffic would land in the
unattributed bucket, which is the one number this product promises to be honest about.
