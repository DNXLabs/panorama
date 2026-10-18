---
title: Credentials
description: >-
  The kinds of credential Panorama issues, what each can open, how long each
  lives, and how to create and revoke them.
sidebar:
  order: 4
---

Panorama issues several kinds of credential, and they are not interchangeable.
The prefix tells you which you are holding.

| Prefix | What it is | Who holds it | Opens |
| --- | --- | --- | --- |
| `pnt_` | Gateway token | An engineer's coding tool | Model requests, and the console API as that person. Signs in to the console until single sign-on is configured |
| `pnk_` | Service token | A CI pipeline | Model requests only, billed to a team |
| `pnb_` | Setup code | Whoever is setting the deployment up | One setup session, once |
| `pns_` | Console session | A browser | The console. A cookie you never handle. |
| `png_` | CLI grant | A laptop, from `panorama login` | Nothing in this release |

Every one is stored as a SHA-256 hash. None can be read back after it is shown.

## Gateway tokens

An engineer mints their own on **My setup**, with the **Mint** button. It is
shown once.

- **It lives 30 days, and that cannot be extended.** There is no expiry field
  to ask for longer. `auth.max_token_ttl` in the configuration can make it
  *shorter*, never longer.
- **Nothing renews it and nothing warns before it expires.** The engineer mints
  another and replaces it.
- **Revoking is permanent**, from the same screen. Mint a new one.
- **Nobody can mint one for somebody else**, administrators included.
  Recovering access is always "revoke and mint your own".

The lifetime is fixed because a pasted token is the credential Panorama can
protect least: it sits in a dotfile, it does not rotate, and nothing tells the
deployment when a laptop leaves the building.

The one exception to "mint your own" is bootstrapping. On the machine that
holds the database:

```sh
panorama token create -email you@example.com -role admin
```

This creates the account if it does not exist. It is how you get a first
credential on a deployment with no single sign-on.

### Sign in to the console with a gateway token

**Until single sign-on is configured**, the sign-in screen accepts a gateway
token. Paste it under **Gateway token** and press **Sign in**. You get an
ordinary console session for the token's owner, with their role, so an
administrator's token opens every screen. The token keeps working in coding
tools as before.

Once an identity provider is configured, that field is gone and everybody signs
in through the provider. Only a personal gateway token is accepted: a service
token, a command-line grant or a setup code is refused.

## Service tokens

For pipelines. A service token belongs to a **team**, not a person: it bills
that team's budget and keeps working after whoever created it has left.

There is no console screen for them yet. Use the API with an administrator's
gateway token:

```sh
# create
curl -s -X POST https://panorama.example.com/api/v1/service-tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"team": "platform", "name": "nightly-build", "expires_in_days": 90}'

# list
curl -s https://panorama.example.com/api/v1/service-tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN"

# revoke
curl -s -X DELETE https://panorama.example.com/api/v1/service-tokens/<id> \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

`team` is the team's slug. `name` says which pipeline this is. The token is in
the create response, once. `expires_in_days` is optional, because a pipeline's
lifetime is a decision for somebody who knows the pipeline.

The pipeline uses it exactly as an engineer uses a gateway token.

- **A service token cannot reach the console or its API at all**, by design.
- **A team cannot be deleted while a service token bills it.** Deleting the
  team would silently revoke every pipeline credential on it, at whatever hour
  the pipeline next runs. Revoke the tokens first.

## Setup codes

A setup code opens a **setup session**: one hour, no account, able to configure
single sign-on and the model providers and nothing else.

Panorama prints one to its log at startup while nobody can sign in. Issue
another at any time, which also cancels any earlier one:

```sh
panorama admin setup
```

There is no password anywhere in Panorama. The ability to run that command is
already proof of access to the deployment, so a password would be a second lock
on the same door.

## Command-line sign-in: built, and switched off

`panorama login` signs a machine in through the browser and leaves a
credential that rotates on its own, which is the better design. **This release
does not offer it.** The **Command-line sign-in** setting under **Settings →
Organization** is off and cannot be turned on, and the gateway refuses the flow
on the server side.

Until it is released, `login`, `logout`, `token`, `whoami` and `configure` in
the [CLI](/docs/reference/cli/) have nothing to work with.
