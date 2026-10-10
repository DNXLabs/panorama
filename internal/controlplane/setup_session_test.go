// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/store"
)

// redeemSetupCode mints a code, redeems it, and returns the cookie and CSRF
// token a setup session holds.
func redeemSetupCode(t *testing.T, srv *Server, db *store.DB) (cookie, csrf string) {
	t.Helper()

	code, err := IssueSetupCode(t.Context(), db)
	if err != nil {
		t.Fatalf("issue setup code: %v", err)
	}

	rec, body := post(t, srv, "/api/v1/auth/setup", map[string]string{"code": code}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem = %d, want 200: %s", rec.Code, rec.Body)
	}

	for _, c := range rec.Result().Cookies() {
		if c.Name == setupCookie {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("redeeming a setup code set no setup cookie")
	}
	csrf, _ = body["csrf_token"].(string)
	if csrf == "" {
		t.Fatal("setup session carried no CSRF token")
	}
	return cookie, csrf
}

func withSetup(cookie, csrf string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: setupCookie, Value: cookie})
		if csrf != "" {
			r.Header.Set(csrfHeader, csrf)
		}
	}
}

func getWith(t *testing.T, srv *Server, path string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// TestASetupSessionReachesNothingButIdentity.
//
// The whole security claim. A setup code is minted by anyone with access to
// the deployment and requires no account, so what it opens has to be confined
// to the thing it exists to repair - and confined by the server, since the
// console is not a control.
//
// The route list is not asserted from a fixture: every route that is not the
// identity configuration must refuse, so a handler added later is refused by
// default rather than quietly reachable.
func TestASetupSessionReachesNothingButIdentity(t *testing.T) {
	srv, db := newBareConsole(t)
	cookie, csrf := redeemSetupCode(t, srv, db)

	t.Run("may read the identity configuration", func(t *testing.T) {
		rec := getWith(t, srv, "/api/v1/identity", withSetup(cookie, csrf))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /identity = %d, want 200: %s", rec.Code, rec.Body)
		}
	})

	// The deployment's own configuration, which is what a setup session is for.
	for _, path := range []string{"/api/v1/setup", "/api/v1/identity/presets"} {
		t.Run("may read "+path, func(t *testing.T) {
			rec := getWith(t, srv, path, withSetup(cookie, csrf))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body)
			}
		})
	}

	t.Run("may write the group mapping", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/identity/groups",
			strings.NewReader(`{"groups":[{"group":"platform","role":"admin"}],"default_role":"developer"}`))
		req.Header.Set("Content-Type", "application/json")
		withSetup(cookie, csrf)(req)

		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT /identity/groups = %d, want 200: %s", rec.Code, rec.Body)
		}
	})

	// Everything else. A setup principal has no user id, so several of these
	// would panic rather than merely leak if they ran at all.
	for _, path := range []string{
		"/api/v1/me",
		"/api/v1/requests",
		"/api/v1/usage",
		"/api/v1/work",
		"/api/v1/sessions",
		"/api/v1/prices",
		"/api/v1/policy",
		"/api/v1/users",
		"/api/v1/teams",
		"/api/v1/settings",
		"/api/v1/tokens",
		"/api/v1/integrations",
		"/api/v1/outcomes",
		"/api/v1/auth/sessions",
		// Which models this deployment permits. A setup session establishes
		// that a model provider can be reached; what may be asked of it is an
		// administrator's decision, and the provider step reads what discovery
		// found from /api/v1/providers rather than from here.
		"/api/v1/models",
	} {
		t.Run("refuses "+path, func(t *testing.T) {
			rec := getWith(t, srv, path, withSetup(cookie, csrf))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("GET %s = %d, want 403", path, rec.Code)
			}
		})
	}
}

// TestASetupSessionCannotChooseModels.
//
// A one-time code proves access to the deployment, which is the right authority
// for "how does this reach a model provider" and the wrong one for "which models does
// this company pay for, and in which country are its prompts processed". The
// second is an administrator's, made once there is an administrator, and stored
// in the database where it can be changed without a deploy.
//
// It may still ask the providers what the account can invoke: that is the read
// the live check picks a model from, and the permissions it needs are exactly
// what the provider step exists to prove.
func TestASetupSessionCannotChooseModels(t *testing.T) {
	srv, db := newBareConsole(t)
	cookie, csrf := redeemSetupCode(t, srv, db)

	send := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		withSetup(cookie, csrf)(req)
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		return rec
	}

	rec := send(http.MethodPut, "/api/v1/routes",
		`{"name":"fast","target":"us.anthropic.claude-haiku-4-5-20251001-v1:0"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a setup session creating a route = %d, want 403: %s", rec.Code, rec.Body)
	}

	if del := send(http.MethodDelete, "/api/v1/routes/fast", ""); del.Code != http.StatusForbidden {
		t.Errorf("a setup session removing a route = %d, want 403: %s", del.Code, del.Body)
	}

	// Discovery is not a decision, and refusing it would leave the Bedrock
	// check with nothing to invoke on the one deployment that most needs it.
	// It fails here for want of a provider, which is a 400 and not a 403.
	if disc := send(http.MethodPost, "/api/v1/models/discover", `{}`); disc.Code == http.StatusForbidden {
		t.Error("a setup session may not ask Bedrock what this account can invoke")
	}
}

// TestASetupSessionCannotMintAGatewayToken. The one that would turn a
// repair credential into spending money.

func TestASetupSessionCannotMintAGatewayToken(t *testing.T) {
	srv, db := newBareConsole(t)
	cookie, csrf := redeemSetupCode(t, srv, db)

	rec, _ := post(t, srv, "/api/v1/tokens", map[string]string{"name": "laptop"},
		withSetup(cookie, csrf))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /tokens = %d, want 403", rec.Code)
	}
}

// TestASetupCodeWorksOnce.
func TestASetupCodeWorksOnce(t *testing.T) {
	srv, db := newBareConsole(t)

	code, err := IssueSetupCode(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}

	if rec, _ := post(t, srv, "/api/v1/auth/setup", map[string]string{"code": code}, nil); rec.Code != http.StatusOK {
		t.Fatalf("first redemption = %d, want 200", rec.Code)
	}
	rec, _ := post(t, srv, "/api/v1/auth/setup", map[string]string{"code": code}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second redemption = %d, want 401", rec.Code)
	}
}

// TestIssuingASetupCodeEndsNothingUntilItIsRedeemed, and redeeming one ends
// any session opened before it. Exactly one should ever be live.
func TestRedeemingEndsAnEarlierSetupSession(t *testing.T) {
	srv, db := newBareConsole(t)

	first, firstCSRF := redeemSetupCode(t, srv, db)
	if rec := getWith(t, srv, "/api/v1/identity", withSetup(first, firstCSRF)); rec.Code != http.StatusOK {
		t.Fatalf("the first session should work before the second is opened, got %d", rec.Code)
	}

	second, secondCSRF := redeemSetupCode(t, srv, db)

	if rec := getWith(t, srv, "/api/v1/identity", withSetup(first, firstCSRF)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the first session survived a second redemption: %d", rec.Code)
	}
	if rec := getWith(t, srv, "/api/v1/identity", withSetup(second, secondCSRF)); rec.Code != http.StatusOK {
		t.Fatalf("the second session does not work: %d", rec.Code)
	}
}

// TestASetupWriteNeedsItsCSRFToken. Cookie authentication is ambient here
// exactly as it is for a console session, and the rule must not be weaker for
// having been written separately.
func TestASetupWriteNeedsItsCSRFToken(t *testing.T) {
	srv, db := newBareConsole(t)
	cookie, csrf := redeemSetupCode(t, srv, db)

	body := `{"groups":[{"group":"platform","role":"admin"}],"default_role":"developer"}`

	req := httptest.NewRequest(http.MethodPut, "/api/v1/identity/groups", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: setupCookie, Value: cookie})
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a write with no CSRF header = %d, want 403", rec.Code)
	}

	// A read is unaffected, as with a console session.
	if got := getWith(t, srv, "/api/v1/identity", withSetup(cookie, "")); got.Code != http.StatusOK {
		t.Fatalf("a read without a CSRF header = %d, want 200", got.Code)
	}

	_ = csrf
}

// TestASetupPrincipalIsNotAnAccount. Nothing about it may render as a
// person: no id, no email, and is_admin false, or a console would show it as
// one and an audit would count it as one.
func TestASetupPrincipalIsNotAnAccount(t *testing.T) {
	srv, db := newBareConsole(t)
	cookie, csrf := redeemSetupCode(t, srv, db)

	rec := getWith(t, srv, "/api/v1/auth/session", withSetup(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/session = %d, want 200", rec.Code)
	}

	var body struct {
		User struct {
			ID      string `json:"id"`
			Email   string `json:"email"`
			Role    string `json:"role"`
			Kind    string `json:"kind"`
			IsAdmin bool   `json:"is_admin"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if body.User.Kind != store.PrincipalKindSetup {
		t.Errorf("kind = %q, want %q", body.User.Kind, store.PrincipalKindSetup)
	}
	if body.User.ID != "" || body.User.Email != "" {
		t.Errorf("a setup session rendered an identity: id=%q email=%q", body.User.ID, body.User.Email)
	}
	if body.User.IsAdmin {
		t.Error("a setup session rendered as an administrator")
	}
	if body.User.Role != store.RoleSetup {
		t.Errorf("role = %q, want %q", body.User.Role, store.RoleSetup)
	}

	// And no user row was created for it, because it is not an account.
	users, err := db.CountUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Errorf("redeeming a setup code created %d user(s)", users)
	}
}

// TestTheSetupRoleCannotBecomeAnAccount.
//
// It is an authorization label on a live principal and nothing else. Every path
// that writes a role to a person goes through ValidRole, and the database
// refuses it besides.
func TestTheSetupRoleCannotBecomeAnAccount(t *testing.T) {
	if store.ValidRole(store.RoleSetup) {
		t.Fatal("the setup role is assignable to a user")
	}

	_, db := newBareConsole(t)

	if err := db.SetGroupRole(t.Context(), "platform", store.RoleSetup); err == nil {
		t.Fatal("a group was mapped to the setup role")
	}
	if _, err := db.EnsureUser(t.Context(), "a@example.com", "A", store.RoleSetup); err == nil {
		t.Fatal("a user was created with the setup role")
	}
}

// TestEndingASetupSessionRevokesIt, rather than leaving it live in a closed
// tab for the rest of its window.
func TestEndingASetupSessionRevokesIt(t *testing.T) {
	srv, db := newBareConsole(t)
	cookie, csrf := redeemSetupCode(t, srv, db)

	rec, _ := post(t, srv, "/api/v1/auth/signout", nil, withSetup(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("sign out = %d, want 200: %s", rec.Code, rec.Body)
	}

	if got := getWith(t, srv, "/api/v1/identity", withSetup(cookie, csrf)); got.Code != http.StatusUnauthorized {
		t.Fatalf("the session survived being ended: %d", got.Code)
	}
}

// TestAMalformedSetupCookieIsRefusedBeforeTheDatabase.
func TestAMalformedSetupCookieIsRefusedBeforeTheDatabase(t *testing.T) {
	srv, _ := newBareConsole(t)

	for _, value := range []string{
		"not-a-token",
		auth.SessionPrefix + strings.Repeat("A", 43),   // a console session's shape
		auth.SetupCodePrefix + strings.Repeat("A", 43), // the code's shape, not the session's
	} {
		rec := getWith(t, srv, "/api/v1/identity", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: setupCookie, Value: value})
		})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("cookie %q = %d, want 401", value, rec.Code)
		}
	}
}

// TestASetupSessionMayFixWhoAdministers.
//
// "The first sign-in owns the deployment" can seat the wrong person - a
// colleague who reached the URL first. Proof of access to the deployment is the
// right authority to correct that, and it is the same authority that could
// already grant admin by mapping a group to it.
func TestASetupSessionMayFixWhoAdministers(t *testing.T) {
	srv, db := newBareConsole(t)
	ctx := t.Context()

	wrong, err := db.EnsureUser(ctx, "wrong@example.com", "Wrong", store.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	right, err := db.EnsureUser(ctx, "right@example.com", "Right", store.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}

	cookie, csrf := redeemSetupCode(t, srv, db)

	// Listing is narrow: four fields and a status, not the people screen.
	rec := getWith(t, srv, "/api/v1/setup/administrators", withSetup(cookie, csrf))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET administrators = %d, want 200: %s", rec.Code, rec.Body)
	}
	var listed struct {
		Users []map[string]any `json:"users"`
	}
	if derr := json.Unmarshal(rec.Body.Bytes(), &listed); derr != nil {
		t.Fatal(derr)
	}
	if len(listed.Users) != 2 {
		t.Fatalf("listed %d users, want 2", len(listed.Users))
	}
	for _, u := range listed.Users {
		for _, leaked := range []string{"tokens", "teams", "last_request_at", "budget_state"} {
			if _, present := u[leaked]; present {
				t.Errorf("the setup listing carries %q, which is none of its business", leaked)
			}
		}
	}

	// Promote the right person first, then demote the wrong one. The last-admin
	// guard makes that order the only one that works, which is the point of it.
	patchAdmin := func(t *testing.T, userID, role string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"user_id": userID, "role": role})
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/setup/administrators", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		withSetup(cookie, csrf)(req)
		out := httptest.NewRecorder()
		srv.mux.ServeHTTP(out, req)
		return out
	}

	if got := patchAdmin(t, right.ID, store.RoleAdmin); got.Code != http.StatusOK {
		t.Fatalf("promoting = %d, want 200: %s", got.Code, got.Body)
	}
	if got := patchAdmin(t, wrong.ID, store.RoleDeveloper); got.Code != http.StatusOK {
		t.Fatalf("demoting = %d, want 200: %s", got.Code, got.Body)
	}

	after, err := db.UserByID(ctx, right.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Role != store.RoleAdmin {
		t.Errorf("the right person is %q, want admin", after.Role)
	}
	demoted, err := db.UserByID(ctx, wrong.ID)
	if err != nil {
		t.Fatal(err)
	}
	if demoted.Role == store.RoleAdmin {
		t.Error("the wrong person is still an administrator")
	}
}

// TestTheLastAdministratorSurvivesASetupSession, so demoting the only admin
// before promoting a replacement is refused rather than leaving nobody.
func TestTheLastAdministratorSurvivesASetupSession(t *testing.T) {
	srv, db := newBareConsole(t)

	only, err := db.EnsureUser(t.Context(), "only@example.com", "Only", store.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := redeemSetupCode(t, srv, db)

	body, _ := json.Marshal(map[string]string{"user_id": only.ID, "role": store.RoleDeveloper})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/setup/administrators", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	withSetup(cookie, csrf)(req)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("demoting the last administrator = %d, want 400", rec.Code)
	}
}

// TestTheSignInGateIssuesASetupCode.
//
// A code minted only at startup, once consumed - by finishing a setup session
// or ending one - could not be replaced without running a one-off task against
// the cluster. The gate mints into that gap.
//
// What the gate must NOT do is replace an outstanding code. It is an
// unauthenticated endpoint reachable by anything that can load the console, so
// minting on every request would let a second tab, a link unfurler or a crawler
// invalidate the code somebody is halfway through typing.
func TestTheSignInGateIssuesASetupCode(t *testing.T) {
	outstanding := func(t *testing.T, db *store.DB) string {
		t.Helper()
		hash, err := db.Setting(t.Context(), setupCodeKey)
		if err != nil {
			t.Fatalf("reading the outstanding setup code: %v", err)
		}
		return hash
	}

	t.Run("mints one when the deployment has none", func(t *testing.T) {
		srv, db := newBareConsole(t)
		if _, err := db.Setting(t.Context(), setupCodeKey); err == nil {
			t.Fatal("a bare console should start with no setup code, so this test would prove nothing")
		}

		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/state", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("auth/state = %d, want 200", rec.Code)
		}
		if outstanding(t, db) == "" {
			t.Error("loading the gate on an unconfigured deployment must leave a setup code outstanding")
		}
	})

	t.Run("does not replace an outstanding one", func(t *testing.T) {
		srv, db := newBareConsole(t)
		if _, err := IssueSetupCode(t.Context(), db); err != nil {
			t.Fatalf("issue setup code: %v", err)
		}
		before := outstanding(t, db)

		for range 5 {
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/state", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("auth/state = %d, want 200", rec.Code)
			}
		}

		if after := outstanding(t, db); after != before {
			t.Error("the gate replaced an outstanding setup code; anyone loading the console could then invalidate the code an operator is typing")
		}
	})

	// Once somebody can sign in, the code is destroyed and the gate is not the
	// place to get another - `panorama admin setup` is.
	t.Run("mints nothing once an account exists", func(t *testing.T) {
		srv, db := newBareConsole(t)
		if _, err := db.CreateUser(t.Context(), "someone@example.com", "Someone", store.RoleAdmin); err != nil {
			t.Fatalf("create user: %v", err)
		}

		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/state", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("auth/state = %d, want 200", rec.Code)
		}
		if _, err := db.Setting(t.Context(), setupCodeKey); err == nil {
			t.Error("a deployment somebody can sign in to must not have a setup code minted by a page load")
		}
	})
}

// TestDecliningToMintIsSaidOutLoud.
//
// The gate mints only into a gap, so on a deployment that already has a code
// it does nothing - which is correct and, in a log, indistinguishable from the
// feature being broken. Without the log line, telling the two apart would mean
// trying to redeem a code to see whether one existed.
func TestDecliningToMintIsSaidOutLoud(t *testing.T) {
	_, db := newBareConsole(t)

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := ensureSetupCode(t.Context(), db, log); err != nil {
		t.Fatalf("ensureSetupCode: %v", err)
	}
	if !strings.Contains(buf.String(), "PANORAMA IS NOT SET UP") {
		t.Fatalf("the first call must mint and log a code, got: %s", buf.String())
	}

	buf.Reset()
	if err := ensureSetupCode(t.Context(), db, log); err != nil {
		t.Fatalf("ensureSetupCode: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "PANORAMA IS NOT SET UP") {
		t.Error("the second call minted a second code; an outstanding one must be left alone")
	}
	if !strings.Contains(got, "issued no new one") {
		t.Errorf("declining to mint must say so, or a working gate reads as a broken one; got: %s", got)
	}
}

// TestAStaleSetupCodeDoesNotLockTheDeploymentOut.
//
// The failure this exists for: only the hash of a code is
// stored, so an outstanding code cannot be shown to anybody. A code that nobody
// can read - scrolled out of a log, printed into an exec session, or on a task
// that has since been replaced - would otherwise refuse to be replaced forever,
// and the gate would keep declining to mint the one code that could be read.
//
// Past the grace window the code is assumed lost and a fresh one is issued.
// Within it, an outstanding code is still left strictly alone, because this
// endpoint is unauthenticated and internet facing on a normal deployment.
func TestAStaleSetupCodeDoesNotLockTheDeploymentOut(t *testing.T) {
	mint := func(t *testing.T, db *store.DB, issuedAt string) string {
		t.Helper()
		if _, err := IssueSetupCode(t.Context(), db); err != nil {
			t.Fatalf("issue setup code: %v", err)
		}
		if issuedAt != "" {
			if err := db.SetSetting(t.Context(), setupCodeIssuedKey, issuedAt); err != nil {
				t.Fatalf("backdate the code: %v", err)
			}
		}
		hash, err := db.Setting(t.Context(), setupCodeKey)
		if err != nil {
			t.Fatalf("read the code hash: %v", err)
		}
		return hash
	}

	t.Run("a code older than the window is replaced", func(t *testing.T) {
		_, db := newBareConsole(t)
		before := mint(t, db, time.Now().UTC().Add(-setupCodeGrace-time.Minute).Format(time.RFC3339))

		if err := ensureSetupCode(t.Context(), db, discardLogger()); err != nil {
			t.Fatalf("ensureSetupCode: %v", err)
		}
		after, err := db.Setting(t.Context(), setupCodeKey)
		if err != nil {
			t.Fatal(err)
		}
		if after == before {
			t.Error("a code past the grace window was kept; nothing could then issue one anybody can read")
		}
	})

	t.Run("a code inside the window is left alone", func(t *testing.T) {
		_, db := newBareConsole(t)
		before := mint(t, db, "")

		if err := ensureSetupCode(t.Context(), db, discardLogger()); err != nil {
			t.Fatalf("ensureSetupCode: %v", err)
		}
		after, err := db.Setting(t.Context(), setupCodeKey)
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Error("a code issued moments ago was replaced; anyone loading the console could invalidate the one being typed")
		}
	})

	// A code from before the timestamp existed. Treated as lost rather than
	// fresh: the alternative is a deployment that cannot issue a readable code.
	t.Run("a code with no timestamp is replaced", func(t *testing.T) {
		_, db := newBareConsole(t)
		before := mint(t, db, "")
		if _, err := db.DeleteSetting(t.Context(), setupCodeIssuedKey); err != nil {
			t.Fatal(err)
		}

		if err := ensureSetupCode(t.Context(), db, discardLogger()); err != nil {
			t.Fatalf("ensureSetupCode: %v", err)
		}
		after, err := db.Setting(t.Context(), setupCodeKey)
		if err != nil {
			t.Fatal(err)
		}
		if after == before {
			t.Error("a code with no issued-at was kept; a pre-upgrade code would lock the deployment out")
		}
	})
}
