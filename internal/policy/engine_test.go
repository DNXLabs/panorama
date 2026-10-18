// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

// fakeRepo is policy storage without a database.
//
// Locked, because the tests of the background loop write to it while Run reads.
type fakeRepo struct {
	mu         sync.Mutex
	set        Set
	spend      map[string]map[string]Spend // scope -> subject -> spend
	err        error
	loads      int
	generation int64
}

func (f *fakeRepo) LoadPolicy(context.Context) (Set, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.err != nil {
		return Set{}, f.err
	}
	return f.set, nil
}

func (f *fakeRepo) SpendSince(_ context.Context, scope string, _ time.Time) (map[string]Spend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.spend[scope], nil
}

func (f *fakeRepo) PolicyGeneration(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation, nil
}

// write changes the stored policy the way another process would: the set, and
// the generation every write moves.
func (f *fakeRepo) write(fn func(*Set)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.set)
	f.generation++
}

func (f *fakeRepo) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

// newEngine builds a loaded engine with the clock pinned.
func newEngine(t *testing.T, repo *fakeRepo, now string) *Engine {
	t.Helper()

	// Every route in these tests is on a serving provider unless a test says
	// otherwise: what is under test is routing and budgets, and a provider
	// question is asked by the tests that are about providers.
	if repo.set.Providers == nil {
		repo.set.Providers = servingProviders(providers.APIAnthropicMessages)
	}

	e := New(Options{
		Repository:     repo,
		ReloadInterval: -1, // one-shot: no background loop to race
		Now:            func() time.Time { return at(t, now) },
	})
	if err := e.Load(t.Context()); err != nil {
		t.Fatalf("load: %v", err)
	}
	return e
}

// plain is the ordinary route: one name, one model, available to everybody,
// and served right up to the hard limit.
func plain(name, target string) Route {
	return Route{Name: name, Target: bedrockRef(target), ServesOverSoft: true}
}

// bedrockRef names a model on the bedrock provider.
func bedrockRef(model string) providers.ModelRef {
	return providers.ModelRef{Provider: providers.KindBedrock, Model: model}
}

// servingProviders is a bedrock provider that is built and serving, whose every
// model speaks the given APIs.
func servingProviders(apis ...providers.API) map[providers.Kind]providers.State {
	settings := providers.Settings{Kind: providers.KindBedrock, Enabled: true, Region: "us-east-1"}
	instance := &providers.Instance{
		Settings: settings,
		Provider: idleProvider{},
		APIs:     func(string) []providers.API { return apis },
	}
	return map[providers.Kind]providers.State{
		providers.KindBedrock: {Settings: settings, Instance: instance},
	}
}

// idleProvider is a provider nothing in this package invokes.
type idleProvider struct{}

func (idleProvider) Name() string { return string(providers.KindBedrock) }
func (idleProvider) Invoke(context.Context, *providers.Request) (*providers.Response, error) {
	return nil, errors.New("not invoked in policy tests")
}
func (idleProvider) InvokeStream(context.Context, *providers.Request) (providers.EventStream, error) {
	return nil, errors.New("not invoked in policy tests")
}

// stopsAtSoft is the route an administrator marks as not surviving a soft
// limit - the expensive one somebody is asked to switch off.
func stopsAtSoft(name, target string) Route {
	r := plain(name, target)
	r.ServesOverSoft = false
	return r
}

func TestNilEngineDecidesNothing(t *testing.T) {
	var e *Engine

	d := e.Evaluate(Input{Route: "default"})
	if d.Blocked || d.Refused || !d.Target.IsZero() || d.Warning != "" {
		t.Errorf("a nil engine decided %+v, want nothing", d)
	}
	// And every other entry point tolerates it, so the data plane needs no nil
	// check on the request path.
	e.Record(Completed{UserID: "usr_1", At: time.Now()})
	if err := e.Load(t.Context()); err != nil {
		t.Errorf("Load on a nil engine: %v", err)
	}
	e.Run(t.Context())
	if got := e.Status(); got != nil {
		t.Errorf("Status on a nil engine = %v", got)
	}
}

// A name nobody created is refused. There is no allowlist beside the routes and
// no wildcard in them, so this is the whole of "may this request proceed".
func TestAnUnknownNameIsRefused(t *testing.T) {
	repo := &fakeRepo{set: Set{Routes: []Route{plain("default", "us.anthropic.sonnet")}}}
	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "anthropic.claude-sonnet-5"})
	if !d.Refused {
		t.Fatalf("a model id was served as though it were a route: %+v", d)
	}
	if d.RefusedReason != RefusedUnknownRoute {
		t.Errorf("reason = %q, want %q", d.RefusedReason, RefusedUnknownRoute)
	}
	// The refusal tells them what they can use instead, which is the whole
	// action available to the person reading it.
	if !strings.Contains(d.RefusedMessage, "default") {
		t.Errorf("the refusal does not name a route they can use:\n%s", d.RefusedMessage)
	}
}

func TestAKnownNameResolvesToItsModel(t *testing.T) {
	repo := &fakeRepo{set: Set{Routes: []Route{plain("default", "us.anthropic.sonnet")}}}
	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "default"})
	if !d.Served() {
		t.Fatalf("a route with a model did not serve: %+v", d)
	}
	if d.Target.Model != "us.anthropic.sonnet" {
		t.Errorf("target = %q, want the route's model", d.Target)
	}
	if d.Blocked || d.BlockReason != "" {
		t.Errorf("an ordinary request was recorded as stopped by a budget: %+v", d)
	}
}

// Conditions decide who a route exists for. Someone outside them is refused
// exactly as though the name had never been created.
func TestConditionsDecideWhoARouteExistsFor(t *testing.T) {
	restricted := plain("opus", "us.anthropic.opus")
	restricted.Teams = []string{"tm_platform"}

	repo := &fakeRepo{set: Set{
		Routes: []Route{plain("default", "us.anthropic.sonnet"), restricted},
		Teams:  map[string][]string{"usr_platform": {"tm_platform"}},
	}}
	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	if d := e.Evaluate(Input{UserID: "usr_platform", Route: "opus"}); !d.Served() {
		t.Errorf("a member of the named team was refused: %+v", d)
	}

	d := e.Evaluate(Input{UserID: "usr_other", Route: "opus"})
	if !d.Refused {
		t.Fatal("a route restricted to one team served somebody outside it")
	}
	if d.RefusedReason != RefusedNotAvailable {
		t.Errorf("reason = %q, want %q", d.RefusedReason, RefusedNotAvailable)
	}
	// The refusal must not name it, or the conditions would be a listing of
	// what other teams have.
	if strings.Contains(d.RefusedMessage, "opus") && !strings.Contains(d.RefusedMessage, `"opus"`) {
		t.Errorf("the refusal offers back a route this caller may not use:\n%s", d.RefusedMessage)
	}
	if strings.Contains(d.RefusedMessage, "Routes you can use: default, opus") {
		t.Errorf("a restricted route was listed as available:\n%s", d.RefusedMessage)
	}
}

// Conditions intersect: a route naming a person and a team admits that person
// when they are in that team, and nobody else.
func TestConditionsIntersect(t *testing.T) {
	route := plain("pair", "us.anthropic.sonnet")
	route.Users = []string{"usr_1"}
	route.Teams = []string{"tm_1"}

	repo := &fakeRepo{set: Set{
		Routes: []Route{route},
		Teams:  map[string][]string{"usr_1": {"tm_2"}, "usr_2": {"tm_1"}},
	}}
	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	if d := e.Evaluate(Input{UserID: "usr_1", Route: "pair"}); !d.Refused {
		t.Error("the named user was admitted while outside the named team")
	}
	if d := e.Evaluate(Input{UserID: "usr_2", Route: "pair"}); !d.Refused {
		t.Error("a member of the named team was admitted while not the named user")
	}

	repo.set.Teams["usr_1"] = []string{"tm_1", "tm_2"}
	if err := e.Load(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if d := e.Evaluate(Input{UserID: "usr_1", Route: "pair"}); !d.Served() {
		t.Errorf("somebody satisfying both conditions was refused: %+v", d)
	}
}

func TestAToolConditionMatchesTheToolTheRequestCameFrom(t *testing.T) {
	route := plain("claude-only", "us.anthropic.sonnet")
	route.Tools = []string{"claude-code"}

	repo := &fakeRepo{set: Set{Routes: []Route{route}}}
	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	if d := e.Evaluate(Input{UserID: "usr_1", Route: "claude-only", Tool: "claude-code"}); !d.Served() {
		t.Errorf("the named tool was refused: %+v", d)
	}
	if d := e.Evaluate(Input{UserID: "usr_1", Route: "claude-only", Tool: "cline"}); !d.Refused {
		t.Error("a tool the route does not name was served")
	}
	// Case is not a permission. A client that capitalises itself differently
	// tomorrow must not lose access.
	if d := e.Evaluate(Input{UserID: "usr_1", Route: "claude-only", Tool: "Claude-Code"}); !d.Served() {
		t.Error("the tool condition is case-sensitive, so a client's capitalisation decides access")
	}
}

// A service token has no user id, so a route naming people is not one a
// pipeline can reach - and its team condition still works.
func TestAServiceTokenReachesATeamRouteAndNotAPersonalOne(t *testing.T) {
	byTeam := plain("ci", "us.anthropic.haiku")
	byTeam.Teams = []string{"tm_1"}
	byPerson := plain("mine", "us.anthropic.opus")
	byPerson.Users = []string{"usr_1"}

	repo := &fakeRepo{set: Set{Routes: []Route{byTeam, byPerson}}}
	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	if d := e.Evaluate(Input{TeamID: "tm_1", Route: "ci"}); !d.Served() {
		t.Errorf("a service token was refused its own team's route: %+v", d)
	}
	if d := e.Evaluate(Input{TeamID: "tm_1", Route: "mine"}); !d.Refused {
		t.Error("a route naming people admitted a request with no person behind it")
	}
}

func TestHardBudgetBlocks(t *testing.T) {
	repo := &fakeRepo{
		set: Set{
			Routes: []Route{plain("default", "us.anthropic.sonnet")},
			Budgets: []Budget{{
				ID: "bdg_1", Scope: ScopeUser, ScopeID: "usr_1", Period: PeriodDay,
				Name: "daily", Enabled: true, HardLimit: usd(t, "10.00"),
			}},
		},
		spend: map[string]map[string]Spend{
			ScopeUser: {"usr_1": {Cost: *usd(t, "12.50"), Requests: 40}},
		},
	}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "default"})
	if !d.Blocked {
		t.Fatal("a user $2.50 past a $10 hard limit was not blocked")
	}
	if d.Budget != StateOverHard {
		t.Errorf("budget state = %q, want over_hard", d.Budget)
	}
	if !strings.Contains(d.BlockMessage, "$12.50") || !strings.Contains(d.BlockMessage, "$10.00") {
		t.Errorf("the block message does not quote the figures:\n%s", d.BlockMessage)
	}

	// A different user is untouched by someone else's budget.
	if other := e.Evaluate(Input{UserID: "usr_2", Route: "default"}); other.Blocked {
		t.Error("another user was blocked by a budget that does not govern them")
	}
}

// TestSoftLimitWarnsAndServes is the difference between the two limits.
func TestSoftLimitWarnsAndServes(t *testing.T) {
	repo := &fakeRepo{
		set: Set{
			Routes: []Route{plain("default", "us.anthropic.sonnet")},
			Budgets: []Budget{{
				ID: "bdg_1", Scope: ScopeOrg, Period: PeriodMonth, Name: "deployment",
				Enabled: true, SoftLimit: usd(t, "800.00"), HardLimit: usd(t, "1000.00"),
			}},
		},
		spend: map[string]map[string]Spend{
			ScopeOrg: {"": {Cost: *usd(t, "812.00"), Requests: 9_000}},
		},
	}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "default"})
	if d.Blocked {
		t.Fatal("a soft limit blocked a request")
	}
	if d.Budget != StateOverSoft {
		t.Errorf("budget state = %q, want over_soft", d.Budget)
	}
	if d.Warning == "" {
		t.Error("crossing the soft limit produced no warning")
	}
	if d.Target.Model != "us.anthropic.sonnet" {
		t.Errorf("target = %q: a route that serves past a soft limit keeps its model", d.Target)
	}
}

// "Over soft limit, use a cheaper model", as the gateway expresses it:
// the expensive route stops, the engineer is told which routes still work, and
// they pick one. The gateway does not change anybody's model for them, because
// a harness is configured with a route's context window once and cannot be told
// mid-session that it shrank.
func TestASoftLimitStopsARouteAndNamesTheAlternatives(t *testing.T) {
	repo := &fakeRepo{
		set: Set{
			Routes: []Route{
				stopsAtSoft("standard", "us.anthropic.opus"),
				plain("economy", "us.anthropic.haiku"),
			},
			Budgets: []Budget{{
				ID: "bdg_1", Scope: ScopeTeam, ScopeID: "tm_1", Period: PeriodMonth,
				Name: "platform", Enabled: true, SoftLimit: usd(t, "100.00"),
			}},
			Teams: map[string][]string{"usr_1": {"tm_1"}},
		},
		spend: map[string]map[string]Spend{
			ScopeTeam: {"tm_1": {Cost: *usd(t, "140.00"), Requests: 2_000}},
		},
	}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "standard"})
	if !d.Blocked {
		t.Fatal("a route marked as stopping at the soft limit served a request past one")
	}
	if d.BlockReason != BlockedOverSoft {
		t.Errorf("reason = %q, want %q: told apart from running out of money", d.BlockReason, BlockedOverSoft)
	}
	if !d.Target.IsZero() {
		t.Errorf("a stopped decision still names a model to invoke: %q", d.Target)
	}
	// The message is the whole remedy: without a name to switch to, this is a
	// hard limit that arrived early.
	if !strings.Contains(d.BlockMessage, "economy") {
		t.Errorf("the refusal does not name the route that would work:\n%s", d.BlockMessage)
	}
	if strings.Contains(d.BlockMessage, "standard") && !strings.Contains(d.BlockMessage, `"standard"`) {
		t.Errorf("the refusal offers the route it just stopped:\n%s", d.BlockMessage)
	}

	// The cheap route is served to the same person in the same state.
	cheap := e.Evaluate(Input{UserID: "usr_1", Route: "economy"})
	if cheap.Blocked || cheap.Target.Model != "us.anthropic.haiku" {
		t.Errorf("the route people are told to switch to was stopped by the same budget: %+v", cheap)
	}
	if cheap.Warning == "" {
		t.Error("a route serving past a soft limit did not warn that the limit was crossed")
	}

	// Someone outside that team is inside budget and gets the expensive route.
	other := e.Evaluate(Input{UserID: "usr_2", Route: "standard"})
	if other.Blocked {
		t.Error("a user no budget touches was stopped by somebody else's soft limit")
	}
	if other.Target.Model != "us.anthropic.opus" {
		t.Errorf("a user outside the team was routed to %q", other.Target)
	}
}

// A refusal must not offer a name that the same budget state would also stop.
// An engineer who switches, is refused again, and switches back has learned
// that the list is a guess.
func TestARefusalOnlyOffersRoutesThatAreStillServed(t *testing.T) {
	repo := &fakeRepo{
		set: Set{
			Routes: []Route{
				stopsAtSoft("standard", "us.anthropic.opus"),
				stopsAtSoft("heavy", "us.anthropic.opus-global"),
				plain("economy", "us.anthropic.haiku"),
			},
			Budgets: []Budget{{
				ID: "bdg_1", Scope: ScopeOrg, Period: PeriodMonth, Name: "deployment",
				Enabled: true, SoftLimit: usd(t, "100.00"),
			}},
		},
		spend: map[string]map[string]Spend{ScopeOrg: {"": {Cost: *usd(t, "140.00")}}},
	}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "standard"})
	if strings.Contains(d.BlockMessage, "heavy") {
		t.Errorf("offered a route the same soft limit stops:\n%s", d.BlockMessage)
	}
	if !strings.Contains(d.BlockMessage, "economy") {
		t.Errorf("did not offer the one route that works:\n%s", d.BlockMessage)
	}

	// The same filtering applies to a name nobody created, which is the other
	// door onto the same list.
	unknown := e.Evaluate(Input{UserID: "usr_1", Route: "nonsense"})
	if strings.Contains(unknown.RefusedMessage, "heavy") {
		t.Errorf("an unknown-route refusal offered a stopped route:\n%s", unknown.RefusedMessage)
	}
}

// When there is nowhere to go, the message says so rather than telling somebody
// to switch to a list with nothing in it.
func TestAStopWithNoAlternativeSaysThereIsNone(t *testing.T) {
	repo := &fakeRepo{
		set: Set{
			Routes: []Route{stopsAtSoft("standard", "us.anthropic.opus")},
			Budgets: []Budget{{
				ID: "bdg_1", Scope: ScopeOrg, Period: PeriodMonth, Name: "deployment",
				Enabled: true, SoftLimit: usd(t, "100.00"),
			}},
		},
		spend: map[string]map[string]Spend{ScopeOrg: {"": {Cost: *usd(t, "140.00")}}},
	}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "standard"})
	if !d.Blocked {
		t.Fatal("the route served past its soft limit")
	}
	if !strings.Contains(d.BlockMessage, "nothing to switch to") {
		t.Errorf("the message implies an alternative exists:\n%s", d.BlockMessage)
	}
	if !strings.Contains(d.BlockMessage, "administrator") {
		t.Errorf("the message does not say who can change this:\n%s", d.BlockMessage)
	}
}

// A hard limit blocks before a route's own soft-limit setting is consulted.
// A route that served past one would be a way to spend past a ceiling somebody
// set, and every route serves past a soft limit by default.
func TestAHardLimitBlocksARouteThatServesPastSoft(t *testing.T) {
	repo := &fakeRepo{
		set: Set{
			Routes: []Route{plain("default", "us.anthropic.opus")},
			Budgets: []Budget{{
				ID: "bdg_1", Scope: ScopeOrg, Period: PeriodMonth, Name: "deployment",
				Enabled: true, SoftLimit: usd(t, "10.00"), HardLimit: usd(t, "20.00"),
			}},
		},
		spend: map[string]map[string]Spend{ScopeOrg: {"": {Cost: *usd(t, "40.00")}}},
	}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "default"})
	if !d.Blocked {
		t.Fatal("a route served past a hard limit")
	}
	if d.BlockReason != BlockedBudgetExhausted {
		t.Errorf("reason = %q, want %q", d.BlockReason, BlockedBudgetExhausted)
	}
	if !d.Target.IsZero() {
		t.Errorf("a blocked decision still names a model to invoke: %q", d.Target)
	}
}

// A route with no model admits a request and has nowhere to send it. The write
// path refuses to create one, but the migration that moved targets onto routes
// could only carry across what was there - so this is reachable, and it must
// not look like the engineer's mistake.
func TestARouteWithNoModelIsTheDeploymentsFault(t *testing.T) {
	repo := &fakeRepo{set: Set{Routes: []Route{
		{Name: "broken", ServesOverSoft: true},
	}}}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "broken"})
	if !d.Refused {
		t.Fatal("a route with no model served a request")
	}
	if d.RefusedReason != RefusedNoTarget {
		t.Errorf("reason = %q, want %q", d.RefusedReason, RefusedNoTarget)
	}
	if !strings.Contains(d.RefusedMessage, "configuration error") {
		t.Errorf("the message blames the engineer for a deployment's mistake:\n%s", d.RefusedMessage)
	}
}

// TestTheMostSpecificBudgetIsNamed: when a user and the deployment are both
// over, the message names the one whose owner can most directly act.
func TestTheMostSpecificBudgetIsNamed(t *testing.T) {
	repo := &fakeRepo{
		set: Set{
			Routes: []Route{plain("default", "us.anthropic.sonnet")},
			Budgets: []Budget{
				{ID: "bdg_org", Scope: ScopeOrg, Period: PeriodMonth, Name: "deployment",
					Enabled: true, HardLimit: usd(t, "100.00")},
				{ID: "bdg_user", Scope: ScopeUser, ScopeID: "usr_1", Period: PeriodMonth, Name: "personal",
					Enabled: true, HardLimit: usd(t, "10.00")},
			},
		},
		spend: map[string]map[string]Spend{
			ScopeOrg:  {"": {Cost: *usd(t, "500.00")}},
			ScopeUser: {"usr_1": {Cost: *usd(t, "50.00")}},
		},
	}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "default"})
	if d.BudgetID != "bdg_user" {
		t.Errorf("budget = %q, want the user's own", d.BudgetID)
	}
	if !strings.Contains(d.BlockMessage, "personal") {
		t.Errorf("the message names the wrong budget:\n%s", d.BlockMessage)
	}
}

func TestRecordFeedsTheLedger(t *testing.T) {
	repo := &fakeRepo{set: Set{
		Routes: []Route{plain("default", "us.anthropic.sonnet")},
		Budgets: []Budget{
			{ID: "bdg_org", Scope: ScopeOrg, Period: PeriodDay, Enabled: true, HardLimit: usd(t, "1.00")},
			{ID: "bdg_user", Scope: ScopeUser, ScopeID: "usr_1", Period: PeriodDay, Enabled: true, HardLimit: usd(t, "1.00")},
		},
		Teams: map[string][]string{"usr_1": {"tm_1"}},
	}}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")
	now := at(t, "2026-08-14T12:00:00Z")

	cost := &pricing.Cost{Total: *usd(t, "0.60")}
	e.Record(Completed{UserID: "usr_1", At: now, Cost: cost, UsedTokens: true})

	if d := e.Evaluate(Input{UserID: "usr_1", Route: "default"}); d.Blocked {
		t.Fatal("60c of a $1 limit blocked")
	}

	e.Record(Completed{UserID: "usr_1", At: now, Cost: cost, UsedTokens: true})

	d := e.Evaluate(Input{UserID: "usr_1", Route: "default"})
	if !d.Blocked {
		t.Error("$1.20 of a $1 limit did not block: the ledger is not being fed")
	}
}

// TestUnpricedRequestsAreCountedAndDeclared: enforcing against a number known
// to be incomplete, without saying so, is the failure this product exists to
// avoid in its other numbers.
func TestUnpricedRequestsAreCountedAndDeclared(t *testing.T) {
	hard := pricing.Money(1_000_000_000) // $1
	repo := &fakeRepo{set: Set{
		Routes: []Route{plain("default", "us.anthropic.sonnet")},
		Budgets: []Budget{{
			ID: "bdg_1", Scope: ScopeOrg, Period: PeriodDay, Name: "deployment",
			Enabled: true, HardLimit: &hard,
		}},
	}}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")
	now := at(t, "2026-08-14T12:00:00Z")

	// One request past the limit, and two nobody could price.
	e.Record(Completed{UserID: "usr_1", At: now, Cost: &pricing.Cost{Total: 1_200_000_000}, UsedTokens: true})
	e.Record(Completed{UserID: "usr_1", At: now, UsedTokens: true})
	e.Record(Completed{UserID: "usr_1", At: now, UsedTokens: true})

	d := e.Evaluate(Input{UserID: "usr_1", Route: "default"})
	if !d.Blocked {
		t.Fatal("$1.20 of a $1 limit did not block")
	}
	if !strings.Contains(d.BlockMessage, "could not be priced") {
		t.Errorf("the message does not admit the figure is incomplete:\n%s", d.BlockMessage)
	}
}

// A request that never reached a provider must not count. Otherwise the
// requests a block produces keep the user blocked.
func TestBlockedRequestsDoNotCountThemselvesUp(t *testing.T) {
	hard := pricing.Money(10)
	repo := &fakeRepo{set: Set{
		Routes: []Route{plain("default", "us.anthropic.sonnet")},
		Budgets: []Budget{{
			ID: "bdg_1", Scope: ScopeOrg, Period: PeriodDay, Enabled: true, HardLimit: &hard,
		}},
	}}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")
	now := at(t, "2026-08-14T12:00:00Z")

	e.Record(Completed{UserID: "usr_1", At: now, Cost: &pricing.Cost{Total: 10}, UsedTokens: true})

	before := e.Status()[0].Spend.Requests

	// Ten blocked attempts. The data plane never calls Record for these, and
	// this asserts the shape of that contract rather than the call site.
	for range 10 {
		if d := e.Evaluate(Input{UserID: "usr_1", Route: "default"}); !d.Blocked {
			t.Fatal("expected a block")
		}
	}

	if after := e.Status()[0].Spend.Requests; after != before {
		t.Errorf("requests went from %d to %d: evaluation is counting itself", before, after)
	}
}

func TestDisabledBudgetsAreIgnored(t *testing.T) {
	repo := &fakeRepo{
		set: Set{
			Routes: []Route{plain("default", "us.anthropic.sonnet")},
			Budgets: []Budget{{ID: "bdg_1", Scope: ScopeOrg, Period: PeriodDay,
				Enabled: false, HardLimit: usd(t, "0.01")}},
		},
		spend: map[string]map[string]Spend{ScopeOrg: {"": {Cost: *usd(t, "500.00")}}},
	}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	d := e.Evaluate(Input{UserID: "usr_1", Route: "default"})
	if d.Blocked {
		t.Error("a disabled budget blocked a request")
	}
}

func TestLoadFailsLoudly(t *testing.T) {
	repo := &fakeRepo{err: errors.New("database is on fire")}

	e := New(Options{Repository: repo, ReloadInterval: -1})
	if err := e.Load(t.Context()); err == nil {
		t.Fatal("Load returned no error when storage failed")
	}
}

func TestStatusReportsEveryBudget(t *testing.T) {
	repo := &fakeRepo{
		set: Set{Budgets: []Budget{{
			ID: "bdg_1", Scope: ScopeOrg, Period: PeriodMonth, Name: "deployment",
			Enabled: true, SoftLimit: usd(t, "80.00"), HardLimit: usd(t, "100.00"),
		}}},
		spend: map[string]map[string]Spend{
			ScopeOrg: {"": {Cost: *usd(t, "90.00"), Requests: 12, Unpriced: 1}},
		},
	}

	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	status := e.Status()
	if len(status) != 1 {
		t.Fatalf("Status returned %d rows, want 1", len(status))
	}
	s := status[0]
	if s.State != StateOverSoft {
		t.Errorf("state = %q, want over_soft", s.State)
	}
	if s.Percent != 90 {
		t.Errorf("percent = %d, want 90", s.Percent)
	}
	if s.Spend.Unpriced != 1 {
		t.Errorf("unpriced = %d, want 1", s.Spend.Unpriced)
	}
	if !s.WindowStart.Equal(at(t, "2026-08-01T00:00:00Z")) {
		t.Errorf("window start = %s", s.WindowStart)
	}
}

// Available is what a refusal offers back, and it is filtered the same way the
// refusal itself is: a route somebody may not use is not one they can see.
func TestAvailableIsFilteredByCondition(t *testing.T) {
	restricted := plain("opus", "us.anthropic.opus")
	restricted.Teams = []string{"tm_1"}

	set := Set{
		Routes: []Route{plain("default", "us.anthropic.sonnet"), restricted},
		Teams:  map[string][]string{"usr_1": {"tm_1"}},
	}
	set.Index()

	member := set.Available(Input{UserID: "usr_1"}, set.Teams["usr_1"])
	if len(member) != 2 {
		t.Errorf("a team member sees %v, want both routes", member)
	}

	outsider := set.Available(Input{UserID: "usr_2"}, nil)
	if len(outsider) != 1 || outsider[0] != "default" {
		t.Errorf("an outsider sees %v, want only the unrestricted route", outsider)
	}
}
