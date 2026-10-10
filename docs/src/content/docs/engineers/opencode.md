---
title: opencode
description: >-
  The provider block that points opencode at Panorama, where the token goes,
  and what opencode does better and worse than Claude Code through the gateway.
sidebar:
  order: 3
---

**My setup** in the console prints this block with your own routes in it. See
[Set up your coding tool](/docs/engineers/setup/). This page explains what it
prints.

## The block

opencode is configured by a file, `~/.config/opencode/opencode.json`. There is
no environment-variable form.

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "panorama": {
      "name": "Panorama",
      "npm": "@ai-sdk/anthropic",
      "options": {
        "baseURL": "https://panorama.your-company.internal/v1",
        "apiKey": "pnt_..."
      },
      "models": {
        "standard": {
          "name": "standard",
          "headers": { "user-agent": "opencode (panorama)" },
          "limit": { "context": 1000000, "output": 64000 }
        }
      }
    }
  },
  "model": "panorama/standard",
  "small_model": "panorama/fast"
}
```

Merge it into your existing file if you have one.

- **`baseURL` ends in `/v1`.** Claude Code's does not. The two tools build
  their request paths differently, so the same gateway has two addresses on My
  setup on purpose.
- **Every route you may use is declared as a model**, with its real context
  window and output limit beside it. Copy these numbers from My setup and do
  not edit them: they come from the model your route runs on.
- **`model` and `small_model`** are your everyday route and your background
  route. Without them opencode has a provider and no model selected.
- **`user-agent`** is how Panorama recognises opencode traffic.

If your routes include models that are not Claude, more providers appear beside
the first, and each route is listed under one of them:

| Provider | Package | For routes on |
| --- | --- | --- |
| `panorama` | `@ai-sdk/anthropic` | Claude models |
| `panorama-openai` | `@ai-sdk/openai` | Models that speak OpenAI's Responses API, such as GPT and gpt-oss |
| `panorama-chat` | `@ai-sdk/openai-compatible` | Models that speak only Chat Completions, such as Qwen and DeepSeek |

A GPT model is sent the Responses API because it refuses the tools opencode
sends on Chat Completions. If opencode fails on a GPT route with *"The model
provider rejected this request"*, your file sends that route to Chat
Completions: copy the block from My setup again.

## Keep the token out of the file

Replace the `apiKey` value with a reference to an environment variable, and
export the token in your shell:

```jsonc
"apiKey": "{env:PANORAMA_TOKEN}"
```

```sh
export PANORAMA_TOKEN=pnt_...
```

That way the file can be shared or committed and the credential cannot.

The token lasts 30 days and nothing renews it. Mint a new one on My setup and
update the variable. See
[Set up your coding tool](/docs/engineers/setup/#your-token-expires-after-30-days).

## Web search

When your administrator has turned on web search, the block My setup prints
also has an `mcp` section, beside the providers:

```jsonc
"mcp": {
  "panorama": {
    "type": "remote",
    "url": "https://panorama.your-company.internal/mcp",
    "enabled": true,
    "headers": { "Authorization": "Bearer {env:PANORAMA_TOKEN}" }
  }
}
```

It gives opencode a `web_search` tool, served by Panorama. `opencode mcp list`
shows it as connected. It carries the same token as the providers, so the
same `{env:PANORAMA_TOKEN}` reference works for both.

## Switching routes

Use opencode's model list. Every route you may use is in it. You will need this
when a budget passes its soft limit and your route stops: the refusal names the
routes that still work. See
[When a request is refused](/docs/engineers/refusals/).

## How opencode compares to Claude Code here

**Better: it knows your route's real context window.** A route name is not a
model any tool has heard of. opencode is told the window outright in its
provider block. Claude Code has to be told through two extra environment
variables, and assumes 200k when they are missing.

**Worse: attribution stops at the repository.** opencode sends its working
directory and no branch, so Panorama can tie your work to a repository and not
to a ticket. Claude Code sends the branch. If ticket-level cost matters to your
team, that is the difference. See
[How attribution works](/docs/start/attribution/#what-each-tool-gives-you).

**Worse: no exact-attribution step.** opencode's headers are fixed strings in a
file, so a branch written into it would be wrong at your next checkout.
