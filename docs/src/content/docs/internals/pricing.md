---
title: The cost model
description: >-
  Four token classes, integer nanodollars, prices that change with the size of
  the prompt, and where each rate comes from.
sidebar:
  order: 2
---

Coding-agent economics live in prompt caching. Panorama accounts for **all four
token classes** (input, output, cache read, cache write) with separate
per-model rates.

Ignoring cache classes overstates coding-agent cost several-fold and is the
fastest way to discredit the headline feature.

## Money is integer nanodollars, never a float

`pricing.Money` and `pricing.Rate` are int64, and parsing reads decimal text
digit by digit.

A column of costs summed as float64 disagrees with itself depending on
summation order, and nanodollars are small enough that one cheap request is not
zero.

Money is rendered from those integers in exactly one place in the console. A
decimal string from the API is for display of a published rate, never for
arithmetic.

## Unpriced is null, not zero

A request whose model has no rate, or whose token class has none, is recorded
with a **null** cost and counted separately, and that count appears beside
every total.

Pricing an unknown at zero would understate spend and look like a discount.
This is the same commitment as unattributed spend, applied to pricing.

In the audit trail a cost of `-` is a request that never reached a provider,
which is a different absence from `unpriced`. One has no cost. The other has a
cost nobody can compute. `$0.00` would be false about both. A row whose
requests are only partly priced shows the priced total with `+N unpriced`
under it, rather than a total that quietly leaves them out.

## A price can change with the size of the prompt

Some models cost more for a long prompt. OpenAI's GPT-5.x on Bedrock charges a
second set of rates above 272K input tokens: input doubles and output rises by
half. The model card says the higher rate "applies to all input and output
tokens, not just tokens above 272K", so it is not a band like a tax bracket. It
replaces the whole price for that request.

Panorama holds that second price as a **tier** on the model's row: a prompt
size, and the rates that apply above it. The prompt size counts every input
token, cached or not, because a long session that mostly reads the cache is
still a long prompt. A model can have several tiers, and the highest one the
prompt exceeds applies.

`panorama prices list` shows each tier on its own line under the model it
belongs to.

## Where a rate comes from

Four sources, and a stronger one always replaces a weaker one for the same
model:

| Source | What it is |
| --- | --- |
| `config` | A rate you set under `pricing.models`. Always wins. |
| `aws-pricing-api` | Fetched from the AWS Price List API by the daily refresh. |
| `seed` | Compiled into the binary: a snapshot of the Price List API, so a deployment prices its first request before it has refreshed, and an air-gapped one prices requests at all. |
| `aws-model-card` | Compiled into the binary: read from the model's card in the Bedrock user guide, for a model the Price List API does not publish. |

The Price List API has no long-context tiers. When the refresh fetches a rate
for a model that has a tier, the tier is kept if the fetched rate matches the
one it replaces. If the two disagree, the tier's rates are stale, so Panorama
keeps the threshold with no rate above it: a request that large is recorded
unpriced rather than billed at the short-context rate, and the log says so at
WARN.

## Models the Price List API does not publish

OpenAI's GPT-5.x is billed through AWS Marketplace, and the Price List API has
no row for it on either endpoint. Its only published price is the table on its
model card.

A workflow in Panorama's own repository reads those cards after every merge and
once a week. It lists every model each provider can serve, and for each one
nothing prices yet:

1. If the Price List API publishes it, the rate is copied as it stands.
2. Otherwise a language model reads the model's card. Nothing it reports is
   kept unless code finds it on the page: every figure in a row it quoted
   verbatim, in the column whose header names that token class, under a
   heading it named that is not another set of regions' prices, with a stated
   unit of per million tokens.

What it finds is added to a table that every release compiles into the binary,
next to the seed. The table only grows: a model it has priced is never read
again, and a release attaches the exact table it embedded. Models it could not
price are listed in an issue on the repository, and are recorded unpriced
until a rate is set under `pricing.models`.

## Three things about Bedrock pricing

Each is checked against the live API rather than taken from documentation.

### 1. Current model prices are not under `AmazonBedrock`

That catalogue holds five Anthropic products, all retired models. Everything
anyone runs a coding agent against is published under
**`AmazonBedrockFoundationModels`**, in a different shape with different
attributes.

### 2. The inference-profile prefix is a price input

AWS publishes two rate sets per model, and global is roughly 10% cheaper.
Sonnet 5 input is $2.20 per million through `us.`, $2.00 through `global.`.

The prefix is therefore part of the price-table key and **must never be
normalised away**.

### 3. Prompt cache writes have two TTLs at different prices

Five minutes and one hour, roughly 1.6x apart. Bedrock reports the split in
`cache_creation`, and a single cache-write number cannot be costed correctly.

## Joining prices to model ids

The pricing catalogue identifies models only by display name, so the refresher
joins `pricing:GetProducts` against `bedrock:ListFoundationModels` to reach a
model id.

Usage types are matched against an **allowlist**. Batch rates are half the
on-demand ones, and pattern-matching would eventually halve a bill.

bedrock-mantle's prices come from `-mantle-` usage types. A Claude model with
none published is priced at the runtime regional rate for its family, and left
unpriced when that is ambiguous.
