---
title: Codex CLI
description: >-
  The config.toml entry that points Codex CLI at Panorama, where the token
  goes, and the Responses API features the gateway refuses.
sidebar:
  order: 4
---

**My setup** in the console prints this with your own route in it. See
[Set up your coding tool](/docs/engineers/setup/). This page explains what it
prints.

## The configuration

Codex is configured by a file, `~/.codex/config.toml`. Add this beside whatever
is already there:

```toml
# Panorama: the model here is a route name.
model = "standard"
model_provider = "panorama"
model_context_window = 200000
model_max_output_tokens = 64000

[model_providers.panorama]
name = "Panorama"
base_url = "https://panorama.your-company.internal/v1"
env_key = "PANORAMA_TOKEN"
wire_api = "responses"
```

Then start Codex with your token in the variable that file names:

```sh
export PANORAMA_TOKEN=pnt_...
codex
```

- **`model` is a route name**, never a model id.
- **`model_context_window` and `model_max_output_tokens`** belong to the route
  you chose, which is why My setup prints one block per route. If it leaves
  them out, your route's model has no recorded window. Do not invent one.
- **`wire_api = "responses"`**: Codex speaks OpenAI's Responses API and nothing
  else.

## Web search

When your administrator has turned on web search, the block My setup prints
ends with one more table:

```toml
[mcp_servers.panorama]
url = "https://panorama.your-company.internal/mcp"
bearer_token_env_var = "PANORAMA_TOKEN"
```

It gives Codex a `web_search` tool, served by Panorama, and reads the token
from the same variable as the provider entry.

## Only some routes are offered

My setup lists only the routes whose model speaks the Responses API, and shows
one route selector, not two. If the list is empty, your deployment has no such
route yet. An administrator needs the `bedrock-mantle`
[model provider](/docs/configure/providers/) turned on and a route on one of
its models.

A Claude route is served over the Anthropic Messages API, so Codex cannot use
it. Asking for one returns a 400 that names the endpoint it is served on.

## What the gateway refuses

Panorama keeps no request or response body anywhere, and asks the same of the
provider. Each of these is refused with a 400 that says why:

| You send | Why it is refused |
| --- | --- |
| `store: true` | A stored response is your prompt and its answer kept on the provider's side. `store` is forced to `false`. |
| `previous_response_id` | It continues a stored response, and there are none. |
| `background: true` | Background responses are stored ones. |
| a `service_tier` other than the default | Requests are priced at the standard rate, so another tier would be billed wrongly. |

None of this changes how Codex behaves. It resends the conversation each turn
when there is no server-side storage, which is what it does anyway.

## Your token

It lasts 30 days and nothing renews it. Mint a new one on My setup and update
`PANORAMA_TOKEN`. Codex has no way to fetch a credential on demand, so a
session that outlives its token needs a new shell.
