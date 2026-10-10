# Editions

Everything in this repository is open source, under the GNU Affero General Public
License v3.0 only (see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE)). There is no
enterprise-licensed code here, and no commercial terms are in force.

An enterprise edition is **planned**, as a separately licensed set of
capabilities distributed by DNX Technology Pty Ltd under commercial terms. This
file says what it is expected to cover and, more importantly, what it will never
take away from the open-source edition.

## Intended scope

- Multi-account and multi-organization management
- Central analytics: many gateways in separate accounts reporting to one
  dashboard, hosted by DNX or run by the customer
- Audit log export for compliance retention
- Billed-spend precision via AWS Cost and Usage Report ingestion
- Curated policy packs

Nothing above is a commitment; the list describes intent at the time of writing.

## Boundary commitment

The open-source edition is complete for a single organization: full gateway, full
policy engine, full analytics, full attribution, and full single sign-on.
Enterprise capabilities are additive. No capability present in the open-source
edition will be moved behind the commercial license.

Analytics for a single deployment are open source and stay open source. The
enterprise capability is aggregation, not analysis: many gateways, separate
accounts, one dashboard. A deployment that runs its own gateway and reads its own
numbers needs nothing from the enterprise edition, and separating the gateway from
the analytics engine to make aggregation possible does not move anything across
this line.

Single sign-on is named on its own because it is the capability open-core products
gate most often, and because gating any part of it gates all of it. SAML is not an
enterprise feature. For an identity provider whose sign-in for customer managed
applications is SAML 2.0 only - AWS IAM Identity Center among them - SAML is the
only way to sign in at all, so putting SAML behind the commercial license would put
signing in behind the commercial license. Both protocols belong to the open-source
edition: OIDC today, SAML when it is built.

## Contact

Commercial licensing enquiries: https://www.dnx.solutions
