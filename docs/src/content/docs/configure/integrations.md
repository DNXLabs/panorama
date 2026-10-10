---
title: GitHub integration
description: >-
  Connect GitHub so that attributed work gains titles, authors and merge
  status. Creating the GitHub App, the permissions it needs, connecting it, and
  what each connection state means.
sidebar:
  order: 6
---

:::caution[Experimental]
The GitHub integration is built and has not yet been tested against a real
GitHub organization. Expect rough edges, and check what it reports against
GitHub before you rely on cost per merged pull request.
:::

Attribution works without GitHub. What connecting it adds is **pull request
titles, authors, and whether the work merged**, and with those, cost per merged
pull request.

Panorama asks GitHub only about branches and pull requests it has **already
seen in your engineers' traffic**. It never lists your repositories, so it
needs read access only to what your engineers touch, and the number of API
calls follows the number of conversations, not the size of your organization.

## Before you start

**Set an encryption key.** The GitHub credential is encrypted before it is
stored, and the key stays in your configuration, so a database dump or backup
carries nothing usable. Any passphrase of 16 characters or more works:

```sh
PANORAMA_ENCRYPTION_KEY=a-long-passphrase-you-will-not-lose
```

Or mint a full-strength key with `panorama admin generate-key`. Restart after
setting it. The CloudFormation template generates one for you.

Without a key, the GitHub form is replaced by a message saying so.

:::caution[Keep the key]
Losing or changing it makes the stored credential unreadable. Nothing is
recoverable and nothing else breaks: you enter the credential again.
:::

## Create a GitHub App

A GitHub App is the right credential for a real deployment. It is installed on
the organization with a stated scope, so its access does not belong to whoever
set it up.

1. In your GitHub organization: **Settings → Developer settings → GitHub Apps →
   New GitHub App**.
2. Give it a name and any homepage URL. **Untick Webhook → Active.** Panorama
   polls; it receives nothing.
3. Under **Repository permissions**, grant read-only access to:
   - **Pull requests**
   - **Contents**
   - **Metadata** (GitHub selects this for you)

   Nothing else, and no organization or account permissions.
4. Create the app. Note its **App ID**.
5. **Generate a private key** and download the `.pem` file.
6. **Install the app** on your organization, on all repositories or on the ones
   your engineers work in.

## Connect it

In **Settings → Integrations**, on the GitHub band:

1. Leave the credential shape on **GitHub App**.
2. Enter the **App ID**.
3. Leave **Installation ID** empty if the app is installed in one place. It is
   found automatically. If it is installed in several, the error lists them
   with their IDs.
4. Paste the whole contents of the `.pem` file into **Private key**.
5. For GitHub Enterprise Server only, set **API base URL**.
6. Press **Connect**.

The credential is write-only. No screen and no API ever shows it again, which
is why there is no masked value to reveal. To replace it, paste a new one.

### Trying it with a personal access token

Choose **Token** and paste a token with read access to the repositories. This
is fine for an evaluation and wrong for a deployment: the token belongs to a
person, every read is recorded as theirs, and it stops working when they leave.
The console labels a token-backed connection as one.

## Check it worked

The band shows one of four states:

| State | Meaning |
| --- | --- |
| `not connected` | No credential is stored. |
| `never run` | Connected, and the first check has not happened yet. |
| `connected` | The last check succeeded. |
| `failing` | The last check failed. The error is shown, and is kept after it clears, because an intermittent failure is the one worth chasing. |

Checks run every 15 minutes (`integrations.sync_interval`). Press **Check now**
to run one immediately. **Last run** reads like *"42 confirmed of 45 asked"*.

A failing connection also appears on **Analytics → Attention**.

## What goes wrong

**Some work items stay unconfirmed.** That is normal. A branch nobody pushed is
not on GitHub, and Panorama records it as absent, not as an error. The
attribution itself is unaffected.

**`failing`, with a rate limit message.** GitHub's API budget for the app is
used up. The message says when it resets. While a source is unreachable,
Panorama confirms nothing at all, so that one broken API call cannot mark every
work item in the deployment as "not found".

**"this app is not installed anywhere".** Step 6 was missed.

**"the private key is not PEM".** Paste the file's full contents, including the
`-----BEGIN` and `-----END` lines.

**It was `connected` and is now `failing` after a redeploy.** The encryption
key changed. Enter the credential again.

## Good to know

- **Merged means merged.** Panorama reads a pull request's `merged_at`, never
  its `state`, because a merged pull request reports `closed`.
- **Confirmation never changes attribution.** It adds a relation: this branch
  was opened as that pull request.
- **Jira is not in v1.** It appears on the screen as *coming soon*. Naming it
  as an integration in the configuration file is refused at startup.
