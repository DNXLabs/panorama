---
title: Set up your coding tool
description: >-
  For engineers whose company runs Panorama: sign in, mint a token on My setup,
  paste the configuration it prints, and check that the gateway is receiving
  you.
sidebar:
  order: 1
---

Your company runs Panorama, and your coding tool needs to talk to it instead of
to a model provider directly. This takes about two minutes, and the console
writes the configuration for you.

## 1. Open My setup

Sign in to your company's Panorama console with your usual single sign-on, and
open **My setup** in the sidebar. Your administrator may have sent you the link,
which ends in `/setup`.

## 2. Choose your routes and your tool

Under **Configuration**:

- **Everyday work**: the [route](/docs/start/concepts/#routing) your tool uses
  for what you ask it to do.
- **Background and subagents**: the route for requests your tool makes on its
  own, such as naming a session. Pick a cheaper route if you have one. This is
  not optional: your tool asks for it whether you set it or not.
- **Tool**: opencode, Claude Code or Codex CLI.

A route is a name your administrator made up. You never choose a model. If the
lists are empty, no route has been created that you may use: ask your
administrator.

## 3. Mint a token

Under **Gateway tokens**, press **Mint**. The token is shown **once**, so copy
it now. Treat it as a password.

## 4. Follow the instructions on the screen

The **Instructions** section is generated from the choices above, with your
routes already in it. Paste what it prints. Each tool is configured
differently:

| Tool | How it is configured |
| --- | --- |
| [Claude Code](/docs/engineers/claude-code/) | Environment variables, or `~/.claude/settings.json` |
| [opencode](/docs/engineers/opencode/) | A provider block in `~/.config/opencode/opencode.json` |
| [Codex CLI](/docs/engineers/codex/) | A provider entry in `~/.codex/config.toml` |

If your administrator has turned on [web search](/docs/configure/web-search/),
the instructions also add Panorama's MCP server, which gives your tool a
`web_search` tool. For Claude Code it is a separate **Add web search** step;
for opencode and Codex it is part of the file.

## 5. Check it worked

Start your tool and ask it anything. Within about ten seconds, the **Verify**
panel on My setup changes from `waiting` to `receiving`, and shows:

- **Latest request**: when it arrived, and the tool Panorama detected.
- **Route resolved**: the model your route became.
- **Provenance**: how your work was identified. `not yet` usually means you
  were not inside a git repository.

If it stays on `waiting`, or your tool shows an error, see
[When a request is refused](/docs/engineers/refusals/).

## Your token expires after 30 days

Nothing renews it and nothing warns you first. When your tool starts getting
*"This Panorama gateway token is not valid"*, come back to My setup, mint a
new one, and replace the old one.

If a token may have leaked, press **Revoke** next to it and mint another.
Revoking is permanent.

## What Panorama sees

Your prompts and your code pass through and are **never stored**. What is kept
is metadata: which route and model, how many tokens, what it cost, and the
repository, branch and ticket you were working on.

You see your own usage on **My usage**. Administrators, and the managers of
your teams, can see per-person spend. Other engineers cannot see yours.
