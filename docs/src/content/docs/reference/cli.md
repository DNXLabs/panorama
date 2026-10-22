---
title: CLI
description: >-
  Every command panorama ships, which of them run inside the container, and
  which run on an engineer's laptop.
sidebar:
  order: 2
---

## What the CLI is for

Panorama is operated from the console. The CLI exists for three things the
console cannot do for you:

- **Getting in when nobody can sign in**: `admin setup`, `token create`.
- **Checks that belong in a pipeline**, because they exit non-zero on failure:
  `sso doctor`, `providers check`, `policy check`.
- **Repair and scripting**: migrations, applying routes and budgets from a
  file, refreshing prices.

Most commands have a screen that does the same thing:

| Command | In the console |
| --- | --- |
| `sso presets`, `sso probe` | **Settings → Single sign-on**: choose a provider, **Check this connection** |
| `sso doctor` | **Settings → Single sign-on → Verify** |
| `sso claims` | **Settings → Single sign-on → Groups your provider has sent** |
| `providers list`, `providers set` | **Settings → Model providers** |
| `providers check` | **Settings → Model providers → Check** |
| `providers policy` | **Settings → Model providers → Role policy** |
| `models discover` | **Routing → Model catalogue → Refresh catalogue** |
| `models list`, `prices list` | **Routing → Model catalogue** |
| `policy show` | **Routing**, and **People & teams** for budgets |
| `policy apply` | **Routing → New route**, and the limits on **People & teams** |
| `team create`, `team list`, `team add` | **People & teams → Teams** |
| `token create` | **My setup → Mint**, for your own token |
| `configure` | **My setup → Instructions** |

`admin setup`, `admin generate-key`, `migrate`, `prices refresh`, `serve` and
`version` have no screen.

## Where commands run

This is the distinction that causes the most confusion, so it comes first.

**`login`, `logout`, `token`, `whoami` and `configure` run on a developer's own
machine** and need no configuration at all: they keep a credential in
`~/.panorama` and talk to a deployment over HTTP.

**Everything else runs where the database is.** Run one of those on a laptop
and it opens or creates a *local* SQLite file instead of the deployment's,
which succeeds and does nothing you wanted.

```sh
docker compose -f deploy/compose/docker-compose.yml \
  exec panorama panorama <command>
```

Configuration comes from the file and from `PANORAMA_`-prefixed environment
variables; the environment wins. `-config <path>` names the file.

A command's flags may come before or after its argument:
`providers check bedrock -model X` and `providers check -model X bedrock` are
the same command. A command that takes no argument refuses one rather than
ignoring it, and `--` ends the flags.

## Running the gateway

```sh
panorama serve                    # the gateway and control plane. The default
panorama version                  # build information, and where its source is
```

`serve` is what the container runs, so you rarely type it.

## Database

```sh
panorama migrate up               # apply pending migrations
panorama migrate status           # show which have been applied
```

## Setting up

```sh
panorama admin setup              # issue a one-time code to configure this deployment
panorama admin generate-key       # mint the key that protects stored credentials
```

`admin setup` works at any time, which is the way back in if nobody can sign
in. See [Credentials](/docs/reference/credentials/#setup-codes).

## Single sign-on

```sh
panorama sso presets              # what each identity provider needs
panorama sso probe                # validate an issuer, print the config to apply
panorama sso doctor               # check the connection against what it has seen
panorama sso claims               # the groups sign-ins have actually carried
```

`doctor` exits non-zero on failure, so it works as a deployment check. `probe`
writes nothing. See [Single sign-on](/docs/configure/sso/).

## Model providers and models

```sh
panorama providers list           # each provider, its region and its state
panorama providers set bedrock -region us-east-1
panorama providers check bedrock  # prove a provider can invoke a model, with one token
panorama providers check bedrock -model us.anthropic.claude-haiku-4-5-20251001-v1:0
panorama providers policy         # the IAM policy this deployment's role needs

panorama models discover          # ask each provider what this account can invoke
panorama models list              # the discovered catalogue, with residency and price
```

`providers check` spends one output token, deliberately: an SCP, a missing
subscription and a wrong identifier all arrive as the same `AccessDenied`, and
one real request is what tells them apart. It streams, as a coding tool does.
With no `-model`, it tries each route's model on the provider first, then the
cheapest model it can serve, and moves past a model Bedrock refuses for a reason
of that model's own (up to three). Each refused model is printed with Bedrock's
words, the fix, and what Bedrock reports about the model's availability:

```text
bedrock could not invoke global.anthropic.claude-haiku-5-5 in us-east-1.

  AccessDeniedException: anthropic.claude-haiku-5-5 is not available for this account. ...

AWS does not offer this model to this AWS account. ...

Bedrock reports for anthropic.claude-haiku-5-5: agreement NOT_AVAILABLE, authorization AUTHORIZED, entitlement AVAILABLE, region AVAILABLE.

bedrock invoked us.anthropic.claude-haiku-4-5-20251001-v1:0 in us-east-1 in 812ms, streamed. One token of output.
```

It exits non-zero when no model could be invoked. See
[Model providers](/docs/configure/providers/) and
[The provider check fails](/docs/troubleshooting/#the-provider-check-fails).

## Prices

```sh
panorama prices list              # the model rate table
panorama prices refresh           # fetch current rates from the AWS Price List API
```

The `SOURCE` column says where each rate came from. A model with a
long-context price has a second line under it, `above 272000 input tokens`,
with the rates that replace the first line's for a prompt that large. See
[The cost model](/docs/internals/pricing/).

## Routes and budgets

```sh
panorama policy show              # routes and budgets, with current spend
panorama policy check -f policy.yaml    # validate without applying
panorama policy apply -f policy.yaml
```

`check` and `apply` run the same validation the console does, so a model that
is not in the catalogue is refused when you run the command, not when somebody
makes a request. **`apply` removes any route or budget that is not in the
file.** It reports what it added, updated, removed and left unchanged, and
writes nothing for an entry that is already as the file says, so re-applying
an unchanged file changes nothing. A running gateway serves a change within a
second, on every replica. See
[Routes as a file](/docs/configure/routes/#routes-as-a-file) and the
[Policy file reference](/docs/reference/policy/).

## People and teams

```sh
panorama team create -name "Platform Engineering"     # -slug and -description are optional
panorama team list
panorama team add -team platform -email priya@example.com    # -role lead, or member (the default)
```

## Credentials

Where the database is:

```sh
panorama token create -email you@example.com -role admin    # -name labels the token
```

It mints a gateway token for somebody and creates their account if it does not
exist. This is how a deployment with no single sign-on gets its first
credential, and until single sign-on is configured the console's sign-in
screen accepts the token. `-role` is `admin`, `manager` or `developer`, and only
applies to a new account.

On an engineer's own machine:

```sh
panorama login                    # sign this machine in, through a browser
panorama logout                   # forget a deployment on this machine
panorama token                    # print this machine's token, refreshing when due
panorama token --quiet            # just the token, for $(…)
panorama whoami                   # who this machine is signed in as
```

:::caution[Those five, and `configure` below, do nothing in this release]
They belong to command-line sign-in, which is built and switched off. The
gateway refuses the flow, so `panorama login` cannot complete and the others
have no credential to read. Engineers mint a token on **My setup** in the
console instead. See
[Credentials](/docs/reference/credentials/#command-line-sign-in-built-and-switched-off).
:::

`panorama token` and `panorama token create` are different commands. The first
prints the credential this machine already holds. The second writes a new one
to the database.

## Configuring a coding tool

```sh
panorama configure claude-code -write
panorama configure opencode -write
panorama configure codex -write
```

Each writes the configuration file that tool reads, with only the routes whose
model speaks an API the tool does. It depends on command-line sign-in, so it is
off in this release. **My setup** in the console prints the same configuration.
See [Set up your coding tool](/docs/engineers/setup/).
