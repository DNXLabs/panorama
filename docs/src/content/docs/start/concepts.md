---
title: Concepts
description: >-
  The dozen words the rest of these pages use, each in a sentence or two:
  route, provider, budget, work item, cost center, and the credentials.
sidebar:
  order: 3
---

Every other page assumes these. Each entry links to the page that covers it
properly.

## Routing

**Route.** A name an engineer's tool asks for in place of a model, such as
`standard` or `deep`. An administrator decides which model the name runs
on and who may use it. The list of routes is the whole of what a deployment
permits: a name that is not a route is refused, and that includes a real model
id. See [Routes](/docs/configure/routes/).

**Model provider.** Where models are invoked. There are two kinds,
`bedrock` (Amazon Bedrock's runtime endpoint) and `bedrock-mantle` (Bedrock's
OpenAI-compatible endpoint), and each is set to one AWS region. See
[Model providers](/docs/configure/providers/).

**Catalogue.** The models your AWS account can actually invoke, which Panorama
discovers by asking each provider. A route can only point at a model in the
catalogue.

**Inference profile.** The prefix on a Bedrock model id, such as `us.`, `au.`
or `global.`. It decides which countries a request may be processed in and what
it costs, so `us.anthropic.claude-sonnet-5` and
`global.anthropic.claude-sonnet-5` are two different catalogue entries.

## Spending

**Budget.** A spending limit on a person, a team or the whole organization,
over a day, a week or a month. See [Budgets](/docs/configure/budgets/).

**Soft limit.** The warning line. Past it, requests are still served, except on
routes you marked as stopping there. The engineer is told which routes still
work and switches.

**Hard limit.** The stop line. Past it, every request governed by that budget
is refused until the window resets or somebody raises the limit.

**Unpriced.** A request whose model has no published rate. It is recorded with
no cost and counted separately, never as $0.00.

## Attribution

**Work item.** The thing a request paid for: a branch, a pull request or a
ticket. See [How attribution works](/docs/start/attribution/).

**Session.** One conversation in a coding tool. Attribution is decided once per
session and every request in it inherits the answer.

**Provenance.** How Panorama knows a request belongs to a work item: the client
said so (*stamp*), it was read from the request's structure (*structural*), or
a ticket key was mentioned in the text (*content*). It is shown beside every
attribution.

**Unattributed.** Spend that could not be tied to a work item. It is always
shown next to the attributed figure.

**Cost center.** An accounting line, such as `CC-100 Payments`. Rules map a
repository or a ticket prefix to one. It reports and warns; it never blocks.
See [Cost centers](/docs/configure/cost-centers/).

## People

**Role.** `admin`, `manager` or `developer`. An admin configures everything. A
manager sees the totals for teams they lead. A developer sees their own usage
and nothing else. Roles come from your identity provider's groups.

**Team.** A group of people whose spend rolls up together, and the smallest
unit a manager is shown.

## Credentials

You can tell them apart by prefix. See
[Credentials](/docs/reference/credentials/).

| Prefix | What it is | Who holds it |
| --- | --- | --- |
| `pnt_` | Gateway token | An engineer's coding tool |
| `pnk_` | Service token | A CI pipeline, billed to a team |
| `pnb_` | Setup code | Whoever is setting the deployment up, once |

**Setup session.** What a setup code opens: an hour in which you can configure
single sign-on and the model providers, and nothing else. It is not an account.
