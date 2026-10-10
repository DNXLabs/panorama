---
title: Deploying on a server
description: >-
  Docker Compose on a VM, in eight steps: configure, start, open a setup
  session, connect single sign-on, check Bedrock, create routes, onboard
  engineers, and connect GitHub.
sidebar:
  order: 3
---

Docker Compose on one VM, with SQLite built in. It suits a small team on one
machine, or a deployment outside AWS. On AWS, the
[CloudFormation template](/docs/deploy/aws/) is the recommended path: it brings
the load balancer, the database and two tasks, and the
[Quickstart](/docs/start/try-it/) follows it.

## Before you start

- A host with **Docker**, reachable by your engineers over HTTPS. See
  [Requirements](/docs/deploy/requirements/#network) for the proxy settings
  that matter.
- An **instance role** carrying [the Bedrock policy](/docs/deploy/requirements/#aws-credentials).
- An application registered at your **identity provider**, with a client id
  and a client secret. You will get the exact redirect URI to register in
  step 4.

After the first three steps, everything is done in the console. Where a step
also has a command, it is shown second, for scripts and for checking things
from a shell. Those commands run **inside the container**, because that is
where the database is:

```sh
alias pano='docker compose -f deploy/compose/docker-compose.yml exec -T panorama panorama'
```

## 1. Configure

```sh
git clone https://github.com/dnxlabs/panorama.git
cd panorama
cp deploy/compose/.env.example deploy/compose/.env
```

:::note
Compose reads `.env` **from the directory holding the compose file**, not from
where you run the command. A `.env` in the repository root is silently ignored.
:::

Set at least these two in `deploy/compose/.env`:

```sh
AWS_REGION=us-east-1
PANORAMA_SERVER_BASE_URL=https://panorama.your-company.internal
```

`PANORAMA_SERVER_BASE_URL` is the address people actually type. It is the
console's address, the OIDC redirect URI, and what engineers point a tool at.
It defaults to `http://localhost:8088`, which is wrong on a server.

`AWS_REGION` sets the Bedrock provider's region on the **first start only**.
After that you change it in the console.

Set nothing else for AWS. The instance role is found automatically.

## 2. Pick a database

SQLite is the default, needs nothing, and runs the whole product on one
container. Use Postgres when you want more than one replica. See
[Choosing a database](/docs/deploy/database/).

## 3. Start it

```sh
docker compose -f deploy/compose/docker-compose.yml up -d --build
```

The compose file builds from source, so the first run takes a few minutes. The
build fetches the latest release's table of prices for models the AWS Price
List API does not publish (GPT-5.x on Bedrock), so a source build prices the
same models a published image does. Set `PANORAMA_FOUND_PRICES` in `.env` to a
release tag to pin it, or to `none` to embed nothing, in which case those
models are counted as unpriced. Check it is ready:

```sh
curl https://panorama.your-company.internal/readyz
```

## 4. Connect single sign-on

A new deployment has no accounts, so it prints a one-time **setup code** to its
log:

```sh
docker compose -f deploy/compose/docker-compose.yml logs panorama | grep "one-time code"
```

Open the console and enter the code. That opens a **setup session**: one hour,
no account, and able to configure only single sign-on and the model providers.
Lost the code? `pano admin setup` issues a fresh one at any time.

In the setup session:

1. Choose your identity provider. The screen shows the **redirect URI** to
   register there, and what else that provider needs.
2. Enter the issuer URL and client id, and press **Check this connection**. It
   proves the issuer works and prints the configuration to apply. Nothing is
   saved.
3. Put that configuration in `deploy/compose/.env`, with the client secret:

   ```sh
   PANORAMA_OIDC_ISSUER=https://your-org.okta.com
   PANORAMA_OIDC_CLIENT_ID=...
   PANORAMA_OIDC_CLIENT_SECRET=...
   ```

4. Restart: `docker compose -f deploy/compose/docker-compose.yml up -d`.
5. Press **End session**, then **Continue with single sign-on**.

**The first person to sign in becomes an administrator.** Panorama does not own
identities, so there is no local admin user to create. Sign in yourself first.

Then map your groups to roles, which can only be done after that first
sign-in, because group names are only knowable from a real token. See
[Map groups to roles](/docs/configure/sso/#map-groups-to-roles).

Single sign-on is the step most likely to go wrong, and it usually goes wrong
silently: sign-in works and everybody has the wrong role. Three pages cover it:

- [Single sign-on](/docs/configure/sso/): how sign-in works, and the steps.
- [Identity provider guides](/docs/configure/sso-providers/): Entra ID, Okta,
  Keycloak, Cognito, Auth0, PingOne and Google Workspace, and why each behaves
  as it does.
- [SSO troubleshooting](/docs/configure/sso-troubleshooting/): by symptom.

When it is working, **Settings → Single sign-on → Verify** says so. From a
shell, `pano sso doctor` runs the same check and exits non-zero on a failure,
so it fits a deployment pipeline.

:::danger[AWS IAM Identity Center will not work]
It is the first thing people try. Identity Center only offers SAML sign-in to
third-party apps, and sends no groups. Point Panorama at the provider *behind*
it: Entra ID, Okta, your directory.
:::

## 5. Check Bedrock

Once signed in, an administrator sees a `setup · n steps left` chip that opens
a checklist of what remains. Its second step is the one that matters here.

Go to **Settings → Model providers**, and on the Amazon Bedrock block press
**Check**. It makes one real request for one token of output. **Verified**
means the role, the region and the network path all work.

The second block, **Amazon Bedrock (Mantle)**, reads `not configured` on a new
deployment. It is the only way to the open-weight and other non-Claude models,
has a region of its own, and is turned on from the same screen when you want
them. See
[Model providers](/docs/configure/providers/).

If it fails, see
[Troubleshooting](/docs/troubleshooting/#the-provider-check-fails).

## 6. Create routes

Every request is refused until a route exists. On **Routing**, press **New
route**, choose a model, name the route after it (`claude-sonnet-5` for Claude
Sonnet 5, so Claude Code prepares its requests for that model), and save. Most
teams want a second, cheaper route for background requests. See
[Routes](/docs/configure/routes/).

Then set a budget, so nothing runs away before anybody is watching. See
[Budgets](/docs/configure/budgets/).

## 7. Onboard engineers

Send engineers the link to **My setup**:

```
https://panorama.your-company.internal/setup
```

They sign in, mint their own token, pick their tool, and paste what the screen
prints. See [Set up your coding tool](/docs/engineers/setup/).

A token lives 30 days. Nothing renews it, so tell people to expect to mint a
new one monthly. Who has used the gateway, and who has not, is on **People &
teams**.

For CI, create a **service token**. It belongs to a team, bills that team's
budget, and outlives whoever created it. There is no screen for it yet, so use
the API with an administrator's token:

```sh
curl -s -X POST https://panorama.your-company.internal/api/v1/service-tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"team": "platform", "name": "nightly-build", "expires_in_days": 90}'
```

See [Credentials](/docs/reference/credentials/#service-tokens).

## 8. Connect GitHub (optional)

The GitHub integration is experimental: it has not yet been tested against a
real GitHub organization.

This is what turns "PAY-1183 cost $18.40" into "PAY-1183 cost $18.40, merged".
It needs an encryption key first, because the GitHub credential is encrypted
before it is stored. Add any passphrase of 16 characters or more to `.env` and
restart:

```sh
PANORAMA_ENCRYPTION_KEY=a-long-passphrase-you-will-not-lose
```

Then follow [GitHub integration](/docs/configure/integrations/).

## Check it worked

- **Settings → Single sign-on → Verify** reports no failures.
- **Settings → Model providers** shows `connected`.
- An engineer's **My setup** shows `receiving` after their first request.
- **Audit trail** lists that request with a repository and a branch.
