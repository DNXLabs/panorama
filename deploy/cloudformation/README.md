# Deploying Panorama on AWS

One CloudFormation template. Fargate behind an Application Load Balancer,
against Aurora Serverless v2, in a VPC you already have.

`panorama.yaml` is the deployable artifact — not a synthesis step. There is no
bootstrap stack, nothing to install on the machine that deploys it, and your
security team can read the whole thing before it runs.

---

## What it creates

| | |
| --- | --- |
| Compute | An ECS cluster, a Fargate service on ARM64, a task definition |
| Ingress | An Application Load Balancer, an HTTPS listener, an HTTP→HTTPS redirect, a target group on `/readyz` |
| Data | An Aurora Serverless v2 PostgreSQL cluster with one `db.serverless` instance |
| Secrets | A generated database password, and a generated encryption key unless you bring one |
| Identity | A task role (Bedrock, and invoking an AgentCore gateway for web search) and an execution role (image pull, secret injection) |
| Network | Three security groups — load balancer, tasks, database — and the rules between them |
| Optional | A Route 53 alias record, if you name a hosted zone |
| Optional | An AgentCore gateway with a Web Search Tool target and its service role, with `CreateWebSearchGateway=true`, which the stack refuses outside the three regions that have the Web Search Tool. Its URL is passed to the tasks, so search is on from the first start; Settings, Web search shows it read-only, with a switch to turn search off. It is also the `WebSearchGatewayUrl` output |

## What it does not create, and why

**A VPC.** The account this lands in already has a network and a set of
opinions about it. `VpcId`, `ServiceSubnetIds` and `LoadBalancerSubnetIds` are
inputs.

**VPC endpoints.** Whether you need them is a property of your network rather
than of this product — see [Connectivity](#connectivity) below.

**A migration runner.** Migrations are an ordered step you take before the
service rolls. That ordering is what makes the rolling update safe, so it is
explicit rather than hidden: see [Upgrading](#upgrading).

**A certificate or a hosted zone.** `CertificateArn` is an input, and TLS
terminates at the load balancer.

---

## Before you start

1. **A VPC with subnets in at least two availability zones.** Either shape
   works:
   - *Private subnets with a NAT gateway* — the intended production shape.
     Leave `AssignPublicIp` at `DISABLED`.
   - *Public subnets* — set `AssignPublicIp: ENABLED`, which is how a task with
     no NAT gateway reaches ECR, Bedrock and Secrets Manager. The tasks are
     still only reachable through the load balancer's security group.

   The load balancer's subnets can differ from the service's: public subnets
   for an internet-facing load balancer in front of private tasks is the usual
   arrangement.

2. **An ACM certificate** in the same region, for the hostname you will use.

3. **Bedrock access in the region you name.** No model access request is
   needed — Bedrock subscribes the account on first invocation, which is what
   the `aws-marketplace:Subscribe` statement in the task role is for.

The image is public, at `public.ecr.aws/dnxsolutions/panorama`, so there is nothing
to request before deploying. See [The image](#the-image).

## Connectivity

The tasks must be able to reach five things. **With a NAT gateway, or in public
subnets with `AssignPublicIp: ENABLED`, you need nothing further.** Without
either, create these interface endpoints in the VPC, plus the S3 gateway
endpoint, before deploying:

| Endpoint | Type | What breaks without it |
| --- | --- | --- |
| `com.amazonaws.<region>.bedrock-runtime` | Interface | Every request routed to the `bedrock` provider. This is the hot path |
| `com.amazonaws.<region>.bedrock` | Interface | Model discovery for `bedrock` - the catalogue stays empty, so no route can be created |
| `com.amazonaws.<region>.bedrock-mantle` | Interface | Only if the `bedrock-mantle` provider is enabled: every request routed to it, and its discovery (host `bedrock-mantle.<region>.api.aws`) |
| `com.amazonaws.<region>.ecr.api` and `.ecr.dkr` | Interface | The image pull from your [mirror](#mirroring-into-your-own-account); tasks never start |
| `com.amazonaws.<region>.s3` | Gateway | The image pull from your mirror: ECR layers live in S3 |
| `com.amazonaws.<region>.secretsmanager` | Interface | Secret injection; tasks never start |
| `com.amazonaws.<region>.logs` | Interface | All logging, including the setup code |

**ECR Public has no VPC endpoint.** With neither a NAT gateway nor public IPs, a
task cannot reach `public.ecr.aws`, so the image has to come from a repository
in your own account: [mirror it](#mirroring-into-your-own-account), or put a
pull-through cache in front of it, and set `ImageRepository`. The two ECR
endpoints and the S3 gateway above are for that pull.

**The Pricing API is the exception, and it is worth deciding on deliberately.**
`pricing:GetProducts` is what makes spend real rather than compiled-in, and it
has no PrivateLink endpoint. A fully private deployment either keeps a NAT
route for that one call or runs on the rates built into the binary, which go
stale. Panorama says which it is doing rather than pretending — but it will not
tell you before you deploy, so decide now.

---

## Deploying

The first deploy is three commands, because migrations run before the service
does. Every later deploy is two.

### 1. Create the stack with no tasks

Deploy the template attached to a release rather than this directory's copy.
It is the same file with one line changed - the `Release` mapping names that
release's version - so it runs the image it was released with, and the next
release's template moves the image with it. This copy belongs to no release
and runs `stable`.

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

`DesiredCount=0` is the point of this step. The database, the load balancer and
the task definition all get created; nothing starts against an empty schema.
Takes about fifteen minutes, nearly all of it Aurora.

> **Comma-separated lists in `--parameter-overrides`.** The shorthand syntax
> above works because the subnet IDs contain no spaces. If your shell fights
> you, use `--parameter-overrides file://params.json` instead.

### 2. Run the migrations

The stack outputs the exact command, already filled in with your cluster,
subnets and security group:

```sh
aws cloudformation describe-stacks --stack-name panorama \
  --query "Stacks[0].Outputs[?OutputKey=='MigrateCommand'].OutputValue" --output text
```

Run what it prints, then wait for the task to stop and confirm it exited zero:

```sh
aws ecs describe-tasks --cluster panorama --tasks <task-arn> \
  --query "tasks[0].containers[0].exitCode"
```

A non-zero exit is in CloudWatch Logs under `/aws/ecs/panorama`.

### 3. Start the service

```sh
aws cloudformation deploy --stack-name panorama --template-file panorama.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides DesiredCount=2
```

Two is the smallest count that survives an availability zone. Give it
explicitly: `aws cloudformation deploy` keeps the current value of every
parameter left out of `--parameter-overrides`, so leaving `DesiredCount` out
keeps it at `0`.

The Launch Stack button in the manual's Quickstart opens the AWS console's
quick-create page with the latest release's template, read from
`https://panorama-public-artifacts.s3.amazonaws.com/panorama/stable/panorama.yaml`,
and `DesiredCount` set to `0`. Every release's template is also there under its
version, as `panorama/<version>/panorama.yaml`.

### 4. Point DNS at it

Skip this if you gave a `HostedZoneId`; the alias record is already there.
Otherwise create a CNAME or an alias to the `LoadBalancerDnsName` output.

### 5. Sign in

A fresh deployment has no accounts, so it issues a one-time setup code. Mint
one deterministically rather than reading it out of the logs — with more than
one task, two can mint at the same moment and only the second survives:

```sh
aws ecs run-task --cluster panorama --task-definition panorama \
  --launch-type FARGATE \
  --network-configuration "awsvpcConfiguration={subnets=[subnet-aaa,subnet-bbb],securityGroups=[<ServiceSecurityGroupId>],assignPublicIp=DISABLED}" \
  --overrides '{"containerOverrides":[{"name":"panorama","command":["admin","setup"]}]}'
```

> The cluster, the log group and the task family are all named after the
> stack, so every `--cluster panorama` above assumes you called it `panorama`.
> The `MigrateCommand` output is filled in with your actual names.

The code is in that task's log stream. Open the console at the `ConsoleUrl`
output and enter it. That session configures single sign-on, the group-to-role
mapping and Bedrock — and nothing else. It creates no account: the first
account is created by the first sign-in, and it is an administrator.

**Evaluating without single sign-on?** Mint a gateway token instead with
`panorama token create`, run the same way as the task above. It authenticates a
coding tool and the console's API, not a browser: the Routing screen, My setup
and Analytics need a single sign-on account.

---

## The image

Every release is published to **`public.ecr.aws/dnxsolutions/panorama`**, built by
the public repository's release workflow. Pulling it needs no credentials and
no access granted to your account, and it is served from a CDN, so a task in
any region pulls it at the same speed.

Two parameters name what to run:

- `ImageRepository`, the repository without a tag. It defaults to the public
  one; set it to pull from a [mirror](#mirroring-into-your-own-account) or a
  pull-through cache in your own account.
- `ImageTag`, the whole of *which* image, in any of these shapes, because they
  are all just names for an image in the same repository:

| Value | What it is |
| --- | --- |
| empty | The version the template was released with (`stable` in this directory's copy). The default |
| `stable` | The latest release |
| `beta` | The latest prerelease. Empty until one is published |
| `v0.1.3` | A version. A release never overwrites one |
| `sha256:d7ae…` | The exact bytes. The strongest form |

There is deliberately no separate channel or digest parameter. Three parameters
with a precedence between them would leave two of the three reaching nothing at
any given moment, so editing an inert one would render an identical template and
CloudFormation would refuse with *No updates are to be performed*, correctly and
unhelpfully. One parameter has no precedence to get wrong, and every edit to it
is a real change.

**Tasks run on ARM64 by default**, because the binary is CGO-free and ARM64
Fargate is cheaper for identical work. Every published image carries both
architectures. An image you build yourself has to carry the one the task runs
on: a task whose architecture the image lacks fails to start with an exec format
error, which reads like a corrupt image rather than a mismatch. Set
`TaskCpuArchitecture=X86_64` if you are pulling a single-architecture amd64
build.

**Empty is the default because it is the template's own version.** The release
workflow writes that version into the `Release` mapping, not into the
parameter's default, and that is the point: a stack keeps the value a
parameter had when the stack was created, so a default of `v1.4.0` would be
stored as `v1.4.0` and the next release's template would leave the stack on it.
An empty parameter stays empty, and the mapping comes with whichever template
is deployed. So an upgrade is a new template, and the template and the image
cannot drift apart.

**Pin something immutable in production, not a channel.** `stable` and `beta`
move; a version tag and a digest do not. Two consequences follow from pinning a
channel, and both bite: deploying the same template twice can produce two
different versions, and ECS will not re-pull a tag it already has, so a moving
tag gives you either a silent no-op deploy or a fleet running two versions.

ECR Public has no tag immutability setting, so a version tag stays put because
the release workflow refuses to push one that already exists. A digest is
stronger still, because it survives a mirror and a retag, and it is what
`image.json` on each GitHub release publishes.

```sh
# follow the beta channel
--parameter-overrides ImageTag=beta

# pin a release
--parameter-overrides ImageTag=v0.1.3

# pin the exact bytes
--parameter-overrides ImageTag=sha256:d7ae...
```

**To resolve a tag to a digest**, ask the registry, or read `image.json` on the
release:

```sh
docker buildx imagetools inspect public.ecr.aws/dnxsolutions/panorama:stable \
  --format '{{.Manifest.Digest}}'
```

**To check where an image came from**, verify its build provenance. Each
release's image carries a signed attestation naming the workflow run and the
commit of the public repository it was built from:

```sh
gh attestation verify oci://public.ecr.aws/dnxsolutions/panorama:v0.1.3 -R dnxlabs/panorama
```

### How an image gets published

`.github/workflows/release.yml` builds and pushes it, on a GitHub release being
published. The release tag is the version: publishing `v1.4.0` produces
`panorama:v1.4.0`, stamped into the binary so `panorama version` agrees with
the release somebody is looking at.

One build, both architectures, pushed under two names at most:

| Release | Tags written |
| --- | --- |
| `v1.4.0`, latest published release | `v1.4.0`, `stable` |
| `v1.5.0-beta.1`, latest prerelease | `v1.5.0-beta.1`, `beta` |
| `v1.3.5`, published after `v1.4.0` exists | `v1.3.5` only |

That last row is the rule worth knowing. A channel tag only ever moves forward:
a backported patch published after a higher release writes its own version tag
and moves nothing, because dragging `stable` backwards would downgrade every
deployment following it. The run says so in a warning rather than leaving the
channel silently unmoved. How the workflow is authorized to push is in its own
comments.

### Mirroring into your own account

Required in private subnets with no NAT gateway, and reasonable anywhere a
deploy should not depend on a registry outside your account.

```sh
SRC=public.ecr.aws/dnxsolutions/panorama
MINE=<your-account>.dkr.ecr.<your-region>.amazonaws.com/panorama

aws ecr create-repository --repository-name panorama --region <your-region>

# imagetools copies the whole manifest list, so both architectures and the
# digest survive. `docker pull && docker push` would flatten it to whichever
# architecture the machine running it happens to be.
docker buildx imagetools create -t "$MINE:v1.4.0" "$SRC:v1.4.0"
```

Then point the stack at it:

```sh
--parameter-overrides \
    ImageRepository=<your-account>.dkr.ecr.<your-region>.amazonaws.com/panorama \
    ImageTag=v1.4.0
```

An [ECR pull-through cache rule][ptc] does the same without the manual step, and
needs no stored credential for an upstream of ECR Public:

```sh
aws ecr create-pull-through-cache-rule --region <your-region> \
  --ecr-repository-prefix ecr-public --upstream-registry-url public.ecr.aws
# then
--parameter-overrides \
    ImageRepository=<your-account>.dkr.ecr.<your-region>.amazonaws.com/ecr-public/dnxsolutions/panorama
```

ECR fetches from the upstream on its own side, so a task behind VPC endpoints
can use the cache. What it needs is permission: importing an image the cache
does not hold yet takes `ecr:BatchImportUpstreamImage` (and `ecr:CreateRepository`
the first time), which the stack's execution role does not carry. Either pull
each version once yourself before deploying it (`docker pull` of the cache URI,
as somebody holding those permissions), or grant them to the execution role.

[ptc]: https://docs.aws.amazon.com/AmazonECR/latest/userguide/pull-through-cache.html

---

## Upgrading

The new release's template, in this order, every time:

```sh
curl -fsSLO https://github.com/dnxlabs/panorama/releases/download/<version>/panorama.yaml

# 1. register the new revision with no tasks, and migrate on it
aws cloudformation deploy --stack-name panorama --template-file panorama.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides DesiredCount=0

# 2. run the MigrateCommand output, wait for exit code 0

# 3. roll the service
aws cloudformation deploy --stack-name panorama --template-file panorama.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides DesiredCount=2
```

Every parameter left out keeps its value, which is why `DesiredCount` is given
both times. A stack that set `ImageTag` keeps that image whatever template is
deployed: pass the new one, or `ImageTag=""` to follow the template again.

If you would rather not drop to zero on an upgrade, register the new task
definition by deploying the new template with the current `DesiredCount`,
then run the migration task against the *new* revision before the service
finishes rolling — the migration and the rollout overlap, which is safe for the
reason in the next paragraph and is why the old tasks have to tolerate the new
schema.

**Every migration must be backward-compatible with the previous release.**
Migrations run before the rollout and both versions serve at once. Expand and
contract: add nullable columns and new tables in release N, backfill, stop
writing the old shape in N+1, drop it in N+2. A rename or a drop in the same
release as the code change breaks the tasks still serving, silently, on your
traffic.

**Rollback: roll the digest back, fix the schema forward.** Task definition
revisions make the code trivial to revert. Migrations do not revert.

### What happens to a request that is streaming when its task is replaced

ECS deregisters the target and waits for the load balancer to report it unused —
up to the full 300-second deregistration delay — *before* it sends `SIGTERM`.
So an in-flight turn gets that whole window, and the process then gets
`stopTimeout` to finish its own books.

The honest limit: **Fargate caps `stopTimeout` at 120 seconds and it cannot be
raised.** A turn still streaming when the deregistration delay expires has its
connection closed. A coding agent that loses a stream retries the turn, so in
practice this is survivable.

Four settings carry that behaviour, and three of them are non-default. Do not
tune them without reading why:

| Setting | Value | Default | Why |
| --- | --- | --- | --- |
| ALB `idle_timeout` | 600s | 60s | Measures the gap between bytes. A long time-to-first-token or an extended thinking block truncates the stream mid-turn |
| Target group `deregistration_delay` | 300s | 300s | The whole window an in-flight stream gets. The ECS guidance to drop this to 5s explicitly excepts streaming connections |
| ECS `stopTimeout` | 120s | 30s | Fargate's maximum |
| `server.shutdown_timeout` | 110s | 30s | Just under `stopTimeout`, so the process finishes its own drain rather than being killed part-way through |

---

## Operations

### Running a command

Issuing a setup code, minting a token, checking migration status — all of it is
a one-off task built from the current task definition, so it runs the same
image the service runs, with the same database and the same secrets:

```sh
aws ecs run-task --cluster <stack-name> --task-definition <stack-name> \
  --launch-type FARGATE \
  --network-configuration "awsvpcConfiguration={subnets=[<service-subnets>],securityGroups=[<ServiceSecurityGroupId>],assignPublicIp=<AssignPublicIp>}" \
  --overrides '{"containerOverrides":[{"name":"panorama","command":["admin","setup"]}]}'
```

Swap the `command` for what you need — `["admin","setup"]`,
`["token","create","-email","you@example.com","-role","admin"]`,
`["migrate","status"]`, `["sso","doctor"]`. The output is in that task's log
stream under `/aws/ecs/<stack-name>`.

**To get a shell in a running task:**

```sh
aws ecs execute-command --cluster <stack-name> --task <task-id> \
  --container panorama --interactive --command /busybox/sh
```

Execute-command is on, with no parameter to turn it off: a debugging door that
has to be enabled during the incident is a door nobody has. The runtime image
is the `debug-nonroot` distroless variant, which carries busybox — hence
`/busybox/sh` rather than `/bin/sh`, which does not exist.

**Two things that grants, worth reading once.** The session runs as **root**
inside the container, not as the `nonroot` user the process runs as. And it
lands beside the Bedrock credentials and the database connection. So anyone
holding `ecs:ExecuteCommand` on this cluster has an interactive root shell in
the gateway — control that with IAM on the caller, not by assuming the door is
obscure. Every session is recorded in CloudTrail, naming the caller and the
task.

For the routine things — a setup code, a token, migration status — prefer the
one-off task above. It needs no live task, leaves the running service alone,
and its output goes to the log group where you can find it again.

### Connecting single sign-on

The stack creates an empty secret for it and the task definition already reads
every field from it, so **nothing about connecting SSO edits the task
definition** — which matters, because the task definition belongs to
CloudFormation and a hand edit is reverted by the next stack update without
saying so.

```sh
SECRET=$(aws cloudformation describe-stacks --stack-name <stack> \
  --query "Stacks[0].Outputs[?OutputKey=='OidcSecretArn'].OutputValue" --output text)

aws secretsmanager put-secret-value --secret-id "$SECRET" --secret-string '{
  "issuer":        "https://login.microsoftonline.com/<tenant>/v2.0",
  "client_id":     "...",
  "client_secret": "...",
  "scopes":        "",
  "groups_claim":  ""
}'

aws ecs update-service --cluster <stack> --service panorama --force-new-deployment
```

The setup wizard prints this with your own values already filled in, once it
has probed the issuer.

**Every field ships empty, and empty means something.** An environment variable
set to the empty string is treated as absent, so `scopes` and `groups_claim`
fall through to the defaults in the binary and only need filling in for a
provider that differs. An empty `issuer` means SSO is off, which is how the
deployment starts.

They are blank rather than `REPLACE_ME` for a reason worth knowing before you
change it: an issuer that is set makes the app demand https, a client id and a
client secret, so `REPLACE_ME` fails validation and **the task will not start**.
A plausible-looking one like `https://replace-me.invalid` passes validation and
is worse — the console then advertises a sign-in button that dead-ends, because
discovery is lazy and nothing fails until somebody clicks it.

**The secret is read once, when a task starts.** A running task keeps the
connection it booted with, which is why step two is not optional.

### Rotating the database password

Automatic rotation is deliberately **off**. RDS managed master passwords rotate
every seven days, and a task reads its password from an environment variable
once, at start — so a rotation nobody asked for breaks every new database
connection until the service is redeployed. That is a 3am outage with no cause
anyone would look for.

Rotation is therefore two deliberate steps:

```sh
aws secretsmanager update-secret --secret-id <DatabaseSecretArn> \
  --secret-string '{"username":"panorama","password":"<new>"}'
aws rds modify-db-cluster --db-cluster-identifier <cluster> \
  --master-user-password '<new>' --apply-immediately
aws ecs update-service --cluster panorama --service panorama --force-new-deployment
```

Turning managed rotation on later is a configuration change rather than a code
change — that is what the discrete `database.host` / `user` / `password` fields
bought — but it needs something to force a new deployment when the secret
rotates, and this template does not ship that something.

### The encryption key

Generated once, injected into every task, and it protects the integration
credentials stored in the database. **Copy the `EncryptionKeySecretArn` output's
value somewhere outside this account.** Changing it does not fail — it makes
every stored GitHub credential unreadable at the next confirmation pass, which
surfaces as an integration that was working and is now `failing`.

Bring your own with `EncryptionKeySecretArn` if you are migrating from a
compose deployment and want the credentials it already stored to stay readable.

### Moving to another region or account

`DatabaseSnapshotIdentifier` creates the cluster from a snapshot instead of
empty. Copy a snapshot of the old cluster into the new region (re-encrypted with
that region's `alias/aws/rds`), pass it with the old deployment's encryption key
as `EncryptionKeySecretArn`, and fill the new OIDC secret from the old one. The
database name, user and encryption come from the snapshot; the password is set
from the new stack's secret after the restore. Leave the parameter set for the
life of the stack, because changing it replaces the cluster. The steps are in
the manual's *Move to another region*.

### Logs

Everything is in `/aws/ecs/panorama`, as JSON. Structured context — `userId`,
`teamId`, `sessionId`, `model`, `provider`, `tool` — is on every metric and log
line where it is known, so CloudWatch Logs Insights is usable directly:

```
fields @timestamp, msg, userId, model, decision
| filter decision = "blocked"
| sort @timestamp desc
```

### Sizing

The defaults are 0.5 vCPU and 1 GB per task, and 2 tasks.

`server.max_request_bytes` is 32 MiB and a coding-agent request body is held in
memory while the extractors read it, so 0.25 vCPU is optimistic rather than
frugal. The database pool is set to 10 per task rather than the file default of
25, because each task pins its idle connections and a gateway whose hot path
never touches the database does not need 25 — four tasks at 25 would be 100
connections, which is more than a small instance allows.

---

## Known gaps

Stated rather than discovered:

- **The audit-write drain is not called on shutdown.** `DataPlane.Drain()`
  exists and nothing invokes it, so the goroutines writing the request row, the
  session attribution and the ledger increment are cut off when a task stops.
  Today that costs audit rows on a rolling deploy. Fixed before this deployment
  path is recommended for production.
- **The interval jobs have no leader election.** The work-item confirmer, the
  model discoverer and the session sweeper run in every task, so with N tasks
  you get N× the GitHub API calls and a race on the model catalogue rewrite.
  Harmless at N=2 and worth fixing before N is larger.
- **`X-Forwarded-For` is not trusted**, which is the right default for a
  gateway reachable directly and the wrong one behind a load balancer: every
  row in the audit trail records the load balancer's private IP rather than the
  client's. The fix is a `server.trusted_proxies` list, not trusting the header.
- **SQLite is not an option here.** It is one writer, so it cannot back more
  than one task. The compose stack is where SQLite belongs.

## Deleting the stack

The database cluster has a `Snapshot` deletion policy, so deleting the stack
keeps the data as a final snapshot. Set `DatabaseDeletionProtection=true` in
production and you will have to turn it off before a delete succeeds — which is
the intended friction.

The secrets are deleted with a recovery window, so a stack deleted by mistake
can be rebuilt against the same encryption key for a while afterwards. Not
forever: keep the copy.
