// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package ssosetup

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Facts are the things only the database knows, supplied by the caller so this
// package stays free of a store dependency and can be tested without one.
type Facts struct {
	// SSOSignIns is how many accounts have ever arrived through SSO. Zero
	// means the round trip has never been completed, which no amount of
	// discovery can substitute for.
	SSOSignIns int
	// ObservedGroups are the group strings sign-ins have actually carried.
	ObservedGroups []string
	// SignInsWithoutGroups counts sign-ins whose groups claim was empty.
	SignInsWithoutGroups int
	// MappedGroups are the group strings an administrator has mapped to roles.
	MappedGroups []string
	// DefaultRole is what an unmapped person falls to, or "none".
	DefaultRole string
}

// Deployment is the live connection, as configured.
type Deployment struct {
	Issuer      string
	ClientID    string
	HasSecret   bool
	Scopes      []string
	GroupsClaim string
	BaseURL     string
	RedirectURI string
}

// Diagnose checks a running deployment's SSO against what it has actually seen.
//
// The order matters: it walks the path a sign-in takes, so the first failure is
// the first thing that would break rather than the first thing that happened to
// be cheap to check.
func Diagnose(ctx context.Context, d Deployment, f Facts) []Finding {
	if strings.TrimSpace(d.Issuer) == "" {
		return []Finding{{
			Level:  LevelWarn,
			Title:  "Single sign-on is not configured",
			Detail: "This deployment has no issuer, so the console is reachable only with a gateway token.",
			Remedy: "Run `panorama sso probe -issuer <your issuer>` to validate one and print the configuration to apply.",
		}}
	}

	out := []Finding{}
	out = append(out, checkSecret(d))
	out = append(out, checkBaseURL(d)...)

	probe := Run(ctx, Probe{
		Issuer:      d.Issuer,
		ClientID:    d.ClientID,
		Preset:      Preset{Scopes: d.Scopes, GroupsClaim: d.GroupsClaim},
		Scopes:      d.Scopes,
		GroupsClaim: d.GroupsClaim,
	})
	out = append(out, probe.Findings...)

	out = append(out, checkRoundTrip(f))
	out = append(out, checkGroupsArriving(d, f)...)
	out = append(out, checkMapping(f)...)

	return out
}

// checkSecret states where the secret is, rather than checking that it exists.
//
// Configuration validation already refuses to start a deployment whose issuer
// is set without one, so by the time anything here runs the secret is present
// by construction. Re-deriving that rule would give it two owners, and two
// derivations of one rule drift. What this line is for is the question people
// actually arrive with - where did it go, and can the console show it - which
// is worth answering next to the checks rather than in documentation.
func checkSecret(d Deployment) Finding {
	if !d.HasSecret {
		// Reachable only if a caller assembled a Deployment by hand rather than
		// from a loaded configuration. Reported rather than assumed away.
		return Finding{
			Level:  LevelFail,
			Title:  "No client secret",
			Detail: "The code exchange would fail after the user has already been redirected.",
			Remedy: "Set PANORAMA_OIDC_CLIENT_SECRET, or point oidc.client_secret_file at a mounted secret, and restart.",
		}
	}
	return Finding{
		Level:  LevelOK,
		Title:  "Client secret is set",
		Detail: "It came from the environment or a mounted file. It is not in the database, is not read back here, and no screen can show it.",
	}
}

// checkBaseURL catches the mismatch that produces redirect_uri_mismatch.
//
// The redirect URI is derived from server.base_url, so a base_url that is stale
// or left at its default makes Panorama hand out a redirect URI that can never
// be reached - and the error surfaces at the provider, phrased in the
// provider's words, well away from the setting that caused it.
func checkBaseURL(d Deployment) []Finding {
	u, err := url.Parse(d.BaseURL)
	if err != nil || u.Host == "" {
		return []Finding{{
			Level:  LevelFail,
			Title:  "server.base_url is not a URL",
			Detail: fmt.Sprintf("%q cannot produce a redirect URI.", d.BaseURL),
			Remedy: "Set PANORAMA_SERVER_BASE_URL to the URL a browser reaches this console at.",
		}}
	}

	host := u.Hostname()
	isLocal := host == "localhost" || host == "127.0.0.1" || host == "::1"

	switch {
	case isLocal:
		return []Finding{{
			Level:  LevelWarn,
			Title:  "Redirect URI points at localhost",
			Detail: fmt.Sprintf("Panorama will send people to %s after they authenticate.", d.RedirectURI),
			Remedy: "Correct for a laptop evaluation. For anything anyone else signs in to, set PANORAMA_SERVER_BASE_URL to the URL their browser actually reaches - the redirect happens in their browser, not in this container.",
		}}
	case u.Scheme != "https":
		return []Finding{{
			Level:  LevelFail,
			Title:  "Redirect URI is not https",
			Detail: fmt.Sprintf("%s is not a redirect URI most providers will accept for a non-local host.", d.RedirectURI),
			Remedy: "Serve the console over TLS and set PANORAMA_SERVER_BASE_URL to its https URL.",
		}}
	default:
		return []Finding{{
			Level:  LevelOK,
			Title:  "Redirect URI is well-formed",
			Detail: d.RedirectURI,
			Remedy: "It still has to be registered at your provider, exactly as written - a trailing slash is a different URI.",
		}}
	}
}

func checkRoundTrip(f Facts) Finding {
	if f.SSOSignIns > 0 {
		return Finding{
			Level:  LevelOK,
			Title:  "The round trip has been completed",
			Detail: fmt.Sprintf("%d account(s) arrived through single sign-on.", f.SSOSignIns),
		}
	}
	return Finding{
		Level:  LevelWarn,
		Title:  "Nobody has signed in through SSO yet",
		Detail: "Discovery proves the issuer is real. It does not prove the redirect URI is registered, the secret is right, or that groups arrive - only a sign-in does.",
		Remedy: "Sign in once through the provider, then run this again.",
	}
}

// checkGroupsArriving is the check that exists because the failure it catches
// is invisible. A deployment where the groups claim never arrives works
// perfectly and puts everyone on the default role.
func checkGroupsArriving(d Deployment, f Facts) []Finding {
	if f.SSOSignIns == 0 {
		return nil
	}

	if len(f.ObservedGroups) == 0 {
		return []Finding{{
			Level:  LevelFail,
			Title:  fmt.Sprintf("No sign-in has ever carried a %q claim", d.GroupsClaim),
			Detail: fmt.Sprintf("%d sign-in(s) completed and none brought group membership, so every one of them took the default role (%s).", f.SSOSignIns, orNone(f.DefaultRole)),
			Remedy: "Either the claim is called something else at your provider, or the provider is not emitting it for this application. Most providers require it to be switched on per application; requesting the scope is not enough.",
		}}
	}

	if f.SignInsWithoutGroups > 0 {
		return []Finding{{
			Level:  LevelWarn,
			Title:  "Some sign-ins carried no groups",
			Detail: fmt.Sprintf("%d sign-in(s) arrived with an empty %q claim while others carried one.", f.SignInsWithoutGroups, d.GroupsClaim),
			Remedy: "Usually people who are in no group at the provider, which is fine. If it is everyone in a particular directory, check that the application's group assignment covers them.",
		}}
	}

	return []Finding{{
		Level:  LevelOK,
		Title:  fmt.Sprintf("Group membership is arriving in %q", d.GroupsClaim),
		Detail: fmt.Sprintf("%d distinct group(s) seen: %s.", len(f.ObservedGroups), strings.Join(truncate(f.ObservedGroups, 8), ", ")),
	}}
}

// checkMapping compares what has been mapped against what has been seen, in
// both directions.
//
// A mapped group nobody has ever been in is the useful half: it is almost
// always a typo or a name from a different provider, and it fails silently by
// granting nothing.
func checkMapping(f Facts) []Finding {
	var out []Finding

	if len(f.MappedGroups) == 0 && f.DefaultRole == "none" {
		out = append(out, Finding{
			Level:  LevelFail,
			Title:  "Nobody can sign in",
			Detail: "No group is mapped to a role and the default role is \"none\", so every sign-in is refused after authenticating.",
			Remedy: "Map at least one group to admin, or set a default role.",
		})
	}

	lowerSeen := lowered(f.ObservedGroups)
	var unseen []string
	for _, m := range f.MappedGroups {
		if !slices.Contains(lowerSeen, strings.ToLower(strings.TrimSpace(m))) {
			unseen = append(unseen, m)
		}
	}
	if len(unseen) > 0 && len(f.ObservedGroups) > 0 {
		out = append(out, Finding{
			Level:  LevelWarn,
			Title:  "Mapped groups nobody has been seen in",
			Detail: strings.Join(unseen, ", "),
			Remedy: "These grant nothing. Usually a typo, a display name where the provider emits an id, or a group nobody who has signed in yet belongs to.",
		})
	}

	lowerMapped := lowered(f.MappedGroups)
	var unmapped []string
	for _, g := range f.ObservedGroups {
		if !slices.Contains(lowerMapped, strings.ToLower(strings.TrimSpace(g))) {
			unmapped = append(unmapped, g)
		}
	}
	if len(unmapped) > 0 {
		out = append(out, Finding{
			Level:  LevelOK,
			Title:  "Groups seen but not mapped",
			Detail: strings.Join(truncate(unmapped, 12), ", "),
			Remedy: fmt.Sprintf("Members of these take the default role (%s). Map any that should carry more.", orNone(f.DefaultRole)),
		})
	}

	return out
}

func lowered(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}

func truncate(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	out := make([]string, 0, n+1)
	out = append(out, in[:n]...)
	return append(out, fmt.Sprintf("and %d more", len(in)-n))
}

func orNone(role string) string {
	if role == "" {
		return "none"
	}
	return role
}

// Worst returns the most severe level in a set of findings, which is what an
// exit code and a status badge are both derived from.
func Worst(findings []Finding) string {
	worst := LevelOK
	for _, f := range findings {
		switch f.Level {
		case LevelFail:
			return LevelFail
		case LevelWarn:
			worst = LevelWarn
		}
	}
	return worst
}
