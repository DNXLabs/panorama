// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
)

// cheapModel is what the economy route this file's soft-limit tests offer runs
// on. A provider-native id, not a name a client could ask for.
const cheapModel = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

// defaultRoute is the ordinary route the fixtures serve: the one name a client
// may ask for, pointing at the model the fake provider expects.
func defaultRoute() policy.Route {
	return policy.Route{Name: testModel, Target: bedrock(resolvedModel), ServesOverSoft: true}
}

// routeStoppingAtSoft is the expensive route an administrator marks as not
// surviving a soft limit - the one an engineer is asked to switch off.
func routeStoppingAtSoft() policy.Route {
	r := defaultRoute()
	r.ServesOverSoft = false
	return r
}

// economyRoute is where they are told to go instead.
func economyRoute(name, target string) policy.Route {
	return policy.Route{Name: name, Target: bedrock(target), ServesOverSoft: true}
}

// overSoft is a budget that is already past its warning line.
func overSoft() []policy.Budget {
	soft := pricing.Money(1_000_000_000) // $1
	return []policy.Budget{{
		ID: "bdg_1", Scope: policy.ScopeOrg, Period: policy.PeriodDay,
		Name: "deployment", Enabled: true, SoftLimit: &soft,
	}}
}

// newPolicyFixture builds a data plane whose policy engine is loaded from an
// in-memory set, with the background loop switched off.
func newPolicyFixture(t *testing.T, provider *fakeProvider, set policy.Set) *fixture {
	t.Helper()

	if len(set.Routes) == 0 {
		set.Routes = []policy.Route{defaultRoute()}
	}
	return newFixtureWith(t, fixtureConfig{provider: provider, set: &set})
}

type stubPolicyRepo struct {
	set   policy.Set
	spend map[string]map[string]policy.Spend
}

func (s *stubPolicyRepo) LoadPolicy(context.Context) (policy.Set, error) { return s.set, nil }

func (s *stubPolicyRepo) SpendSince(_ context.Context, scope string, _ time.Time) (map[string]policy.Spend, error) {
	return s.spend[scope], nil
}

func (s *stubPolicyRepo) PolicyGeneration(context.Context) (int64, error) { return 0, nil }

// A route is an alias: the client names it and never names the model behind it.
func TestARouteResolvesToItsModel(t *testing.T) {
	provider := &fakeProvider{}
	f := newPolicyFixture(t, provider, policy.Set{})

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if provider.lastModel != resolvedModel {
		t.Errorf("invoked %q, want the route's model %q", provider.lastModel, resolvedModel)
	}

	f.drain()
	row := f.latest(t)
	if row.Decision != store.DecisionAllowed {
		t.Errorf("decision = %q, want allowed: nothing moved this request", row.Decision)
	}
	if row.RequestedModel != testModel {
		t.Errorf("requested_model = %q; what the client asked for was overwritten", row.RequestedModel)
	}
}

// Over the soft limit the expensive route stops, and the engineer is told what
// to switch to rather than moved without being told. The provider is never
// reached, so nothing is spent on a request that was refused.
func TestOverTheSoftLimitTheRouteStopsAndNamesTheAlternative(t *testing.T) {
	provider := &fakeProvider{}
	f := newPolicyFixture(t, provider, policy.Set{
		Routes:  []policy.Route{routeStoppingAtSoft(), economyRoute("economy", cheapModel)},
		Budgets: overSoft(),
	})

	f.engine.Record(policy.Completed{
		UserID: f.user.ID, At: time.Now().UTC(),
		Cost: &pricing.Cost{Total: 2_000_000_000}, UsedTokens: true,
	})

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a route that stops at the soft limit served a request", rec.Code)
	}
	if provider.lastModel != "" {
		t.Errorf("invoked %q: a stopped request reached the provider and cost money", provider.lastModel)
	}
	// The message is the remedy, and it travels in the error body because that
	// is the only channel a coding tool renders.
	if !strings.Contains(rec.Body.String(), "economy") {
		t.Errorf("the refusal does not name the route that still works: %s", rec.Body)
	}

	f.drain()
	row := f.latest(t)
	if row.Decision != store.DecisionBlocked {
		t.Errorf("decision = %q, want blocked", row.Decision)
	}
	// The pair is what tells this apart from running out of money, and it is
	// what the console reads to say which of the two happened.
	if row.ErrorKind != policy.BlockedOverSoft {
		t.Errorf("error_kind = %q, want %q", row.ErrorKind, policy.BlockedOverSoft)
	}
	if row.BudgetState != string(policy.StateOverSoft) {
		t.Errorf("budget_state = %q, want over_soft", row.BudgetState)
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d: a stopped request must not count against the budget that stopped it", row.Attempts)
	}
}

// A route that says nothing about soft limits keeps serving past one, warning
// rather than stopping: defaulting the other way would turn a soft limit into a
// hard one.
func TestARouteThatServesPastSoftIsOnlyWarned(t *testing.T) {
	provider := &fakeProvider{}
	f := newPolicyFixture(t, provider, policy.Set{
		Routes:  []policy.Route{defaultRoute()},
		Budgets: overSoft(),
	})

	f.engine.Record(policy.Completed{
		UserID: f.user.ID, At: time.Now().UTC(),
		Cost: &pricing.Cost{Total: 2_000_000_000}, UsedTokens: true,
	})

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a soft limit stopped a route that serves past one", rec.Code)
	}
	if provider.lastModel != resolvedModel {
		t.Errorf("invoked %q, want the route's own model", provider.lastModel)
	}
	if rec.Header().Get("x-panorama-budget-warning") == "" {
		t.Error("crossing the soft limit was not reported in the response headers")
	}

	f.drain()
	row := f.latest(t)
	if row.BudgetState != string(policy.StateOverSoft) {
		t.Errorf("budget_state = %q, want over_soft", row.BudgetState)
	}
}

// A client cannot name a model. The route is the only name, and a model id is
// exactly what a misconfigured tool sends.
func TestAClientCannotNameAModelDirectly(t *testing.T) {
	provider := &fakeProvider{}
	f := newPolicyFixture(t, provider, policy.Set{})

	rec := f.do(t, `{"model":"`+resolvedModel+`","max_tokens":16}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: the model behind a route was askable by name", rec.Code)
	}
	if provider.invocations != 0 {
		t.Errorf("the provider was invoked %d times for a name nobody routed", provider.invocations)
	}

	f.drain()
	row := f.latest(t)
	if row.Attempts != 0 {
		t.Errorf("attempts = %d, want 0: a request that never reached a provider must not count against a budget", row.Attempts)
	}
	if row.ErrorKind != policy.RefusedUnknownRoute {
		t.Errorf("error_kind = %q, want %q", row.ErrorKind, policy.RefusedUnknownRoute)
	}
}

// A condition decides who a route exists for, and somebody outside it is
// refused exactly as though the name had never been created.
func TestARouteConditionRefusesSomebodyOutsideIt(t *testing.T) {
	provider := &fakeProvider{}
	restricted := defaultRoute()
	restricted.Teams = []string{"tm_nobody-is-on-this"}

	f := newPolicyFixture(t, provider, policy.Set{Routes: []policy.Route{restricted}})

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if provider.invocations != 0 {
		t.Error("a route restricted to a team served somebody outside it")
	}

	f.drain()
	row := f.latest(t)
	if row.ErrorKind != policy.RefusedNotAvailable {
		t.Errorf("error_kind = %q, want %q - the log cannot tell a condition doing its job from a misconfigured tool",
			row.ErrorKind, policy.RefusedNotAvailable)
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", row.Attempts)
	}
}

func TestPolicyBlocksWithAHumaneMessage(t *testing.T) {
	provider := &fakeProvider{}
	hard := pricing.Money(10_000_000_000) // $10
	f := newPolicyFixture(t, provider, policy.Set{Budgets: []policy.Budget{{
		ID: "bdg_1", Scope: policy.ScopeOrg, Period: policy.PeriodMonth,
		Name: "deployment", Enabled: true, HardLimit: &hard,
	}}})

	// Spend past the limit, the same way a completed request would.
	f.engine.Record(policy.Completed{
		UserID: f.user.ID, At: time.Now().UTC(),
		Cost: &pricing.Cost{Total: 12_000_000_000}, UsedTokens: true,
	})

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if provider.invocations != 0 {
		t.Error("a blocked request still reached the provider")
	}

	// The client is answered in its own error format, so the message lands in
	// the tool's normal error display rather than as an unparseable blob.
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the error body is not JSON: %v", err)
	}
	if body.Error.Type != "permission_error" {
		// Not rate_limit_error: that invites a client to retry with backoff,
		// and a monthly budget would have it retrying for weeks.
		t.Errorf("error type = %q, want permission_error", body.Error.Type)
	}
	for _, want := range []string{"$12.00", "$10.00", "resets", "administrator"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Errorf("the block message does not contain %q:\n%s", want, body.Error.Message)
		}
	}

	f.drain()
	row := f.latest(t)
	if row.Decision != store.DecisionBlocked {
		t.Errorf("decision = %q, want blocked", row.Decision)
	}
	if row.BudgetState != string(policy.StateOverHard) {
		t.Errorf("budget_state = %q, want over_hard", row.BudgetState)
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", row.Attempts)
	}
}

func TestSoftLimitWarnsInAHeaderAndServesTheRequest(t *testing.T) {
	provider := &fakeProvider{}
	soft := pricing.Money(1_000_000_000) // $1
	f := newPolicyFixture(t, provider, policy.Set{Budgets: []policy.Budget{{
		ID: "bdg_1", Scope: policy.ScopeOrg, Period: policy.PeriodDay,
		Name: "deployment", Enabled: true, SoftLimit: &soft,
	}}})

	f.engine.Record(policy.Completed{
		UserID: f.user.ID, At: time.Now().UTC(),
		Cost: &pricing.Cost{Total: 2_000_000_000}, UsedTokens: true,
	})

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a soft limit blocked", rec.Code)
	}
	if got := rec.Header().Get("x-panorama-budget-state"); got != "over_soft" {
		t.Errorf("budget state header = %q, want over_soft", got)
	}
	warning := rec.Header().Get("x-panorama-budget-warning")
	if warning == "" {
		t.Fatal("no warning header on a request over the soft limit")
	}
	if strings.ContainsAny(warning, "\r\n") {
		t.Errorf("the warning header is not a single line: %q", warning)
	}

	f.drain()
	if row := f.latest(t); row.BudgetState != string(policy.StateOverSoft) {
		t.Errorf("budget_state = %q, want over_soft", row.BudgetState)
	}
}

// A route is one model, so a throttle is the client's to retry: it reaches them
// as a 429, which every client this gateway serves handles, after exactly one
// invocation. The request is attempted - it counts, and a budget sees it - and
// it is not priced, because nothing ran.
func TestAThrottleReachesTheClientAsA429(t *testing.T) {
	for _, stream := range []bool{false, true} {
		provider := &fakeProvider{
			invokeErr: &providers.Error{Kind: providers.ErrKindThrottled, StatusCode: 429, Message: "slow down"},
		}
		f := newPolicyFixture(t, provider, policy.Set{})

		body := `{"model":"` + testModel + `","max_tokens":16}`
		if stream {
			body = `{"model":"` + testModel + `","max_tokens":16,"stream":true}`
		}
		rec := f.do(t, body)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("stream=%v: status = %d, want 429", stream, rec.Code)
		}
		var envelope apiError
		decode(t, rec.Body.Bytes(), &envelope)
		if envelope.Error.Type != errTypeRateLimit {
			t.Errorf("stream=%v: error type = %q, want %q so the client backs off and retries",
				stream, envelope.Error.Type, errTypeRateLimit)
		}
		if provider.invocations != 1 {
			t.Errorf("stream=%v: provider invoked %d times, want exactly 1 - there is nowhere else to send it",
				stream, provider.invocations)
		}

		f.drain()
		row := f.latest(t)
		if row.Attempts != 1 {
			t.Errorf("stream=%v: attempts = %d, want 1", stream, row.Attempts)
		}
		if row.Decision != store.DecisionError {
			t.Errorf("stream=%v: recorded %q, want an error", stream, row.Decision)
		}
		if row.Cost != nil {
			t.Errorf("stream=%v: a throttled request was priced", stream)
		}
	}
}

// A route whose provider is switched off, unconfigured, or failed to build has
// nowhere to send a request it admits. That is the deployment's fault: a 502,
// the provider never reached, and the engineer told it is not theirs to fix.
func TestARouteWhoseProviderIsNotServingIsTheDeploymentsFault(t *testing.T) {
	provider := &fakeProvider{}
	off := providers.Settings{Kind: providers.KindBedrock, Enabled: false, Region: "us-east-1"}
	f := newPolicyFixture(t, provider, policy.Set{
		Providers: map[providers.Kind]providers.State{providers.KindBedrock: {Settings: off}},
	})

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "configuration error on this deployment") {
		t.Errorf("the refusal does not say whose fault it is: %s", rec.Body)
	}
	if provider.invocations != 0 {
		t.Error("a route on a disabled provider reached it")
	}

	f.drain()
	if row := f.latest(t); row.ErrorKind != policy.RefusedProviderUnavailable || row.Attempts != 0 {
		t.Errorf("recorded %q with %d attempts, want %q and none", row.ErrorKind, row.Attempts,
			policy.RefusedProviderUnavailable)
	}
}

// A route whose model does not speak the API a request arrived in is refused
// with the endpoint that would work - the route exists for this caller, so
// naming it gives nothing away, and the fix is a base URL in their tool.
func TestARouteOnTheWrongSurfaceNamesTheEndpoint(t *testing.T) {
	provider := &fakeProvider{}
	f := newFixtureWith(t, fixtureConfig{
		provider: provider,
		apis:     []providers.API{providers.APIOpenAIChat},
	})

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16,"messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	for _, want := range []string{"Anthropic Messages", "/v1/chat/completions"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the refusal does not mention %q: %s", want, rec.Body)
		}
	}
	if provider.invocations != 0 {
		t.Error("a request on the wrong surface reached the provider")
	}

	f.drain()
	if row := f.latest(t); row.ErrorKind != policy.RefusedWrongAPI {
		t.Errorf("error_kind = %q, want %q", row.ErrorKind, policy.RefusedWrongAPI)
	}
}

// A deployment with no routes refuses everything, and says so. There is no
// mode in which a request proceeds without a route: the routes are the
// allowlist, so an empty one is a closed door rather than an open one.
func TestNoRoutesRefusesEverything(t *testing.T) {
	provider := &fakeProvider{}
	f := newFixtureWith(t, fixtureConfig{provider: provider, set: &policy.Set{}})

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if provider.invocations != 0 {
		t.Error("a deployment with no routes reached the provider")
	}

	f.drain()
	row := f.latest(t)
	if row.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", row.Attempts)
	}
	if row.BudgetState != "" {
		t.Errorf("budget_state = %q, want empty when no budget governs the request", row.BudgetState)
	}
}

// A request that is served with no budget anywhere records an empty budget
// state rather than inventing one.
func TestNoBudgetLeavesTheStateEmpty(t *testing.T) {
	provider := &fakeProvider{}
	f := newFixture(t, provider)

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	f.drain()
	row := f.latest(t)
	if row.BudgetState != "" {
		t.Errorf("budget_state = %q, want empty", row.BudgetState)
	}
	if row.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", row.Attempts)
	}
}

// TestBudgetLedgerIsFedByCompletedRequests closes the loop: a request that runs
// counts toward the budget that governs the next one.
func TestBudgetLedgerIsFedByCompletedRequests(t *testing.T) {
	provider := &fakeProvider{
		response: &providers.Response{
			Body:  []byte(`{"usage":{"input_tokens":1000,"output_tokens":1000}}`),
			Usage: providers.Usage{InputTokens: 1000, OutputTokens: 1000},
		},
	}

	// Each of these requests costs $0.0066 at the fixture's rates, so two of
	// them are what crosses a one-cent limit.
	limit := pricing.Money(10_000_000) // $0.01
	f := newPolicyFixture(t, provider, policy.Set{Budgets: []policy.Budget{{
		ID: "bdg_1", Scope: policy.ScopeOrg, Period: policy.PeriodDay,
		Name: "deployment", Enabled: true, HardLimit: &limit,
	}}})

	for i := range 2 {
		if rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, rec.Code)
		}
		// The ledger is fed off the request path, so wait for it.
		f.drain()
	}

	if rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16}`); rec.Code != http.StatusForbidden {
		t.Errorf("the third request, past the limit the first two spent = %d, want 403", rec.Code)
	}
}
