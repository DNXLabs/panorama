// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/store"
)

// TestDeviceLoginEndToEnd walks the whole of `panorama login` over HTTP: a CLI
// with no credential asks, a signed-in person approves, the CLI collects a
// grant, and the grant buys a gateway token that actually authenticates.
func TestDeviceLoginEndToEnd(t *testing.T) {
	f := newConsole(t)
	f.signInFromTheCommandLine(t)
	// Polled back to back here, which a real CLI does not do - it waits the
	// interval it was given. The cadence rule has its own test.
	f.server.cfg.Auth.DevicePollInterval = 0

	rec, start := f.send(t, http.MethodPost, "/api/v1/cli/device", "", map[string]any{
		"client_name": "alice-laptop",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("starting a device flow = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	deviceCode, _ := start["device_code"].(string)
	userCode, _ := start["user_code"].(string)
	if !auth.WellFormed(auth.GrantPrefix, deviceCode) {
		t.Fatalf("device_code = %q, want a well-formed grant-shaped secret", deviceCode)
	}
	if len(userCode) != 9 || userCode[4] != '-' {
		t.Fatalf("user_code = %q, want ABCD-EFGH", userCode)
	}
	if got, _ := start["verification_uri"].(string); got != "http://localhost:8088/cli" {
		t.Errorf("verification_uri = %q, want it built from base_url", got)
	}

	// Before a decision, the CLI is told to keep waiting - in RFC 8628's words,
	// because a client written against the spec must work here unchanged.
	rec, poll := f.send(t, http.MethodPost, "/api/v1/cli/device/grant", "", map[string]any{
		"device_code": deviceCode,
	})
	if rec.Code != http.StatusBadRequest || poll["error"] != "authorization_pending" {
		t.Fatalf("polling before approval = %d %v, want 400 authorization_pending", rec.Code, poll["error"])
	}

	// The person sees what they are approving.
	rec, pending := f.get(t, "/api/v1/cli/pending?user_code="+userCode, f.devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("reading the pending sign-in = %d: %s", rec.Code, rec.Body.String())
	}
	if pending["client_name"] != "alice-laptop" {
		t.Errorf("client_name = %v, want the name the CLI reported", pending["client_name"])
	}

	rec, _ = f.send(t, http.MethodPost, "/api/v1/cli/decide", f.devToken, map[string]any{
		"user_code": userCode, "approve": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("approving = %d: %s", rec.Code, rec.Body.String())
	}

	rec, granted := f.send(t, http.MethodPost, "/api/v1/cli/device/grant", "", map[string]any{
		"device_code": deviceCode,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("collecting the grant = %d: %s", rec.Code, rec.Body.String())
	}
	grant, _ := granted["grant"].(string)
	if !auth.WellFormed(auth.GrantPrefix, grant) {
		t.Fatalf("grant = %q, want a well-formed grant", grant)
	}
	if granted["name"] != "alice-laptop" {
		t.Errorf("grant name = %v, want the machine that asked", granted["name"])
	}

	// A device code is single-use, so a replayed poll mints nothing.
	rec, replay := f.send(t, http.MethodPost, "/api/v1/cli/device/grant", "", map[string]any{
		"device_code": deviceCode,
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("replaying the device code produced a second grant: %v", replay)
	}

	// The grant buys a gateway token.
	rec, exchanged := f.send(t, http.MethodPost, "/api/v1/cli/token", grant, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("exchanging the grant = %d: %s", rec.Code, rec.Body.String())
	}
	token, _ := exchanged["token"].(string)
	if !auth.WellFormed(auth.Prefix, token) {
		t.Fatalf("token = %q, want a gateway token", token)
	}

	// And the token is a real credential for the person who approved it.
	rec, me := f.get(t, "/api/v1/me", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("using the minted token = %d: %s", rec.Code, rec.Body.String())
	}
	user, _ := me["user"].(map[string]any)
	if user["email"] != f.developer.Email {
		t.Errorf("the token acts as %v, want %v", user["email"], f.developer.Email)
	}
}

// TestGrantIsNotAGatewayToken is the reason grants have their own prefix. A
// grant lives for a month; if it were spendable, a leaked one would be worth
// far more than the hour-long token it exchanges for.
func TestGrantIsNotAGatewayToken(t *testing.T) {
	f := newConsole(t)
	f.signInFromTheCommandLine(t)

	grant, err := auth.GenerateGrant()
	if err != nil {
		t.Fatal(err)
	}
	if _, cerr := f.db.CreateGrant(t.Context(), f.developer.ID, "laptop",
		grant.Hash, grant.DisplayPrefix, nil); cerr != nil {
		t.Fatal(cerr)
	}

	// It works where it should.
	if rec, _ := f.send(t, http.MethodPost, "/api/v1/cli/token", grant.Plaintext, nil); rec.Code != http.StatusOK {
		t.Fatalf("the grant does not work at the exchange: %d", rec.Code)
	}

	// And nowhere else.
	if rec, _ := f.get(t, "/api/v1/me", grant.Plaintext); rec.Code != http.StatusUnauthorized {
		t.Errorf("the console accepted a grant as a credential: %d", rec.Code)
	}
}

// TestRevokingAGrantStopsItsToken is what "sign this laptop out" has to mean.
func TestRevokingAGrantStopsItsToken(t *testing.T) {
	f := newConsole(t)
	f.signInFromTheCommandLine(t)

	grant, err := auth.GenerateGrant()
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.db.CreateGrant(t.Context(), f.developer.ID, "laptop",
		grant.Hash, grant.DisplayPrefix, nil)
	if err != nil {
		t.Fatal(err)
	}

	rec, exchanged := f.send(t, http.MethodPost, "/api/v1/cli/token", grant.Plaintext, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: %d", rec.Code)
	}
	token, _ := exchanged["token"].(string)

	if rec, _ := f.get(t, "/api/v1/me", token); rec.Code != http.StatusOK {
		t.Fatalf("the minted token does not work: %d", rec.Code)
	}

	if rec, _ := f.send(t, http.MethodDelete, "/api/v1/grants/"+record.ID, f.devToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("revoking = %d: %s", rec.Code, rec.Body.String())
	}

	if rec, _ := f.get(t, "/api/v1/me", token); rec.Code != http.StatusUnauthorized {
		t.Error("the token outlived the grant that minted it; signing a lost laptop out " +
			"has to stop it now rather than whenever the token happens to expire")
	}
	if rec, _ := f.send(t, http.MethodPost, "/api/v1/cli/token", grant.Plaintext, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the revoked grant still mints tokens: %d", rec.Code)
	}
}

// TestApprovingASignInNeedsACredential. The device endpoints are the only new
// unauthenticated routes, and the authorization they defer to is this one.
func TestApprovingASignInNeedsACredential(t *testing.T) {
	f := newConsole(t)
	f.signInFromTheCommandLine(t)

	_, start := f.send(t, http.MethodPost, "/api/v1/cli/device", "", map[string]any{"client_name": "cli"})
	userCode, _ := start["user_code"].(string)

	for _, route := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/cli/pending?user_code=" + userCode, nil},
		{http.MethodPost, "/api/v1/cli/decide", map[string]any{"user_code": userCode, "approve": true}},
		{http.MethodGet, "/api/v1/grants", nil},
	} {
		rec, _ := f.send(t, route.method, route.path, "", route.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a credential = %d, want 401", route.method, route.path, rec.Code)
		}
	}
}

// TestUserCodeIsForgiving. The code is transcribed by eye, so the ways a person
// actually types it are the same code rather than a failed sign-in.
func TestUserCodeIsForgiving(t *testing.T) {
	f := newConsole(t)
	f.signInFromTheCommandLine(t)

	_, start := f.send(t, http.MethodPost, "/api/v1/cli/device", "", map[string]any{"client_name": "cli"})
	userCode, _ := start["user_code"].(string)

	bare := userCode[:4] + userCode[5:]
	for _, typed := range []string{userCode, bare, " " + userCode + " ", strings.ToLower(userCode)} {
		rec, _ := f.get(t, "/api/v1/cli/pending?user_code="+url.QueryEscape(typed), f.devToken)
		if rec.Code != http.StatusOK {
			t.Errorf("code typed as %q was not recognised: %d", typed, rec.Code)
		}
	}
}

// TestServiceTokenSpendsAtTheGatewayAndNowhereElse. RoleService is on no
// route's requireRole list, so a pipeline's credential authenticates for model
// calls and is refused by every console endpoint - including ones added later.
func TestServiceTokenSpendsAtTheGatewayAndNowhereElse(t *testing.T) {
	f := newConsole(t)

	team, err := f.db.CreateTeam(t.Context(), "Platform", "platform", "")
	if err != nil {
		t.Fatal(err)
	}

	rec, created := f.send(t, http.MethodPost, "/api/v1/service-tokens", f.adminToken, map[string]any{
		"team": "platform", "name": "nightly",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a service token = %d: %s", rec.Code, rec.Body.String())
	}
	token, _ := created["token"].(string)
	if !auth.WellFormed(auth.ServicePrefix, token) {
		t.Fatalf("token = %q, want a service-prefixed credential", token)
	}

	// It resolves to the team, at the gateway.
	principal, err := AuthenticateGateway(t.Context(), f.db, token)
	if err != nil {
		t.Fatalf("the gateway rejected a live service token: %v", err)
	}
	if principal.TeamID != team.ID || !principal.IsService() {
		t.Errorf("principal = %+v, want a service principal billing %s", principal, team.ID)
	}

	// And reaches nothing in the console.
	for _, path := range []string{"/api/v1/me", "/api/v1/usage", "/api/v1/tokens", "/api/v1/requests"} {
		rec, _ := f.get(t, path, token)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with a service token = %d, want 403", path, rec.Code)
		}
	}
}

// TestOnlyAdministratorsMintServiceTokens. It is the one credential somebody
// creates that they will not be the principal of, and it spends a team's money.
func TestOnlyAdministratorsMintServiceTokens(t *testing.T) {
	f := newConsole(t)
	if _, err := f.db.CreateTeam(t.Context(), "Platform", "platform", ""); err != nil {
		t.Fatal(err)
	}
	manager, managerToken := f.newUser(t, "manager@example.com", store.RoleManager)
	_ = manager

	rec, _ := f.send(t, http.MethodPost, "/api/v1/service-tokens", managerToken, map[string]any{
		"team": "platform", "name": "nightly",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a manager minted a service token: %d", rec.Code)
	}
	rec, _ = f.send(t, http.MethodPost, "/api/v1/service-tokens", f.devToken, map[string]any{
		"team": "platform", "name": "nightly",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a developer minted a service token: %d", rec.Code)
	}

	// A manager may still see which pipelines can spend a budget.
	if rec, _ := f.get(t, "/api/v1/service-tokens", managerToken); rec.Code != http.StatusOK {
		t.Errorf("a manager cannot list service tokens: %d", rec.Code)
	}
	if rec, _ := f.get(t, "/api/v1/service-tokens", f.devToken); rec.Code != http.StatusForbidden {
		t.Errorf("a developer listed service tokens: %d", rec.Code)
	}
}

// TestCommandLineSignInIsOffUntilItIsReleased.
//
// The whole flow is refused on the server rather than merely unmentioned in the
// console. A screen that hid the button and called that the rule would be the
// control this console is not - so the check is that a CLI holding no console
// at all gets the same answer a reader does.
func TestCommandLineSignInIsOffUntilItIsReleased(t *testing.T) {
	f := newConsole(t)

	// Every door the flow opens, including the two that are unauthenticated
	// because a device flow's caller has no credential yet.
	for _, route := range []struct {
		path  string
		token string
		body  any
	}{
		{"/api/v1/cli/device", "", map[string]any{"client_name": "alice-laptop"}},
		{"/api/v1/cli/device/grant", "", map[string]any{"device_code": "png_nonsense"}},
		{"/api/v1/cli/token", "png_nonsense", nil},
		{"/api/v1/cli/decide", f.devToken, map[string]any{"user_code": "ABCD-EFGH", "approve": true}},
	} {
		rec, body := f.send(t, http.MethodPost, route.path, route.token, route.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with the flow off = %d, want 403: %s", route.path, rec.Code, rec.Body.String())
			continue
		}
		// Named rather than vague. Somebody meeting this has a tool that will
		// not start and nothing else to tell a switched-off feature from a
		// broken deployment.
		if msg, _ := body["error"].(string); !strings.Contains(msg, "switched off") ||
			!strings.Contains(msg, "gateway token") {
			t.Errorf("POST %s says %q; want it to name the switch and the path that does work", route.path, msg)
		}
	}

	if rec, _ := f.get(t, "/api/v1/cli/pending?user_code=ABCD-EFGH", f.devToken); rec.Code != http.StatusForbidden {
		t.Errorf("reading a pending sign-in with the flow off = %d, want 403", rec.Code)
	}

	// Listing and revoking are deliberately NOT behind the switch. A grant
	// already on somebody's laptop has to stay revocable whatever the switch
	// says, and the list is how an operator finds the one to revoke.
	if rec, _ := f.get(t, "/api/v1/grants", f.devToken); rec.Code != http.StatusOK {
		t.Errorf("listing grants with the flow off = %d, want 200 - a credential already issued stays revocable", rec.Code)
	}

	// And the console is told the same fact once, on the response every screen
	// already has, rather than each screen working it out.
	_, session := f.get(t, "/api/v1/auth/session", f.devToken)
	if session["cli_sign_in"] != false {
		t.Errorf("the session says cli_sign_in = %v, want false", session["cli_sign_in"])
	}
}

// TestNoToolIsOfferedACredentialThisDeploymentRefuses.
//
// `cli` present on a tool is what says the sign-in path works, and My setup
// builds the whole credential question out of it. Serving it while the server
// refuses the flow would print an engineer a set of commands that cannot
// succeed, which is worse than never having mentioned them.
func TestNoToolIsOfferedACredentialThisDeploymentRefuses(t *testing.T) {
	f := newConsole(t)

	_, body := f.send(t, http.MethodGet, "/api/v1/setup/tools", f.devToken, nil)
	for _, tl := range mapsOf(body["tools"]) {
		if _, ok := tl["cli"]; ok {
			t.Errorf("%v is served a cli block while command-line sign-in is off", tl["id"])
		}
	}

	// The same deployment with the switch thrown serves it again, which is what
	// makes this a switch rather than a deletion.
	f.signInFromTheCommandLine(t)
	_, on := f.send(t, http.MethodGet, "/api/v1/setup/tools", f.devToken, nil)
	var served int
	for _, tl := range mapsOf(on["tools"]) {
		if _, ok := tl["cli"]; ok {
			served++
		}
	}
	if served == 0 {
		t.Error("no tool is served a cli block with command-line sign-in switched on")
	}
}

// TestAnAdministratorCannotTurnCommandLineSignInOnYet.
//
// The refusal is on the write path, where the person deciding is, and it says
// what to do instead. Off is always writable, so an administrator on the
// release that hands the CLI over can still turn it back off.
func TestAnAdministratorCannotTurnCommandLineSignInOnYet(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodPatch, "/api/v1/settings", f.adminToken,
		map[string]any{"cli.sign_in": "true"})
	if cliSignInReleased {
		if rec.Code != http.StatusOK {
			t.Fatalf("turning it on in a build that offers it = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	} else {
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("turning it on = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if msg, _ := body["error"].(string); !strings.Contains(msg, "not available in this release") ||
			!strings.Contains(msg, "My setup") {
			t.Errorf("the refusal says %q; want the reason and the credential that does work", msg)
		}
		// Refused, and therefore not written: a setting that read "on" over a
		// flow the server refuses would be two answers to one question.
		if _, err := f.db.Setting(t.Context(), settingCLISignIn); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a refused save wrote the row anyway (err = %v)", err)
		}
	}

	if rec, _ := f.send(t, http.MethodPatch, "/api/v1/settings", f.adminToken,
		map[string]any{"cli.sign_in": "false"}); rec.Code != http.StatusOK {
		t.Fatalf("turning it off = %d, want 200", rec.Code)
	}
	if rec, _ := f.send(t, http.MethodPatch, "/api/v1/settings", f.adminToken,
		map[string]any{"cli.sign_in": "yes"}); rec.Code != http.StatusBadRequest {
		t.Errorf("a value that is neither true nor false = %d, want 400", rec.Code)
	}

	// What the screen renders the switch from: whether this build offers the
	// flow at all. Served rather than compiled into the console, so the screen
	// is not a second place it is decided.
	_, settings := f.get(t, "/api/v1/settings", f.adminToken)
	deployment, _ := settings["deployment"].(map[string]any)
	if deployment["cli_sign_in_released"] != cliSignInReleased {
		t.Errorf("cli_sign_in_released = %v, want %v", deployment["cli_sign_in_released"], cliSignInReleased)
	}
}
