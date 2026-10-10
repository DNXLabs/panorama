// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
)

// send issues an authenticated request with an arbitrary method.
func (f *consoleFixture) send(t *testing.T, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var reader *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, req)

	var out map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

// TestAdministrativeWritesNeedTheRole. The role check lives in one wrapper
// rather than in each handler, and this is what proves the wrapper is on.
func TestAdministrativeWritesNeedTheRole(t *testing.T) {
	f := newConsole(t)

	for _, route := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/api/v1/settings", map[string]string{"org.name": "x"}},
		{http.MethodPut, "/api/v1/identity/groups", map[string]any{"groups": []any{}}},
		{http.MethodPost, "/api/v1/identity/check", map[string]string{}},
		{http.MethodPut, "/api/v1/budgets", map[string]any{"scope": "org", "period": "month", "hard_limit": "10.00"}},
		{http.MethodDelete, "/api/v1/budgets/bdg_1", nil},
		{http.MethodPatch, "/api/v1/users/usr_1", map[string]string{"role": "admin"}},
		{http.MethodPost, "/api/v1/teams", map[string]string{"name": "Platform"}},
		{http.MethodDelete, "/api/v1/teams/platform", nil},
		{http.MethodPost, "/api/v1/setup/step", map[string]string{"step": "identity", "action": "skip"}},
		{http.MethodPut, "/api/v1/providers/bedrock", map[string]string{"region": "us-east-1"}},
		{http.MethodPost, "/api/v1/providers/bedrock/check", map[string]string{}},
		{http.MethodPost, "/api/v1/providers/bedrock/preflight", map[string]string{}},
		{http.MethodPost, "/api/v1/integrations/run", map[string]string{}},
		// Choosing which models a deployment permits, and where their traffic is
		// processed, is an administrator's decision. A developer reading the list
		// is fine; a developer adding a route would be an allowlist anyone can
		// widen.
		{http.MethodPost, "/api/v1/models/discover", map[string]string{}},
		{http.MethodPut, "/api/v1/routes", map[string]string{"name": "fast", "target": "x"}},
		{http.MethodDelete, "/api/v1/routes/fast", nil},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec, _ := f.send(t, route.method, route.path, f.devToken, route.body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("a developer got %d, want 403", rec.Code)
			}
			rec, _ = f.send(t, route.method, route.path, "", route.body)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("an anonymous caller got %d, want 401", rec.Code)
			}
		})
	}
}

// TestTheUserListCarriesNoSpend is the second trust stance written as a test
// against the endpoint most likely to acquire a money column by accident.
func TestTheUserListCarriesNoSpend(t *testing.T) {
	f := newConsole(t)
	f.record(t, f.developer.ID, "r1", "us.anthropic.claude-sonnet-5", money(9_000_000), time.Now().UTC())

	rec, body := f.send(t, http.MethodGet, "/api/v1/users", f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}

	users, _ := body["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("%d users, want 2", len(users))
	}

	for _, u := range users {
		row, _ := u.(map[string]any)
		for _, forbidden := range []string{
			"cost", "spend", "spent_usd", "spent_nanos", "cost_nanos", "quota_used",
			"requests", "tokens_used", "percent_of_limit",
		} {
			if _, present := row[forbidden]; present {
				t.Errorf("the user list carries %q; a list of named people with a money column is a leaderboard", forbidden)
			}
		}
	}

	// And it is not reachable by a developer at all.
	rec, _ = f.send(t, http.MethodGet, "/api/v1/users", f.devToken, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a developer read the user list: %d", rec.Code)
	}
}

// TestAPersonMintsTheirOwnTokenAndNobodyElses. A token acts as its owner, so an
// administrator who could mint one would be able to make requests the audit
// trail attributes to somebody else.
func TestAPersonMintsTheirOwnTokenAndNobodyElses(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodPost, "/api/v1/tokens", f.devToken, map[string]any{"name": "laptop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d %s, want 201", rec.Code, rec.Body.String())
	}

	minted, _ := body["token"].(string)
	if !strings.HasPrefix(minted, "pnt_") {
		t.Fatalf("token = %q, want an pnt_ credential", minted)
	}

	// It works.
	rec, _ = f.send(t, http.MethodGet, "/api/v1/me", minted, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("the minted token does not authenticate: %d", rec.Code)
	}

	// There is no way to ask for one on someone else's behalf: the request
	// body has no field for it, and an unknown field is refused outright.
	rec, _ = f.send(t, http.MethodPost, "/api/v1/tokens", f.adminToken,
		map[string]any{"name": "for-dev", "user_id": f.developer.ID})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("minting for another user = %d, want it refused as an unknown field", rec.Code)
	}

	// Listing shows metadata and never the credential again.
	_, body = f.send(t, http.MethodGet, "/api/v1/tokens", f.devToken, nil)
	tokens, _ := body["tokens"].([]any)
	if len(tokens) != 2 {
		t.Fatalf("%d tokens, want the fixture's and the minted one", len(tokens))
	}
	for _, tk := range tokens {
		row, _ := tk.(map[string]any)
		if _, present := row["token"]; present {
			t.Error("the token list handed back a credential")
		}
	}
}

// TestAGatewayTokenExpiresAndTheListSaysWhen. The expiry is the whole point of
// the change and is invisible unless the screen can state it before anybody
// mints one - so the deadline comes back with the credential, the list carries
// the lifetime a mint would give, and nothing in the request can ask for more.
func TestAGatewayTokenExpiresAndTheListSaysWhen(t *testing.T) {
	f := newConsole(t)

	rec, created := f.send(t, http.MethodPost, "/api/v1/tokens", f.devToken, map[string]any{"name": "laptop"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d %s, want 201", rec.Code, rec.Body.String())
	}

	raw, _ := created["expires_at"].(string)
	if raw == "" {
		t.Fatal("a minted token came back with no expiry")
	}
	expiresAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("expires_at = %q: %v", raw, err)
	}
	// A day either side, so the assertion is about the lifetime rather than
	// about how long the test took to get here.
	want := time.Now().UTC().Add(auth.GatewayTokenTTL)
	if expiresAt.Before(want.Add(-24*time.Hour)) || expiresAt.After(want.Add(24*time.Hour)) {
		t.Errorf("expires_at = %s, want about %s", expiresAt, want)
	}

	// An expiry is not a choice, so a field asking for one is refused like any
	// other unknown.
	rec, _ = f.send(t, http.MethodPost, "/api/v1/tokens", f.devToken,
		map[string]any{"name": "forever", "expires_in_days": 3650})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("asking for a longer expiry = %d, want it refused", rec.Code)
	}

	_, body := f.send(t, http.MethodGet, "/api/v1/tokens", f.devToken, nil)
	days, _ := body["lifetime_days"].(float64)
	if int(days) != int(auth.GatewayTokenTTL.Hours()/24) {
		t.Errorf("lifetime_days = %v, want the lifetime a mint would actually give", days)
	}
	for _, tk := range mapsOf(body["tokens"]) {
		if tk["name"] == "laptop" && tk["expires_at"] == nil {
			t.Error("the list shows a minted token with no expiry beside it")
		}
	}
}

func TestOnlyTheOwnerOrAnAdminRevokesAToken(t *testing.T) {
	f := newConsole(t)

	_, created := f.send(t, http.MethodPost, "/api/v1/tokens", f.devToken, map[string]any{"name": "laptop"})
	id, _ := created["id"].(string)

	// Another developer cannot even learn it exists.
	_, otherToken := f.newUser(t, "other@example.com", store.RoleDeveloper)
	rec, _ := f.send(t, http.MethodDelete, "/api/v1/tokens/"+id, otherToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("another developer got %d revoking someone else's token, want 404", rec.Code)
	}

	// An administrator can: a laptop that walks out of the building is exactly
	// the case where waiting for its owner is the wrong answer.
	rec, _ = f.send(t, http.MethodDelete, "/api/v1/tokens/"+id, f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("an administrator got %d revoking a token, want 200", rec.Code)
	}
}

func TestTeamsAreCreatedAndPopulated(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodPost, "/api/v1/teams", f.adminToken,
		map[string]string{"name": "Platform Engineering"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	slug, _ := body["slug"].(string)
	if slug != "platform-engineering" {
		t.Errorf("slug = %q, want it derived from the name", slug)
	}

	rec, _ = f.send(t, http.MethodPost, "/api/v1/teams/"+slug, f.adminToken,
		map[string]any{"email": f.developer.Email, "role": "lead"})
	if rec.Code != http.StatusOK {
		t.Fatalf("add member = %d %s", rec.Code, rec.Body.String())
	}

	_, body = f.send(t, http.MethodGet, "/api/v1/teams/"+slug, f.adminToken, nil)
	members, _ := body["members"].([]any)
	if len(members) != 1 {
		t.Fatalf("%d members, want 1", len(members))
	}

	// Somebody with no account is not conjured into one by a team form.
	rec, _ = f.send(t, http.MethodPost, "/api/v1/teams/"+slug, f.adminToken,
		map[string]any{"email": "nobody@example.com"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("adding an unknown address = %d, want 404", rec.Code)
	}
}

// TestTheLastAdministratorCannotBeRemoved. Recoverable with `panorama admin
// setup`, but only from a shell, and this is one query.
func TestTheLastAdministratorCannotBeRemoved(t *testing.T) {
	f := newConsole(t)

	rec, _ := f.send(t, http.MethodPatch, "/api/v1/users/"+f.admin.ID, f.adminToken,
		map[string]string{"role": store.RoleDeveloper})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("self-demotion = %d, want it refused", rec.Code)
	}

	rec, _ = f.send(t, http.MethodPatch, "/api/v1/users/"+f.admin.ID, f.adminToken,
		map[string]string{"status": "disabled"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("self-disable = %d, want it refused", rec.Code)
	}

	// With a second administrator in place, both become possible.
	rec, _ = f.send(t, http.MethodPatch, "/api/v1/users/"+f.developer.ID, f.adminToken,
		map[string]string{"role": store.RoleAdmin})
	if rec.Code != http.StatusOK {
		t.Fatalf("promotion = %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = f.send(t, http.MethodPatch, "/api/v1/users/"+f.admin.ID, f.adminToken,
		map[string]string{"role": store.RoleManager})
	if rec.Code != http.StatusBadRequest {
		t.Error("an administrator was allowed to demote themselves even with a second one present")
	}
}

// TestSetupStateIsDerivedNotRemembered. A stored "done" flag says done after
// somebody removes the issuer from the configuration.
func TestSetupStateIsDerivedNotRemembered(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodGet, "/api/v1/setup", f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}

	steps, _ := body["steps"].([]any)
	if len(steps) != 6 {
		t.Fatalf("%d steps, want the six the wizard runs", len(steps))
	}

	state := map[string]string{}
	for _, s := range steps {
		row, _ := s.(map[string]any)
		id, _ := row["id"].(string)
		st, _ := row["state"].(string)
		state[id] = st
	}
	// Reaching a provider and choosing models are separate steps, and
	// separately unfinished. Folded together, a deployment that could not reach
	// its provider at all and one that simply had not picked a model yet were
	// the same row. And a provider configured but never checked is not done:
	// configured is not verified.
	if state["provider"] != "todo" {
		t.Errorf("provider = %q on a deployment that has never invoked one, want todo", state["provider"])
	}
	if state["models"] != "todo" {
		t.Errorf("models = %q with no routes, want todo", state["models"])
	}
	if state["team"] != "todo" {
		t.Errorf("team = %q with no teams, want todo", state["team"])
	}
	if body["complete"] != false {
		t.Error("a fresh deployment reported setup complete")
	}

	// Skipping is recorded, because nothing else can reveal it.
	rec, _ = f.send(t, http.MethodPost, "/api/v1/setup/step", f.adminToken,
		map[string]string{"step": "integrations", "action": "skip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("skip = %d %s", rec.Code, rec.Body.String())
	}

	_, body = f.send(t, http.MethodGet, "/api/v1/setup", f.adminToken, nil)
	steps, _ = body["steps"].([]any)
	for _, s := range steps {
		row, _ := s.(map[string]any)
		if row["id"] == "integrations" && row["state"] != "skipped" {
			t.Errorf("integrations = %v after skipping, want skipped", row["state"])
		}
	}

	// Creating a team alone is not enough: the step is a team *and* a budget.
	if _, err := f.db.CreateTeam(t.Context(), "Platform", "", ""); err != nil {
		t.Fatal(err)
	}
	_, body = f.send(t, http.MethodGet, "/api/v1/setup", f.adminToken, nil)
	steps, _ = body["steps"].([]any)
	for _, s := range steps {
		row, _ := s.(map[string]any)
		if row["id"] == "team" && row["state"] != "todo" {
			t.Errorf("team = %v with a team but no budget, want todo", row["state"])
		}
	}
}

// TestAProviderCheckRefusesWithoutAProvider, rather than reporting a success it
// did not earn - and names where a provider is configured.
func TestAProviderCheckRefusesWithoutAProvider(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodPost, "/api/v1/providers/bedrock-mantle/check", f.adminToken, map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a provider that is not configured", rec.Code)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "Choose a region") {
		t.Errorf("error = %q, want it to name what to do", message)
	}
}

// TestToolSetupIsHonestAboutWhatItCannotDo. A setup screen that hides what it
// cannot do is a screen an engineer stops trusting.
func TestToolSetupIsHonestAboutWhatItCannotDo(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodGet, "/api/v1/setup/tools", f.devToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}

	tools, _ := body["tools"].([]any)
	byID := map[string]map[string]any{}
	for _, tl := range tools {
		row, _ := tl.(map[string]any)
		id, _ := row["id"].(string)
		byID[id] = row
	}

	claude := byID["claude-code"]
	if claude == nil || claude["supported"] != true {
		t.Fatal("Claude Code is not listed as supported")
	}
	if shell, _ := claude["shell"].(string); !strings.Contains(shell, "ANTHROPIC_BASE_URL") {
		t.Errorf("the Claude Code block is %q, want the one environment variable", shell)
	}
	// Nothing on this screen carries a credential.
	if shell, _ := claude["shell"].(string); !strings.Contains(shell, tokenPlaceholder) {
		t.Error("the setup block does not use the placeholder; it must not carry a real token")
	}

	if copilot := byID["copilot"]; copilot == nil || copilot["supported"] == true {
		t.Error("Copilot is missing or claimed as supported; it cannot be proxied and the screen says so")
	}
	// Codex is served over Responses now, and configured from this screen.
	codex := byID["codex-cli"]
	if codex == nil || codex["supported"] != true {
		t.Fatal("Codex CLI is not listed as supported")
	}
	config, _ := codex["config"].(string)
	for _, want := range []string{`wire_api = "responses"`, `env_key = "PANORAMA_TOKEN"`, "/v1"} {
		if !strings.Contains(config, want) {
			t.Errorf("the Codex config.toml does not contain %q:\n%s", want, config)
		}
	}
	// Cline and Continue speak an API this gateway serves, and are not
	// configured here: the screen says both halves.
	if cline := byID["cline"]; cline == nil || cline["supported"] == true {
		t.Error("Cline is claimed as configured from this screen")
	}
}

// opencode speaks all three APIs, one provider block each: a route on a Claude
// model is declared under the Anthropic provider, a route on a model that
// speaks Responses under OpenAI's own, and a route on a model that speaks only
// Chat Completions under the OpenAI-compatible one - so all are in its picker,
// each sent to the endpoint its model speaks. A model speaking both OpenAI APIs
// goes to Responses, because that is where a reasoning model takes tools.
func TestTheOpenCodeBlockDeclaresEachRouteUnderItsAPI(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)
	if _, err := f.db.SaveProvider(t.Context(), providers.Settings{
		Kind: providers.KindBedrockMantle, Enabled: true, Region: "us-west-2",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReplaceCatalogue(t.Context(), providers.KindBedrockMantle, []store.CatalogueModel{{
		ID: "openai.gpt-oss-120b", FoundationModelID: "openai.gpt-oss-120b", Vendor: "OpenAI",
		DisplayName: "gpt-oss-120b", Streaming: true, Region: "us-west-2",
		APIs:          []providers.API{providers.APIOpenAIChat, providers.APIOpenAIResponses},
		ContextWindow: 128_000, MaxOutputTokens: 16_000,
	}, {
		ID: "qwen.qwen3-coder-480b-a35b-instruct", FoundationModelID: "qwen.qwen3-coder-480b-a35b-instruct",
		Vendor: "Qwen", DisplayName: "Qwen3 Coder 480B", Streaming: true, Region: "us-west-2",
		APIs: []providers.API{providers.APIOpenAIChat},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := f.policy.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, route := range []map[string]any{
		{"name": "claude", "target": globalHaiku},
		{"name": "oss", "target": "bedrock-mantle/openai.gpt-oss-120b"},
		{"name": "qwen", "target": "bedrock-mantle/qwen.qwen3-coder-480b-a35b-instruct"},
	} {
		if rec, _ := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, route); rec.Code != http.StatusOK {
			t.Fatalf("PUT /routes %v = %d: %s", route["name"], rec.Code, rec.Body)
		}
	}

	_, body := f.send(t, http.MethodGet, "/api/v1/setup/tools", f.devToken, nil)
	var opencode, codex, claude map[string]any
	for _, tl := range mapsOf(body["tools"]) {
		switch tl["id"] {
		case "opencode":
			opencode = tl
		case "codex-cli":
			codex = tl
		case "claude-code":
			claude = tl
		}
	}

	var config struct {
		Provider map[string]struct {
			NPM    string                    `json:"npm"`
			Models map[string]map[string]any `json:"models"`
		} `json:"provider"`
		Model string `json:"model"`
	}
	raw, _ := opencode["config"].(string)
	decodeJSON(t, []byte(raw), &config)
	if _, ok := config.Provider["panorama"].Models["claude"]; !ok || config.Provider["panorama"].NPM != "@ai-sdk/anthropic" {
		t.Errorf("the Claude route is not under the Anthropic provider: %s", raw)
	}
	responses := config.Provider["panorama-openai"]
	oss, ok := responses.Models["oss"]
	if !ok || responses.NPM != "@ai-sdk/openai" {
		t.Errorf("the route that speaks Responses is not under OpenAI's provider: %s", raw)
	}
	chat := config.Provider["panorama-chat"]
	if _, ok := chat.Models["qwen"]; !ok || chat.NPM != "@ai-sdk/openai-compatible" {
		t.Errorf("the Chat Completions route is not under the OpenAI-compatible provider: %s", raw)
	}
	if _, twice := chat.Models["oss"]; twice {
		t.Errorf("a route is declared under two providers: %s", raw)
	}
	if limit, _ := oss["limit"].(map[string]any); limit == nil || limit["context"] != float64(128_000) {
		t.Errorf("the oss route's limit = %v, want its recorded window", oss["limit"])
	}

	// Claude Code is offered only the route whose model speaks Messages, and
	// Codex only the one that speaks Responses.
	if routes, _ := claude["routes"].([]any); len(routes) != 1 || routes[0] != "claude" {
		t.Errorf("Claude Code is offered %v, want only the Claude route", claude["routes"])
	}
	if routes, _ := codex["routes"].([]any); len(routes) != 1 || routes[0] != "oss" {
		t.Errorf("Codex is offered %v, want only the Responses route", codex["routes"])
	}
	if toml, _ := codex["config"].(string); !strings.Contains(toml, `model = "oss"`) ||
		!strings.Contains(toml, "model_context_window = 128000") {
		t.Errorf("the Codex config does not choose the route with its window:\n%s", toml)
	}
	// One rendering per route, so the console swaps a whole block when the
	// reader picks another route rather than rewriting a window it cannot know.
	if configs, _ := codex["configs"].(map[string]any); configs["oss"] != codex["config"] {
		t.Errorf("the per-route Codex config for oss is not the default one: %v", configs)
	}
}

// TestTheOpenCodeBlockChoosesAModel. Declaring every route a person may use and
// selecting none of them left opencode on whatever its picker had last, which
// on a first run is nothing at all - the provider was configured and the tool
// still had no model. The two keys that fix that are also the pair the console
// rewrites when a reader changes their routes, so their absence would be a
// screen editing keys that are not there.
func TestTheOpenCodeBlockChoosesAModel(t *testing.T) {
	f := newConsole(t)
	// The `cli` half of the assertion is about a flow this release does not
	// serve, so the fixture is a deployment that has switched it on.
	f.signInFromTheCommandLine(t)

	rec, body := f.send(t, http.MethodGet, "/api/v1/setup/tools", f.devToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}

	var block string
	var cli map[string]any
	for _, tl := range mapsOf(body["tools"]) {
		if tl["id"] != "opencode" {
			continue
		}
		block, _ = tl["config"].(string)
		cli, _ = tl["cli"].(map[string]any)
	}
	if block == "" {
		t.Fatal("no opencode configuration was served")
	}

	var config struct {
		Model      string `json:"model"`
		SmallModel string `json:"small_model"`
	}
	if err := json.Unmarshal([]byte(block), &config); err != nil {
		t.Fatalf("the opencode block is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(config.Model, "panorama/") {
		t.Errorf("model = %q, want a route in this deployment's own provider", config.Model)
	}
	if !strings.HasPrefix(config.SmallModel, "panorama/") {
		t.Errorf("small_model = %q, want a route in this deployment's own provider", config.SmallModel)
	}

	// Signing a machine in and configuring a tool are two steps taken at two
	// moments, so they are served as two strings rather than one the console
	// has to split on a newline.
	if login, _ := cli["login"].(string); !strings.Contains(login, "panorama login") {
		t.Errorf("cli.login = %q, want the sign-in command on its own", login)
	}
	if conf, _ := cli["configure"].(string); !strings.Contains(conf, "panorama configure opencode") {
		t.Errorf("cli.configure = %q, want the configure command", conf)
	}
}

func mapsOf(value any) []map[string]any {
	list, _ := value.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

// TestOutcomesSayWhyTheyAreEmpty. The difference between "nothing merged" and
// "nothing was asked" is the difference between a finding and a missing
// integration.
func TestOutcomesSayWhyTheyAreEmpty(t *testing.T) {
	f := newConsole(t)
	f.record(t, f.developer.ID, "r1", "us.anthropic.claude-sonnet-5", money(1_000_000), time.Now().UTC())

	rec, body := f.send(t, http.MethodGet, "/api/v1/outcomes", f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if body["confirmed"] != false {
		t.Error("outcomes reported something confirmed with no integration connected")
	}
	if body["integration"] != false {
		t.Error("outcomes claimed an integration is connected")
	}
	if _, present := body["cost_per_merged_pr"]; present {
		t.Error("a cost per merged pull request was reported with nothing merged")
	}

	buckets, _ := body["by_outcome"].([]any)
	if len(buckets) != 1 {
		t.Fatalf("%d outcome rows, want the one unconfirmed bucket", len(buckets))
	}
	row, _ := buckets[0].(map[string]any)
	if row["label"] != "Not confirmed" {
		t.Errorf("label = %v, want work nobody has asked about to say so", row["label"])
	}
}

// A budget saved from the console is in force immediately. Somebody who has
// just raised a limit for a blocked colleague should not have to explain a
// thirty-second wait.
func TestABudgetSavedFromTheConsoleIsInForce(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodPut, "/api/v1/budgets", f.adminToken, map[string]any{
		"scope": "org", "period": "month", "name": "deployment", "hard_limit": "500.00",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("the response carries no id, so the console has nowhere to send the next edit")
	}

	_, body = f.send(t, http.MethodGet, "/api/v1/policy", f.adminToken, nil)
	budgets, _ := body["budgets"].([]any)
	if len(budgets) != 1 {
		t.Fatalf("%d budgets in force straight after saving, want 1", len(budgets))
	}
	row, _ := budgets[0].(map[string]any)
	limits, _ := row["limits"].(map[string]any)
	// Exact, and to the nanodollar: the wire carries the number the enforcement
	// path uses, and the console trims it for display rather than the other way
	// round.
	if limits["hard_usd"] != "500.000000000" {
		t.Errorf("hard limit = %v, want exactly $500", limits["hard_usd"])
	}

	// A malformed amount is refused with the reason, not accepted and rounded.
	rec, body = f.send(t, http.MethodPut, "/api/v1/budgets", f.adminToken, map[string]any{
		"scope": "org", "period": "day", "hard_limit": "five hundred",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed amount = %d, want 400", rec.Code)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Error("the refusal names no reason")
	}

	// A budget with no limit at all would enforce nothing while looking like it
	// enforces something.
	rec, _ = f.send(t, http.MethodPut, "/api/v1/budgets", f.adminToken, map[string]any{
		"scope": "org", "period": "week",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a budget with no limits = %d, want 400", rec.Code)
	}

	// A budget is counted in money. A field the server does not know is
	// refused rather than dropped, so a limit in some other unit is never saved
	// as a budget with no limit.
	rec, _ = f.send(t, http.MethodPut, "/api/v1/budgets", f.adminToken, map[string]any{
		"scope": "org", "period": "week", "hard_requests": 50000,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a budget with an unknown field = %d, want 400", rec.Code)
	}

	// Removing it takes the cap away.
	rec, _ = f.send(t, http.MethodDelete, "/api/v1/budgets/"+id, f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	_, body = f.send(t, http.MethodGet, "/api/v1/policy", f.adminToken, nil)
	if budgets, _ := body["budgets"].([]any); len(budgets) != 0 {
		t.Errorf("%d budgets after removing the only one", len(budgets))
	}
}

// A budget on somebody who is not here enforces nothing and looks exactly like
// one that does.
func TestABudgetNeedsASubjectThatExists(t *testing.T) {
	f := newConsole(t)

	rec, _ := f.send(t, http.MethodPut, "/api/v1/budgets", f.adminToken, map[string]any{
		"scope": "user", "scope_id": "usr_nobody", "period": "month", "hard_limit": "10.00",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a budget on a user who does not exist = %d, want 400", rec.Code)
	}

	rec, _ = f.send(t, http.MethodPut, "/api/v1/budgets", f.adminToken, map[string]any{
		"scope": "user", "scope_id": f.developer.ID, "period": "month", "hard_limit": "10.00",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("a budget on somebody who is here = %d, want 200", rec.Code)
	}
}

// TestExportsCarryTheUnattributedRow. An export that dropped it would be the
// false hundred per cent this product exists not to publish.
func TestExportsCarryTheUnattributedRow(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	session := f.attribute(t, "fp-1", f.developer.ID, "feature/PAY-1", "PAY-1", now)
	f.recordIn(t, f.developer.ID, "attributed", session.ID, 3_000_000, now)
	f.recordIn(t, f.developer.ID, "orphan", "", 7_000_000, now)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/export/work.csv", nil)
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	rec := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content type = %q, want text/csv", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the export does not forbid content sniffing; a branch name is not markup")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "(unattributed)") {
		t.Errorf("the work export dropped the unattributed row:\n%s", body)
	}
	if !strings.Contains(body, "PAY-1") {
		t.Errorf("the work export is missing the attributed row:\n%s", body)
	}
}

// TestIntegrationsAlwaysListBothSystems, connected or not, with what is lost
// without each.
func TestIntegrationsAlwaysListBothSystems(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodGet, "/api/v1/integrations", f.devToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}

	rows, _ := body["integrations"].([]any)
	if len(rows) != 2 {
		t.Fatalf("%d integrations listed, want github and jira whether or not they are on", len(rows))
	}
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if entry["connected"] != false {
			t.Errorf("%v reported as connected on a deployment with nothing configured", entry["kind"])
		}
		if degraded, _ := entry["degraded"].(string); degraded == "" {
			t.Errorf("%v does not say what is lost without it", entry["kind"])
		}
	}
}

// TestOneGroupCannotBeMappedTwiceInDifferentCase. ResolveRole lowercases both
// sides, so two mappings differing only in case are one group with two answers,
// and the one that binds depends on the order rows come back in. The console
// refuses the pair; this is the server refusing it too, because this document
// is also written by scripts holding a gateway token.
func TestOneGroupCannotBeMappedTwiceInDifferentCase(t *testing.T) {
	f := newConsole(t)

	rec, _ := f.send(t, http.MethodPut, "/api/v1/identity/groups", f.adminToken, map[string]any{
		"groups": []map[string]string{
			{"group": "Platform", "role": "admin"},
			{"group": "platform", "role": "developer"},
		},
		"default_role": "developer",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mapping one group in two casings = %d, want 400", rec.Code)
	}

	_, after := f.send(t, http.MethodGet, "/api/v1/identity", f.adminToken, nil)
	if groups, _ := after["groups"].([]any); len(groups) != 0 {
		t.Fatalf("%d mappings written by a refused save, want none", len(groups))
	}

	// One casing is still one group, so the ordinary save is untouched.
	rec, _ = f.send(t, http.MethodPut, "/api/v1/identity/groups", f.adminToken, map[string]any{
		"groups":       []map[string]string{{"group": "Platform", "role": "admin"}},
		"default_role": "developer",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("mapping one group = %d, want 200", rec.Code)
	}
}

// TestAStaleMappingSaveIsRefused.
//
// The mapping is replaced whole, so a console tab that loaded it before a
// change carries an opinion about rows that did not exist then - and saving
// deletes them: a mapping that empties itself is a demotion nobody performed.
func TestAStaleMappingSaveIsRefused(t *testing.T) {
	f := newConsole(t)

	_, loaded := f.send(t, http.MethodGet, "/api/v1/identity", f.adminToken, nil)
	stale, _ := loaded["mapping_version"].(string)
	if stale == "" {
		t.Fatal("no mapping_version served, so a console has nothing to save against")
	}

	// Somebody else maps a group to admin.
	rec, _ := f.send(t, http.MethodPut, "/api/v1/identity/groups", f.adminToken, map[string]any{
		"groups":       []map[string]string{{"group": "platform", "role": "admin"}},
		"default_role": "developer",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("mapping a group = %d", rec.Code)
	}

	// The tab that loaded before it saves what it believes: an empty mapping.
	rec, _ = f.send(t, http.MethodPut, "/api/v1/identity/groups", f.adminToken, map[string]any{
		"groups":          []any{},
		"default_role":    "developer",
		"mapping_version": stale,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a stale save got %d, want 409 - it silently deleted an admin mapping", rec.Code)
	}

	_, after := f.send(t, http.MethodGet, "/api/v1/identity", f.adminToken, nil)
	groups, _ := after["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("%d mappings after the refused save, want the admin mapping intact", len(groups))
	}

	// And a save against the current version goes through, so the refusal is a
	// concurrency check rather than a lock.
	fresh, _ := after["mapping_version"].(string)
	rec, _ = f.send(t, http.MethodPut, "/api/v1/identity/groups", f.adminToken, map[string]any{
		"groups":          []map[string]string{{"group": "platform", "role": "manager"}},
		"default_role":    "developer",
		"mapping_version": fresh,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("a current save got %d, want 200", rec.Code)
	}
}

// TestATeamRosterIsReplacedInOneRequest. The console edits membership as a list
// of checkboxes; translating that into a stream of add and remove calls would
// leave a half-applied roster on the first failure, with nothing on the screen
// saying which half landed.
func TestATeamRosterIsReplacedInOneRequest(t *testing.T) {
	f := newConsole(t)
	other, _ := f.newUser(t, "third@example.com", store.RoleDeveloper)

	rec, body := f.send(t, http.MethodPost, "/api/v1/teams", f.adminToken,
		map[string]string{"name": "Platform"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	slug, _ := body["slug"].(string)

	rec, _ = f.send(t, http.MethodPost, "/api/v1/teams/"+slug, f.adminToken, map[string]any{
		"members": []any{
			map[string]string{"user_id": f.developer.ID, "role": "lead"},
			map[string]string{"user_id": other.ID, "role": "member"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set roster = %d %s", rec.Code, rec.Body.String())
	}

	_, body = f.send(t, http.MethodGet, "/api/v1/teams/"+slug, f.adminToken, nil)
	if members, _ := body["members"].([]any); len(members) != 2 {
		t.Fatalf("%d members, want 2", len(members))
	}

	// The roster is the whole membership, so somebody absent from it is off the
	// team. Sending the same list twice changes nothing.
	rec, _ = f.send(t, http.MethodPost, "/api/v1/teams/"+slug, f.adminToken, map[string]any{
		"members": []any{map[string]string{"user_id": other.ID, "role": "member"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("shrink roster = %d %s", rec.Code, rec.Body.String())
	}
	_, body = f.send(t, http.MethodGet, "/api/v1/teams/"+slug, f.adminToken, nil)
	members, _ := body["members"].([]any)
	if len(members) != 1 {
		t.Fatalf("%d members after shrinking, want 1", len(members))
	}
	if first, ok := members[0].(map[string]any); !ok || first["user_id"] != other.ID {
		t.Errorf("the wrong person survived: %v", members[0])
	}

	// An empty list is a real instruction and not a missing field.
	rec, _ = f.send(t, http.MethodPost, "/api/v1/teams/"+slug, f.adminToken,
		map[string]any{"members": []any{}})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty roster = %d %s", rec.Code, rec.Body.String())
	}
	_, body = f.send(t, http.MethodGet, "/api/v1/teams/"+slug, f.adminToken, nil)
	if members, _ := body["members"].([]any); len(members) != 0 {
		t.Errorf("%d members after emptying the roster, want 0", len(members))
	}
}

// TestARosterNamingSomebodyWhoIsGoneIsRefused. Written before the check
// existed: a membership row referencing nobody fails on the foreign key and
// arrives as a 500, which reads as "Panorama is broken" rather than "that
// person has left".
func TestARosterNamingSomebodyWhoIsGoneIsRefused(t *testing.T) {
	f := newConsole(t)

	_, body := f.send(t, http.MethodPost, "/api/v1/teams", f.adminToken,
		map[string]string{"name": "Platform"})
	slug, _ := body["slug"].(string)

	rec, _ := f.send(t, http.MethodPost, "/api/v1/teams/"+slug, f.adminToken, map[string]any{
		"members": []any{map[string]string{"user_id": "usr_nobody", "role": "member"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a roster naming a stranger = %d, want 400", rec.Code)
	}
}

// TestDeletingATeamIsRefusedWhileAPipelineBillsIt. `service_tokens.team_id`
// cascades, so deleting the team revokes those credentials - silently, at
// whatever hour the pipeline next runs.
func TestDeletingATeamIsRefusedWhileAPipelineBillsIt(t *testing.T) {
	f := newConsole(t)

	_, body := f.send(t, http.MethodPost, "/api/v1/teams", f.adminToken,
		map[string]string{"name": "Platform"})
	slug, _ := body["slug"].(string)
	teamID, _ := body["id"].(string)

	minted, err := auth.GenerateService()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token, err := f.db.CreateServiceToken(t.Context(), teamID, f.admin.ID, "ci", minted.Hash, minted.DisplayPrefix, nil)
	if err != nil {
		t.Fatalf("create service token: %v", err)
	}

	rec, _ := f.send(t, http.MethodDelete, "/api/v1/teams/"+slug, f.adminToken, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete with a live service token = %d, want 409", rec.Code)
	}

	if rerr := f.db.RevokeServiceToken(t.Context(), token.ID); rerr != nil {
		t.Fatalf("revoke: %v", rerr)
	}
	rec, _ = f.send(t, http.MethodDelete, "/api/v1/teams/"+slug, f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete after revoking = %d %s", rec.Code, rec.Body.String())
	}

	rec, _ = f.send(t, http.MethodGet, "/api/v1/teams/"+slug, f.adminToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("the team is still there: %d", rec.Code)
	}
}

// TestClaudeCodeIsNotToldAWindow.
//
// A route for Claude Code is named with its model's Anthropic ID, so Claude
// Code knows the window and output cap itself. The variables that would say
// them can describe only the served primary route, and a reader who picked
// another route would paste that route's name beside the first one's window -
// so neither the copy-paste block nor the settings file carries them, even for
// a model whose window this deployment has recorded.
func TestClaudeCodeIsNotToldAWindow(t *testing.T) {
	f := newConsole(t)
	// The settings file is served as part of the `cli` block, which a
	// deployment with command-line sign-in switched off does not get - and the
	// two renderings agreeing is exactly what this checks.
	f.signInFromTheCommandLine(t)

	if err := f.db.ReplaceCatalogue(t.Context(), providers.KindBedrock, []store.CatalogueModel{{
		ID:                "us.anthropic.claude-sonnet-5",
		FoundationModelID: "anthropic.claude-sonnet-5",
		Vendor:            "Anthropic", DisplayName: "Claude Sonnet 5",
		Geography: "us", Regions: []string{"us-east-1"},
		Streaming: true, Lifecycle: "ACTIVE", RequiresProfile: true,
		ContextWindow: 1_000_000, MaxOutputTokens: 64_000,
		APIs: []providers.API{providers.APIAnthropicMessages},
	}}); err != nil {
		t.Fatalf("seed catalogue: %v", err)
	}

	rec, _ := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "standard", "target": "bedrock/us.anthropic.claude-sonnet-5",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("creating the route = %d: %s", rec.Code, rec.Body)
	}
	if err := f.policy.Load(t.Context()); err != nil {
		t.Fatalf("reload policy: %v", err)
	}

	rec, body := f.send(t, http.MethodGet, "/api/v1/setup/tools", f.devToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}

	var claude map[string]any
	for _, tl := range mapsOf(body["tools"]) {
		if tl["id"] == "claude-code" {
			claude = tl
		}
	}
	if claude == nil {
		t.Fatal("no Claude Code row")
	}

	env := map[string]string{}
	for _, row := range mapsOf(claude["env"]) {
		name, _ := row["name"].(string)
		value, _ := row["value"].(string)
		env[name] = value
	}

	for _, name := range windowVariables {
		if got, ok := env[name]; ok {
			t.Errorf("the shell block sets %s=%s", name, got)
		}
	}

	cli, _ := claude["cli"].(map[string]any)
	settings, _ := cli["settings"].(map[string]any)
	fileEnv, _ := settings["env"].(map[string]any)
	if fileEnv == nil {
		t.Fatal("the settings block carries no env")
	}
	for _, name := range windowVariables {
		if got, ok := fileEnv[name]; ok {
			t.Errorf("the settings file sets %s=%v", name, got)
		}
	}
}

// windowVariables are the two Claude Code variables the setup never sends.
var windowVariables = []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS", "CLAUDE_CODE_MAX_OUTPUT_TOKENS"}
