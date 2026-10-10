---
title: The shape of it
description: >-
  One Go binary containing the data plane, the control plane and the embedded
  console, and the choices behind it.
sidebar:
  order: 1
---

One Go binary containing the data plane (the proxy), the control plane (the API
and the background jobs), and the embedded React console. The parts are
separable internally, and it ships as a single artifact. For what happens to a
request, see the diagram on [What Panorama is](/docs/start/what-it-is/).

| Concern | Choice |
| ------- | ------ |
| Language | Go, single static binary (pure-Go SQLite, no cgo) |
| Storage | SQLite by default, Postgres for production; same schema via migrations |
| Human auth | Generic OIDC |
| Tool auth | Per-user gateway tokens, revocable, good for 30 days |
| Jobs | Interval workers inside the same process, on SQLite and Postgres alike. No queue and no broker to operate |
| Provider | A `Provider` interface; two v1 implementations, bedrock-runtime and bedrock-mantle |
| UI | React + TypeScript, built by Vite, embedded with `go:embed` |
| Packaging | Container-first: docker-compose for evaluation, one CloudFormation template for AWS |

## Model ids are namespaced by provider

`bedrock/us.anthropic.claude-...`, `bedrock-mantle/openai.gpt-oss-120b`, split
on the first `/`.

A route, a policy file, the price table's overrides and the audit trail's model
filter all hold the reference, and a bare id in a policy file is refused rather
than guessed onto a provider.

Nothing in core imports AWS SDKs outside the Bedrock provider. The AWS
packaging is a CloudFormation template and imports nothing.

## v1 is single-organization

One deployment is one org. Multi-org and multi-account are enterprise edition.

## Layout

```
cmd/panorama/          single binary entrypoint
cmd/fakebedrock/       Bedrock's wire protocol, for a stack with no AWS
cmd/pricefinder/       finds prices the binary lacks; run by a workflow, not shipped
internal/
  dataplane/           proxy, streaming, API surfaces (anthropic, openai-compat)
  providers/           Provider interface, the registry, + bedrock/
  attribution/         provenance ladder, extractors, session grouping
  pricing/             price table, cache-aware cost computation, long-context tiers
  pricefinder/         Price List snapshots and model cards, read and checked
  policy/              routes, quotas, budgets
  costcenters/         allocation rules, the glob, the stamp lookup
  controlplane/        REST API for the UI, OIDC, token minting
  ssosetup/            OIDC presets, issuer probe, config rendering, doctor
  tools/               which coding tool a request came from, as one list
  integrations/        Source interface, mock/, and github/
  jobs/                interval workers: work-item confirmation, session sweep
  store/               sqlite/postgres, migrations
  auth/                gateway token minting and verification
  config/              configuration loading and validation
  logging/             slog setup
  version/             build metadata
ui/app/                React + TypeScript console (built to ui/dist)
docs/                  this documentation site
test/e2e/              the end-to-end suite, behind the `e2e` build tag
deploy/                compose/, cloudformation/
```
