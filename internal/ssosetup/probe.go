// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package ssosetup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ProbeTimeout bounds a discovery attempt. An identity provider that cannot
// answer in this long is a finding rather than something to wait out.
const ProbeTimeout = 10 * time.Second

// Probe is a question about an issuer that is not configured yet.
type Probe struct {
	// Issuer is the URL to run discovery against.
	Issuer string
	// ClientID is echoed into the rendered configuration. Discovery does not
	// need it - it is not a secret and no request carries it - so an empty one
	// still probes, and only the rendered artifact is left incomplete.
	ClientID string
	// Preset supplies scopes and the groups claim when they are not given
	// explicitly.
	Preset Preset
	// Scopes overrides the preset's scopes.
	Scopes []string
	// GroupsClaim overrides the preset's claim name.
	GroupsClaim string
	// RedirectURI is this deployment's own callback URL, filled into the
	// provider notes so they name it rather than describing it. Derived from
	// server.base_url; empty is tolerated and falls back to the generic
	// phrasing.
	RedirectURI string
}

// Finding is one thing learned about an issuer, at a severity that says what to
// do about it.
type Finding struct {
	// Level is "ok", "warn", or "fail". Only "fail" means the connection
	// cannot work; "warn" means it will connect and then disappoint, which is
	// the failure mode that costs the most time to diagnose after the fact.
	Level  string `json:"level"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Remedy string `json:"remedy,omitempty"`
}

// Levels a finding can carry.
const (
	LevelOK   = "ok"
	LevelWarn = "warn"
	LevelFail = "fail"
)

// ProbeResult is what discovery proved, plus the configuration that would use
// it.
type ProbeResult struct {
	Issuer   string `json:"issuer"`
	PresetID string `json:"preset_id"`

	// OK reports that the connection would establish. It is false only for
	// findings at LevelFail - a provider that connects but will not emit
	// groups is a working connection with a warning, not a broken one.
	OK bool `json:"ok"`

	AuthorizationURL string `json:"authorization_url,omitempty"`
	TokenURL         string `json:"token_url,omitempty"`
	JWKSURL          string `json:"jwks_url,omitempty"`
	UserInfoURL      string `json:"userinfo_url,omitempty"`

	ScopesSupported []string `json:"scopes_supported,omitempty"`
	ClaimsSupported []string `json:"claims_supported,omitempty"`

	Scopes      []string `json:"scopes"`
	GroupsClaim string   `json:"groups_claim"`

	Findings []Finding `json:"findings"`
	Notes    []string  `json:"notes,omitempty"`
	DocsURL  string    `json:"docs_url,omitempty"`
}

// discovery is the part of the provider metadata document Panorama reads.
//
// go-oidc parses the endpoints it needs and exposes the rest through Claims,
// so the two advertised lists come from here. Both are optional in the spec,
// and their absence is treated as "unknown" rather than "empty" - a provider
// that does not publish scopes_supported is not a provider that supports no
// scopes.
type discovery struct {
	ScopesSupported []string `json:"scopes_supported"`
	ClaimsSupported []string `json:"claims_supported"`
	JWKSURL         string   `json:"jwks_uri"`
	UserInfoURL     string   `json:"userinfo_endpoint"`
}

// Run performs discovery and reports what it found.
//
// The outbound request goes to a URL an administrator supplied. That is the
// point of the command, and Panorama is deployed inside the network whose
// identity provider it is being pointed at - a self-hosted Keycloak on a
// private address is the ordinary case, not the suspicious one - so this does
// not refuse private hosts. What it does refuse is a non-https issuer, which
// would carry tokens in the clear, matching the same rule config validation
// applies at startup.
func Run(ctx context.Context, p Probe) ProbeResult {
	res := ProbeResult{
		Issuer:      strings.TrimSpace(p.Issuer),
		PresetID:    p.Preset.ID,
		Scopes:      p.Scopes,
		GroupsClaim: strings.TrimSpace(p.GroupsClaim),
		// Substituted here, so no caller can display a raw placeholder.
		Notes:   p.Preset.NotesWith(p.RedirectURI),
		DocsURL: p.Preset.DocsURL,
	}
	if len(res.Scopes) == 0 {
		res.Scopes = p.Preset.Scopes
	}
	if res.GroupsClaim == "" {
		res.GroupsClaim = p.Preset.GroupsClaim
	}

	if fail := validateIssuer(res.Issuer); fail != nil {
		res.Findings = append(res.Findings, *fail)
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	provider, err := oidc.NewProvider(ctx, res.Issuer)
	if err != nil {
		res.Findings = append(res.Findings, discoveryFailure(res.Issuer, err))
		return res
	}

	res.AuthorizationURL = provider.Endpoint().AuthURL
	res.TokenURL = provider.Endpoint().TokenURL

	var doc discovery
	if cerr := provider.Claims(&doc); cerr != nil {
		// The document parsed well enough for go-oidc to build a provider, so
		// this is a malformed optional field rather than a dead issuer. Recorded
		// and carried on, because the connection itself is proven at this point.
		res.Findings = append(res.Findings, Finding{
			Level:  LevelWarn,
			Title:  "Provider metadata could not be read in full",
			Detail: fmt.Sprintf("Discovery succeeded, but the metadata document did not parse: %v.", cerr),
			Remedy: "The connection will still work. Panorama cannot pre-check the scopes and claims this provider advertises.",
		})
	}
	res.JWKSURL = doc.JWKSURL
	res.UserInfoURL = doc.UserInfoURL
	res.ScopesSupported = doc.ScopesSupported
	res.ClaimsSupported = doc.ClaimsSupported

	res.Findings = append(res.Findings, Finding{
		Level:  LevelOK,
		Title:  "Discovery succeeded",
		Detail: fmt.Sprintf("%s is an OIDC provider and publishes the endpoints Panorama uses.", res.Issuer),
	})

	res.Findings = append(res.Findings, checkScopes(res.Scopes, doc.ScopesSupported)...)
	res.Findings = append(res.Findings, checkGroupsClaim(res.Issuer, res.GroupsClaim, doc.ClaimsSupported))

	if p.ClientID == "" {
		res.Findings = append(res.Findings, Finding{
			Level:  LevelWarn,
			Title:  "No client id given",
			Detail: "Discovery does not need one, so this probe is still valid.",
			Remedy: "Register an application at your provider and pass its client id, or the configuration below will be incomplete.",
		})
	}

	res.OK = !slices.ContainsFunc(res.Findings, func(f Finding) bool { return f.Level == LevelFail })
	return res
}

// validateIssuer rejects what cannot be an issuer before any network call.
func validateIssuer(issuer string) *Finding {
	if issuer == "" {
		return &Finding{
			Level:  LevelFail,
			Title:  "No issuer given",
			Detail: "An issuer URL is what discovery runs against.",
			Remedy: "Find it in your provider's OIDC application settings; it is the URL that serves /.well-known/openid-configuration.",
		}
	}
	if !strings.HasPrefix(issuer, "https://") {
		return &Finding{
			Level:  LevelFail,
			Title:  "Issuer is not https",
			Detail: fmt.Sprintf("%q would carry tokens in the clear.", issuer),
			Remedy: "Use the https form of the issuer. Panorama refuses a plaintext issuer at startup too, so this would not have started.",
		}
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return &Finding{
			Level:  LevelFail,
			Title:  "Issuer is not a URL",
			Detail: fmt.Sprintf("%q could not be parsed as one.", issuer),
			Remedy: "Give the issuer as a full URL, with no path beyond what your provider publishes.",
		}
	}
	if strings.Contains(issuer, "/.well-known/") {
		return &Finding{
			Level:  LevelFail,
			Title:  "Issuer includes the discovery path",
			Detail: "Panorama appends /.well-known/openid-configuration itself.",
			Remedy: fmt.Sprintf("Use %s instead.", strings.SplitN(issuer, "/.well-known/", 2)[0]),
		}
	}
	return nil
}

// discoveryFailure turns a go-oidc error into something actionable.
//
// The library's messages are accurate and unhelpful at the moment somebody is
// staring at them, and the three failures below account for most first
// attempts: a wrong host, an issuer that serves something other than OIDC, and
// an issuer whose published value does not match the URL used to reach it.
func discoveryFailure(issuer string, err error) Finding {
	msg := err.Error()
	f := Finding{
		Level: LevelFail,
		Title: "Discovery failed",
		// Truncated: go-oidc puts the whole response body in the error, and an
		// issuer that answered with a web page rather than metadata answers
		// with an entire web page. The first line is the part that identifies
		// the failure; the rest is the thing that was not metadata.
		Detail: fmt.Sprintf("Panorama could not read %s/.well-known/openid-configuration: %s",
			issuer, firstLine(msg, 180)),
	}

	switch {
	// go-oidc has worded this several ways across versions; "did not match" is
	// the stable part, and in this context nothing else produces it.
	case strings.Contains(msg, "did not match"):
		f.Title = "Issuer mismatch"
		f.Remedy = "The provider's document names a different issuer than the URL used to reach it. Use exactly the value its \"issuer\" field carries - a trailing slash is enough to cause this."
		if strings.Contains(issuer, "/common/") || strings.Contains(issuer, "/organizations/") {
			// Entra's multi-tenant endpoints publish a templated issuer
			// containing {tenantid}, which no verifier can match. Worth naming,
			// because it is the first URL an Entra administrator tries.
			f.Remedy = "This is a multi-tenant endpoint, and it publishes a templated issuer containing {tenantid} that cannot match anything. Use your own tenant id in place of \"common\" - the Directory (tenant) ID on the app registration's overview."
		}
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "dial tcp"), strings.Contains(msg, "timeout"), errors.Is(err, context.DeadlineExceeded):
		f.Title = "Issuer is unreachable"
		f.Remedy = "Check the hostname, and check that this deployment can reach it - a private identity provider needs a route from wherever Panorama runs, which is not always where you are running this command."
	case strings.Contains(msg, "404"), strings.Contains(msg, "expected JSON"), strings.Contains(msg, "invalid character"):
		f.Title = "Not an OIDC issuer"
		f.Remedy = "The URL answered, but not with provider metadata. This is usually a tenant or realm missing from the path."
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"):
		f.Title = "TLS verification failed"
		f.Remedy = "The issuer's certificate did not verify from here. A private CA has to be trusted by the container Panorama runs in."
	default:
		f.Remedy = "Confirm the issuer in your provider's OIDC application settings."
	}
	return f
}

// firstLine reduces an error to its identifying part: the text up to the first
// newline, capped at n runes.
func firstLine(s string, n int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// checkScopes compares what Panorama will request against what the provider
// says it supports.
func checkScopes(want, supported []string) []Finding {
	if !slices.Contains(want, "openid") {
		return []Finding{{
			Level:  LevelFail,
			Title:  "Scopes do not include openid",
			Detail: "Without it the provider issues no ID token, and an ID token is the whole mechanism.",
			Remedy: "Add openid to the scope list.",
		}}
	}
	if len(supported) == 0 {
		// Optional metadata. Absent means unknown, and asserting a problem from
		// missing information is how a check earns a reputation for lying.
		return nil
	}

	var missing []string
	for _, s := range want {
		if !slices.Contains(supported, s) {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		return []Finding{{
			Level:  LevelOK,
			Title:  "Every requested scope is supported",
			Detail: strings.Join(want, ", "),
		}}
	}
	return []Finding{{
		Level:  LevelWarn,
		Title:  "Provider does not advertise every requested scope",
		Detail: fmt.Sprintf("Not advertised: %s. Advertised: %s.", strings.Join(missing, ", "), strings.Join(supported, ", ")),
		Remedy: "Some providers accept scopes they do not list, so this may still work. If sign-in fails at the authorization step, remove the unadvertised scopes first.",
	}}
}

// checkGroupsClaim reports whether group membership is likely to arrive.
//
// This is the check worth having. A connection that authenticates but carries
// no groups looks entirely successful until somebody notices every person in
// the deployment landed on the default role, and by then the person who
// configured it has moved on.
func checkGroupsClaim(issuer, claim string, supported []string) Finding {
	if claim == "" {
		return Finding{
			Level:  LevelFail,
			Title:  "No groups claim named",
			Detail: "Role mapping reads exactly one claim, and it has no default that would be safe to guess.",
			Remedy: "Name the claim your provider emits group membership in.",
		}
	}
	if len(supported) == 0 {
		return Finding{
			Level:  LevelWarn,
			Title:  fmt.Sprintf("Cannot pre-check the %q claim", claim),
			Detail: "This provider does not publish claims_supported, so whether groups arrive can only be settled by a real sign-in.",
			Remedy: "Sign in once, then run `panorama sso claims` to see what actually arrived.",
		}
	}
	if slices.Contains(supported, claim) {
		return Finding{
			Level:  LevelOK,
			Title:  fmt.Sprintf("Provider advertises the %q claim", claim),
			Detail: "Group membership should reach Panorama. What the values look like is still only knowable from a real sign-in.",
		}
	}

	// Absence here is not evidence of absence, and saying "does not advertise"
	// made it sound like one - somebody who had correctly added the claim read
	// it as a report that they had not. claims_supported is optional metadata
	// describing what a provider MAY supply, and per-application claim
	// configuration does not appear in it, because the document is not
	// per-application.
	f := Finding{
		Level: LevelWarn,
		Title: fmt.Sprintf("Cannot confirm the %q claim from discovery", claim),
		Detail: fmt.Sprintf(
			"It is not in the advertised list (%s), but that list rarely reflects claims configured per application, so this is not evidence the claim is missing.",
			strings.Join(supported, ", ")),
		Remedy: "Sign in once and run `panorama sso claims`. That is the only thing that settles it.",
	}

	if isMicrosoftIssuer(issuer) {
		f.Detail = "Microsoft publishes one fixed claims list for the whole platform, identical for every tenant, and it never includes groups. Adding the claim in Token configuration does not change it, so this check cannot see your configuration either way."
		// The real Entra failure, and invisible from here: the claim is added
		// per token type, and Panorama reads the ID token.
		f.Remedy = "Check the groups claim is ticked for the ID token - a claim added only to the access token reaches nothing here. If you chose \"Groups assigned to the application\", assign the groups to the app under Enterprise applications as well. Then sign in once and run `panorama sso claims`."
	}
	return f
}

// isMicrosoftIssuer reports whether an issuer is Entra ID, including the
// sovereign clouds, which publish the same static claims list.
//
// The host is compared exactly rather than searched for as a substring: nothing
// here grants anything - it only chooses which sentence to print - but a match
// on "login.microsoftonline.com.example.net" would be wrong, and a matcher that
// is wrong in a harmless place is a matcher someone later copies into a
// harmful one.
func isMicrosoftIssuer(issuer string) bool {
	u, err := url.Parse(issuer)
	if err != nil {
		return false
	}
	return slices.Contains([]string{
		"login.microsoftonline.com",
		"login.microsoftonline.us",
		"login.partner.microsoftonline.cn",
		"login.microsoftonline.de",
		"sts.windows.net",
	}, strings.ToLower(u.Hostname()))
}
