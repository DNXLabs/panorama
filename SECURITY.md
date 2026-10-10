# Security policy

Panorama holds credentials that can spend money (an AWS role that invokes
Bedrock, gateway tokens, an OIDC client secret, integration credentials), so a
vulnerability in it is worth reporting privately.

## Reporting a vulnerability

**Do not open a public issue.** Report it through GitHub's private vulnerability
reporting:
[Report a vulnerability](https://github.com/dnxlabs/panorama/security/advisories/new).
Only the maintainers can see the report.

Include what you can of:

- the version (`panorama version`, or the image tag or digest);
- how it is deployed (compose, the CloudFormation template, something else);
- the steps to reproduce, and what an attacker gains.

## In scope

- Authentication and authorization: gateway tokens, CLI grants, service tokens,
  console sessions, setup codes, OIDC sign-in, and role checks on the console
  API.
- Visibility: any way for a developer to see another developer's usage, or for
  anybody to read another organization's data.
- Prompt or response bodies being stored anywhere. Panorama's commitment is that
  they never are.
- The proxy: request smuggling, header passthrough, SigV4 signing sent somewhere
  it should not be.
- Stored credentials: integration secrets are sealed with the deployment's
  encryption key; anything that recovers one without it.
- The CloudFormation template granting more than it documents.

## Out of scope

- A deployment configured against the documentation, for example the
  development sign-in (`PANORAMA_DEV_SIGN_IN_AS`) enabled on a server reached
  through a tunnel.
- Denial of service by volume against a deployment with no rate limiting in
  front of it.
- Vulnerabilities in Amazon Bedrock, an identity provider or a coding tool
  themselves; report those to their vendors.
