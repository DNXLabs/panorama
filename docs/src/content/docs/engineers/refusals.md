---
title: When a request is refused
description: >-
  Every message Panorama sends when it will not serve a request, what each one
  means, and what to do about it. Find yours by its first words.
sidebar:
  order: 5
---

Panorama's refusals are written to be read. Each says what happened and what to
do. Find yours below by how it starts. Every error also carries a
`request_id`: quote it when you ask an administrator for help.

## "No valid Panorama gateway token was supplied"

**401.** Your tool sent no token, or something that is not one.

- Claude Code: `ANTHROPIC_AUTH_TOKEN` is unset in the shell that started it.
- opencode and Codex: `PANORAMA_TOKEN` is unset, or the config file still holds
  the `pnt_YOUR_TOKEN` placeholder.

A gateway token starts with `pnt_`. See
[Set up your coding tool](/docs/engineers/setup/).

## "This Panorama gateway token is not valid"

**401.** The token was real once. It has **expired** (they last 30 days), been
**revoked**, or your account has been deactivated. The message does not say
which, on purpose. Mint a new one on **My setup**.

## "Panorama does not serve ..."

**403.** You asked for a name that is not a route you can use. The message
lists the routes you *can* use:

```
Panorama does not serve "claude-sonnet-5".

Routes you can use: fast, standard.

Set the route name as your model - ANTHROPIC_MODEL for Claude Code.
```

The usual causes:

- **You set a model id.** Set a route name. You never name a model here.
- **Claude Code's background model is unset.** If the refused name is a Haiku
  model id you never typed, set `ANTHROPIC_DEFAULT_HAIKU_MODEL` to a route. See
  [Claude Code](/docs/engineers/claude-code/#why-there-are-two-model-variables).
- **The route exists and is not for you.** Routes can be limited to people,
  teams or tools, and one you may not use looks exactly like one that does not
  exist.

If the message says *"There is no route you can use on this deployment yet"*,
an administrator needs to create one.

## "Panorama is not serving ...: the ... budget is past its soft limit"

**403.** A budget that covers you has crossed its warning line, and the route
you are on is one your administrator marked as stopping there. The message says
how much is used, when it resets, and:

```
Routes still served: fast, standard.
```

**Switch to one of those and carry on**: `/model` in Claude Code, or the model
list in opencode. Your tool already knows every route you may use.

If it says *"No other route on this deployment is served past this limit"*,
there is nothing to switch to until the budget resets or an administrator
raises it.

## "Panorama stopped this request: the ... budget is used up"

**403.** A hard limit. Nothing is served until the time the message gives, or
until an administrator raises the limit. Switching routes does not help.

The budget named may be yours, your team's or the whole organization's.

## "The route ... is not served over the ... API"

**400.** Your tool is talking to an endpoint the route's model does not speak.
The message names the right one. In practice: Codex was pointed at a Claude
route, or Claude Code at an open-weight one. Pick a route My setup offers for
your tool.

## "The model provider rejected this request."

**400.** Amazon Bedrock refused the request itself, before running it. Panorama
does not pass on Bedrock's reason, which is in the gateway's log and on the
**Audit trail** as `upstream returned 400`.

With **Claude Code**, the usual cause is a route whose name is not its model's
name, so Claude Code sent what only its newest model accepts. Switch to another
route with `/model`, and tell your administrator which route failed. See
[Name a route after its model](/docs/engineers/claude-code/#name-a-route-after-its-model).

With a script of your own, the request named a parameter that model does not
have.

## "The model is rate limited right now. Retry shortly."

**429.** Amazon Bedrock throttled the request. Your tool retries these on its
own. If it keeps happening, tell your administrator: the account's Bedrock
quota for that model is too low, or another route would spread the load.

## "The model is temporarily unavailable. Retry shortly."

**503.** Bedrock is having trouble with that model. Retry, or switch routes.

## "Panorama could not serve ...: ... That is a configuration error on this deployment"

**502.** Not your fault, and nothing on your machine will fix it. The route
points at a model or a provider that is not available. Send the `request_id` to
an administrator.

## "Bedrock refused this model for this Panorama deployment's AWS account"

**502.** Amazon Bedrock would not serve the route's model to the AWS account
this deployment runs in. Nothing on your machine will fix it, and switching
routes helps only if another route's model is not refused too. Send the
`request_id` to an administrator: they can see Bedrock's reason under
**Settings → Model providers**. See
[Engineers report "Bedrock refused this model"](/docs/troubleshooting/#engineers-report-bedrock-refused-this-model).

## "Panorama does not store responses"

**400**, Codex and other Responses API clients only. See
[what the gateway refuses](/docs/engineers/codex/#what-the-gateway-refuses).

## "The request body exceeds Panorama's 32 MiB limit"

**413.** Usually a very large file or image pasted into the conversation. Start
a new session, or compact the current one.

## My setup still says `waiting`

Nothing from you has reached the gateway at all, so there is no refusal to
read. Check that your tool is actually using the address on My setup: for
Claude Code, `echo $ANTHROPIC_BASE_URL` in the same shell. A refused request
still counts as received, so `waiting` always means a connection problem, not a
permission one.
