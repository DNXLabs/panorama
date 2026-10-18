// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/store"
)

// mintGatewayToken creates an account and a personal gateway token for it, the
// way `panorama token create` does.
func mintGatewayToken(t *testing.T, db *store.DB, email, role string, expiresAt *time.Time) (string, *store.User, string) {
	t.Helper()

	user, err := db.EnsureUser(t.Context(), email, "", role)
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	minted, err := auth.Generate()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token, err := db.CreateGatewayToken(t.Context(), user.ID, "test", minted.Hash, minted.DisplayPrefix, expiresAt)
	if err != nil {
		t.Fatalf("create gateway token: %v", err)
	}
	return minted.Plaintext, user, token.ID
}

// sessionCookieFrom returns the console session cookie a response set.
func sessionCookieFrom(srv *Server, rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == srv.cookieName(sessionCookie) && c.Value != "" {
			return c
		}
	}
	return nil
}

// TestAGatewayTokenSignsInToTheConsole is the evaluation path: a deployment
// with no identity provider, a token minted where it runs, and a browser that
// ends up holding an ordinary console session for that token's owner.
func TestAGatewayTokenSignsInToTheConsole(t *testing.T) {
	srv, db := newBareConsole(t)
	token, user, _ := mintGatewayToken(t, db, "you@example.com", store.RoleAdmin, nil)

	rec, _ := post(t, srv, "/api/v1/auth/token", map[string]string{"token": token}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in with a gateway token = %d, want 200: %s", rec.Code, rec.Body)
	}
	cookie := sessionCookieFrom(srv, rec)
	if cookie == nil {
		t.Fatal("signing in with a gateway token set no session cookie")
	}
	if !auth.WellFormed(auth.SessionPrefix, cookie.Value) {
		t.Errorf("the session cookie is not a console session: %q", cookie.Value[:4])
	}
	if cookie.Value == token {
		t.Error("the session cookie is the gateway token itself")
	}

	// The session is the owner's, with the owner's role.
	me := getWith(t, srv, "/api/v1/auth/session", func(r *http.Request) { r.AddCookie(cookie) })
	if me.Code != http.StatusOK {
		t.Fatalf("GET /auth/session with the new cookie = %d, want 200: %s", me.Code, me.Body)
	}
	for _, want := range []string{user.ID, `"role":"admin"`, `"csrf_token"`} {
		if !strings.Contains(me.Body.String(), want) {
			t.Errorf("the session does not carry %s: %s", want, me.Body)
		}
	}
}

// TestTheTokenSignInIsAbsentOnceSingleSignOnIsConfigured. An organization
// that put its console behind its identity provider does not keep a second
// door beside it.
func TestTheTokenSignInIsAbsentOnceSingleSignOnIsConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Database.DSN = "file:" + filepath.Join(t.TempDir(), "panorama.db")
	cfg.OIDC.Issuer = "https://login.example.com"
	cfg.OIDC.ClientID = "panorama"
	cfg.OIDC.ClientSecret = "secret"

	db, err := store.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if merr := db.Migrate(t.Context()); merr != nil {
		t.Fatalf("migrate: %v", merr)
	}
	srv := New(cfg, db, discardLogger())

	token, _, _ := mintGatewayToken(t, db, "you@example.com", store.RoleAdmin, nil)
	rec, _ := post(t, srv, "/api/v1/auth/token", map[string]string{"token": token}, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("token sign-in with single sign-on configured = %d, want 404", rec.Code)
	}
	if sessionCookieFrom(srv, rec) != nil {
		t.Error("a session cookie was set with single sign-on configured")
	}

	state := getWith(t, srv, "/api/v1/auth/state", nil)
	if !strings.Contains(state.Body.String(), `"token_sign_in":false`) {
		t.Errorf("the sign-in state offers the token door with single sign-on configured: %s", state.Body)
	}
}

func TestTheSignInStateOffersTheTokenDoorWithoutSingleSignOn(t *testing.T) {
	srv, _ := newBareConsole(t)
	state := getWith(t, srv, "/api/v1/auth/state", nil)
	if !strings.Contains(state.Body.String(), `"token_sign_in":true`) {
		t.Errorf("the sign-in state does not offer the token door: %s", state.Body)
	}
}

// TestOnlyALiveGatewayTokenSignsIn. Every other credential, and a gateway
// token that no longer works, is refused - identically, so the response does
// not say which it was.
func TestOnlyALiveGatewayTokenSignsIn(t *testing.T) {
	srv, db := newBareConsole(t)
	ctx := t.Context()

	past := time.Now().Add(-time.Hour)
	expired, _, _ := mintGatewayToken(t, db, "expired@example.com", store.RoleAdmin, &past)
	revoked, _, revokedID := mintGatewayToken(t, db, "revoked@example.com", store.RoleAdmin, nil)
	if err := db.RevokeToken(ctx, revokedID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	disabled, disabledUser, _ := mintGatewayToken(t, db, "disabled@example.com", store.RoleAdmin, nil)
	if err := db.SetUserStatus(ctx, disabledUser.ID, "disabled"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	// A real service token, stored, so the refusal is not merely of a string
	// nobody issued.
	_, ownerUser, _ := mintGatewayToken(t, db, "owner@example.com", store.RoleAdmin, nil)
	team, err := db.CreateTeam(ctx, "Platform", "platform", "")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	service, err := auth.GenerateService()
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := db.CreateServiceToken(ctx, team.ID, ownerUser.ID, "ci", service.Hash, service.DisplayPrefix, nil); serr != nil {
		t.Fatalf("create service token: %v", serr)
	}

	grant, err := auth.GenerateGrant()
	if err != nil {
		t.Fatal(err)
	}
	if _, gerr := db.CreateGrant(ctx, ownerUser.ID, "laptop", grant.Hash, grant.DisplayPrefix, nil); gerr != nil {
		t.Fatalf("create grant: %v", gerr)
	}
	setupCode, err := IssueSetupCode(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	for i, c := range []struct{ name, token string }{
		{"an unknown token", "pnt_" + strings.Repeat("A", 43)},
		{"an expired token", expired},
		{"a revoked token", revoked},
		{"a disabled owner", disabled},
		{"a service token", service.Plaintext},
		{"a CLI grant", grant.Plaintext},
		{"a setup code", setupCode},
		{"an Anthropic key", "sk-ant-api03-" + strings.Repeat("x", 40)},
		{"nothing", ""},
		{"a truncated gateway token", "pnt_short"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A fresh address per case, so the limiter is not what answers.
			rec, _ := post(t, srv, "/api/v1/auth/token", map[string]string{"token": c.token},
				func(r *http.Request) { r.RemoteAddr = "192.0.2." + strconv.Itoa(i+1) + ":1234" })
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("sign in with %s = %d, want 401: %s", c.name, rec.Code, rec.Body)
			}
			if sessionCookieFrom(srv, rec) != nil {
				t.Errorf("sign in with %s set a session cookie", c.name)
			}
		})
	}
}

// TestTheTokenSignInRefusesAnythingButJSON is the login-CSRF guard. A form on
// another site can post text/plain or urlencoded bodies here without a
// preflight; if one could sign a browser in, the victim's next token would be
// minted into the attacker's account.
func TestTheTokenSignInRefusesAnythingButJSON(t *testing.T) {
	srv, db := newBareConsole(t)
	token, _, _ := mintGatewayToken(t, db, "attacker@example.com", store.RoleDeveloper, nil)

	for _, contentType := range []string{
		"text/plain",
		"text/plain;charset=UTF-8",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"",
	} {
		t.Run(contentType, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token",
				strings.NewReader(`{"token":"`+token+`"}`))
			if contentType != "" {
				req.Header.Set("Content-Type", contentType)
			}
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Errorf("Content-Type %q = %d, want 415", contentType, rec.Code)
			}
			if sessionCookieFrom(srv, rec) != nil {
				t.Errorf("Content-Type %q set a session cookie", contentType)
			}
		})
	}

	t.Run("application/json with a charset is still JSON", func(t *testing.T) {
		rec, _ := post(t, srv, "/api/v1/auth/token", map[string]string{"token": token},
			func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") })
		if rec.Code != http.StatusOK {
			t.Errorf("application/json; charset=utf-8 = %d, want 200: %s", rec.Code, rec.Body)
		}
	})
}

func TestTheTokenSignInIsRateLimited(t *testing.T) {
	srv, _ := newBareConsole(t)

	var limited bool
	for range 20 {
		rec, _ := post(t, srv, "/api/v1/auth/token", map[string]string{
			"token": "pnt_" + strings.Repeat("A", 43),
		}, nil)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("twenty token sign-in attempts from one address were all served")
	}
}
