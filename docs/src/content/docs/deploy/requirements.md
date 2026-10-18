---
title: Requirements
description: >-
  What a Panorama deployment needs, which of the two deployment paths fits you,
  and the AWS permissions the gateway runs with.
sidebar:
  order: 1
---

## What you need

- **An AWS account with Amazon Bedrock**, and a region to invoke it in.
- **An OIDC identity provider** for the console: Entra ID, Okta, Keycloak,
  Cognito, Auth0, PingOne or Google Workspace. AWS IAM Identity Center will not
  work. See [Single sign-on](/docs/configure/sso/).
- **Somewhere to run it**: your AWS account, which is the recommended path, or
  any host that runs one container.

## Pick a path

**Deploy on AWS unless you have a reason not to.** One CloudFormation template
creates the whole deployment, with the load balancer, the database and the
streaming settings already right. The [Quickstart](/docs/start/try-it/) follows
it from start to finish.

| | [On AWS](/docs/deploy/aws/) (recommended) | [On a server](/docs/deploy/server/) |
| --- | --- | --- |
| **What it is** | One CloudFormation template | Docker Compose on a VM you run |
| **Runs on** | Fargate behind an Application Load Balancer | Any host with Docker, EC2 being the usual one |
| **Database** | Aurora Serverless v2 | SQLite, or Postgres beside it |
| **Replicas** | Two by default | One |
| **TLS and load balancing** | Included | Yours to put in front |
| **Image** | Pulled from `public.ecr.aws/dnxsolutions/panorama`, or a mirror in your account | Built from source on the host |
| **Good for** | A team on AWS | A small team on one machine, or anywhere outside AWS |

Both run the same binary and the same schema, so moving from one to the other
later is a deployment change and not a migration project.

[On your laptop](/docs/deploy/laptop/) runs it with Docker Compose and your own
credentials, for development or a look at the console. Nobody else can use it.

## AWS credentials

**Panorama never asks you to paste an AWS access key.** It uses the AWS SDK's
default credential chain, so the intended setup is an EC2 instance role or an
ECS task role.

The role needs one policy. The console shows it, ready to copy, under
**Settings → Model providers → Role policy → Show the policy**, and the
CloudFormation template's task role already carries it. Before you have a
console to look at, the binary prints the same JSON:

```sh
panorama providers policy
```

A test fails the build if the three ever disagree.

**The policy grants both Bedrock endpoints in every region, on purpose.** There
are no condition keys and no region in any ARN, so an administrator changing a
provider's region in the console never needs somebody with IAM access. If your
organization requires region-scoped IAM, scope it yourself and expect a region
change to become an IAM change.

The same policy lets the deployment call any AgentCore gateway, which is how
[web search](/docs/configure/web-search/) is reached. It cannot search through
a gateway on its own: each gateway's service role decides that.

## Network

The container needs outbound HTTPS to:

- **Amazon Bedrock**, for both invoking models and listing them.
- **The AWS Pricing API**, to price requests at current rates. Without it,
  Panorama runs on the rates compiled into the binary, which go stale.
- **Your identity provider**, to sign people in.
- **GitHub**, only if you connect it.
- **Amazon Bedrock AgentCore**, only if you turn on web search.

Engineers' machines need to reach the container, and nothing else.

Put a load balancer or reverse proxy with TLS in front of it. Two settings
matter for a streaming gateway:

- **Idle timeout of several minutes.** A model can think for longer than the
  usual 60 seconds between bytes, and a short timeout cuts the stream mid-turn.
  The CloudFormation template uses 600 seconds.
- **Health check on `/readyz`**, which returns 200 only when the database is
  reachable and the schema is current.

`/healthz` and `/readyz` are the only unauthenticated routes, and they expose
no organization data.

## Model access

There is no model access to request. Bedrock enables every foundation model by
default and subscribes the account through AWS Marketplace on the first
request, which can take a few minutes to activate. Two things can still stand
in the way, and neither shows up until a request is made:

- **Anthropic's use case form.** Before any Anthropic model answers, each AWS
  account submits Anthropic's use case details once, in the Amazon Bedrock
  console. Until then every Claude request is refused with *"Model use case
  details have not been submitted for this account"*. It takes up to fifteen
  minutes to apply after you submit it.
- **What AWS and your organization allow.** A newly released model may not be
  offered to every account yet (*"is not available for this account"*), and a
  service control policy may deny `bedrock:InvokeModel`.

That is why setup includes a real one-token request: it is the only check that
tells a working account from one that looks right. See
[Troubleshooting](/docs/troubleshooting/#the-provider-check-fails).
