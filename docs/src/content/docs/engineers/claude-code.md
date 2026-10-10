---
title: Claude Code
description: >-
  The variables that point Claude Code at Panorama, what each one does, why
  there are two model variables, why a route for Claude Code is named after its
  model, and how to get exact attribution.
sidebar:
  order: 2
---

**My setup** in the console prints all of this with your own routes and token
in it. See [Set up your coding tool](/docs/engineers/setup/). This page
explains what it prints.

## The variables

```sh
export ANTHROPIC_BASE_URL=https://panorama.your-company.internal
export ANTHROPIC_AUTH_TOKEN=pnt_...
export ANTHROPIC_MODEL=claude-sonnet-5
export ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-4-5-20251001

claude
```

Put them in your shell profile, or use the config-file form My setup offers,
which writes the same values under `env` in `~/.claude/settings.json` and
applies to every Claude Code run on the machine.

| Variable | What it is |
| --- | --- |
| `ANTHROPIC_BASE_URL` | Your Panorama address, with no `/v1` on the end. |
| `ANTHROPIC_AUTH_TOKEN` | The token you minted. |
| `ANTHROPIC_MODEL` | The **route** for everyday work. A route name, never a model id. |
| `ANTHROPIC_DEFAULT_HAIKU_MODEL` | The route for Claude Code's own background requests and subagents. |

## Why there are two model variables

Claude Code makes requests you did not ask for, such as summarising a
conversation or naming a session, on a small fast model it picks with
`ANTHROPIC_DEFAULT_HAIKU_MODEL`.

Leave that unset and those requests ask for a model id. A model id is not a
route, so they are refused, and it looks like a broken tool when it is a
missing variable.

There is no third variable to set. The Opus and Sonnet tier variables are only
consulted when you pick that tier in `/model`, and pointing them at a route
would make "Opus" quietly serve something else.

## Why `ANTHROPIC_MODEL` and not `ANTHROPIC_DEFAULT_MODEL`

`ANTHROPIC_DEFAULT_MODEL` applies only when no settings file sets `model`, and
`/model` saves that key. If you have ever picked a model in Claude Code, you
have one, and `ANTHROPIC_DEFAULT_MODEL` would appear to do nothing.

`ANTHROPIC_MODEL` always wins. `/model` still switches routes within a session.

## Name a route after its model

**A route for Claude Code carries the model's own name**: `claude-sonnet-5`
for Claude Sonnet 5, `claude-haiku-4-5-20251001` for Claude Haiku 4.5. The
route editor offers that name when a route's tool condition includes
`claude-code`, with **Use this name**.

Claude Code prepares each request for the model it thinks it is talking to.
Given a name it recognises, it knows that model's context window and output
limit, and sends only what that model accepts. Given a name it does not
recognise, such as `fast`, it assumes the newest model there is, and sends
adaptive thinking, an effort level and a system message in the middle of the
conversation. An older model refuses the whole request. What you see is *"The
model provider rejected this request."*, and your administrator sees the
reason in Panorama's log, for example `role 'system' is not supported on this
model`.

Measured on a real deployment: Claude Haiku 4.5 behind a route called `fast`
refused every subagent request, and behind `claude-haiku-4-5-20251001` served
all of them.

Two things fail on Bedrock whatever the route is called:

- **Structured output** on Claude Sonnet 5 and Claude Opus 5
  (`output_config.format: Extra inputs are not permitted`).
- **The built-in WebSearch tool**, which is Anthropic's server-side tool
  (`tool type 'web_search_20250305' is not supported`). Bedrock does not
  offer it. Use Panorama's instead: see [Web search](#web-search).

opencode and Codex CLI declare what a route's model is in their own
configuration, and a route for them can have any name.

## Web search

When your administrator has turned on web search, My setup shows an **Add web
search** step. It has two parts:

1. A command that registers Panorama's MCP server for every project:

   ```sh
   claude mcp add --transport http --scope user panorama https://panorama.example.com/mcp \
     --header "Authorization: Bearer <your token>"
   ```

2. A deny rule for Claude Code's own WebSearch tool, in
   `~/.claude/settings.json`:

   ```json
   { "permissions": { "deny": ["WebSearch"] } }
   ```

   Without it, Claude reaches for the built-in tool first and gets an error.

Claude then has a `web_search` tool from the `panorama` server. `/mcp` inside
Claude Code shows it as connected. The server carries your token, so when you
mint a new one, run the command again with it (`claude mcp remove panorama`
first).

## Switching routes

Use `/model` and pick another route. Every route you may use is already listed,
because Panorama answers Claude Code's model list with your routes.

You will need this when a budget passes its soft limit and your route stops.
The refusal names the routes that still work. See
[When a request is refused](/docs/engineers/refusals/).

## Optional: exact attribution

Claude Code already tells Panorama your branch, so your work is attributed to
the right ticket with nothing configured. To make that exact instead of
inferred, My setup offers a third step, **Stamp the precision tier**:

```sh
export ANTHROPIC_CUSTOM_HEADERS="x-panorama-repo: $(git remote get-url origin 2>/dev/null | sed -E 's#.*[:/]([^/]+/[^/]+?)(\.git)?$#\1#'), x-panorama-branch: $(git rev-parse --abbrev-ref HEAD 2>/dev/null)"
```

It reads your checkout when the shell starts, so run it from the repository you
are working in. See [How attribution works](/docs/start/attribution/).

## Your token

It lasts 30 days and nothing renews it. Mint a new one on My setup and replace
`ANTHROPIC_AUTH_TOKEN`. See
[Set up your coding tool](/docs/engineers/setup/#your-token-expires-after-30-days).
