---
title: Model providers
description: >-
  Turn on a Bedrock provider, prove it can invoke a model, change its region
  safely, read the model catalogue, and correct it when AWS ships something
  this build has not heard of.
sidebar:
  order: 2
---

A model provider is where Panorama invokes models. There are two kinds, and
each is set to **one AWS region**:

| Kind | What it serves |
| --- | --- |
| `bedrock` | Amazon Bedrock's runtime endpoint: Claude, through inference profiles. |
| `bedrock-mantle` | Bedrock's OpenAI-compatible endpoint: open-weight and other models, and Claude too. Needed for Codex CLI. |

The two may be in different regions. One kind cannot be in two regions at once.

## Turn a provider on

In **Settings → Model providers**, on the provider's block:

1. Choose a **Region**. Before anything is saved, Panorama asks that region what
   it offers and tells you how many models it lists.
2. Press **Turn on**. Panorama reads the region's model catalogue and starts
   fetching its prices.
3. Press **Check**. This makes one real request, for one token of output, on
   the cheapest model it can serve. The state changes from `never checked` to
   `connected`.

Do not skip the check. A role that looks right and a role that works are told
apart by one real request and by nothing else, because no AWS API reports a
service control policy that blocks invocation.

From a shell:

```sh
panorama providers set bedrock -region us-east-1
panorama providers check bedrock
panorama providers list
```

`AWS_REGION` (or `bedrock.region` in the configuration file) sets the `bedrock`
provider's region on the **first start only**. After that the console owns it,
and changing the variable does nothing except log a warning.

The IAM policy the deployment's role needs is on the same screen under **Role
policy**. It covers both kinds in every region, so none of the steps on this
page ever needs an IAM change. See
[Requirements](/docs/deploy/requirements/#aws-credentials).

## Change a region

Pick the new region in the same select. Before you save, Panorama names any
**routes that point at a model the new region does not list**. Those routes
would refuse every request until you point them at a model it does.

Saving clears the provider's `connected` state, because a check that passed in
one region says nothing about another. Press **Check** again.

## Turn a provider off

Set **Routes may use it** to **Off**. Panorama refuses while a route still uses
the provider, and names the routes, so that you cannot strand them by accident.

## The model catalogue

Nobody types in a list of models. Panorama asks each provider what your account
can invoke and keeps the answer. Open it from **Routing → Model catalogue**.

It refreshes every six hours (`providers.discovery_interval`). To refresh now,
press **Refresh catalogue**, or run `panorama models discover`.

What the columns and tags tell you:

- **Processed in** is where a request may be sent, and it is your
  data-residency answer. On `bedrock` it comes from the **inference profile**,
  the prefix on the model id. `au.anthropic.claude-sonnet-5` and
  `global.anthropic.claude-sonnet-5` are separate rows because they route to
  different countries and are priced differently. A regional profile often
  costs about 10% more than `global`, and not always, so each row shows its own
  published rate.
- **`legacy`** means AWS has scheduled the model for retirement. A route
  pointing at one is an outage with a date on it.
- **`1M context`** or **`200K context`** is the model's context window.
- **`browse only`** means this build does not know which API the model speaks,
  so you can see it and cannot route to it.
- **`no streaming`** and **`unavailable`** models cannot be routed to either.

`bedrock-mantle` has no inference profiles. A model runs in the region the
provider is set to.

## When AWS ships something this build does not know

Three facts about a model are not published by any Bedrock API: its context
window, which API it speaks, and which `anthropic-beta` flags Bedrock accepts
for it. Panorama ships tables for all three, and lets you correct them without
waiting for a release.

On the provider's block, **Model tables → Correct one**:

| Field | Format | Use it when |
| --- | --- | --- |
| **Context windows** | `anthropic.claude-sonnet-5 = 1000000`, one per line | A new model shows no context tag. Until corrected, its window is recorded as unknown and engineers' tools are told nothing, which is safer than a guess. |
| **APIs** | `qwen.qwen3-coder = openai-chat`, one per line. Values: `anthropic-messages`, `openai-chat`, `openai-responses` | A new model is `browse only`. |
| **Beta flags to drop** | One flag per line | Requests from a newly updated Claude Code start failing with an invalid beta flag. See [Troubleshooting](/docs/troubleshooting/#it-works-against-apianthropiccom-and-fails-here). |

A model family name covers every region prefix and revision of it, so one line
is usually enough.

## Good to know: Bedrock is not the Anthropic API

Bedrock's Claude surface differs from `api.anthropic.com` in three ways that
break requests, and Panorama handles all three:

1. **Current Claude models cannot be invoked by their bare model id.** They
   need an inference profile id such as `us.` or `global.`. This is why a route
   points at a specific catalogue entry.
2. **Beta flags travel differently.** Clients send an `anthropic-beta` header,
   and Bedrock reads `anthropic_beta` from the body. Panorama translates.
3. **Bedrock rejects a whole request over one beta flag it does not know.**
   Coding tools adopt new flags faster than Bedrock does, so Panorama drops the
   flags on its list and logs each drop at WARN.

## Good to know: GPT models take a different output limit

On bedrock-mantle, OpenAI's GPT-5 and later models refuse `max_tokens` on Chat
Completions and read the same limit as `max_completion_tokens`. opencode, and
most other tools that speak Chat Completions, send `max_tokens`. Panorama
renames it for these models, so the model stops where the tool asked it to.

A request that sets both keeps `max_completion_tokens`. When the two differ,
Panorama drops `max_tokens` and logs the drop at WARN.

These models also refuse tools on Chat Completions while they are reasoning,
and a coding tool sends tools with every request. So My setup points opencode
at the Responses API for any route whose model speaks it, as Codex already is.
Chat Completions is left for the models that speak nothing else.
