<div align="center">

# Panorama

**An open-source AI gateway that connects coding-AI spend to real work.**

Sponsored by [DNX Solutions](https://www.dnx.solutions) · AGPL-3.0-only

</div>

---

Panorama sits between your engineers' coding agents and Amazon Bedrock. It runs in
your own infrastructure: one endpoint, one binary, no prompt or code leaving your
network.

Point Claude Code at it and you get authentication, per-person and per-team budgets,
model routing you control, and — the part nothing else does — **cost attached to the
actual work**, because the gateway reads the branch, ticket and pull request out of
the traffic as it passes. No client configuration, no wrapper, no plugin.

> **Pre-release.** It runs, but it has not been through a production deployment
> yet. See [Status](#status).

## What you get

| | |
| --- | --- |
| **One governed endpoint** | Every request authenticated, checked against a budget, routed to a model you chose, and logged. |
| **Cost you can trust** | Input, output, cache read and cache write priced separately, per model, per region. A coding agent is 90% cache reads — meter them as input and you overstate the bill several-fold. |
| **Cost per branch, ticket and PR** | Extracted from the request in flight, then confirmed against GitHub (experimental). "PAY-1183 cost $18.40, merged." |
| **Budgets that hold** | Per person, per team, per deployment. Soft limits warn, and close the routes you marked as too expensive to keep serving — the engineer is told which routes still work. Hard limits stop the request with a message that says who can raise it. |
| **SSO and revocable credentials** | Generic OIDC for the console. An engineer's coding tool holds a gateway token they mint themselves, revocable and good for 30 days. |

## Principles

These are settled. Changing one takes a decision in an issue first, not a pull
request.

**Trust**

1. **Prompts and responses are never stored.** Bodies are pattern-matched in flight
   and discarded; only extracted metadata persists. The extraction rules are open
   source so you can check.
2. **An engineer sees only their own numbers.** Named per-person spend exists for
   administrators and for managers of that person's teams, because a budget holder
   needs per-seat cost. Nothing ranks one engineer against another.
3. **Unattributed spend is always shown** beside attributed numbers. An honest 70%
   beats a false 100%. The same goes for price: an unpriced request is counted as
   unpriced, never as $0.

**Design**

4. **Streaming is sacred.** Nothing on the request path buffers or blocks; the target
   is under 10 ms added to time-to-first-token. Policy is decided from memory, and
   anything beyond regex and structural matching runs asynchronously.
5. **Relayed, never translated.** Each API shape is passed through natively, and no
   surface ships without an attribution extractor, so no traffic lands unattributed by
   omission.
6. **Your infrastructure, one binary.** One deployment is one organization. SQLite or
   Postgres, nothing loaded from a third-party origin, air-gapped deployments supported.
7. **Allowlists, not denylists.** Every model a client may use is a route an
   administrator created; there is no "allow any model" switch. Errors are logged and
   returned, never hidden behind a fallback.

**Scope**

8. **A feature is in when a FinOps buyer would miss it in their first hour.** That is
   why Jira, a Grafana pack, telemetry, Helm and non-AWS providers are not in v1.
9. **The open-source edition is complete for one organization.** SSO and SAML are never
   paywalled. A planned enterprise edition would add multi-organization management,
   audit export and billed-spend reconciliation (see [Licensing](#licensing)).

The reasoning behind the finer decisions is in the manual's
[Design decisions](https://panorama.dnx.ai/docs/internals/decisions/).

## Requirements

- An AWS account with Bedrock access, and a region.
- An OIDC identity provider for the console: Entra ID, Okta, Keycloak, Cognito,
  Auth0, PingOne or Google Workspace. AWS IAM Identity Center will not work
  ([why](#single-sign-on)).
- For the recommended deployment on AWS: the AWS CLI, a VPC with subnets in two
  availability zones, and an ACM certificate for the hostname.
- For a server instead: Docker. SQLite is built in, so there is no database to
  set up; Postgres is supported and is a choice, not a requirement.

Panorama never asks you to paste an AWS access key. It uses the default credential
chain, so an ECS task role or EC2 instance role is the intended path. The policy
that role needs is the `TaskRole` in
[`deploy/cloudformation/panorama.yaml`](deploy/cloudformation/panorama.yaml),
and the console's setup step and `panorama providers policy` print the same
statements as JSON to paste as an inline policy. A test fails the build when the
two disagree, so this README does not carry a third copy.

It grants both Bedrock endpoints - `bedrock-runtime` and `bedrock-mantle` - in
every region, with no condition on the model. Which region each provider invokes
is chosen in **Settings → Model providers**, after the role exists, and that
choice never needs an IAM change.

Four things in it are worth knowing before you narrow it.

**The invoke statement names two ARN shapes because one invocation is checked
against both.** Current Anthropic models refuse their bare model id and are
reached through an inference profile, and AWS authorises that against the
profile *and* against the foundation model in **every region the profile may
route to** — so a policy naming only the profile fails, and one naming only the
model fails too. If you narrow this to named models, list both shapes and every
region that profile covers, and note that a `global.` profile's foundation-model
ARN has an empty region field (`arn:aws:bedrock:::foundation-model/…`), which
the `*` above matches and a hand-written region does not. Getting that wrong
takes out `global.` routing only, so it looks like one model being broken rather
than a policy being wrong.

**The `aws-marketplace` statement is the one people leave out**, because nothing
about it says Bedrock. Bedrock subscribes the account to a foundation model on
its first invocation, so without it the first request against a model this
account has never used fails with `AccessDenied` — the same error a missing
`bedrock:InvokeModel` gives — and the same request then succeeds once somebody
has subscribed the model by hand. It cannot be scoped to a model ARN.

**`bedrock-mantle` is a separate service, and `bedrock:InvokeModel` does not
authorise it.** Every inference call there - Anthropic Messages, Chat
Completions, Responses - is `bedrock-mantle:CreateInference`, checked against the
project the request runs in; a request naming no project runs in the account's
default one, which `project/*` covers. Nothing grants `CallWithBearerToken`: the
gateway signs with SigV4 and never holds a Bedrock API key.

**`pricing:GetProducts` is what makes spend real rather than estimated.** Without
it Panorama keeps serving requests and prices them from the rates compiled into
the binary, which go stale.

## Quickstart: deploy on AWS

One CloudFormation template is the whole deployment: Fargate behind an
Application Load Balancer, against Aurora Serverless v2, in a VPC you already
have. There is no bootstrap stack and nothing to install, and your security
team can read all of it before it runs. It takes about forty minutes, most of
it waiting for the database. The
[Quickstart](https://panorama.dnx.ai/docs/start/try-it/) is the full
walk-through and [Deploying on AWS](https://panorama.dnx.ai/docs/deploy/aws/)
has the detail; this is the short form.

### 1. Create the stack

From the AWS console, in the region you are deploying to:

[**Launch stack →**](https://console.aws.amazon.com/cloudformation/home#/stacks/quickcreate?templateURL=https%3A%2F%2Fpanorama-public-artifacts.s3.amazonaws.com%2Fpanorama%2Fstable%2Fpanorama.yaml&stackName=panorama&param_DesiredCount=0)

It opens CloudFormation with the latest release's template and `DesiredCount`
set to `0`; fill in your network, hostname and certificate. Or from the CLI,
with the template attached to the latest release:

```sh
curl -fsSLO https://github.com/dnxlabs/panorama/releases/latest/download/panorama.yaml

aws cloudformation deploy \
  --stack-name panorama \
  --template-file panorama.yaml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
      VpcId=vpc-0123456789abcdef0 \
      ServiceSubnetIds=subnet-aaa,subnet-bbb \
      LoadBalancerSubnetIds=subnet-ccc,subnet-ddd \
      DomainName=panorama.example.com \
      CertificateArn=arn:aws:acm:us-east-1:111122223333:certificate/... \
      BedrockRegion=us-east-1 \
      DesiredCount=0
```

`DesiredCount=0` creates the database, the load balancer and the task
definition, and starts nothing against an empty schema. Add `HostedZoneId` and
the stack creates the DNS record too. A release's template runs that release,
and upgrading is deploying the next one's.

### 2. Migrate and start

Run the command in the stack's `MigrateCommand` output and wait for that task
to exit zero. Then start the service, one task per availability zone:

```sh
aws cloudformation deploy --stack-name panorama --template-file panorama.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides DesiredCount=2
```

Every parameter left out keeps its value, so `DesiredCount` is the only one to
give.

### 3. Connect single sign-on

A fresh deployment has no accounts. Mint a one-time setup code with a one-off
task (`admin setup`; the command is in the
[Quickstart](https://panorama.dnx.ai/docs/start/try-it/#3-connect-single-sign-on)),
open the `ConsoleUrl` output and enter it. The setup session checks your
identity provider and prints the value for the `OidcSecretArn` secret. Store
it, force a new deployment of the service, and sign in. **The first person to
sign in becomes an administrator.**

### 4. In the console

1. **Settings, Model providers, Check**, on the Amazon Bedrock block. One real
   request, one token of output, proving the task role works. The Mantle block
   beside it is the way to the non-Claude models, and can stay off.
2. **Routing, New route.** Name it `claude-sonnet-5` and choose Claude Sonnet 5 (a
   route Claude Code uses is named after its model). A new
   deployment has no routes and refuses every request until one exists.
3. **My setup.** Choose your tool, mint a token, and paste what the screen
   prints. It writes the configuration for Claude Code, opencode or Codex CLI
   with your routes already in it.
4. **Analytics** and **Audit trail** show what each request cost, and the
   repository and branch it paid for, with nothing configured in the tool.

The template sets the four things a streaming gateway needs and AWS does not
default to: a 600-second load balancer idle timeout, a 300-second
deregistration delay, the longest stop timeout Fargate allows, and a shutdown
timeout just under it.

## Deploying on a server instead

Docker Compose on one VM, with SQLite built in, for a small team on one machine
or a deployment outside AWS.
[Deploying on a server](https://panorama.dnx.ai/docs/deploy/server/) is the
full walk-through.

```sh
git clone https://github.com/dnxlabs/panorama.git
cd panorama
cp deploy/compose/.env.example deploy/compose/.env
```

Compose reads `.env` **from the directory holding the compose file**, not from where
you run the command. Set at least:

```sh
AWS_REGION=us-east-1
PANORAMA_SERVER_BASE_URL=https://panorama.your-company.internal
PANORAMA_OIDC_ISSUER=https://your-org.okta.com
PANORAMA_OIDC_CLIENT_ID=...
PANORAMA_OIDC_CLIENT_SECRET=...
```

`PANORAMA_SERVER_BASE_URL` is the console's address, the OIDC redirect URI, and
what engineers point a tool at. It defaults to `http://localhost:8088`, which is
wrong on a server. On EC2, attach an instance role with the permissions above
and set nothing else for AWS: the default chain finds it.

```sh
docker compose -f deploy/compose/docker-compose.yml up -d --build
curl https://panorama.your-company.internal/readyz
```

The compose files build from source, so the first run takes a few minutes. The build
fetches the latest release's table of prices for models the AWS Price List API does
not publish, so it prices what a published image prices (`PANORAMA_FOUND_PRICES` in
`.env.example` pins or skips it). `/readyz` returns 200 only when the database is
reachable **and** the schema is current, which makes it the right target for a
load balancer health check. Give that load balancer an idle timeout long enough
for streaming responses.

A fresh deployment writes a one-time setup code to its log:

```sh
docker compose -f deploy/compose/docker-compose.yml logs panorama | grep pnb_
```

Enter it in the console, check single sign-on, sign in, and carry on with
[step 4 above](#4-in-the-console).

**SQLite is the default and needs nothing.** It is built into the binary, lives
in a Docker volume, and runs the whole product, every background job included.
For a single container serving a team, it is a real answer, not a trial mode.
One rule, because both ways of breaking it are silent: **the SQLite file must sit
on a real volume**, never a bind mount from the host, an NFS share, or anything
else emulating POSIX locking. `docker-compose.yml` already does this.

**Use Postgres when you outgrow one process**: more than one replica, or a
database your existing backup, failover and monitoring already cover.
`docker-compose.postgres.yml` runs Postgres beside Panorama; to use your own,
set `PANORAMA_DATABASE_DSN` with `sslmode=require`. See
[Choosing a database](https://panorama.dnx.ai/docs/deploy/database/).

## Running it on your laptop

For working on Panorama, or a look at the console before deploying it:
[On your laptop](https://panorama.dnx.ai/docs/deploy/laptop/) runs the compose
stack with your own AWS credentials and a development sign-in that only works on
`localhost`. To change the code, `make dev`; see [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Single sign-on

A fresh deployment has no users and the console is authenticated, so Panorama
issues a one-time **setup code**. Entering it opens a **setup session**: one hour,
no account, and nothing reachable but this deployment's own configuration: SSO,
Bedrock, and which identity-provider groups map to which role.

Set SSO up, end the session, and sign in through your provider. **Your account is
created by that first sign-in.** Panorama does not own identities, so there is no
local admin user to create. If you get locked out, `panorama admin setup` issues a
fresh code at any time.

Once it works, confirm it:

```sh
panorama sso doctor    # redirect URI, secret, and whether anyone has signed in
panorama sso claims    # the group names your provider actually sent
```

`doctor` exits non-zero on failure, so it works as a deployment check. Run
`claims` before mapping any groups: providers disagree about what a group even is —
Entra sends object IDs, Keycloak can send a full path — and a mapping naming a group
nobody is in grants nothing and fails silently.

> **AWS IAM Identity Center will not work**, and it is the first thing people try.
> It is not an OIDC provider for third-party apps (sign-in is SAML 2.0 only), and it
> has no groups attribute to map roles from. Point Panorama at the provider *behind*
> it — Entra ID, Okta, your directory.

## Choosing models

**Settings, Model providers** holds one band per provider, each with its own
region: Amazon Bedrock (bedrock-runtime, Claude through inference profiles) and
Amazon Bedrock Mantle (every model in the region you choose, including
open-weight models over the OpenAI APIs). Panorama asks each one what your
account can invoke rather than making you type it, and lists one row per
**invocable target**, named `provider/model`. A model appears once per geography
it can be reached through, because `us.` and `global.` route to different
countries and bill about 10% apart.

Then create **routes** on the Routing screen. A route is a name your engineers
use pointing at a model you chose. Clients can only ask for route names, never
model ids, so there is no "allow any model" switch to leave on. Routes live in
the database, so changing one needs no redeploy. A route answers only on the APIs
its model speaks: a Claude route at `/v1/messages`, an open-weight route at
`/v1/chat/completions` and, where the model supports it, `/v1/responses`.

**Name a route Claude Code uses after its model**, such as `claude-sonnet-5` or
`claude-haiku-4-5-20251001`. Claude Code prepares each request for the model it
believes it is talking to, and treats a name it does not recognise as the newest
model there is, which an older model behind the route then refuses. **Do not call
one `default`**, `best`, `fable`, `sonnet`, `opus`, `haiku` or `opusplan`: Claude
Code resolves those names itself, so the route is never asked for. Panorama
refuses them when you create the route.

The provider's **Check** makes one real request. Nothing else proves access
works: model access is on by default and auto-subscribes, so an SCP, a missing
subscribe permission and a typo all look identical until you invoke something.

## Giving engineers a credential

An engineer signs in to the console, opens **My setup**, and mints a **gateway
token**. The screen then prints the configuration for their tool — Claude Code,
opencode or Codex CLI — with their routes already in it.

A minted token **lives 30 days**. Nothing rotates it and nothing warns when it
expires, so an engineer comes back to My setup, mints another, and replaces it.
That is the whole credential story in this release, and the screen says so.
Revoking is on the same screen and is permanent.

For CI, mint a **service token** under a team instead: it bills the team,
outlives whoever created it, and can reach no console endpoint at all. There is
no screen for it yet, so it is created through the API. See
[Credentials](https://panorama.dnx.ai/docs/reference/credentials/#service-tokens).

**Command-line sign-in is built and switched off.** `panorama login` signs a
machine in and leaves a self-rotating credential, but this release does not hand
it to engineers.

## Connecting GitHub (optional, experimental)

**Experimental:** the GitHub integration is built and has not yet been tested
against a real GitHub organization.

Integrations → GitHub, as an administrator. A GitHub App for a real deployment, a
personal access token for evaluation. This is what turns "this branch cost $18.40"
into "this merged PR cost $18.40". It needs an encryption key, because the
credential is encrypted before it is stored: the CloudFormation stack generates
one, and on a server you set `PANORAMA_ENCRYPTION_KEY` to any passphrase of 16
characters or more.

Panorama only ever asks about work items it has already seen in traffic, so the App
needs read access to what your engineers touched and nothing more.

## Where commands run

This catches everyone once. Two groups:

| Commands | Where | Why |
| --- | --- | --- |
| `login` `logout` `token` `whoami` `configure` | **A developer's laptop** | They talk to a deployment over HTTP with a credential in `~/.panorama`. |
| `token create` `admin` `sso` `models` `policy` `prices` `migrate` | **Where the database is** | They open the database directly. |

So on AWS, the second group runs as a one-off ECS task from the service's own
task definition, with the same image, database and secrets; its output is in the
task's log stream. See
[Running a command](https://panorama.dnx.ai/docs/deploy/aws/#running-a-command).

```sh
aws ecs run-task --cluster panorama --task-definition panorama \
  --launch-type FARGATE \
  --network-configuration "awsvpcConfiguration={subnets=[<service-subnets>],securityGroups=[<ServiceSecurityGroupId>],assignPublicIp=<AssignPublicIp>}" \
  --overrides '{"containerOverrides":[{"name":"panorama","command":["sso","doctor"]}]}'
```

On a server, it goes through the container:

```sh
docker compose -f deploy/compose/docker-compose.yml exec panorama panorama sso doctor
```

The runtime image is distroless — there is nothing to `exec bash` into. The
binary is the entrypoint, and it is what you invoke.

## Budgets and routing

Budgets are set on **People & teams** — on a person, a team, or the whole
deployment, over a day, a week or a month.

- A **soft limit** warns. Any route you marked as not serving past one stops, and
  the engineer is refused with the budget named and the routes that still work
  listed — they switch (`/model` in Claude Code, the model list in opencode) and
  carry on. Panorama never swaps the model for them: a harness is told a route's
  context window once, when it is configured, and has no way to be told
  mid-session that it shrank.
- A **hard limit** stops the request, with a message saying how much of which limit
  is used, when it resets, and who can raise it.

A team's hard limit governs every member without a tighter cap of their own. Each
budget panel names what actually stops that person. A person's or a team's panel
holds its changes until **Save changes**, and **Cancel** discards them.

Routes and budgets can also live in version control:

```sh
panorama policy check -f policy.yaml   # validate, including that every model exists
panorama policy apply -f policy.yaml   # idempotent
panorama policy show                   # what is in force, and what has been spent
```

See [`policy.example.yaml`](policy.example.yaml).

## Operating it

**Health.** `/healthz` says the process is up; `/readyz` says the database is
reachable and the schema is current. They are the only two unauthenticated
endpoints in the product and they expose no org data.

**Backups.** Everything is in the database: Aurora on AWS, SQLite in the
`panorama-data` Docker volume on a server, or Postgres wherever you put it. Stored
integration credentials are encrypted, so a backup carries nothing usable without
the encryption key — keep that key somewhere else, and keep it. The stack keeps it
in the `EncryptionKeySecretArn` secret.

**Upgrades.** On AWS, deploy the new image tag with `DesiredCount=0`, run the
migration task, and deploy again; see
[Upgrading](https://panorama.dnx.ai/docs/deploy/upgrading/). On a server, the
compose files build from source, so an upgrade is `git pull` then
`docker compose … up -d --build`, and migrations run at startup while
`PANORAMA_DATABASE_AUTO_MIGRATE` is on.

**When Claude Code behaves differently** against Panorama than against
api.anthropic.com: MCP tool search is off unless `ENABLE_TOOL_SEARCH=true`, and
Remote Control is disabled — exactly as against Bedrock directly.

## Configuration

A YAML file plus `PANORAMA_`-prefixed environment variables; environment wins. Every
key is documented in [`config.example.yaml`](config.example.yaml). Validation is
strict: Panorama refuses to start on invalid configuration rather than falling back
to a default that surprises you later.

The file holds only what a deployment cannot start without - how to reach your
identity provider, the encryption key, and a region to seed the first model
provider with. Everything an administrator switches on afterwards, model
providers and their regions included, lives in the database, behind a screen.

## Status

**Pre-release.** It runs, but has not been through a production deployment yet.

**Works today:** the gateway on the Anthropic Messages, OpenAI Chat Completions and
Responses APIs; Amazon Bedrock through bedrock-runtime and bedrock-mantle; budgets and
routing; attribution to branches, tickets and pull requests; cost centers; the
console and analytics; deployment on AWS with one CloudFormation template. Claude Code, opencode and Codex CLI are
configured from My setup. Cline and Continue speak an API the gateway serves and are
not configured from My setup yet.

**Experimental:** confirming work against GitHub (pull request titles, authors and
merge status). It is built and has not yet been tested against a real GitHub
organization.

**Next:** simulate a policy change before applying it, and a one-click budget raise.

## Documentation

**[panorama.dnx.ai/docs](https://panorama.dnx.ai/docs/)** is the manual:
deployment, configuration, the reference, and a page per coding tool. Every
released series keeps its own copy, so a deployment running v1.2 can read the
v1.2 documentation.

It is built from [`docs/`](docs/) in this repository, because a page and the
code it describes change in the same pull request or they drift.

| | |
| --- | --- |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | How it works inside, and why. Start here to change the code. |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Development setup, the build, pull requests. |
| [`docs/README.md`](docs/README.md) | The documentation site: how to run it, and how versions work. |
| [`config.example.yaml`](config.example.yaml) | Every configuration key, documented. |
| [`policy.example.yaml`](policy.example.yaml) | Routes and budgets as a file. |
| [`ui/README.md`](ui/README.md) | The console's design decisions. |

## Licensing

**AGPL-3.0-only**: see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE). The manual's
prose and screenshots (`docs/`) are CC BY 4.0; see [`docs/README.md`](docs/README.md).
The open-source edition is complete for a single organization: full
gateway, full policy engine, full analytics, full attribution, full SSO. No
feature gates, nothing that stops working until you pay.

An enterprise edition covering multi-organization management, audit export and
CUR-based billed-spend precision is planned, under a separate commercial licence.
No enterprise code exists yet; [`EDITIONS.md`](EDITIONS.md) says what it is
expected to cover and what will never move behind it.

SAML is *not* an enterprise feature. For providers like Identity Center it is the
only way to sign in at all, so gating it would gate SSO. Not implemented yet; when it
lands, it lands here.

Contributions require a signed CLA; see [`CLA.md`](CLA.md).
