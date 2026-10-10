// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
)

// consoleFixture is a server with a migrated database, a mounted console, and
// two users with tokens.
type consoleFixture struct {
	server   *Server
	db       *store.DB
	policy   *policy.Engine
	registry *providers.Registry
	// provider is what the fixture's registry builds for both Bedrock kinds:
	// it records what the live check invokes and answers from its catalogue.
	provider   *recordingProvider
	adminToken string
	devToken   string
	admin      *store.User
	developer  *store.User
}

// newConsole builds a console fixture. The options adjust ConsoleOptions before
// mounting, which is how a test reproduces a deployment whose models come from
// its configuration file rather than from the console.
func newConsole(t *testing.T, options ...func(*ConsoleOptions)) *consoleFixture {
	t.Helper()

	cfg := config.Default()
	cfg.Database.DSN = "file:" + migratedDatabaseFile(t)

	db, err := store.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	srv := New(cfg, db, discardLogger())

	seed, err := pricing.Seed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	provider := &recordingProvider{}
	registry := providers.NewRegistry(discardLogger(), testDescriptors(provider)...)
	consoleOpts := ConsoleOptions{Prices: pricing.NewTable(seed), Providers: registry}
	for _, apply := range options {
		apply(&consoleOpts)
	}
	// A negative reload interval keeps the engine one-shot: the fixture loads
	// policy explicitly when a test needs it, rather than racing a ticker.
	engine := policy.New(policy.Options{
		Repository: db, Providers: consoleOpts.Providers, ReloadInterval: -1, Logger: discardLogger(),
	})
	consoleOpts.Policy = engine
	srv.MountConsole(consoleOpts)

	// Every fixture has a serving bedrock provider, which is what a deployment
	// that can route anything has. A test about having none removes it.
	if _, err := db.SaveProvider(t.Context(), providers.Settings{
		Kind: providers.KindBedrock, Enabled: true, Region: "us-east-1",
	}); err != nil {
		t.Fatalf("configure the bedrock provider: %v", err)
	}
	if err := engine.Load(t.Context()); err != nil {
		t.Fatalf("load policy: %v", err)
	}

	f := &consoleFixture{server: srv, db: db, policy: engine, registry: registry, provider: provider}
	f.admin, f.adminToken = f.newUser(t, "admin@example.com", store.RoleAdmin)
	f.developer, f.devToken = f.newUser(t, "dev@example.com", store.RoleDeveloper)
	return f
}

// signInFromTheCommandLine switches on the credential path a test is about.
//
// It ships off - this release does not hand `panorama login` to engineers, so
// a fixture out of the box refuses the device flow and serves no `cli` block on
// any tool. The row is what the server reads, so this is exactly what a
// deployment that wanted the flow would have in its database, and the day
// cliSignInReleased flips nothing here changes.
func (f *consoleFixture) signInFromTheCommandLine(t *testing.T) {
	t.Helper()

	if err := f.db.SetSetting(t.Context(), settingCLISignIn, "true"); err != nil {
		t.Fatalf("switching command-line sign-in on: %v", err)
	}
}

func (f *consoleFixture) newUser(t *testing.T, email, role string) (*store.User, string) {
	t.Helper()

	user, err := f.db.CreateUser(t.Context(), email, email, role)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	minted, err := auth.Generate()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if _, err := f.db.CreateGatewayToken(t.Context(), user.ID, "test", minted.Hash, minted.DisplayPrefix, nil); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return user, minted.Plaintext
}

// get issues an authenticated console request.
func (f *consoleFixture) get(t *testing.T, path, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, req)

	var body map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		decodeJSON(t, rec.Body.Bytes(), &body)
	}
	return rec, body
}

// record writes one request to the audit trail.
func (f *consoleFixture) record(t *testing.T, userID, requestID, model string, cost *pricing.Cost, at time.Time) {
	t.Helper()

	rec := store.RequestRecord{
		RequestID: requestID, UserID: userID, StartedAt: at,
		Surface: "anthropic", Provider: "bedrock",
		RequestedModel: "claude-sonnet-5", ResolvedModel: model,
		Tool: "claude-code", StatusCode: 200, Decision: store.DecisionAllowed,
		InputTokens: 100, OutputTokens: 50, CacheReadTokens: 9000, CacheWriteTokens: 300,
		Cost: cost,
	}
	if cost != nil {
		rec.CostSource = pricing.SourceSeed
	}
	if err := f.db.RecordRequest(t.Context(), rec); err != nil {
		t.Fatalf("record request: %v", err)
	}
}

func money(total int64) *pricing.Cost {
	return &pricing.Cost{Input: pricing.Money(total), Total: pricing.Money(total)}
}

// TestConsoleAPIRequiresAuthentication is the non-negotiable: no route into the
// console API answers without a valid token.
func TestConsoleAPIRequiresAuthentication(t *testing.T) {
	f := newConsole(t)

	for _, path := range []string{
		"/api/v1/me", "/api/v1/requests", "/api/v1/requests/groups", "/api/v1/usage", "/api/v1/prices",
	} {
		rec, _ := f.get(t, path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, rec.Code)
		}

		rec, _ = f.get(t, path, "pnt_not-a-real-token")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a bogus token = %d, want 401", path, rec.Code)
		}
	}
}

func TestConsoleAPIRejectsARevokedToken(t *testing.T) {
	f := newConsole(t)

	page, err := f.db.ListRequests(t.Context(), store.RequestFilter{})
	if err != nil || page == nil {
		t.Fatalf("sanity: %v", err)
	}

	tokens, err := f.db.AuthenticateToken(t.Context(), auth.Hash(f.devToken))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.RevokeToken(t.Context(), tokens.TokenID); err != nil {
		t.Fatal(err)
	}

	rec, _ := f.get(t, "/api/v1/me", f.devToken)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a revoked token = %d, want 401", rec.Code)
	}
}

func TestMeReportsTheCaller(t *testing.T) {
	f := newConsole(t)

	rec, body := f.get(t, "/api/v1/me", f.devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	user, _ := body["user"].(map[string]any)
	if user["email"] != "dev@example.com" {
		t.Errorf("email = %v, want dev@example.com", user["email"])
	}
	if body["can_see_everyone"] != false {
		t.Errorf("can_see_everyone = %v, want false for a developer", body["can_see_everyone"])
	}

	_, adminBody := f.get(t, "/api/v1/me", f.adminToken)
	if adminBody["can_see_everyone"] != true {
		t.Errorf("can_see_everyone = %v, want true for an admin", adminBody["can_see_everyone"])
	}
}

// TestDeveloperSeesOnlyTheirOwnRequests is the trust stance enforced at the
// API: an engineer's own usage is theirs to see, and nobody else's is.
func TestDeveloperSeesOnlyTheirOwnRequests(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	f.record(t, f.developer.ID, "dev-1", "us.anthropic.claude-sonnet-5", money(1_000_000), now)
	f.record(t, f.admin.ID, "admin-1", "us.anthropic.claude-sonnet-5", money(5_000_000), now)

	_, body := f.get(t, "/api/v1/requests", f.devToken)
	requests, _ := body["requests"].([]any)
	if len(requests) != 1 {
		t.Fatalf("a developer saw %d requests, want only their own", len(requests))
	}
	first, _ := requests[0].(map[string]any)
	if first["request_id"] != "dev-1" {
		t.Errorf("a developer saw %v, which is not theirs", first["request_id"])
	}

	// An attempt to widen the scope by hand must not work.
	_, body = f.get(t, "/api/v1/requests?scope=organization", f.devToken)
	requests, _ = body["requests"].([]any)
	if len(requests) != 1 {
		t.Errorf("a developer widened their own scope to %d requests", len(requests))
	}

	// The admin sees the whole audit trail, which is not a comparison but a log.
	_, body = f.get(t, "/api/v1/requests", f.adminToken)
	requests, _ = body["requests"].([]any)
	if len(requests) != 2 {
		t.Errorf("an admin saw %d requests, want the whole trail", len(requests))
	}
}

// TestDeveloperSeesOnlyTheirOwnGroups is the same stance on the grouped
// trail, including the one parameter that names somebody else's rows: a group
// id is a filter inside the caller's scope, never a way out of it.
func TestDeveloperSeesOnlyTheirOwnGroups(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	f.record(t, f.developer.ID, "dev-1", "us.anthropic.claude-sonnet-5", money(1_000_000), now.Add(-2*time.Minute))
	f.record(t, f.developer.ID, "dev-2", "us.anthropic.claude-sonnet-5", money(2_000_000), now.Add(-time.Minute))
	f.record(t, f.admin.ID, "admin-1", "us.anthropic.claude-sonnet-5", money(5_000_000), now)

	_, body := f.get(t, "/api/v1/requests/groups", f.devToken)
	groups, _ := body["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("a developer saw %d groups, want only their own", len(groups))
	}
	mine, _ := groups[0].(map[string]any)
	if mine["requests"] != float64(2) {
		t.Errorf("the developer's group holds %v requests, want both of theirs", mine["requests"])
	}
	cost, _ := mine["cost"].(map[string]any)
	if cost["total_nanos"] != float64(3_000_000) {
		t.Errorf("the developer's group costs %v nanos, want the sum of its requests", cost["total_nanos"])
	}
	outcomes, _ := body["outcomes"].(map[string]any)
	requestOutcomes, _ := body["request_outcomes"].(map[string]any)
	if outcomes["all"] != float64(1) || requestOutcomes["all"] != float64(2) {
		t.Errorf("counts = %v groups, %v requests; want 1 and 2", outcomes["all"], requestOutcomes["all"])
	}

	_, body = f.get(t, "/api/v1/requests/groups?scope=organization", f.devToken)
	if groups, _ = body["groups"].([]any); len(groups) != 1 {
		t.Errorf("a developer widened their own scope to %d groups", len(groups))
	}

	_, body = f.get(t, "/api/v1/requests/groups", f.adminToken)
	groups, _ = body["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("an admin saw %d groups, want both people's", len(groups))
	}
	theirs, _ := groups[0].(map[string]any)
	if theirs["user_email"] == "dev@example.com" {
		theirs, _ = groups[1].(map[string]any)
	}

	id, ok := theirs["id"].(string)
	if !ok || id == "" {
		t.Fatalf("the admin's group has no id: %v", theirs["id"])
	}

	// The admin's group id, asked for by the developer, expands to nothing.
	_, body = f.get(t, "/api/v1/requests?group="+id, f.devToken)
	if requests, _ := body["requests"].([]any); len(requests) != 0 {
		t.Errorf("a developer expanded somebody else's group to %d requests", len(requests))
	}
	// And to its own request for the admin.
	_, body = f.get(t, "/api/v1/requests?group="+id, f.adminToken)
	if requests, _ := body["requests"].([]any); len(requests) != 1 {
		t.Errorf("the admin's group expanded to %d requests, want 1", len(requests))
	}
}

func TestUsageScopesToTheDeveloper(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	f.record(t, f.developer.ID, "dev-1", "us.anthropic.claude-sonnet-5", money(1_000_000), now)
	f.record(t, f.admin.ID, "admin-1", "us.anthropic.claude-sonnet-5", money(5_000_000), now)

	_, body := f.get(t, "/api/v1/usage", f.devToken)
	if body["scope"] != "self" {
		t.Errorf("scope = %v, want self", body["scope"])
	}
	totals, _ := body["totals"].(map[string]any)
	cost, _ := totals["cost"].(map[string]any)
	if cost["total_usd"] != "0.001000000" {
		t.Errorf("a developer's total = %v, want only their own $0.001", cost["total_usd"])
	}

	_, body = f.get(t, "/api/v1/usage", f.adminToken)
	if body["scope"] != "organization" {
		t.Errorf("scope = %v, want organization", body["scope"])
	}
	totals, _ = body["totals"].(map[string]any)
	cost, _ = totals["cost"].(map[string]any)
	if cost["total_usd"] != "0.006000000" {
		t.Errorf("the organization total = %v, want $0.006", cost["total_usd"])
	}

	// An admin can still ask for their own.
	_, body = f.get(t, "/api/v1/usage?scope=me", f.adminToken)
	if body["scope"] != "self" {
		t.Errorf("scope = %v, want self when asked for", body["scope"])
	}
}

// TestUnpricedSpendIsAlwaysReported keeps the product commitment: a total never
// hides what it could not price.
func TestUnpricedSpendIsAlwaysReported(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	f.record(t, f.admin.ID, "priced", "us.anthropic.claude-sonnet-5", money(2_000_000), now)
	f.record(t, f.admin.ID, "unpriced", "us.anthropic.claude-unknown", nil, now)

	_, body := f.get(t, "/api/v1/usage", f.adminToken)
	totals, _ := body["totals"].(map[string]any)

	if totals["unpriced_requests"] != float64(1) {
		t.Errorf("unpriced_requests = %v, want 1", totals["unpriced_requests"])
	}
	if totals["unpriced_billable"] != float64(1) {
		t.Errorf("unpriced_billable = %v, want 1", totals["unpriced_billable"])
	}

	// And the individual row says so rather than showing zero.
	_, body = f.get(t, "/api/v1/requests", f.adminToken)
	requests, _ := body["requests"].([]any)
	for _, raw := range requests {
		row, _ := raw.(map[string]any)
		if row["request_id"] != "unpriced" {
			continue
		}
		if row["priced"] != false {
			t.Errorf("priced = %v, want false", row["priced"])
		}
		if _, present := row["cost"]; present {
			t.Error("an unpriced request must not carry a cost field at all")
		}
	}
}

// TestCostCrossesTheWireAsAnIntegerAndAString guards against the rounding that
// a JSON number would introduce.
func TestCostCrossesTheWireAsAnIntegerAndAString(t *testing.T) {
	f := newConsole(t)

	// A cost with more precision than a float64 can hold exactly.
	f.record(t, f.admin.ID, "precise", "us.anthropic.claude-sonnet-5",
		&pricing.Cost{Input: 22_520_701, Total: 22_520_701}, time.Now().UTC())

	_, body := f.get(t, "/api/v1/requests", f.adminToken)
	requests, _ := body["requests"].([]any)
	row, _ := requests[0].(map[string]any)
	cost, _ := row["cost"].(map[string]any)

	if cost["total_usd"] != "0.022520701" {
		t.Errorf("total_usd = %v, want the exact decimal 0.022520701", cost["total_usd"])
	}
	if cost["total_nanos"] != float64(22_520_701) {
		t.Errorf("total_nanos = %v, want 22520701", cost["total_nanos"])
	}
	if _, ok := cost["by_class"].(map[string]any); !ok {
		t.Error("the class breakdown is missing; it is the evidence for the cache claim")
	}
}

func TestRequestsPaginate(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	// In the past: the default window ends at "now", so a request stamped even
	// a second ahead falls outside it.
	for i := range 5 {
		f.record(t, f.admin.ID, "r"+string(rune('a'+i)), "us.anthropic.claude-sonnet-5",
			money(1_000_000), now.Add(-time.Duration(i+1)*time.Second))
	}

	_, body := f.get(t, "/api/v1/requests?limit=2", f.adminToken)
	requests, _ := body["requests"].([]any)
	cursor, _ := body["next_cursor"].(string)
	if len(requests) != 2 || cursor == "" {
		t.Fatalf("first page had %d rows and cursor %q", len(requests), cursor)
	}

	_, body = f.get(t, "/api/v1/requests?limit=2&cursor="+cursor, f.adminToken)
	next, _ := body["requests"].([]any)
	if len(next) != 2 {
		t.Errorf("second page had %d rows, want 2", len(next))
	}

	rec, _ := f.get(t, "/api/v1/requests?cursor=garbage", f.adminToken)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a bad cursor = %d, want 400", rec.Code)
	}
}

func TestUsageRejectsABadTimeRange(t *testing.T) {
	f := newConsole(t)

	for _, query := range []string{
		"?since=yesterday",
		"?since=2026-01-02T00:00:00Z&until=2026-01-01T00:00:00Z",
	} {
		rec, _ := f.get(t, "/api/v1/usage"+query, f.adminToken)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("usage%s = %d, want 400", query, rec.Code)
		}
	}

	// The forms a dashboard actually sends must work.
	for _, query := range []string{"?since=24h", "?since=7d", "?since=2026-01-01T00:00:00Z"} {
		rec, _ := f.get(t, "/api/v1/usage"+query, f.adminToken)
		if rec.Code != http.StatusOK {
			t.Errorf("usage%s = %d, want 200", query, rec.Code)
		}
	}
}

func TestPricesAreServedWithTheirProvenance(t *testing.T) {
	f := newConsole(t)

	rec, body := f.get(t, "/api/v1/prices", f.devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	prices, _ := body["prices"].([]any)
	if len(prices) == 0 {
		t.Fatal("no prices served")
	}

	row, _ := prices[0].(map[string]any)
	for _, field := range []string{"provider", "model", "scope", "source", "rates", "currency"} {
		if _, ok := row[field]; !ok {
			t.Errorf("a price is missing %q; an unexplained cost is an argument", field)
		}
	}
	rates, _ := row["rates"].(map[string]any)
	if _, ok := rates["cache_write_1h"].(string); !ok {
		t.Error("rates must include the one-hour cache write, as a string")
	}
}

// TestRequestFiltersReachTheQuery checks the filter boxes on the audit trail
// against the endpoint, and - the part that matters - that none of them is a
// way around the scope. `filterFrom` is the single chokepoint for who sees
// whose traffic, and a filter it read before pinning the scope would be a
// second place that decided it.
func TestRequestFiltersReachTheQuery(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	f.record(t, f.developer.ID, "dev-sonnet", "us.anthropic.claude-sonnet-5", money(1_000_000), now)
	f.record(t, f.developer.ID, "dev-haiku", "global.anthropic.claude-haiku-4-5", money(10_000), now)
	f.record(t, f.admin.ID, "admin-sonnet", "us.anthropic.claude-sonnet-5", money(5_000_000), now)

	ids := func(t *testing.T, path, token string) []string {
		t.Helper()
		rec, body := f.get(t, path, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, rec.Code)
		}
		rows, _ := body["requests"].([]any)
		out := make([]string, 0, len(rows))
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			id, _ := row["request_id"].(string)
			out = append(out, id)
		}
		return out
	}

	// An admin narrowing to one person, by part of their address.
	got := ids(t, "/api/v1/requests?user=DEV@", f.adminToken)
	if len(got) != 2 {
		t.Errorf("a user filter matched %v, want the developer's two requests", got)
	}

	// And to one model, by part of its id.
	got = ids(t, "/api/v1/requests?model=haiku", f.adminToken)
	if len(got) != 1 || got[0] != "dev-haiku" {
		t.Errorf("a model filter matched %v, want only dev-haiku", got)
	}

	// A developer typing a colleague's address gets nothing, not the colleague.
	got = ids(t, "/api/v1/requests?user=admin@example.com", f.devToken)
	if len(got) != 0 {
		t.Errorf("a developer filtering by a colleague saw %v; a filter must never widen", got)
	}

	// The same filter narrows the aggregate, so a screen and its export cannot
	// disagree about what was asked for.
	_, body := f.get(t, "/api/v1/usage?model=haiku", f.adminToken)
	totals, _ := body["totals"].(map[string]any)
	if totals["requests"] != float64(1) {
		t.Errorf("usage under a model filter counted %v requests, want 1", totals["requests"])
	}
}

// TestRequestRowsCarryTheWorkItem checks that the work a request paid for
// arrives on the row rather than having to be looked up in a second call whose
// own page limit decides whether it is there.
func TestRequestRowsCarryTheWorkItem(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	session, err := f.db.AttributeSession(t.Context(), store.Observation{
		Fingerprint: "fp-1", Method: "conversation-prefix", UserID: f.developer.ID,
		Tool: "claude-code", At: now,
		Signals: attribution.Signals{
			Repo:   attribution.Observation{Value: "acme/panorama", Rung: attribution.RungStructural, Evidence: "git remote"},
			Branch: attribution.Observation{Value: "feat/PAY-1183", Rung: attribution.RungStructural, Evidence: "environment block"},
			Ticket: attribution.Observation{Value: "PAY-1183", Rung: attribution.RungStructural, Evidence: "branch name"},
		},
	})
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}

	rec := store.RequestRecord{
		RequestID: "r1", UserID: f.developer.ID, StartedAt: now, SessionID: session.ID,
		Surface: "anthropic", Provider: "bedrock",
		RequestedModel: "default", ResolvedModel: "us.anthropic.claude-sonnet-5",
		Tool: "claude-code", StatusCode: 200, Decision: store.DecisionAllowed, Attempts: 1,
	}
	if rerr := f.db.RecordRequest(t.Context(), rec); rerr != nil {
		t.Fatal(rerr)
	}

	_, body := f.get(t, "/api/v1/requests", f.devToken)
	rows, _ := body["requests"].([]any)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if row["repo"] != "acme/panorama" || row["branch"] != "feat/PAY-1183" {
		t.Errorf("repo/branch = %v/%v, want the session's", row["repo"], row["branch"])
	}
	if row["work_item"] != "PAY-1183" || row["rung"] != "structural" {
		t.Errorf("work item = %v (%v), want PAY-1183 (structural)", row["work_item"], row["rung"])
	}

	// And the filter that searches them finds it.
	_, body = f.get(t, "/api/v1/requests?repo=PANORAMA&branch=pay-1183", f.devToken)
	rows, _ = body["requests"].([]any)
	if len(rows) != 1 {
		t.Errorf("a repository and branch filter matched %d rows, want 1", len(rows))
	}
}
