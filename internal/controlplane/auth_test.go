// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
)

// newBareConsole is a console with a migrated database and no users at all -
// the state a fresh deployment is actually in.
func newBareConsole(t *testing.T) (*Server, *store.DB) {
	t.Helper()

	cfg := config.Default()
	cfg.Database.DSN = "file:" + filepath.Join(t.TempDir(), "panorama.db")

	db, err := store.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if merr := db.Migrate(t.Context()); merr != nil {
		t.Fatalf("migrate: %v", merr)
	}

	srv := New(cfg, db, discardLogger())
	seed, err := pricing.Seed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	engine := policy.New(policy.Options{Repository: db, ReloadInterval: -1, Logger: discardLogger()})
	if lerr := engine.Load(t.Context()); lerr != nil {
		t.Fatalf("load policy: %v", lerr)
	}
	srv.MountConsole(ConsoleOptions{Prices: pricing.NewTable(seed), Policy: engine})

	return srv, db
}

// post issues a JSON request against a server and returns the recorder.
func post(t *testing.T, srv *Server, path string, body any, mutate func(*http.Request)) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	var out map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

// TestFirstRunOpensASetupSessionAndCreatesNobody walks the circle-breaker: a
// deployment with no users, a code in the log, and a session on the other side
// of it that configures the deployment without becoming an account.
func TestFirstRunOpensASetupSessionAndCreatesNobody(t *testing.T) {
	srv, db := newBareConsole(t)

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/state", nil))

	var state map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state["setup_required"] != true {
		t.Fatalf("state = %v, want a deployment with no users to say so", state)
	}

	code, err := IssueSetupCode(t.Context(), db)
	if err != nil {
		t.Fatalf("IssueSetupCode: %v", err)
	}

	rec, body := post(t, srv, "/api/v1/auth/setup", map[string]string{"code": code}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem = %d %s, want 200", rec.Code, rec.Body.String())
	}

	user, _ := body["user"].(map[string]any)
	if user["role"] != store.RoleSetup {
		t.Errorf("role = %v, want the setup role", user["role"])
	}
	if user["id"] != "" || user["email"] != "" {
		t.Errorf("a setup session rendered an identity: %v", user)
	}
	if body["csrf_token"] == "" || body["csrf_token"] == nil {
		t.Error("no csrf token was handed to the page that just opened a session")
	}

	// The code is spent.
	rec, _ = post(t, srv, "/api/v1/auth/setup", map[string]string{"code": code}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("redeeming twice = %d, want 401 - the code is single-use", rec.Code)
	}

	// And the whole point: nobody was created. The first account arrives from
	// the identity provider this session exists to configure.
	users, err := db.CountUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Errorf("%d user(s) exist, want none - a setup session is not an account", users)
	}
}

// TestFirstRunRefusesEverythingWrongIdentically. Telling a caller that the code
// was right but already claimed, or that none is outstanding, narrows the
// search for anyone trying codes.
func TestFirstRunRefusesEverythingWrongIdentically(t *testing.T) {
	srv, db := newBareConsole(t)

	good, err := IssueSetupCode(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}

	var answers []string
	for _, code := range []string{
		"",
		"not-a-code",
		"pnb_" + strings.Repeat("A", 43),
		good[:len(good)-1] + "x",
	} {
		rec, body := post(t, srv, "/api/v1/auth/setup", map[string]string{"code": code}, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("claim with %q = %d, want 401", code, rec.Code)
		}
		message, _ := body["error"].(string)
		answers = append(answers, message)
	}

	for _, answer := range answers[1:] {
		if answer != answers[0] {
			t.Errorf("two wrong codes got different answers:\n  %q\n  %q", answers[0], answer)
		}
	}
}

// TestTheSetupCodeEndpointIsRateLimited keeps an unauthenticated endpoint from being a
// free way to make a deployment do database work.
func TestTheSetupCodeEndpointIsRateLimited(t *testing.T) {
	srv, _ := newBareConsole(t)

	var limited bool
	for range 20 {
		rec, _ := post(t, srv, "/api/v1/auth/setup", map[string]string{
			"code": "pnb_" + strings.Repeat("A", 43),
		}, nil)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("twenty claim attempts from one address were all served")
	}
}

// TestPrepareSetupDestroysTheCodeOnceThereAreUsers.
func TestPrepareSetupDestroysTheCodeOnceThereAreUsers(t *testing.T) {
	_, db := newBareConsole(t)
	ctx := t.Context()

	if err := PrepareSetup(ctx, db, discardLogger()); err != nil {
		t.Fatalf("PrepareSetup: %v", err)
	}
	if _, err := db.Setting(ctx, setupCodeKey); err != nil {
		t.Fatalf("no code was issued for a deployment with no users: %v", err)
	}

	// Running again leaves the outstanding code alone, so a restart does not
	// invalidate one an operator is halfway through using.
	before, berr := db.Setting(ctx, setupCodeKey)
	if berr != nil {
		t.Fatal(berr)
	}
	if perr := PrepareSetup(ctx, db, discardLogger()); perr != nil {
		t.Fatal(perr)
	}
	after, aerr := db.Setting(ctx, setupCodeKey)
	if aerr != nil {
		t.Fatal(aerr)
	}
	if before != after {
		t.Error("a restart replaced an outstanding setup code")
	}

	if _, err := db.CreateUser(ctx, "someone@example.com", "Someone", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSetup(ctx, db, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Setting(ctx, setupCodeKey); err == nil {
		t.Error("the setup code survived the deployment gaining a user")
	}
}

// TestCookieWritesNeedTheCSRFToken. Cookie authentication is ambient: the
// browser attaches it to a request the page did not make.
func TestCookieWritesNeedTheCSRFToken(t *testing.T) {
	srv, db := newBareConsole(t)

	cookie, csrf := consoleSessionFor(t, srv, db, "admin@example.com", store.RoleAdmin)

	t.Run("without the header", func(t *testing.T) {
		rec, _ := post(t, srv, "/api/v1/auth/signout", map[string]string{}, func(r *http.Request) {
			r.AddCookie(cookie)
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("write without a csrf token = %d, want 403", rec.Code)
		}
	})

	t.Run("with the wrong header", func(t *testing.T) {
		rec, _ := post(t, srv, "/api/v1/auth/signout", map[string]string{}, func(r *http.Request) {
			r.AddCookie(cookie)
			r.Header.Set(csrfHeader, "not-the-token")
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("write with a wrong csrf token = %d, want 403", rec.Code)
		}
	})

	t.Run("reads never need it", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("read with no csrf token = %d, want 200", rec.Code)
		}
	})

	t.Run("with the right header", func(t *testing.T) {
		rec, _ := post(t, srv, "/api/v1/auth/signout", map[string]string{}, func(r *http.Request) {
			r.AddCookie(cookie)
			r.Header.Set(csrfHeader, csrf)
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("write with the csrf token = %d %s, want 200", rec.Code, rec.Body.String())
		}

		// And the session is genuinely gone afterwards.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.AddCookie(cookie)
		after := httptest.NewRecorder()
		srv.mux.ServeHTTP(after, req)
		if after.Code != http.StatusUnauthorized {
			t.Errorf("a signed-out cookie still authenticates: %d", after.Code)
		}
	})
}

// TestGatewayTokensNeedNoCSRF. A token is never attached by a browser on
// someone else's behalf, so there is nothing to forge.
func TestGatewayTokensNeedNoCSRF(t *testing.T) {
	f := newConsole(t)

	rec, _ := post(t, f.server, "/api/v1/auth/signout", map[string]string{}, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+f.devToken)
	})
	if rec.Code != http.StatusOK {
		t.Errorf("token-authenticated write = %d, want 200", rec.Code)
	}
}

// TestDisablingAUserEndsTheirSessions. A role or status held in a live session
// would otherwise outlive the change that revoked it.
func TestDisablingAUserEndsTheirSessions(t *testing.T) {
	srv, db := newBareConsole(t)

	cookie, _ := consoleSessionFor(t, srv, db, "admin@example.com", store.RoleAdmin)

	user, err := db.UserByEmail(t.Context(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserStatus(t.Context(), user.ID, "disabled"); err != nil {
		t.Fatalf("SetUserStatus: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.AddCookie(cookie)
	after := httptest.NewRecorder()
	srv.mux.ServeHTTP(after, req)
	if after.Code != http.StatusUnauthorized {
		t.Errorf("a disabled user's session still works: %d", after.Code)
	}
}

// TestSSOEndpointsAreAbsentWhenSSOIsNot. A console that offered a sign-in
// button leading to a 500 would be worse than one that offered none.
func TestSSOEndpointsAreAbsentWhenSSOIsNot(t *testing.T) {
	srv, _ := newBareConsole(t)

	for _, path := range []string{"/auth/oidc/start", "/auth/oidc/callback"} {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s with no issuer configured = %d, want 404", path, rec.Code)
		}
	}
}

func TestSafeRedirectRefusesEverythingButAPath(t *testing.T) {
	for candidate, want := range map[string]string{
		"/work":                  "/work",
		"/work?tab=ticket":       "/work?tab=ticket",
		"":                       "",
		"//evil.example":         "",
		"https://evil.example":   "",
		"http://evil.example/x":  "",
		"/\\evil.example":        "",
		"/work\nLocation: /evil": "",
		"javascript:alert(1)":    "",
	} {
		if got := safeRedirect(candidate); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", candidate, got, want)
		}
	}
}

// TestGroupsClaimIsReadInEveryShapeProvidersSendIt. Entra sends a bare string
// when a user is in one group; some providers send a space-separated list. A
// deployment whose admins silently never get their role is a support call
// nobody enjoys.
func TestGroupsClaimIsReadInEveryShapeProvidersSendIt(t *testing.T) {
	for name, tc := range map[string]struct {
		claim any
		want  []string
	}{
		"a list":             {claim: []any{"platform", "eng"}, want: []string{"platform", "eng"}},
		"a bare string":      {claim: "platform", want: []string{"platform"}},
		"a separated string": {claim: "platform eng", want: []string{"platform", "eng"}},
		"absent":             {claim: nil, want: nil},
		"a number":           {claim: float64(3), want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := stringsClaim(tc.claim)
			if len(got) != len(tc.want) {
				t.Fatalf("stringsClaim(%v) = %v, want %v", tc.claim, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("stringsClaim(%v)[%d] = %q, want %q", tc.claim, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// consoleSessionFor signs a user in the way the OIDC callback does, without the
// round trip.
//
// It exists because a setup code does not produce one: redeeming a code opens
// a setup session, which is not an account and cannot reach the routes these
// tests are about. A console session only ever comes from signing in.
func consoleSessionFor(t *testing.T, srv *Server, db *store.DB, email, role string) (*http.Cookie, string) {
	t.Helper()

	user, err := db.EnsureUser(t.Context(), email, email, role)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}

	secret, err := auth.NewSecret(auth.SessionPrefix)
	if err != nil {
		t.Fatalf("mint session secret: %v", err)
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		t.Fatalf("mint csrf token: %v", err)
	}

	if _, err := db.CreateConsoleSession(t.Context(), user.ID, secret.Hash, csrf,
		"test", "127.0.0.1", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create console session: %v", err)
	}

	return &http.Cookie{Name: srv.cookieName(sessionCookie), Value: secret.Plaintext}, csrf
}

// TestASignInFromOutsideTheAllowedDomainsIsRefused.
//
// Which claim carries the domain depends on who issued the token. At Google it
// is `hd` and never the address: anybody may open a personal Google account on
// a work address, and it outlives the mailbox, so the account of somebody who
// left still says email_verified on the company's domain years later. `hd` is
// set only on an account the Workspace organization manages. Elsewhere the
// tenant already decided who may sign in, and the list narrows it by address.
func TestASignInFromOutsideTheAllowedDomainsIsRefused(t *testing.T) {
	google := config.OIDC{Issuer: "https://accounts.google.com", AllowedDomains: []string{"example.com"}}
	tenant := config.OIDC{Issuer: "https://your-org.okta.com", AllowedDomains: []string{"example.com"}}
	open := config.OIDC{Issuer: "https://your-org.okta.com"}

	for _, tc := range []struct {
		name    string
		cfg     config.OIDC
		claims  idClaims
		refused bool
	}{
		{"google, a managed account", google,
			idClaims{Email: "ana@example.com", HostedDomain: "example.com"}, false},
		{"google, hd in another case", google,
			idClaims{Email: "ana@example.com", HostedDomain: "Example.COM"}, false},
		{"google, somebody else's workspace", google,
			idClaims{Email: "eve@other.example", HostedDomain: "other.example"}, true},
		{"google, a personal account on a gmail address", google,
			idClaims{Email: "eve@gmail.com"}, true},
		// The one the address alone would have let in.
		{"google, a personal account on the company's own domain", google,
			idClaims{Email: "left-last-year@example.com"}, true},
		// A secondary domain of the same Workspace: hd is the organization, and
		// the organization is what was allowed.
		{"google, a managed account on an alias domain", google,
			idClaims{Email: "ana@example.com.au", HostedDomain: "example.com"}, false},

		{"tenant, inside", tenant, idClaims{Email: "ana@example.com"}, false},
		{"tenant, address in another case", tenant, idClaims{Email: "Ana@EXAMPLE.com"}, false},
		{"tenant, outside", tenant, idClaims{Email: "guest@partner.example"}, true},
		{"tenant, a subdomain is a different domain", tenant, idClaims{Email: "ana@eu.example.com"}, true},
		{"tenant, no address at all", tenant, idClaims{}, true},
		{"tenant, an address the provider will not vouch for", tenant,
			idClaims{Email: "ana@example.com", EmailUnverified: true}, true},
		// An hd claim proves nothing at an issuer that is not Google's: any
		// provider can be configured to emit a claim called hd.
		{"tenant, hd is not read", tenant,
			idClaims{Email: "guest@partner.example", HostedDomain: "example.com"}, true},

		{"no list allows everybody", open, idClaims{Email: "anyone@anywhere.example"}, false},
		{"no list, and nothing to read", open, idClaims{}, false},
	} {
		reason := domainRefusal(tc.cfg, &tc.claims)
		if got := reason != ""; got != tc.refused {
			t.Errorf("%s: refused = %v (%q), want %v", tc.name, got, reason, tc.refused)
		}
	}
}

// Providers disagree about the type of email_verified: most send a boolean and
// Cognito sends a string. Silence is not a denial - Entra ID sends nothing.
func TestOnlyAnOutrightNoMarksAnEmailUnverified(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  bool
	}{
		{false, true}, {"false", true}, {"False", true},
		{true, false}, {"true", false}, {nil, false}, {"", false}, {0, false},
	} {
		if got := claimIsFalse(tc.value); got != tc.want {
			t.Errorf("claimIsFalse(%#v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestTheFirstSignInOwnsTheDeployment.
//
// The setup flow creates no account, and the group mapping that would grant
// admin is written in group names nothing has seen until somebody signs in. So
// the first sign-in has to carry admin itself, or a freshly configured
// deployment has no administrator and no way to appoint one.
func TestTheFirstSignInOwnsTheDeployment(t *testing.T) {
	srv, db := newBareConsole(t)
	ctx := t.Context()

	// No mapping, and the strictest default there is: even so, the first person
	// through is admitted and owns the deployment.
	if err := db.SetSetting(ctx, settingDefaultRole, roleNone); err != nil {
		t.Fatal(err)
	}

	first, _, err := srv.provision(ctx, &idClaims{
		Subject: "sub-1", Email: "first@example.com", DisplayName: "First",
	})
	if err != nil {
		t.Fatalf("the first sign-in was refused: %v", err)
	}
	if first.Role != store.RoleAdmin {
		t.Fatalf("first role = %q, want admin", first.Role)
	}

	// And the window is shut behind them. The second person is resolved
	// normally, which with this mapping and default means refused.
	_, _, err = srv.provision(ctx, &idClaims{
		Subject: "sub-2", Email: "second@example.com", DisplayName: "Second",
	})
	if !errors.Is(err, errNotPermitted) {
		t.Fatalf("the second sign-in = %v, want errNotPermitted - the window is one deployment wide", err)
	}
}

// TestTheOwnerIsStillTheOwnerOnTheirSecondSignIn.
//
// Reported from a live deployment: the account granted the deployment was a
// developer again the next time it signed in, because the grant was recorded as
// a mapping result and every sign-in recomputes those. The window closing is
// about who else may claim ownership, not about the owner keeping it.
func TestTheOwnerIsStillTheOwnerOnTheirSecondSignIn(t *testing.T) {
	srv, _ := newBareConsole(t)
	ctx := t.Context()

	owner := &idClaims{Subject: "sub-1", Email: "owner@example.com", DisplayName: "Owner"}

	first, wasFirst, err := srv.provision(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if !wasFirst {
		t.Error("the first sign-in did not report itself as first, so it lands on the overview rather than on setup")
	}
	if first.Role != store.RoleAdmin {
		t.Fatalf("first role = %q, want admin", first.Role)
	}

	again, wasFirst, err := srv.provision(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if again.Role != store.RoleAdmin {
		t.Errorf("role on the second sign-in = %q, want admin - the owner cannot be demoted by signing in", again.Role)
	}
	if wasFirst {
		t.Error("a second sign-in reported itself as first")
	}
}

// TestTheFirstSignInDoesNotOverrideAMappingThatGrantsMore, so a deployment set
// up by someone who mapped their group in advance is unaffected.
func TestASecondSignInResolvesNormally(t *testing.T) {
	srv, db := newBareConsole(t)
	ctx := t.Context()

	if _, _, err := srv.provision(ctx, &idClaims{Subject: "sub-1", Email: "first@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGroupRole(ctx, "platform", store.RoleManager); err != nil {
		t.Fatal(err)
	}

	second, _, err := srv.provision(ctx, &idClaims{
		Subject: "sub-2", Email: "second@example.com", Groups: []string{"platform"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Role != store.RoleManager {
		t.Errorf("second role = %q, want the mapping to decide (manager)", second.Role)
	}
}
