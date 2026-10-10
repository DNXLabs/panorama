// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package ssosetup validates an OIDC connection and renders the configuration
// that would establish it.
//
// It writes nothing. The connection is configuration - issuer, client id, and
// client secret live in the file and the environment beside every other secret,
// because a client secret in a database is a client secret in a backup - so the
// most this package can do is prove a connection would work and hand back the
// exact text to paste into whichever thing owns the deployment's environment.
//
// That is not a limitation worked around. In three of the four packaging shapes
// Panorama ships (compose, Helm, ECS) the configuration inside the container is
// not the source of truth, and writing to it would leave the running deployment
// silently disagreeing with the manifest that produced it.
//
// Both the console and `panorama sso` are callers, and they render from the
// same functions so the two cannot drift.
package ssosetup

import "strings"

// Preset carries what differs between identity providers.
//
// The fields Panorama needs are the same everywhere; what varies is what each
// provider calls them and what has to be switched on before the values arrive
// at all. Almost every first-run SSO failure is one of these notes rather than
// anything about Panorama, which is why they travel with the preset instead of
// living in documentation nobody reads at the point of failure.
//
// Two claims recur and are worth reading before writing a new preset. Nearly no
// provider emits group membership by default, and several emit identifiers
// rather than names when it is switched on. Both failures are silent: sign-in
// works, and everybody lands on the default role.
type Preset struct {
	// ID is the stable identifier used by the CLI flag and the console.
	ID string `json:"id"`
	// Label is what the console shows.
	Label string `json:"label"`

	// IssuerHint is an example issuer, used as a placeholder. It is never a
	// default: guessing an issuer is how a deployment ends up pointed at
	// somebody else's tenant.
	IssuerHint string `json:"issuer_hint"`
	// Scopes are what this provider needs in order to return groups.
	Scopes []string `json:"scopes"`
	// GroupsClaim is what this provider calls group membership in the token.
	GroupsClaim string `json:"groups_claim"`
	// Notes are the provider-specific things that have to be true before any
	// of this works, in the order somebody would hit them.
	Notes []string `json:"notes"`
	// DocsURL points at the provider's own instructions, since half of this
	// happens outside Panorama entirely.
	DocsURL string `json:"docs_url"`
}

// PresetGeneric is the fallback for a provider with no entry here. It is a
// preset rather than an absence so that the console always has scopes and a
// claim name to render, and so "my provider is not listed" is a supported path
// rather than a dead end.
const PresetGeneric = "generic"

// RedirectURIPlaceholder is substituted into a note with this deployment's own
// redirect URI before the note is shown.
//
// The alternative was a note reading "add Panorama's redirect URI", which asks
// the reader to go and find a value Panorama already knows - it is derived from
// server.base_url and nothing about it depends on the provider. Registering it
// wrongly is the single most common way a first sign-in fails, and a URI
// somebody retyped from memory is how that happens.
const RedirectURIPlaceholder = "{redirect_uri}"

// NotesWith returns the provider's notes with the redirect URI filled in.
//
// Every surface that displays a note goes through here, so none of them can
// show the raw placeholder.
func (p Preset) NotesWith(redirectURI string) []string {
	if redirectURI == "" {
		// Nothing better to say than the generic phrasing. Printing an empty
		// string mid-sentence would read as a bug in the note.
		redirectURI = "Panorama's redirect URI"
	}
	out := make([]string, 0, len(p.Notes))
	for _, n := range p.Notes {
		out = append(out, strings.ReplaceAll(n, RedirectURIPlaceholder, redirectURI))
	}
	return out
}

// presets is ordered by how likely a deployment is to use each one.
//
// Every entry is a provider Panorama can point at directly. AWS IAM Identity
// Center is deliberately absent: it is not an OIDC provider for third-party
// applications - its OAuth 2.0 support is trusted identity propagation, and
// sign-in for customer managed applications is SAML 2.0 only - and it has no
// group attribute it can put in an assertion, so role mapping could not work
// through it either. Point Panorama at the identity source behind it instead.
var presets = []Preset{
	{
		ID:          "entra-id",
		Label:       "Microsoft Entra ID",
		IssuerHint:  "https://login.microsoftonline.com/<tenant-id>/v2.0",
		Scopes:      []string{"openid", "profile", "email"},
		GroupsClaim: "groups",
		Notes: []string{
			"Register an app with {redirect_uri} as a Web redirect URI.",
			"Create a client secret (Certificates & secrets → New client secret). Copy the Value, not the Secret ID - it is shown once only.",
			"Assign the groups you want to map to the app under Enterprise applications → Users and groups. Only these will appear in the token.",
			"Add a groups claim (Token configuration → Add groups claim): choose \"Groups assigned to the application\" and tick ID token - Panorama reads only the ID token. This also avoids the 200-group limit that otherwise drops the claim entirely.",
			"Want group names instead of GUIDs? Edit the Manifest and add \"cloud_displayname\" to additionalProperties on the groups entry under optionalClaims.idToken. (The dialog's source options do not work for cloud-only groups.)",
		},
		DocsURL: "https://learn.microsoft.com/entra/identity-platform/optional-claims#configure-groups-optional-claims",
	},
	{
		ID:          "okta",
		Label:       "Okta",
		IssuerHint:  "https://your-org.okta.com",
		Scopes:      []string{"openid", "profile", "email", "groups"},
		GroupsClaim: "groups",
		Notes: []string{
			"Create an OIDC web application with the authorization code grant, and add {redirect_uri} as a sign-in redirect URI.",
			"The groups claim does not exist until you create it: Security → API → the authorization server issuing your tokens → Claims → Add claim, with a filter matching the groups you intend to map.",
			"Requesting the groups scope is not enough on its own. The claim has to exist on the authorization server that issues the token, and a default org server does not have one.",
			"The value is the group name as Okta shows it, not an id, so that is the string the role mapping matches - and renaming a group in Okta stops it matching.",
		},
		DocsURL: "https://developer.okta.com/docs/guides/customize-tokens-groups-claim/main/",
	},
	{
		ID:          "keycloak",
		Label:       "Keycloak",
		IssuerHint:  "https://keycloak.example.com/realms/your-realm",
		Scopes:      []string{"openid", "profile", "email", "groups"},
		GroupsClaim: "groups",
		Notes: []string{
			"Create a confidential client: standard flow on, client authentication on, and {redirect_uri} under Valid redirect URIs.",
			"Add a Group Membership mapper to the client's dedicated scope and set its token claim name to groups.",
			"Turn that mapper's \"Full group path\" off unless you mean it - with it on, the claim carries /engineering/platform, slashes and all, and that is the string the role mapping has to match.",
		},
		DocsURL: "https://www.keycloak.org/docs/latest/server_admin/#_protocol-mappers",
	},
	{
		ID:          "cognito",
		Label:       "Amazon Cognito",
		IssuerHint:  "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_abcd1234",
		Scopes:      []string{"openid", "profile", "email"},
		GroupsClaim: "cognito:groups",
		Notes: []string{
			"Create an app client with a client secret and the authorization code grant, and add {redirect_uri} to its allowed callback URLs.",
			"Groups created in the user pool itself arrive in cognito:groups with nothing else to configure, as the names you gave them rather than as ids - so that is what the role mapping matches, and renaming one stops it matching.",
			"Federated users are the exception: a group from an upstream SAML or OIDC provider does not become a Cognito group. Map the incoming attribute to a custom attribute and point groups_claim at that instead - Panorama reads whichever claim you name, so it needs no Lambda to rewrite it into cognito:groups.",
		},
		DocsURL: "https://docs.aws.amazon.com/cognito/latest/developerguide/cognito-user-pools-app-idp-settings.html",
	},
	{
		ID:          "auth0",
		Label:       "Auth0",
		IssuerHint:  "https://your-tenant.auth0.com/",
		Scopes:      []string{"openid", "profile", "email"},
		GroupsClaim: "https://panorama/groups",
		Notes: []string{
			"Create a Regular Web Application and add {redirect_uri} to Allowed Callback URLs.",
			"Auth0 emits no group claim of its own. Add a Login Action that sets one on the ID token, under a namespaced name such as https://panorama/groups.",
			"Do not call the claim groups or roles. Both are on Auth0's list of restricted claim names, and an Action that sets one is ignored without failing - sign-in works, and no groups arrive.",
			"Whatever name you choose is what groups_claim has to be set to, exactly.",
			"The issuer keeps its trailing slash. Without it, discovery fails on an issuer mismatch.",
		},
		DocsURL: "https://auth0.com/docs/secure/tokens/json-web-tokens/create-custom-claims",
	},
	{
		ID:          "pingone",
		Label:       "PingOne",
		IssuerHint:  "https://auth.pingone.com/<environment-id>/as",
		Scopes:      []string{"openid", "profile", "email"},
		GroupsClaim: "groups",
		Notes: []string{
			"Create a web application using the authorization code grant, and add {redirect_uri} to its redirect URIs.",
			"Group membership reaches the token through the application's attribute mapping rather than by default. Add a mapping that emits group names into a claim, and set groups_claim to whatever you named it.",
			"Confirm what arrives with `panorama sso claims` after the first sign-in: whether the mapping carries names or identifiers depends on the attribute you mapped.",
		},
		DocsURL: "https://docs.pingidentity.com/pingone/applications/p1_edit_attribute_mapping.html",
	},
	{
		ID:          "google-workspace",
		Label:       "Google Workspace",
		IssuerHint:  "https://accounts.google.com",
		Scopes:      []string{"openid", "profile", "email"},
		GroupsClaim: "groups",
		Notes: []string{
			"Google does not put group membership in an OIDC ID token at all, and no configuration changes that.",
			"Sign-in works. Everyone arrives ungrouped and takes the role set under \"Someone in no mapped group\", so set that to developer and name your administrators under Direct administrators, both on Settings → Single sign-on.",
			"Administrator is the only role assigned by hand, so nobody can be made a manager this way. If you need group-driven roles, Workspace has to sit behind a provider that can emit them.",
			"Set oidc.allowed_domains (PANORAMA_OIDC_ALLOWED_DOMAINS, or the OidcAllowedDomains parameter on the CloudFormation stack) to your Workspace's primary domain. Google's issuer signs in every Google account there is, and the first account to sign in owns the deployment, so Panorama refuses to start on this issuer without it.",
			"Set the OAuth consent screen's user type to Internal as well, so that Google refuses a stranger before Panorama has to.",
		},
		DocsURL: "https://developers.google.com/identity/openid-connect/openid-connect",
	},
	{
		ID:          PresetGeneric,
		Label:       "Generic OIDC",
		IssuerHint:  "https://your-idp.example.com",
		Scopes:      []string{"openid", "profile", "email", "groups"},
		GroupsClaim: "groups",
		Notes: []string{
			"Panorama needs an authorization code flow, a confidential client, and an ID token carrying a subject, an email, and a claim listing group membership.",
			"Assume group membership is off until proven otherwise. Almost no provider emits it by default, and the ones that do often emit identifiers rather than names.",
			"After the first sign-in, `panorama sso claims` shows exactly what arrived. Map groups from that rather than from what the provider's console displays.",
		},
	},
}

// Presets returns every known provider, in display order.
func Presets() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	return out
}

// PresetByID finds a preset. An unknown id is reported rather than silently
// resolved to the generic one, so a typo in a CLI flag does not quietly produce
// a configuration for a different provider.
func PresetByID(id string) (Preset, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// PresetIDs lists every id, for error messages and flag help.
func PresetIDs() []string {
	out := make([]string, 0, len(presets))
	for _, p := range presets {
		out = append(out, p.ID)
	}
	return out
}
