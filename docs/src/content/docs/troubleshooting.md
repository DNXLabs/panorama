---
title: Troubleshooting
description: >-
  For whoever runs the deployment: the provider check fails, the catalogue is
  empty, everybody is a developer, a command changed nothing, streams cut off,
  tasks never start, and where to look first.
---

This page is for whoever operates a deployment. If you are an engineer looking
at an error in your coding tool, see
[When a request is refused](/docs/engineers/refusals/).

## Where to look first

- **Every error carries a `request_id`**, and so does every log line about that
  request. Ask for it, then search the logs for it.
- **Audit trail** in the console lists every request the gateway saw, including
  the refused ones, with the reason under the route name. A row is a sitting:
  the requests one conversation made with one outcome, with no more than thirty
  idle minutes between them, including its fast-model calls and web searches. Click a row to list its requests,
  each with its `request_id`. Filter by user, repository, branch or model. It
  does not update by itself: press **Refresh** to read the newest requests
  again.
- **Logs** are structured. `userId`, `teamId`, `sessionId`, `model`, `provider`
  and `tool` are on every line where they are known. Set
  `PANORAMA_LOG_LEVEL=debug` to log every request.
- **Analytics → Attention** lists what Panorama thinks needs you: budgets at
  their limit, a failing integration, spend no cost center claims.

## The provider check fails

**Check**, on **Settings → Model providers**, makes one real request, streamed
the way a coding tool streams, and the block shows Bedrock's own words when it
fails. From a shell it is `panorama providers check bedrock`. With no model
named, it tries the model of each route on the provider first, then the
cheapest model the provider can serve.

When Bedrock refuses one model for a reason that belongs to that model, such as
a model AWS does not offer the account or a retired one, the check says so and
tries the next model, up to three. It passes if any model answered, and lists
the ones that did not. Any other refusal stops it at the first model, because
the next one would fail the same way.

Under a refusal the check also shows what Bedrock reports about the model:
`agreement`, `authorization`, `entitlement` and `region`. Read it as evidence,
not as the answer. `agreement NOT_AVAILABLE` is normal for a model nobody in
the account has invoked yet, and a model can read `AUTHORIZED` and `AVAILABLE`
everywhere and still be refused.

The causes, by the words in the error:

### No credentials in the container

The error contains `get credentials: failed to refresh cached credentials`. On
a server, attach an instance role. On a laptop, see
[On your laptop](/docs/deploy/laptop/#start-it). Panorama uses the AWS default
credential chain and has no field for a key.

### Expired credentials

The usual one on a laptop. The error contains `ExpiredTokenException`.
Temporary credentials exported into `.env` last a few hours and do not renew
themselves. Export them again and run `up -d` again. A role attached to a
server does not have this problem.

On a new deployment these two can arrive from *listing* models rather than
invoking one, because a check with an empty catalogue reads the catalogue
first. The cause and the fix are the same.

### The Anthropic use case form

The error reads `Model use case details have not been submitted for this
account`. Anthropic asks every AWS account to describe its use case once before
any of its models can be invoked. Nothing in Panorama's role or configuration
changes this.

1. In the Amazon Bedrock console, in the region your provider uses, open the
   **Model catalog** and choose any Anthropic model.
2. Submit the use case details it asks for. Once covers every Anthropic model
   in the account.
3. Wait up to 15 minutes, then press **Check** again.

Every Anthropic model fails the same way until then, so the check does not try
another.

### The model is not available for this account

The error reads `... is not available for this account`. AWS decides which
models each account is offered, and this account has not been offered this
one. It is not a permission: no IAM change grants it. Point the route at
another model, or ask your AWS account team for access. The check moves on to
the next model and reports this one as refused.

### Bedrock has not finished subscribing the account

The error reads `You don't have access to the model with the specified model
ID`, or mentions `aws-marketplace`. Bedrock subscribes the account to a model
through AWS Marketplace the first time it is invoked, and requests can be
refused for a few minutes until the subscription is active. There is no longer
a **Model access** page to enable anything on.

- Press **Check** again after a few minutes. Most of the time that is all.
- If it is still refused after 15 minutes, open **AWS Marketplace → Manage
  subscriptions** for a failed or pending agreement for the model. The `agreement`
  line under the refusal can show its error.
- Make sure the role has `aws-marketplace:Subscribe` and
  `aws-marketplace:ViewSubscriptions`, which **Role policy** includes, and that
  no service control policy denies them.

### Not authorized, or an explicit deny

The error reads `is not authorized to perform`. Two causes:

- The role lacks the policy. Compare it with **Role policy** on the same
  screen.
- The error ends `with an explicit deny in a service control policy`, or names a
  permissions boundary. A policy above the role refuses it, and granting the
  role more cannot override that. Ask whoever runs the AWS organization.

### The model has been retired

The error reads `reached the end of its life`. Point the route at a current
model from the catalogue.

### A timeout, from private subnets

There is no route to Bedrock. You need a NAT gateway or the `bedrock-runtime`
VPC endpoint. See [Connectivity](/docs/deploy/aws/#connectivity).

### It passed in one region and you changed region

A region change clears the check. Run it again.

## Engineers report "Bedrock refused this model"

An engineer whose request Bedrock refused sees *"Bedrock refused this model for
this Panorama deployment's AWS account"*. They are not shown Bedrock's reason,
because it names your account and its roles.

You can see it. **Settings → Model providers** shows the provider's **last
refused request**: when, which model, Bedrock's own words, what to do about
them, and the `request_id` the engineer was given. The causes are the ones
[the provider check](#the-provider-check-fails) meets, and the same fixes apply.
The log line `provider invocation failed` carries the same words and a `hint`.

Once a **Check** invokes that model successfully, the last refused request is
cleared.

## The model catalogue is empty

No route can be created, because there is nothing to point one at.

- The provider is not turned on, or is `failing`. Look at **Settings → Model
  providers**.
- Discovery failed when Panorama started, most often for want of credentials.
  Press **Check** on the provider, which reads the catalogue when there is
  none and reports why it could not. Or open **Routing → Model catalogue** and
  press **Refresh catalogue**, or run `panorama models discover`.
- In private subnets, the `bedrock` VPC endpoint is missing. Invoking models
  uses `bedrock-runtime`; *listing* them uses `bedrock`. You need both.

## Single sign-on

It has [a page of its own](/docs/configure/sso-troubleshooting/), by symptom:
everybody gets the wrong role, one person does, the sign-in page shows an
error, or it worked and then stopped.

## I cannot open the console

**Nobody can sign in**, because single sign-on is not connected yet, the group
mapping refuses you, or the provider changed. Issue a one-time code:

```sh
panorama admin setup
```

Enter it at the console. The session it opens can connect or repair single
sign-on, check the model providers, and, under **Administrators**, make an
account an administrator again. It cannot reach anything else, by design. See
[Credentials](/docs/reference/credentials/#setup-codes).

**On your own laptop with no identity provider**, use the development sign-in.
See [On your laptop](/docs/deploy/laptop/#start-it).

## A command succeeded and nothing changed

You ran it on your laptop. Every command except `login`, `logout`, `token`,
`whoami` and `configure` opens the database directly, so on a machine without
the deployment's database it creates a local SQLite file, succeeds, and does
nothing useful. Run it in the container:

```sh
docker compose -f deploy/compose/docker-compose.yml exec panorama panorama <command>
```

On AWS, run it as a [one-off task](/docs/deploy/aws/#running-a-command).

## My `.env` changes are ignored

Compose reads `.env` from the directory that holds the compose file,
`deploy/compose/`. A `.env` in the repository root is ignored without a
warning. Also, values the compose file sets literally, such as the database
driver, cannot be overridden from `.env`.

## It works against api.anthropic.com and fails here

Almost always a **beta flag**. Coding tools send `anthropic-beta` flags, and
Bedrock rejects a whole request over one flag it does not recognise. Tools
adopt new flags faster than Bedrock does.

Panorama drops the flags on its list before forwarding, and logs each drop at
WARN: `dropped beta flags Bedrock does not accept`. When a tool update adds a
flag that is not on the list yet, requests start failing with an *invalid beta
flag* error from the provider.

1. Find the flag. At debug level, Panorama logs `client requested beta
   features` with the full list.
2. In **Settings → Model providers → Model tables → Correct one**, switch
   **Beta flags to drop** to **My list**. Enter this build's flags plus the new
   one, one per line.
3. Save. It applies from the next request.

Your list replaces the built-in one, so include both. An empty list of your own
forwards every flag, which is how you find out what Bedrock has started
accepting.

The other two differences between Bedrock and the Anthropic API are handled for
you. See
[Model providers](/docs/configure/providers/#good-to-know-bedrock-is-not-the-anthropic-api).

## Claude Code fails on one route with "The model provider rejected this request"

The log line is `provider invocation failed`, and its error carries Bedrock's
reason, such as `adaptive thinking is not supported on this model`. The model
behind that route is older than what Claude Code assumes a route to be. Point
the route at a current model. See
[Choose a model your tools can use](/docs/configure/routes/#choose-a-model-your-tools-can-use).

## opencode fails on a GPT route with "The model provider rejected this request"

The log line is `provider invocation failed`, and its error says `Function
tools with reasoning_effort are not supported` or `'max_tokens' is not
supported with this model`. The engineer's opencode file sends that route to
Chat Completions, which GPT models do not serve to a coding tool. They copy the
block from **My setup** again, which declares the route under the
`panorama-openai` provider and sends it to the Responses API. See
[opencode](/docs/engineers/opencode/#the-block).

## Claude Code compacts the conversation far too early

Its route has a name Claude Code does not recognise, so it assumes a 200k
context window. Name the route after its model, for example `claude-sonnet-5`.
See
[Claude Code](/docs/engineers/claude-code/#name-a-route-after-its-model).

## Requests show as `unpriced`

Panorama records the request with no cost, and counts it separately. It never
records $0.00. The log line is `requests are being recorded unpriced`, once per
model, and its `hint` says which of two things happened:

- **`no price for this model`.** The price table has nothing for it. Refresh,
  and look for the model in the list:

  ```sh
  panorama prices refresh
  panorama prices list
  ```

  If it is still missing, AWS does not publish its price where Panorama can
  fetch it. OpenAI's GPT-5.x is the usual case. A newer release may carry it,
  read from the model's card. Until then, set its rate under `pricing.models`,
  including its long-context price. See the
  [configuration reference](/docs/reference/configuration/).
- **`not for this token class`.** The model has a price, but not for a class
  the request used, such as a cache write. Set the full price under
  `pricing.models`.

If refresh fails, the container cannot reach the AWS Pricing API. It has no VPC
endpoint, so a fully private deployment needs a NAT route for it.

A request larger than a model's long-context threshold can also be recorded
unpriced right after a refresh, with a WARN saying the fetched rate differs from
the compiled one. The long-context price is then unknown, and Panorama will not
bill it at the short-context rate. A newer release fixes it, or state the price
under `pricing.models`.

## Most spend is unattributed

See [Reading the gap](/docs/start/attribution/#reading-the-gap). The usual
causes are opencode traffic, which stops at the repository, and branch names
with no ticket key in them.

## Responses cut off mid-stream

The load balancer or proxy in front of Panorama has a short **idle timeout**.
It measures the gap between bytes, and a model can think for longer than the
common 60-second default. Set it to several minutes. The CloudFormation
template uses 600 seconds.

## On AWS, the tasks never start

- **`CannotPullContainerError`.** The tasks cannot reach the registry. In
  private subnets with no NAT gateway, `public.ecr.aws` is unreachable: pull
  from a mirror in your account and check the ECR and S3 VPC endpoints. See
  [Connectivity](/docs/deploy/aws/#connectivity).
- **`exec format error`.** The image has no ARM64 build. Set
  `TaskCpuArchitecture=X86_64`.
- **The task exits at once after you set the OIDC secret.** A placeholder in
  `issuer` fails validation. Leave it empty until you have the real value. With
  Google as the issuer, the `OidcAllowedDomains` stack parameter is required
  too, and the task's log names it.
- **`/readyz` never turns healthy.** The migrations have not been run. See
  [Deploy, step 2](/docs/deploy/aws/#2-run-the-migrations).

## SQLite: a policy applied and the gateway ignores it

The database file is on a bind mount or a network share. Both ways SQLite fails
there are silent. Put it on a named Docker volume. See
[Choosing a database](/docs/deploy/database/#the-one-rule-about-sqlite).

## The GitHub integration is `failing`

See [GitHub integration](/docs/configure/integrations/#what-goes-wrong).
