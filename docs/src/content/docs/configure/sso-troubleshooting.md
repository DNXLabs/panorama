---
title: SSO troubleshooting
description: >-
  Single sign-on problems by symptom: everybody has the wrong role, one person
  has the wrong role, the sign-in page shows an error, it worked and then
  stopped, and what each Verify finding means.
sidebar:
  order: 3
---

Most single sign-on problems produce no error. Sign-in succeeds and the role is
wrong. So start with the two tools that show what actually happened, then find
your symptom.

## Two tools to start with

**Verify**, on **Settings → Single sign-on**, checks the connection against
what Panorama has seen: the redirect URI, the secret, whether anybody has
signed in, whether groups arrived, and whether your mapped groups have ever
been seen. From a shell it is `panorama sso doctor`, which exits non-zero on a
failure.

**Groups your provider has sent**, on the same screen, lists the exact group
strings that arrived in tokens. From a shell, `panorama sso claims` lists those
and also the **names of every claim** recent tokens carried, which is how you
tell a claim that is called something else from a claim that is missing.

Neither records a claim's value, or who is in which group.

## Everybody who signs in gets the fallback role

The connection works and the tokens carry no groups. It looks like complete
success. Verify reports it as **No sign-in has ever carried a "groups" claim**.

There are two opposite causes, and the fix for one does nothing for the other.
Run `panorama sso claims` and look at the claim names:

- **A differently named claim is there**, such as `cognito:groups`, `roles`, or
  a URL. Your provider sends groups under another name. Set
  `PANORAMA_OIDC_GROUPS_CLAIM` to that name and restart.
- **Nothing resembling groups is there.** Your provider is not sending them.
  Turn the claim on at the provider, for this application, in the **ID token**.
  See your provider under
  [Identity provider guides](/docs/configure/sso-providers/).

The usual specific causes:

| Provider | Usual cause |
| --- | --- |
| Entra ID | The groups claim was added for the access token and not the **ID** token. Or a **Source attribute** was chosen that cloud-only groups do not have, which removes them from the claim. |
| Okta | The claim was created on a different authorization server from the one in your issuer URL. Or its filter matches nothing. |
| Auth0 | The Action sets a claim called `groups` or `roles`, which Auth0 drops without an error. Use a namespaced name. |
| Keycloak | The Group Membership mapper is missing, or is not added to the ID token. |
| Cognito | The users are federated from another provider, so they have no user pool groups. |
| Google Workspace | Google never sends groups. This is expected. |

## One person gets the wrong role, and everybody else is fine

- **They are in more than 200 groups (Entra ID).** Entra then sends no groups
  at all for that person. `panorama sso claims` shows a `_claim_names` claim
  and says the groups were withheld. Switch the groups claim to **Groups
  assigned to the application**. See
  [why](/docs/configure/sso-providers/#why-groups-assigned-to-the-application).
- **They are in the group through a nested group (Entra ID).** With "Groups
  assigned to the application", only direct membership counts.
- **They are in no mapped group.** They get whatever **Someone in no mapped
  group** is set to. Verify lists this as **Some sign-ins carried no groups**,
  which is normal for people who really are in none.
- **They are a direct administrator.** A pinned administrator keeps the role
  whatever their groups say. Look under **Direct administrators**.
- **They have not signed in since you changed the mapping.** Roles change at
  the next sign-in, not when you save.

## A mapping that should match does not

Verify reports **Mapped groups nobody has been seen in**. The string in your
mapping is not a string any token carried:

- **You mapped a display name and the provider sends an ID** (Entra ID by
  default), or a **path** (Keycloak sends `/engineering/platform` with "Full
  group path" on).
- **A typo.** Click a group under **Groups your provider has sent** instead of
  typing it.
- **Nobody in that group has signed in yet.** That is fine, and it resolves
  itself.

Matching ignores case, so capitalisation is never the cause.

## It worked, and then it stopped

- **A group was renamed at the provider.** A mapping by name stops matching,
  silently. Map IDs where your provider offers them, since they do not change.
- **The client secret expired.** Entra ID secrets expire after at most two
  years. People see *"Panorama could not verify the response from your identity
  provider"*, and the log shows `oidc token exchange failed` with
  `AADSTS7000222`. Create a new secret, apply it, and restart.
- **Somebody changed the configuration and did not restart.** The connection is
  read once, at startup. On AWS that means
  `aws ecs update-service --region <region> --cluster panorama --service panorama --force-new-deployment`.
- **The console's address changed.** The redirect URI is built from
  `server.base_url`, so a new address is a new redirect URI, and it has to be
  registered at the provider.

## The sign-in page shows an error

These are the messages a person can see, and what each means for you.

| Message | Cause | Look at |
| --- | --- | --- |
| *Panorama could not reach the identity provider. An administrator can check the issuer URL.* | Discovery failed: the issuer URL is wrong, or the container cannot reach it. | The log line `oidc discovery`. From inside the container, the issuer followed by `/.well-known/openid-configuration` must load. |
| *Your identity provider refused the sign-in.* | The provider sent an error back: the person is not assigned to the application, or consent was denied. | The log line `identity provider refused a sign-in`, and your provider's own sign-in log. |
| *That sign-in did not start here. Try again from the Panorama console.* | The browser came back without the cookie that started the flow: a bookmarked provider URL, two tabs, or `base_url` on a different host from the one the browser used. | Whether people reach the console at exactly `server.base_url`. |
| *That sign-in has expired or was already used. Try again.* | The flow was completed twice, or took too long. | Usually nothing. Try again. |
| *Panorama could not verify the response from your identity provider.* | Exchanging the code failed: a wrong or expired **client secret**, or a wrong client id. | The log line `oidc token exchange failed`, which carries the provider's own error. |
| *Your account is not in a group that may use Panorama. Ask an administrator.* | The fallback is **Cannot sign in**, and this person is in no mapped group. | Their groups at the provider, and the mapping. |
| *Your account is not in a domain that may use this Panorama deployment. Sign in with your work account.* | `PANORAMA_OIDC_ALLOWED_DOMAINS` is set and this account is outside it. At Google, the person chose a personal account in the account picker, or the list names a secondary domain where it should name the Workspace's primary one. Elsewhere, their address is on another domain, or the provider marks it unverified. | The log line `sign-in refused`, whose `reason` names the domain that was refused. |
| *Your identity provider did not send the claims Panorama needs. An administrator can check the requested scopes.* | The ID token lacks something required, most often `email`. | That the `email` scope is requested, and that the account has an email address at the provider. |

If the error appears **on your provider's page**, before the browser returns
to Panorama, it is nearly always the redirect URI. At Entra ID it is
`AADSTS50011`. Register the URI exactly as the console shows it: the scheme,
the host, the port and the absence of a trailing slash all count.

## What Verify and `sso doctor` report

| Finding | Level | Meaning |
| --- | --- | --- |
| Single sign-on is not configured | warn | No issuer is set. |
| No client secret | fail | Set `PANORAMA_OIDC_CLIENT_SECRET`, or point `oidc.client_secret_file` at a mounted file, and restart. |
| `server.base_url` is not a URL | fail | The redirect URI cannot be built. |
| Redirect URI points at localhost | warn | Right for a laptop. For anybody else, the redirect happens in *their* browser, so `base_url` must be the address they use. |
| Redirect URI is not https | fail | Serve the console over TLS. Providers refuse plain `http` except for localhost. |
| Redirect URI is well-formed | ok | It still has to be registered at the provider, exactly as written. |
| Nobody has signed in through SSO yet | warn | Nothing about groups can be known until somebody does. |
| The round trip has been completed | ok | At least one person has signed in through the provider. |
| No sign-in has ever carried a "groups" claim | fail | [Everybody gets the fallback role](#everybody-who-signs-in-gets-the-fallback-role). |
| Some sign-ins carried no groups | warn | Usually people who are in no group, which is fine. |
| Group membership is arriving | ok | |
| Nobody can sign in | fail | The fallback is *Cannot sign in* and no group is mapped. Map a group to admin, or set a fallback role. |
| Mapped groups nobody has been seen in | warn | [The mapping does not match](#a-mapping-that-should-match-does-not). |
| Groups seen but not mapped | ok | Their members take the fallback role. Map any that should carry more. |

## What "Check this connection" reports

The check on the single sign-on screen, `panorama sso probe` from a shell,
reads your provider's discovery document. It needs no secret and changes
nothing.

| Finding | Meaning |
| --- | --- |
| Discovery failed | The issuer did not return a discovery document. Check the URL, and that the container can reach it. |
| Issuer includes the discovery path | Give the issuer only. Panorama adds `/.well-known/openid-configuration` itself. |
| Issuer is not https | Providers publish discovery over TLS only. |
| Provider does not advertise every requested scope | Often harmless: many providers support `groups` without advertising it. If sign-in then fails, remove the scope with `PANORAMA_OIDC_SCOPES`. |
| Cannot confirm the groups claim from discovery | Expected. Few providers list their claims, and none list a custom one. Only a real sign-in can confirm it. |

A passing check proves the issuer is real and reachable. It cannot prove the
client secret, the redirect URI registration, or that groups will arrive. Those
are only proven by signing in.

## The wrong person became the first administrator

The first person to sign in owns the deployment. If that was not who you
intended:

```sh
panorama admin setup
```

Enter the code it prints at the console. Under **Administrators**, press **Make
admin** on the right account and **Remove admin** on the other.

## Nobody can sign in at all

The same command is the way back in. A setup session can repair the connection,
fix the group mapping and restore an administrator, and it needs no working
sign-in. See [Credentials](/docs/reference/credentials/#setup-codes).

## Reading the log

Every sign-in failure is logged with the reason, even when the person sees a
generic message. Search for these:

| Log message | What it tells you |
| --- | --- |
| `oidc discovery` | Why the issuer could not be read. |
| `identity provider refused a sign-in` | The provider's own error code and description. |
| `oidc callback rejected` | With `reason`: the state did not match the browser, or the flow was unknown, used or expired. |
| `oidc token exchange failed` | The provider's error from the code exchange, such as an invalid client secret. |
| `sign-in refused` | With `reason`: no mapped group, the missing claim, or the domain that is not in `oidc.allowed_domains`. |
| `FIRST SIGN-IN - this account owns the deployment` | Who became the first administrator, and when. |
