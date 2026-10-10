// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package ssosetup

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/dnxlabs/panorama/internal/config"
)

// Formats a connection can be rendered into, one per packaging shape Panorama
// ships. The name is what the CLI flag and the console tab carry.
const (
	// FormatEnv is a .env file, which is how the compose deployments take
	// their configuration: both compose files read one from their own
	// directory. It leads because it is the answer for the packaging most
	// people meet first, and because the alternative - editing
	// docker-compose.yml - means changing a committed file to hold a secret.
	FormatEnv   = "env"
	FormatECS   = "ecs"
	FormatShell = "shell"
	FormatYAML  = "yaml"
)

// Formats lists every renderable format, in the order the console shows them.
func Formats() []string {
	return []string{FormatECS, FormatEnv, FormatShell, FormatYAML}
}

// FormatLabel is what a format is called on a screen. The id is a flag value;
// this is the thing an operator recognises.
func FormatLabel(format string) string {
	switch format {
	case FormatEnv:
		return ".env"
	case FormatECS:
		return "ecs"
	case FormatShell:
		return "shell"
	case FormatYAML:
		return "config.yaml"
	default:
		return format
	}
}

// CallbackPath is where the identity provider sends the browser back to.
const CallbackPath = "/auth/oidc/callback"

// RedirectURI derives the callback URL a provider must have registered.
//
// One derivation, called by the server that serves the route and by every
// command that prints it. Three derivations is how a value that has to match a
// provider's configuration character for character starts differing between
// the screen that shows it and the endpoint that receives it.
func RedirectURI(baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		return ""
	}
	return strings.TrimSuffix(baseURL, "/") + CallbackPath
}

// Connection is what has to be true for SSO to work, in the form the renderers
// take.
//
// The client secret is deliberately absent. Every renderer emits a placeholder
// and says where the real value goes, because a secret that passes through a
// rendering function ends up in a scrollback buffer, a screenshot, or a browser
// history entry, and none of those are places a secret should be recoverable
// from.
type Connection struct {
	Issuer      string
	ClientID    string
	Scopes      []string
	GroupsClaim string
	// RedirectURI is not configuration - Panorama derives it from
	// server.base_url - but it is the value that has to be registered at the
	// provider, and it is the single most common thing to get wrong, so every
	// artifact states it.
	RedirectURI string
}

// secretPlaceholder stands in for the client secret everywhere it would appear.
const secretPlaceholder = "REPLACE_WITH_CLIENT_SECRET"

// domainPlaceholder stands in for the domain a shared issuer has to be limited
// to. Shaped so that validation refuses it as written: a configuration pasted
// without being read fails at startup naming the key, and does not start up
// limited to a domain nobody owns.
const domainPlaceholder = "REPLACE_WITH_YOUR_DOMAIN"

// needsDomain reports whether the rendered configuration has to carry
// oidc.allowed_domains. The rule is config.SharedIssuer and is asked there,
// because a configuration rendered without the key is one validation then
// refuses - a screen handing out something that does not boot.
func (c Connection) needsDomain() bool { return config.SharedIssuer(c.Issuer) }

// Render produces the configuration for one packaging shape.
func Render(format string, c Connection) (string, error) {
	if !slices.Contains(Formats(), format) {
		return "", fmt.Errorf("ssosetup: unknown format %q (want %s)", format, strings.Join(Formats(), ", "))
	}

	switch format {
	case FormatEnv:
		return renderEnv(c), nil
	case FormatShell:
		return renderShell(c), nil
	case FormatECS:
		return renderECS(c), nil
	case FormatYAML:
		return renderYAML(c), nil
	}
	// Unreachable: the membership check above is exhaustive over Formats.
	return "", fmt.Errorf("ssosetup: no renderer for %q", format)
}

func (c Connection) scopeList() string {
	if len(c.Scopes) == 0 {
		return ""
	}
	return strings.Join(c.Scopes, ",")
}

func renderEnv(c Connection) string {
	return fmt.Sprintf(`# deploy/compose/.env - git-ignored, and read by both compose files.
#
# Compose looks for it in the directory holding the compose file, not in the one
# you run the command from, so a .env in the repository root is ignored without
# a word about it.
#
# Register this redirect URI at your provider first:
#   %s

PANORAMA_OIDC_ISSUER=%s
PANORAMA_OIDC_CLIENT_ID=%s
PANORAMA_OIDC_SCOPES=%s
PANORAMA_OIDC_GROUPS_CLAIM=%s
PANORAMA_OIDC_CLIENT_SECRET=%s
%s
# Then: docker compose -f deploy/compose/docker-compose.yml up -d
`, c.RedirectURI, c.Issuer, c.ClientID, c.scopeList(), c.GroupsClaim, secretPlaceholder,
		c.domainLines("PANORAMA_OIDC_ALLOWED_DOMAINS="))
}

// domainLines is the allowed-domains setting for the line-oriented formats, or
// nothing for an issuer that names a tenant.
func (c Connection) domainLines(assignment string) string {
	if !c.needsDomain() {
		return ""
	}
	return `
# This issuer signs in every Google account there is, so Panorama refuses to
# start without the domain to limit it to. Your Workspace's primary domain,
# written bare: example.com. Several are separated by commas.
` + assignment + domainPlaceholder + "\n"
}

func renderShell(c Connection) string {
	return fmt.Sprintf(`# For a shell - `+"`make run`"+`, or a binary started by hand. Not persisted
# anywhere; a .env or your deployment's manifest is what survives a restart.
#
# Register this redirect URI at your provider first:
#   %s

export PANORAMA_OIDC_ISSUER=%s
export PANORAMA_OIDC_CLIENT_ID=%s
export PANORAMA_OIDC_SCOPES=%s
export PANORAMA_OIDC_GROUPS_CLAIM=%s

# Read this from wherever your secrets live rather than typing it, so it stays
# out of your shell history.
export PANORAMA_OIDC_CLIENT_SECRET=%s
%s`, c.RedirectURI, c.Issuer, c.ClientID, c.scopeList(), c.GroupsClaim, secretPlaceholder,
		c.domainLines("export PANORAMA_OIDC_ALLOWED_DOMAINS="))
}

// renderECS is the secret's value and nothing else.
//
// Deliberately not a script, and deliberately carrying no comments. It is
// pasted into the Plaintext box of a Secrets Manager secret, and that box holds
// JSON - a header explaining what the document is for would be pasted with it
// and make the document invalid. The explaining is done by whatever displays
// this, which is a screen with room for sentences.
//
// The client secret is a placeholder because Panorama never had it: the probe
// reaches the issuer's discovery document, which is unauthenticated. Nothing
// here could fill it in.
func renderECS(c Connection) string {
	return fmt.Sprintf(`{
  "issuer": "%s",
  "client_id": "%s",
  "client_secret": "%s",
  "scopes": "%s",
  "groups_claim": "%s"
}
`, c.Issuer, c.ClientID, secretPlaceholder, c.scopeList(), c.GroupsClaim)
}

func renderYAML(c Connection) string {
	scopes := "[]"
	if len(c.Scopes) > 0 {
		quoted := make([]string, 0, len(c.Scopes))
		for _, s := range c.Scopes {
			quoted = append(quoted, fmt.Sprintf("%q", s))
		}
		scopes = "[" + strings.Join(quoted, ", ") + "]"
	}

	domains := ""
	if c.needsDomain() {
		domains = `
  # This issuer signs in every Google account there is, so Panorama refuses to
  # start without the domain to limit it to: your Workspace's primary domain.
  allowed_domains: ["` + domainPlaceholder + `"]
`
	}

	return fmt.Sprintf(`# config.yaml. Register this redirect URI at your provider first:
#   %s

oidc:
  issuer: "%s"
  client_id: "%s"
  scopes: %s
  groups_claim: "%s"
%s
  # Leave client_secret empty here. Set PANORAMA_OIDC_CLIENT_SECRET, or point
  # client_secret_file at a mounted secret - a secret in a configuration file
  # is a secret in every backup of it.
  client_secret: ""
  # client_secret_file: "/run/secrets/panorama-oidc-client-secret"

# Then restart Panorama.
`, c.RedirectURI, c.Issuer, c.ClientID, scopes, c.GroupsClaim, domains)
}

// SortedClaims returns claims in a stable order for display. The provider's own
// ordering is not meaningful and varies between requests for some of them,
// which makes an unsorted list look like it changed when it did not.
func SortedClaims(claims []string) []string {
	out := make([]string, len(claims))
	copy(out, claims)
	sort.Strings(out)
	return out
}
