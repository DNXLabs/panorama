---
title: Identity provider guides
description: >-
  What to configure at Microsoft Entra ID, Okta, Keycloak, Amazon Cognito,
  Auth0, PingOne and Google Workspace so that sign-in works and group
  membership arrives, and why each provider behaves the way it does.
sidebar:
  order: 2
---

The console tells you **what** to do at your provider: choose it on the single
sign-on screen and it lists the steps, with your own redirect URI filled in.
This page explains **why**, which is what you need when a step does not behave
as described or your organization does something unusual.

Two facts hold for almost every provider, and between them they cause most
failed setups:

- **Group membership is not sent by default.** You have to turn it on, usually
  per application. Requesting a `groups` scope is rarely enough.
- **What is sent is often not the group's name.** It may be an ID, or a path.
  Panorama matches the string in the token, so that string is what you map.

Both failures are silent. Sign-in works, and everybody lands on the fallback
role. So after connecting any provider, sign in once and look at **Groups your
provider has sent** under **Settings → Single sign-on** before mapping
anything.

| Provider | Scopes | Groups claim | What the claim carries |
| --- | --- | --- | --- |
| [Microsoft Entra ID](#microsoft-entra-id) | `openid profile email` | `groups` | Object IDs, or display names with extra setup |
| [Okta](#okta) | `openid profile email groups` | `groups` | Group names |
| [Keycloak](#keycloak) | `openid profile email groups` | `groups` | Names, or full paths |
| [Amazon Cognito](#amazon-cognito) | `openid profile email` | `cognito:groups` | User pool group names |
| [Auth0](#auth0) | `openid profile email` | a namespaced URI you choose | Whatever your Action sets |
| [PingOne](#pingone) | `openid profile email` | whatever you name it | Depends on the attribute you map |
| [Google Workspace](#google-workspace) | `openid profile email` | none | Nothing: Google sends no groups |

## Microsoft Entra ID

Issuer: `https://login.microsoftonline.com/<tenant-id>/v2.0`

### Steps

1. **App registrations → New registration.** Add the redirect URI from the
   console as a **Web** redirect URI.
2. **Certificates & secrets → New client secret.** Copy the **Value**, not the
   Secret ID. It is shown once.
3. **Enterprise applications → your app → Users and groups.** Assign the groups
   you intend to map.
4. **App registration → Token configuration → Add groups claim.** Choose
   **Groups assigned to the application**, and tick **ID** under the token
   types.
5. For group *names* instead of IDs, edit the **Manifest** as described below.

### Why "Groups assigned to the application"

Entra offers four choices: security groups, directory roles, all groups, or
groups assigned to the application. The last one is right for Panorama, for
three reasons:

- **The 200-group limit.** Entra puts at most 200 groups in a token, nested
  ones included. A user in more than that gets **no groups claim at all**:
  Entra replaces it with a pointer to Microsoft Graph. Panorama does not follow
  it, on purpose, because that would mean holding a credential that can read
  your directory to answer a question the token was meant to answer. So in a
  large tenant, "all security groups" works in testing and then fails for your
  longest-serving staff, who silently drop to the fallback role. Restricting
  the claim to assigned groups keeps it to the handful you map.
  `panorama sso claims` recognises this case: it reports a `_claim_names` claim
  and says the groups were withheld, not misconfigured.
- **It is the only mode that can send names**, as the next section explains.
- **Least disclosure.** Panorama learns about the groups you chose and no
  others.

Two consequences to know before you rely on it:

- **Nested groups are not followed in this mode.** A user must be a *direct*
  member of an assigned group. If your admins are in `Platform` and `Platform`
  is inside the assigned group `Engineering`, they do not get `Engineering`.
- **Assigning groups to an application needs Entra ID P1 or P2.** On the free
  tier you can only assign individual users. Your options there are *Security
  groups* with object IDs, accepting the 200-group limit, or
  [direct administrators](/docs/configure/sso/#keep-an-administrator-who-does-not-depend-on-a-group)
  and a fallback role for everybody else.

### Why you get GUIDs, and how to get names

By default the claim carries each group's **object ID**, such as
`0f3c1e5a-...`. You can map those as they are, and there is a good argument for
doing so: an object ID never changes, and a display name does. Renaming a group
at Entra silently breaks a mapping made by name.

If you want names, the dialog misleads you. Its **Source attribute** options
(`sAMAccountName`, `NetBIOSDomain\sAMAccountName` and so on) are attributes of
groups **synchronized from on-premises Active Directory**. A group created in
Entra itself has none of them. Worse, Entra then *leaves cloud-only groups out
of the claim entirely*, so choosing one of those options makes groups
disappear.

For cloud-only groups, the display name is a separate property,
`cloud_displayname`, and it is set in the manifest. **App registration →
Manifest**:

```json
"groupMembershipClaims": "ApplicationGroup",
"optionalClaims": {
  "idToken": [
    {
      "name": "groups",
      "additionalProperties": ["cloud_displayname"]
    }
  ]
}
```

`cloud_displayname` works **only** with `ApplicationGroup`, which is what
"Groups assigned to the application" sets. Microsoft's reason is sound: a
display name is not unique, so if names were sent for every group, anybody able
to create a group could create one called `Panorama-Admins` and be granted
whatever that name maps to. Limiting names to groups an administrator assigned
to the app closes that.

In a hybrid tenant, list both, and synced groups arrive by `sAMAccountName`
while cloud groups arrive by display name:

```json
"additionalProperties": ["sam_account_name", "cloud_displayname"]
```

### It must be the ID token

Entra configures optional claims per token type. Panorama reads the **ID
token**, so the groups claim has to be under `idToken`. Configuring it only for
the access token is a common mistake, and produces a sign-in with no groups.

Microsoft's references:
[optional claims](https://learn.microsoft.com/entra/identity-platform/optional-claims#configure-groups-optional-claims)
and [group claims](https://learn.microsoft.com/entra/identity/hybrid/connect/how-to-connect-fed-group-claims).

## Okta

Issuer: `https://your-org.okta.com`, or a custom authorization server's issuer
such as `https://your-org.okta.com/oauth2/default`.

### Steps

1. Create an **OIDC web application** with the authorization code grant, and
   add the redirect URI as a sign-in redirect URI.
2. Assign the people or groups who should be able to sign in.
3. **Create the groups claim**, in the place that matches your issuer:
   - Issuer is `https://your-org.okta.com` (the org authorization server): on
     the application, **Sign On → OpenID Connect ID Token → Groups claim
     filter**. Name it `groups` and set a filter.
   - Issuer ends in `/oauth2/default` or another custom server: **Security →
     API → that authorization server → Claims → Add claim**. Name it `groups`,
     include it in the **ID token**, and set a filter.

### Why the scope is not enough

Panorama requests the `groups` scope, and at Okta that does nothing by itself.
A scope is permission to receive a claim, and the claim has to exist on the
**authorization server that issues the token**. Okta has more than one, and the
issuer URL you gave Panorama decides which one signs your tokens. A claim added
to the `default` server does nothing for tokens issued by the org server, and
the reverse.

The filter matters too. A claim whose filter matches none of the user's groups
is left out of the token. `Matches regex` with `.*` sends every group, which is
fine for a first test and more than you want afterwards.

Okta sends **group names**. That is readable, and it means renaming a group at
Okta stops the mapping matching.

[Okta's guide](https://developer.okta.com/docs/guides/customize-tokens-groups-claim/main/).

## Keycloak

Issuer: `https://keycloak.example.com/realms/your-realm`

### Steps

1. Create a **confidential client**: standard flow on, client authentication
   on, and the redirect URI under Valid redirect URIs.
2. In the client's dedicated scope, add a **Group Membership** mapper with the
   token claim name `groups`, added to the **ID token**.
3. Turn the mapper's **Full group path** off, unless you want paths.

### Why "Full group path" matters

Keycloak groups are a tree. With **Full group path** on, which is the default,
the claim carries `/engineering/platform`, leading slash included, and that
whole string is what you would have to map. With it off, the claim carries
`platform`.

Off is easier to read, and has a catch: two groups called `admins` under
different parents become indistinguishable. If your tree reuses names, keep
paths on and map the paths.

Older Keycloak versions put `/auth` before `/realms` in the issuer. If the
connection check reports that discovery failed, try the other form.

[Keycloak's mapper documentation](https://www.keycloak.org/docs/latest/server_admin/#_protocol-mappers).

## Amazon Cognito

Issuer: `https://cognito-idp.<region>.amazonaws.com/<user-pool-id>`

Set the groups claim to `cognito:groups`
(`PANORAMA_OIDC_GROUPS_CLAIM=cognito:groups`).

### Steps

1. Create an **app client with a client secret** and the authorization code
   grant, and add the redirect URI to its allowed callback URLs.
2. Create groups in the user pool and add users to them.

Groups created in the user pool arrive in `cognito:groups` by name, with
nothing else to configure.

### Why federated users have no groups

Cognito is often a broker in front of another provider. A user who signs in
through an upstream SAML or OIDC provider is a Cognito user, and their
*upstream* groups do not become Cognito groups. `cognito:groups` stays empty
for them.

You do not need a Lambda to fix that. Map the upstream groups attribute to a
custom attribute in the user pool, and point Panorama's groups claim at that
attribute's name. Panorama reads whichever claim you name.

This is also why Cognito does not rescue AWS IAM Identity Center: Identity
Center has no groups attribute to pass along in the first place.

[Cognito's app client settings](https://docs.aws.amazon.com/cognito/latest/developerguide/cognito-user-pools-app-idp-settings.html).

## Auth0

Issuer: `https://your-tenant.auth0.com/`, **with the trailing slash**.

### Steps

1. Create a **Regular Web Application** and add the redirect URI to Allowed
   Callback URLs.
2. Add a **Login Action** that sets a groups claim on the ID token, under a
   namespaced name:

   ```js
   exports.onExecutePostLogin = async (event, api) => {
     api.idToken.setCustomClaim('https://panorama/groups', event.authorization?.roles ?? []);
   };
   ```

3. Set Panorama's groups claim to exactly that name
   (`PANORAMA_OIDC_GROUPS_CLAIM=https://panorama/groups`).

### Why the claim cannot be called `groups`

Auth0 has no built-in groups claim, so you add one in an Action. It also keeps
a list of **restricted claim names** that an Action may not set, and both
`groups` and `roles` are on it. Setting one does not fail: in Auth0's words,
"the transaction won't fail, but your custom claim won't be added to your
tokens." So the obvious name produces a sign-in with no groups and no error.

A namespaced name avoids the list. Auth0's guidance is any HTTP or HTTPS URL
that is not an Auth0 domain, such as `https://panorama/groups`. It does not
have to resolve to anything.

What goes in the claim is up to your Action. The example sends Auth0 roles,
which is what most tenants use in place of groups.

### Why the trailing slash

Auth0's issuer, as its discovery document states it, ends in `/`. OIDC requires
the issuer you configure to match that string exactly, so without the slash the
connection check fails on an issuer mismatch.

[Auth0's custom claims](https://auth0.com/docs/secure/tokens/json-web-tokens/create-custom-claims).

## PingOne

Issuer: `https://auth.pingone.com/<environment-id>/as`

1. Create a **web application** using the authorization code grant, and add
   the redirect URI.
2. In the application's **attribute mapping**, add a mapping that emits group
   names into a claim, and set Panorama's groups claim to the name you gave it.

PingOne sends group membership only through a mapping you create, and whether
it carries names or identifiers depends on the attribute you map. Sign in once
and look at what arrived before mapping any roles.

[PingOne's attribute mapping](https://docs.pingidentity.com/pingone/applications/p1_edit_attribute_mapping.html).

## Google Workspace

Issuer: `https://accounts.google.com`

**Google does not put group membership in an OIDC token, and no setting changes
that.** Sign-in works. Everybody arrives with no groups and takes the fallback
role.

What you can do:

- Set **Someone in no mapped group** to *Becomes a developer*, and name your
  administrators under **Direct administrators**. This works well for a small
  team. Nobody can be made a manager this way, since administrator is the only
  role that can be assigned by hand.
- For roles driven by groups, put Workspace behind a provider that can send
  them, such as Keycloak or Okta federating to Google.

### Steps

1. In the Google Cloud console, create an **OAuth client ID** of type *Web
   application*, with the redirect URI the console shows.
2. Set the app's user type to **Internal**, so Google itself refuses anybody
   outside your Workspace. It is on the OAuth consent screen, which newer
   versions of the Google Cloud console call **Audience**.
3. Set your Workspace's primary domain in Panorama's configuration, beside the
   issuer and the client id:

   ```sh
   PANORAMA_OIDC_ALLOWED_DOMAINS=example.com
   ```

   **Panorama refuses to start on Google's issuer without it.** On the
   CloudFormation stack it is the `OidcAllowedDomains` parameter.

### Why Google needs a domain and nobody else does

Every other provider's issuer names your tenant: an Entra tenant id, an Okta
org, a Keycloak realm. Reaching it at all means being one of your people.
`https://accounts.google.com` is the same URL for every Google account on
earth, so a valid token proves somebody is who they say they are and nothing
about whether they work for you. And the first account to sign in to a new
deployment becomes its administrator.

Panorama reads the domain from the token's **`hd`** (hosted domain) claim, and
never from the email address. The address is not enough: anybody can open a
personal Google account on a work address, and that account outlives the
mailbox. Somebody who left last year can still hold a Google account that says
`them@example.com`, verified. `hd` is only present on an account your Workspace
manages, which is why Google's own guidance is to check it.

So with Google as the issuer:

- A personal account is refused, whatever its address.
- An account in another organization's Workspace is refused.
- An account on one of your secondary domains is accepted, because `hd` is the
  organization's primary domain. List the primary domain.

The Internal consent screen and the allowed domain do the same job at two ends.
Set both. The first stops a stranger at Google, and the second means a consent
screen somebody later switches to External does not open your deployment.

## Any other OIDC provider

Choose **Generic OIDC**. Panorama needs:

- an authorization code flow and a **confidential** client,
- a discovery document at `<issuer>/.well-known/openid-configuration`,
- an **ID token** carrying `sub`, `email`, and a claim listing groups.

If your provider names the groups claim something else, set
`PANORAMA_OIDC_GROUPS_CLAIM`. If it needs different scopes, set
`PANORAMA_OIDC_SCOPES`.

A provider with no preset is fully supported. Presets save you a sign-in or
two; they are not what makes it work. What makes it work is that Panorama shows
you the claim names and groups that actually arrived.

## AWS IAM Identity Center

It cannot be used, for two separate reasons:

1. **It is not an OIDC provider for your applications.** Its OAuth support is
   *trusted identity propagation*, which exchanges tokens another provider
   issued. Sign-in to a customer-managed application is SAML 2.0 only, and
   Panorama does not implement SAML yet.
2. **It cannot send groups.** The attributes it can put in an assertion are the
   user's name, email, subject and a few directory fields. There is no groups
   attribute, so even with SAML, or with Cognito in between, roles could not be
   mapped.

Identity Center always has an identity source behind it: Entra ID, Okta, Google
Workspace, or its own directory. Point Panorama at that source directly.
