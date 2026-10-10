// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package ssosetup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/dnxlabs/panorama/internal/config"
)

// TestAnUnknownPresetIsRefusedRatherThanResolved. A typo in -preset must not
// quietly produce a configuration for a different provider, because the whole
// value of a preset is that it carries scopes and a claim name the operator did
// not have to know.
func TestAnUnknownPresetIsRefusedRatherThanResolved(t *testing.T) {
	if _, ok := PresetByID("oktaa"); ok {
		t.Fatal("a misspelled preset resolved to something")
	}
	if _, ok := PresetByID("okta"); !ok {
		t.Fatal("okta did not resolve")
	}
	if _, ok := PresetByID("  OKTA "); !ok {
		t.Fatal("preset lookup should tolerate case and surrounding space")
	}
}

// TestIssuerIsValidatedBeforeAnyNetworkCall. Each of these is a mistake made
// while typing an issuer into a terminal, and each has a specific remedy that
// is more useful than whatever the HTTP client would have said.
func TestIssuerIsValidatedBeforeAnyNetworkCall(t *testing.T) {
	for name, tc := range map[string]struct {
		issuer    string
		wantTitle string
	}{
		"empty":              {"", "No issuer given"},
		"plaintext":          {"http://idp.example.com", "Issuer is not https"},
		"discovery path":     {"https://acme.okta.com/.well-known/openid-configuration", "Issuer includes the discovery path"},
		"not a url at all":   {"https://", "Issuer is not a URL"},
		"scheme-less string": {"idp.example.com", "Issuer is not https"},
	} {
		t.Run(name, func(t *testing.T) {
			res := Run(context.Background(), Probe{Issuer: tc.issuer})
			if res.OK {
				t.Fatalf("%q was accepted", tc.issuer)
			}
			if len(res.Findings) != 1 || res.Findings[0].Title != tc.wantTitle {
				t.Fatalf("want a single %q finding, got %+v", tc.wantTitle, res.Findings)
			}
			if res.Findings[0].Level != LevelFail {
				t.Fatalf("want a failure, got %q", res.Findings[0].Level)
			}
		})
	}
}

// discoveryServer stands in for an identity provider. It answers the one
// document Panorama reads and nothing else.
func discoveryServer(t *testing.T, doc map[string]any) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	if _, ok := doc["issuer"]; !ok {
		doc["issuer"] = srv.URL
	}
	for key, value := range map[string]any{
		"authorization_endpoint": srv.URL + "/authorize",
		"token_endpoint":         srv.URL + "/token",
		"jwks_uri":               srv.URL + "/jwks",
	} {
		if _, ok := doc[key]; !ok {
			doc[key] = value
		}
	}

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	return srv
}

// probeAgainst runs a probe through the test server's own TLS client, which is
// how an httptest TLS server is reached without turning verification off. This
// is also the mechanism a deployment behind a private CA would use, so the
// production path is the one under test.
func probeAgainst(t *testing.T, srv *httptest.Server, p Probe) ProbeResult {
	t.Helper()
	return Run(oidc.ClientContext(context.Background(), srv.Client()), p)
}

// TestAConnectionThatWillNotCarryGroupsIsAWarningNotAFailure.
//
// This is the failure the whole feature exists for: the connection works, the
// sign-in succeeds, and every person silently lands on the default role. It
// must not be reported as a broken connection, because it is not one, and it
// must not be silent.
func TestAConnectionThatWillNotCarryGroupsIsAWarningNotAFailure(t *testing.T) {
	srv := discoveryServer(t, map[string]any{
		"scopes_supported": []string{"openid", "profile", "email"},
		"claims_supported": []string{"sub", "email", "name"},
	})

	res := probeAgainst(t, srv, Probe{
		Issuer:      srv.URL,
		ClientID:    "panorama",
		Scopes:      []string{"openid", "profile", "email"},
		GroupsClaim: "groups",
	})

	if !res.OK {
		t.Fatalf("a reachable issuer without a groups claim is a working connection: %+v", res.Findings)
	}
	if Worst(res.Findings) != LevelWarn {
		t.Fatalf("want a warning, got %q from %+v", Worst(res.Findings), res.Findings)
	}

	var found bool
	for _, f := range res.Findings {
		if strings.Contains(f.Title, "Cannot confirm the \"groups\" claim") {
			found = true
			if f.Level != LevelWarn {
				t.Fatalf("the groups finding should warn, got %q", f.Level)
			}
		}
	}
	if !found {
		t.Fatalf("nothing warned about the missing groups claim: %+v", res.Findings)
	}
}

// TestAdvertisedGroupsClaimReportsOK.
func TestAdvertisedGroupsClaimReportsOK(t *testing.T) {
	srv := discoveryServer(t, map[string]any{
		"scopes_supported": []string{"openid", "profile", "email", "groups"},
		"claims_supported": []string{"sub", "email", "groups"},
	})

	res := probeAgainst(t, srv, Probe{
		Issuer:      srv.URL,
		ClientID:    "panorama",
		Scopes:      []string{"openid", "groups"},
		GroupsClaim: "groups",
	})

	if !res.OK || Worst(res.Findings) != LevelOK {
		t.Fatalf("want a clean probe, got %q from %+v", Worst(res.Findings), res.Findings)
	}
}

// TestMissingMetadataIsUnknownRatherThanEmpty.
//
// scopes_supported and claims_supported are both optional. A provider that
// publishes neither supports neither nothing nor everything - it has told us
// nothing - and asserting a problem from absent information is how a check
// earns a reputation for crying wolf.
func TestMissingMetadataIsUnknownRatherThanEmpty(t *testing.T) {
	srv := discoveryServer(t, map[string]any{})

	res := probeAgainst(t, srv, Probe{
		Issuer:      srv.URL,
		ClientID:    "panorama",
		Scopes:      []string{"openid", "email"},
		GroupsClaim: "groups",
	})

	if !res.OK {
		t.Fatalf("silence about scopes is not a failure: %+v", res.Findings)
	}
	for _, f := range res.Findings {
		if f.Level == LevelWarn && strings.Contains(f.Title, "does not advertise") {
			t.Fatalf("claimed a problem from absent metadata: %+v", f)
		}
	}
}

// TestScopesWithoutOpenIDFail. Without it there is no ID token, and an ID token
// is the entire mechanism.
func TestScopesWithoutOpenIDFail(t *testing.T) {
	res := Run(context.Background(), Probe{
		Issuer:      "https://idp.example.com",
		Scopes:      []string{"profile", "email"},
		GroupsClaim: "groups",
	})
	if res.OK {
		t.Fatal("a scope list without openid was accepted")
	}
}

// TestNoRendererEmitsTheClientSecret. A rendered artifact lands in a terminal
// scrollback, a screenshot, or a browser history entry, and a secret should be
// recoverable from none of them.
func TestNoRendererEmitsTheClientSecret(t *testing.T) {
	conn := Connection{
		Issuer:      "https://idp.example.com",
		ClientID:    "panorama",
		Scopes:      []string{"openid", "groups"},
		GroupsClaim: "groups",
		RedirectURI: "https://panorama.example.com/auth/oidc/callback",
	}

	for _, format := range Formats() {
		t.Run(format, func(t *testing.T) {
			body, err := Render(format, conn)
			if err != nil {
				t.Fatal(err)
			}
			// Every artifact that is an instruction sheet must state the
			// redirect URI: it is the value that has to be registered at the
			// provider and the most common thing to get wrong.
			//
			// ECS is exempt because its artifact is not an instruction sheet.
			// It is the value of a Secrets Manager secret, pasted into a box
			// that holds JSON, so a line naming the redirect URI would be
			// pasted with it and make the document invalid. Both surfaces that
			// render it state the URI themselves instead - the console in its
			// own copy block, `sso probe` on the line above the artifact.
			if format != FormatECS && !strings.Contains(body, conn.RedirectURI) {
				t.Errorf("%s does not state the redirect URI", format)
			}
			// The other half of that exemption, asserted rather than assumed:
			// the ECS artifact has to parse, or the paste it exists for fails.
			if format == FormatECS {
				var into map[string]string
				if jerr := json.Unmarshal([]byte(body), &into); jerr != nil {
					t.Errorf("the ecs artifact is pasted into a JSON field and does not parse: %v", jerr)
				}
				for _, want := range []string{"issuer", "client_id", "client_secret", "scopes", "groups_claim"} {
					if _, ok := into[want]; !ok {
						t.Errorf("the ecs artifact has no %q key, so the secret would be missing a field the task definition reads", want)
					}
				}
			}
			if !strings.Contains(body, conn.Issuer) {
				t.Errorf("%s does not carry the issuer", format)
			}
			// The secret appears only as a placeholder or an indirection.
			for _, forbidden := range []string{"hunter2", "s3cr3t"} {
				if strings.Contains(body, forbidden) {
					t.Errorf("%s leaked a secret", format)
				}
			}
		})
	}

	if _, err := Render("terraform", conn); err == nil {
		t.Fatal("an unknown format was rendered")
	}
}

// TestDoctorCatchesTheSilentLockout. An empty mapping with a default of "none"
// authenticates everybody and then refuses them, which reads as a broken
// identity provider rather than a setting.
func TestDoctorCatchesTheSilentLockout(t *testing.T) {
	findings := checkMapping(Facts{MappedGroups: nil, DefaultRole: "none"})
	if len(findings) == 0 || findings[0].Level != LevelFail {
		t.Fatalf("want a failure, got %+v", findings)
	}
}

// TestDoctorFlagsAMappedGroupNobodyHasEverBeenIn.
//
// The silent half of role mapping: a typo grants nothing and looks configured.
func TestDoctorFlagsAMappedGroupNobodyHasEverBeenIn(t *testing.T) {
	findings := checkMapping(Facts{
		ObservedGroups: []string{"platform", "engineering"},
		MappedGroups:   []string{"platfrm"},
		DefaultRole:    "developer",
	})

	var warned bool
	for _, f := range findings {
		if f.Level == LevelWarn && strings.Contains(f.Detail, "platfrm") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("a mapped group nobody has been seen in went unreported: %+v", findings)
	}
}

// TestDoctorReportsGroupsThatNeverArrive. Distinguished from "some sign-ins had
// no groups", which is ordinary, because none-ever is a misconfiguration and
// some is a person in no group.
func TestDoctorReportsGroupsThatNeverArrive(t *testing.T) {
	d := Deployment{GroupsClaim: "groups"}

	never := checkGroupsArriving(d, Facts{SSOSignIns: 12, ObservedGroups: nil, DefaultRole: "developer"})
	if len(never) != 1 || never[0].Level != LevelFail {
		t.Fatalf("want a failure when no sign-in ever carried a group, got %+v", never)
	}

	some := checkGroupsArriving(d, Facts{
		SSOSignIns: 12, SignInsWithoutGroups: 2,
		ObservedGroups: []string{"platform"}, DefaultRole: "developer",
	})
	if len(some) != 1 || some[0].Level != LevelWarn {
		t.Fatalf("want a warning when only some sign-ins lacked groups, got %+v", some)
	}

	// Nothing to say before the first sign-in: the question is unanswerable,
	// and checkRoundTrip already covers it.
	if got := checkGroupsArriving(d, Facts{SSOSignIns: 0}); got != nil {
		t.Fatalf("want silence before any sign-in, got %+v", got)
	}
}

// TestBaseURLDrivesTheRedirectURICheck. The redirect happens in the user's
// browser, so a base_url left at localhost is correct for an evaluation and
// wrong for anything anyone else signs in to.
func TestBaseURLDrivesTheRedirectURICheck(t *testing.T) {
	for name, tc := range map[string]struct {
		baseURL string
		want    string
	}{
		"localhost warns":        {"http://localhost:8088", LevelWarn},
		"plaintext remote fails": {"http://panorama.example.com", LevelFail},
		"https remote is fine":   {"https://panorama.example.com", LevelOK},
		"not a url fails":        {"::not-a-url", LevelFail},
	} {
		t.Run(name, func(t *testing.T) {
			got := checkBaseURL(Deployment{BaseURL: tc.baseURL, RedirectURI: tc.baseURL + "/auth/oidc/callback"})
			if len(got) != 1 || got[0].Level != tc.want {
				t.Fatalf("want %s, got %+v", tc.want, got)
			}
		})
	}
}

// TestWorstIsTheLevelAnExitCodeComesFrom.
func TestWorstIsTheLevelAnExitCodeComesFrom(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []Finding
		want string
	}{
		"empty is ok":      {nil, LevelOK},
		"a warning wins":   {[]Finding{{Level: LevelOK}, {Level: LevelWarn}}, LevelWarn},
		"a failure wins":   {[]Finding{{Level: LevelWarn}, {Level: LevelFail}}, LevelFail},
		"order is nothing": {[]Finding{{Level: LevelFail}, {Level: LevelOK}}, LevelFail},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Worst(tc.in); got != tc.want {
				t.Fatalf("want %s, got %s", tc.want, got)
			}
		})
	}
}

// TestEveryPresetIsRenderable guards the thing a new preset would break: a
// preset with no scopes, or one whose scopes omit openid, produces a
// configuration that cannot work.
func TestEveryPresetIsRenderable(t *testing.T) {
	for _, p := range Presets() {
		t.Run(p.ID, func(t *testing.T) {
			if !slices.Contains(p.Scopes, "openid") {
				t.Error("scopes must include openid")
			}
			if p.GroupsClaim == "" {
				t.Error("a preset needs a groups claim, even where the provider will not emit one")
			}
			if len(p.Notes) == 0 {
				t.Error("a preset with no notes carries nothing a scope list does not")
			}

			if !strings.HasPrefix(p.IssuerHint, "https://") {
				t.Error("the issuer hint should model a valid issuer")
			}
		})
	}
}

// TestAGoogleConnectionIsRenderedWithTheDomainItNeeds.
//
// Configuration validation refuses a Google issuer with no allowed domain, so
// a rendered configuration without the key is a screen handing out something
// that does not boot. The placeholder is shaped so that validation refuses it
// too: pasted without being read, it fails at startup naming the key, and does
// not start up limited to a domain nobody owns.
func TestAGoogleConnectionIsRenderedWithTheDomainItNeeds(t *testing.T) {
	google := Connection{Issuer: "https://accounts.google.com", ClientID: "panorama",
		Scopes: []string{"openid", "profile", "email"}, GroupsClaim: "groups",
		RedirectURI: "https://panorama.example.com/auth/oidc/callback"}
	tenant := google
	tenant.Issuer = "https://your-org.okta.com"

	for format, want := range map[string]string{
		FormatEnv:   "\nPANORAMA_OIDC_ALLOWED_DOMAINS=" + domainPlaceholder + "\n",
		FormatShell: "\nexport PANORAMA_OIDC_ALLOWED_DOMAINS=" + domainPlaceholder + "\n",
		FormatYAML:  "\n  allowed_domains: [\"" + domainPlaceholder + "\"]\n",
	} {
		body, err := Render(format, google)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body, want) {
			t.Errorf("%s: a Google connection was rendered without its allowed domain:\n%s", format, body)
		}
		body, err = Render(format, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, "ALLOWED_DOMAINS") || strings.Contains(body, "allowed_domains") {
			t.Errorf("%s: an issuer that names a tenant was told to limit its domains:\n%s", format, body)
		}
	}

	// The ECS artifact is the value of a secret the stack wrote once, and ECS
	// refuses to start a task over a JSON key an older stack's secret lacks, so
	// the domain is a stack parameter and the artifact stays as it was.
	body, err := Render(FormatECS, google)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "allowed_domains") {
		t.Errorf("the ECS secret gained a key existing stacks do not have:\n%s", body)
	}

	cfg := config.Default()
	cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.ClientSecret = google.Issuer, "panorama", "s3cret"
	cfg.OIDC.AllowedDomains = []string{domainPlaceholder}
	if err := cfg.Validate(); err == nil {
		t.Error("the placeholder domain validated; pasted unread, it would limit sign-in to a domain nobody owns")
	}
}

// TestIdentityCenterIsNotOffered.
//
// AWS IAM Identity Center is not an OIDC provider for third-party applications:
// its OAuth 2.0 support is trusted identity propagation, sign-in for customer
// managed applications is SAML only, and it has no group attribute it can put
// in an assertion. Offering it as a preset would send the person most likely to
// reach for it - whoever is configuring a Bedrock gateway - into a console
// wizard that ends without a client id.
func TestIdentityCenterIsNotOffered(t *testing.T) {
	if _, ok := PresetByID("aws-identity-center"); ok {
		t.Fatal("Identity Center is offered as an OIDC preset, and cannot work as one")
	}
}

// TestAnIssuerMismatchIsNamedAsOne.
//
// go-oidc reports this as "issuer URL provided to client … did not match the
// issuer URL returned by provider", which does not contain "issuer did not
// match": a pattern looking for that falls through to a generic remedy against
// a live Entra endpoint. The wording here is the real one. Entra's multi-tenant endpoints get their own answer,
// because a templated {tenantid} issuer cannot match anything and it is the
// first URL an administrator tries.
func TestAnIssuerMismatchIsNamedAsOne(t *testing.T) {
	realWording := errors.New(`oidc: issuer URL provided to client ("https://login.microsoftonline.com/common/v2.0") ` +
		`did not match the issuer URL returned by provider ("https://login.microsoftonline.com/{tenantid}/v2.0")`)

	generic := discoveryFailure("https://idp.example.com", realWording)
	if generic.Title != "Issuer mismatch" {
		t.Fatalf("want an issuer mismatch, got %q", generic.Title)
	}
	if strings.Contains(generic.Remedy, "tenant id") {
		t.Error("the Entra-specific remedy leaked onto a non-Entra issuer")
	}

	entra := discoveryFailure("https://login.microsoftonline.com/common/v2.0", realWording)
	if entra.Title != "Issuer mismatch" {
		t.Fatalf("want an issuer mismatch, got %q", entra.Title)
	}
	if !strings.Contains(entra.Remedy, "tenant id") {
		t.Errorf("a multi-tenant endpoint should be named as one, got %q", entra.Remedy)
	}
}

// TestNotesNameTheRedirectURIRatherThanDescribingIt.
//
// "Add Panorama's redirect URI" sends the reader looking for a value Panorama
// already knows: it comes from server.base_url and nothing about it depends on
// the provider. Registering it wrongly is the most common way a first sign-in
// fails, and it fails at the provider rather than here.
func TestNotesNameTheRedirectURIRatherThanDescribingIt(t *testing.T) {
	const uri = "https://panorama.example.com/auth/oidc/callback"

	var carried int
	for _, p := range Presets() {
		notes := p.NotesWith(uri)
		if len(notes) != len(p.Notes) {
			t.Fatalf("%s: NotesWith dropped a note", p.ID)
		}
		for _, n := range notes {
			if strings.Contains(n, RedirectURIPlaceholder) {
				t.Errorf("%s: a raw placeholder reached a reader: %q", p.ID, n)
			}
		}
		if strings.Contains(strings.Join(notes, " "), uri) {
			carried++
		}
	}

	// Not every provider's notes need to mention it, but the ones that describe
	// registering an application must. If this drops to zero, the substitution
	// has been disconnected without anything else failing.
	if carried == 0 {
		t.Fatal("no preset names the redirect URI; the substitution is not reaching any note")
	}
}

// TestNotesSurviveAnUnknownRedirectURI. Diagnose and any caller without a
// configured base URL still render a readable sentence.
func TestNotesSurviveAnUnknownRedirectURI(t *testing.T) {
	preset, _ := PresetByID("entra-id")
	for _, n := range preset.NotesWith("") {
		if strings.Contains(n, RedirectURIPlaceholder) {
			t.Errorf("an empty redirect URI left the placeholder in place: %q", n)
		}
		if strings.Contains(n, "  ") {
			t.Errorf("an empty redirect URI left a gap mid-sentence: %q", n)
		}
	}
}

// TestRedirectURIHasOneDerivation. It was briefly three - the server, the
// probe, and the doctor - and a value that must match a provider's own
// configuration character for character is the worst kind to derive twice.
func TestRedirectURIHasOneDerivation(t *testing.T) {
	for name, tc := range map[string]struct{ baseURL, want string }{
		"plain":          {"https://panorama.example.com", "https://panorama.example.com" + CallbackPath},
		"trailing slash": {"https://panorama.example.com/", "https://panorama.example.com" + CallbackPath},
		"with a port":    {"http://localhost:8088", "http://localhost:8088" + CallbackPath},
		"unset":          {"", ""},
		"blank":          {"   ", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RedirectURI(tc.baseURL); got != tc.want {
				t.Fatalf("RedirectURI(%q) = %q, want %q", tc.baseURL, got, tc.want)
			}
		})
	}
}

// TestAnAbsentClaimIsInconclusiveNotNegative.
//
// claims_supported describes what a provider MAY supply and is not
// per-application, so a claim configured on one application never appears in
// it. A title like "provider does not advertise a groups claim" reads as a
// report that the claim has not been configured, and is shown to somebody who
// has just configured it correctly. Absence of evidence.
func TestAnAbsentClaimIsInconclusiveNotNegative(t *testing.T) {
	f := checkGroupsClaim("https://idp.example.com", "groups", []string{"sub", "email"})

	if f.Level != LevelWarn {
		t.Fatalf("want a warning, got %q", f.Level)
	}
	if strings.Contains(strings.ToLower(f.Title), "does not advertise") {
		t.Errorf("the title still asserts absence: %q", f.Title)
	}
	if !strings.Contains(f.Detail, "not evidence") {
		t.Errorf("the detail should say absence is not evidence, got %q", f.Detail)
	}
	if !strings.Contains(f.Remedy, "sso claims") {
		t.Errorf("the remedy should point at the only thing that settles it, got %q", f.Remedy)
	}
}

// TestEntraGetsItsOwnAnswer.
//
// Microsoft publishes one static claims list for the whole platform - verified
// against a real tenant, which advertises nineteen claims and no groups
// whatever the application is configured to emit. So for Entra the check is not
// merely inconclusive, it is structurally incapable of an answer, and the
// remedy is the failure that actually happens: a groups claim ticked for the
// access token but not the ID token, which is the one Panorama reads.
func TestEntraGetsItsOwnAnswer(t *testing.T) {
	// The real list, from login.microsoftonline.com.
	entraClaims := []string{
		"sub", "iss", "cloud_instance_name", "cloud_instance_host_name",
		"cloud_graph_host_name", "msgraph_host", "aud", "exp", "iat", "auth_time",
		"acr", "nonce", "preferred_username", "name", "tid", "ver", "at_hash",
		"c_hash", "email",
	}

	f := checkGroupsClaim(
		"https://login.microsoftonline.com/60c1e496-337d-46fb-b3e6-c697d4f142c7/v2.0",
		"groups", entraClaims)

	if f.Level != LevelWarn {
		t.Fatalf("want a warning, got %q", f.Level)
	}
	if !strings.Contains(f.Remedy, "ID token") {
		t.Errorf("the Entra remedy must name the ID token, got %q", f.Remedy)
	}
	if strings.Contains(f.Detail, "sub, iss") {
		t.Error("listing Microsoft's fixed platform claims back at the reader is noise")
	}

	// A non-Microsoft issuer with the same claim list keeps the generic answer.
	other := checkGroupsClaim("https://idp.example.com", "groups", entraClaims)
	if strings.Contains(other.Remedy, "ID token") {
		t.Error("the Entra-specific remedy leaked onto another provider")
	}
}

// TestSovereignCloudsAreEntraToo. They publish the same static list, so the
// same answer applies.
func TestSovereignCloudsAreEntraToo(t *testing.T) {
	for _, issuer := range []string{
		"https://login.microsoftonline.com/tenant/v2.0",
		"https://login.microsoftonline.us/tenant/v2.0",
		"https://login.partner.microsoftonline.cn/tenant/v2.0",
		"https://sts.windows.net/tenant/",
	} {
		if !isMicrosoftIssuer(issuer) {
			t.Errorf("%s should be recognised as Entra", issuer)
		}
	}
	for _, issuer := range []string{
		"https://idp.example.com",
		"https://acme.okta.com",
		// A host that merely begins with a Microsoft one. Matching this would
		// only mean printing the wrong advice, but a matcher that is wrong in a
		// harmless place is one somebody later copies into a harmful one.
		"https://login.microsoftonline.com.example.net/tenant/v2.0",
		"https://notlogin.microsoftonline.com/tenant/v2.0",
	} {
		if isMicrosoftIssuer(issuer) {
			t.Errorf("%s should not be recognised as Entra", issuer)
		}
	}
}
