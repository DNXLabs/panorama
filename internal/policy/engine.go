// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/dnxlabs/panorama/internal/costcenters"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

// defaultReloadInterval is how often rules, budgets, and spend are re-read from
// storage.
//
// Half a minute is a compromise between two failure modes. Longer, and a
// deployment running several replicas enforces a budget that much later than it
// should. Shorter, and a busy gateway spends its database on counting rather
// than on serving. Enforcement between reconciles is not blind - this process
// counts its own traffic - so the interval only bounds how quickly one replica
// learns about another's.
const defaultReloadInterval = 30 * time.Second

// Set is the policy configuration as one immutable snapshot.
//
// It is replaced wholesale rather than mutated, so a request is evaluated
// against one coherent version of the policy instead of against a list somebody
// is halfway through editing.
//
// Routes and budgets travel together in one snapshot, refreshed on one
// interval, because they are decided together: a route's rule reacts to a
// budget's state, and two snapshots refreshed independently would spend part of
// every cycle disagreeing about which budget a route was reading.
type Set struct {
	// Routes is every name a client may ask for. It is the allowlist: a name
	// absent from it is refused.
	Routes  []Route
	Budgets []Budget
	// Teams maps a user to the teams they belong to. It is part of the snapshot
	// because a route condition and a budget both reach a person through their
	// teams, and looking membership up per request would put a query back on
	// the path this whole design exists to keep clear.
	Teams map[string][]string
	// ProviderSettings is how each provider is configured, as the repository
	// read it. The engine builds Providers from it.
	ProviderSettings []providers.Settings
	// Providers is every configured provider and what building it produced,
	// keyed by kind. It is in the snapshot for the reason routes are: a request
	// is admitted by a route and sent to that route's provider, and two
	// snapshots refreshed apart would spend part of every cycle disagreeing
	// about whether that provider exists.
	Providers map[providers.Kind]providers.State

	// CostCenters resolves a stamped cost center code to an id. It is carried
	// here for exactly the reason Teams is - to keep a lookup off the request
	// path - and for no other: nothing in this package decides anything with
	// it. Only lines a client may name are in it, so the hot path is one map
	// read with no second condition to get wrong.
	CostCenters costcenters.Stampable

	// byName indexes Routes for the hot path. It is an optimisation, not the
	// definition: Route falls back to a scan when it is absent, so a Set built
	// by hand in a test behaves the same as one the engine loaded.
	byName map[string]int
	// apis caches what each route's model speaks, for the same reason and with
	// the same fallback: RouteAPIs derives it when the cache is absent.
	apis map[string][]providers.API
}

// Index builds the route lookups. The engine calls it after every load.
func (s *Set) Index() {
	s.byName = make(map[string]int, len(s.Routes))
	s.apis = make(map[string][]providers.API, len(s.Routes))
	for i := range s.Routes {
		s.byName[s.Routes[i].Name] = i
		s.apis[s.Routes[i].Name] = s.deriveAPIs(s.Routes[i])
	}
}

// RouteAPIs reports the client APIs a route can be served on: whatever its
// model speaks on its provider. A route on a provider that is not serving
// speaks nothing.
func (s Set) RouteAPIs(r Route) []providers.API {
	if s.apis != nil {
		if list, ok := s.apis[r.Name]; ok {
			return list
		}
	}
	return s.deriveAPIs(r)
}

func (s Set) deriveAPIs(r Route) []providers.API {
	state, ok := s.Providers[r.Target.Provider]
	if !ok || !state.Serving() || state.Instance.APIs == nil {
		return nil
	}
	return state.Instance.APIs(r.Target.Model)
}

// speaks reports whether a route can be served on an API. An empty API asks
// no question about shape.
func (s Set) speaks(r Route, api providers.API) bool {
	return api == "" || slices.Contains(s.RouteAPIs(r), api)
}

// Route returns the route with this name.
func (s Set) Route(name string) (Route, bool) {
	if s.byName != nil {
		i, ok := s.byName[name]
		if !ok {
			return Route{}, false
		}
		return s.Routes[i], true
	}
	for _, r := range s.Routes {
		if r.Name == name {
			return r, true
		}
	}
	return Route{}, false
}

// Available lists the route names this request's conditions admit, in order.
//
// It is filtered rather than complete on purpose: a name whose conditions
// exclude somebody does not exist for them, so naming it anywhere would
// contradict the refusal they would get for asking.
//
// This is the permanent answer, and it is what the setup screens render a
// harness configuration from - see Route.Available for why budget state has no
// business in it. AvailableAt is the one a refusal message offers back.
func (s Set) Available(in Input, teams []string) []string {
	return s.AvailableAt(in, teams, StateNone)
}

// AvailableAt lists the route names this request may ask for right now, given
// what the caller's budgets say.
//
// Every message that offers routes back uses this rather than Available,
// because offering a route that the very budget state producing the message
// would also stop is worse than offering nothing: the engineer switches, is
// refused again, and learns that the list cannot be trusted.
//
// A request's API narrows it too. Offering an engineer in Codex a route whose
// model speaks only the Anthropic Messages API would send them to switch to a
// name the next request refuses for a different reason.
func (s Set) AvailableAt(in Input, teams []string, state State) []string {
	out := make([]string, 0, len(s.Routes))
	for _, r := range s.Routes {
		if r.Available(in, teams) && r.ServesAt(state) && s.speaks(r, in.API) {
			out = append(out, r.Name)
		}
	}
	slices.Sort(out)
	return out
}

// ProviderBuilder turns provider settings into live providers. The registry in
// internal/providers is the one implementation; an interface here so a test can
// hand the engine providers it built itself.
type ProviderBuilder interface {
	Sync(ctx context.Context, settings []providers.Settings) map[providers.Kind]providers.State
}

// Repository is the persistence the engine needs.
//
// An interface rather than the store type, so the dependency runs one way: the
// store imports this package to describe what it is storing.
type Repository interface {
	// LoadPolicy reads the routes, budgets, team membership, and provider
	// settings.
	LoadPolicy(ctx context.Context) (Set, error)
	// SpendSince totals spend per subject of one scope, since a time. The map
	// is keyed by scope id; the org scope uses the empty key.
	SpendSince(ctx context.Context, scope string, since time.Time) (map[string]Spend, error)
}

// Options configures an engine.
type Options struct {
	Repository Repository
	// Providers builds the providers the snapshot carries. Nil leaves
	// Set.Providers as the repository returned it, which is empty unless a
	// test filled it in - and a route to a provider that is not in it is
	// refused.
	Providers ProviderBuilder
	// ReloadInterval is how often policy and spend are re-read. Zero uses the
	// default; negative disables the background loop entirely, which is what
	// tests want.
	ReloadInterval time.Duration
	Logger         *slog.Logger
	// Now is injected by tests.
	Now func() time.Time
}

// Engine evaluates policy against an in-memory snapshot.
//
// A nil *Engine is usable and decides nothing, so a deployment without policy
// runs the same code path as one with it rather than a second, less-tested one.
type Engine struct {
	repo      Repository
	providers ProviderBuilder
	snap      atomic.Pointer[Set]
	ledger    *Ledger
	log       *slog.Logger
	now       func() time.Time
	interval  time.Duration
}

// New builds an engine. It does not read anything; call Load.
func New(opts Options) *Engine {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ReloadInterval == 0 {
		opts.ReloadInterval = defaultReloadInterval
	}

	e := &Engine{
		repo:      opts.Repository,
		providers: opts.Providers,
		ledger:    NewLedger(),
		log:       opts.Logger,
		now:       opts.Now,
		interval:  opts.ReloadInterval,
	}
	e.snap.Store(&Set{})
	return e
}

// Snapshot returns the policy currently in force.
func (e *Engine) Snapshot() Set {
	if e == nil {
		return Set{}
	}
	return *e.snap.Load()
}

// Ledger exposes the spend counters, for diagnostics and tests.
func (e *Engine) Ledger() *Ledger {
	if e == nil {
		return nil
	}
	return e.ledger
}

// Load reads policy and reconciles the ledger. Call it before serving.
func (e *Engine) Load(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if err := e.reload(ctx); err != nil {
		return err
	}
	if err := e.reconcile(ctx); err != nil {
		return err
	}

	set := e.Snapshot()
	e.log.InfoContext(ctx, "policy loaded",
		"routes", len(set.Routes),
		"budgets", len(set.Budgets),
		"reload_interval", e.interval,
	)
	return nil
}

// Run keeps the snapshot and the ledger current until the context is cancelled.
func (e *Engine) Run(ctx context.Context) {
	if e == nil || e.interval < 0 {
		return
	}

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A failure here is an interruption to survive, not a reason to stop
			// enforcing: the engine keeps deciding against the snapshot it
			// already has, and says loudly that it is stale. Silently continuing
			// would be the fallback this codebase does not allow.
			if err := e.reload(ctx); err != nil && ctx.Err() == nil {
				e.log.ErrorContext(ctx, "reloading policy failed; still enforcing the previous snapshot",
					"error", err)
			}
			if err := e.reconcile(ctx); err != nil && ctx.Err() == nil {
				e.log.ErrorContext(ctx, "reconciling budget spend failed; still counting this process's own traffic",
					"error", err)
			}
		}
	}
}

// reload replaces the policy snapshot from storage.
func (e *Engine) reload(ctx context.Context) error {
	set, err := e.repo.LoadPolicy(ctx)
	if err != nil {
		return fmt.Errorf("policy: load: %w", err)
	}
	// Providers are built before the snapshot is published, so no request is
	// ever evaluated against a route whose provider has not been built yet. A
	// build that failed is carried on the state and refused per request,
	// rather than failing the reload - one misconfigured provider must not stop
	// the other from serving.
	if e.providers != nil {
		set.Providers = e.providers.Sync(ctx, set.ProviderSettings)
	}
	set.Index()
	e.snap.Store(&set)

	// Drop ledger windows for budgets that no longer exist, so deleting a
	// budget does not leave its counters accumulating in memory forever.
	keep := make(map[Key]struct{}, len(set.Budgets))
	for _, b := range set.Budgets {
		keep[b.Key()] = struct{}{}
	}
	e.ledger.Forget(keep)

	return nil
}

// reconcile refreshes the ledger from storage.
//
// One query per scope and period rather than one per budget: a deployment that
// gives every developer a monthly budget would otherwise issue a query per
// developer, every interval, forever.
func (e *Engine) reconcile(ctx context.Context) error {
	set := e.Snapshot()
	if len(set.Budgets) == 0 {
		return nil
	}

	now := e.now().UTC()

	type group struct {
		scope  string
		period Period
	}
	groups := make(map[group][]Budget)
	for _, b := range set.Budgets {
		if !b.Enabled {
			continue
		}
		groups[group{b.Scope, b.Period}] = append(groups[group{b.Scope, b.Period}], b)
	}

	for g, budgets := range groups {
		start, _ := g.period.Window(now)

		// Marks are taken before the query so that requests recorded while it is
		// in flight are added on top of the answer rather than erased by it.
		marks := make(map[Key]Mark, len(budgets))
		for _, b := range budgets {
			marks[b.Key()] = e.ledger.Mark(b.Key(), start)
		}

		spend, err := e.repo.SpendSince(ctx, g.scope, start)
		if err != nil {
			return fmt.Errorf("policy: reconcile %s budgets: %w", g.scope, err)
		}

		for _, b := range budgets {
			e.ledger.Reconcile(b.Key(), marks[b.Key()], spend[b.ScopeID])
		}
	}

	return nil
}

// Completed is one finished request, as a budget sees it.
//
// Only requests that actually reached a provider belong here. A request the
// gateway refused or blocked consumed nothing, and counting it would let a
// blocked user stay blocked by the very requests their block produced. The
// reconcile query applies the same test - `attempts > 0` - so the two ways of
// arriving at a budget's spend cannot drift apart.
type Completed struct {
	UserID string
	At     time.Time
	// Cost is nil when the request could not be priced.
	Cost *pricing.Cost
	// UsedTokens separates a request whose cost is unknown from one that
	// genuinely cost nothing because it never produced a token. Only the first
	// represents money missing from the figure a budget is enforced against.
	UsedTokens bool
	// TeamID mirrors Input.TeamID: set for a service token, empty for a person.
	// It has to be carried through to here, because a request is priced after
	// it completes and the ledger must credit the same budgets that enforcement
	// checked.
	TeamID string
}

// Record adds one completed request to every budget that governs it.
func (e *Engine) Record(c Completed) {
	if e == nil {
		return
	}

	set := e.Snapshot()
	if len(set.Budgets) == 0 {
		return
	}

	s := Spend{Requests: 1}
	switch {
	case c.Cost != nil:
		s.Cost = c.Cost.Total
	case c.UsedTokens:
		s.Unpriced = 1
	}

	userID, at := c.UserID, c.At
	teams := set.teamsFor(userID, c.TeamID)
	for _, b := range set.Budgets {
		if !b.Enabled || !b.Governs(userID, teams) {
			continue
		}
		start, _ := b.Period.Window(at)
		e.ledger.Add(b.Key(), start, s)
	}
}

// teamsFor returns the teams whose budgets a request answers to.
//
// A person's teams come from the snapshot's membership map. A service token
// carries its team directly, because there is no user id to look one up with -
// and this is the only place that difference is expressed, so enforcement and
// accounting cannot end up disagreeing about whose budget a pipeline spends.
func (s *Set) teamsFor(userID, teamID string) []string {
	if teamID != "" {
		return []string{teamID}
	}
	return s.Teams[userID]
}

// Governs reports whether this budget applies to a given user.
func (b Budget) Governs(userID string, teams []string) bool {
	switch b.Scope {
	case ScopeOrg:
		return true
	case ScopeUser:
		return b.ScopeID != "" && b.ScopeID == userID
	case ScopeTeam:
		return slices.Contains(teams, b.ScopeID)
	default:
		return false
	}
}

// Evaluate decides what happens to a request.
//
// No storage is touched: everything here is a map lookup against the snapshot
// and the ledger. That is the whole point - this runs between accepting a
// request and dialling the provider, inside a latency budget measured in
// milliseconds.
//
// The order matters and is not arbitrary. The route is found first, because a
// name nobody allowed is refused whatever anyone has spent. Then the hard
// limit, because it stops everything and nothing may route around it. Then the
// soft limit, which stops only the routes an administrator marked as not
// surviving one - and says so, naming what may be used instead.
func (e *Engine) Evaluate(in Input) Decision {
	if e == nil {
		return Decision{}
	}

	set := e.snap.Load()
	if in.Now.IsZero() {
		in.Now = e.now()
	}
	teams := set.teamsFor(in.UserID, in.TeamID)

	d := Decision{Route: in.Route}

	// Resolved before the refusal branches, so a request that never reaches a
	// provider still records what it was stamped with. A refused request costs
	// nothing and allocates nothing, but a deployment whose stamps are all
	// misspelt should be able to see that from the audit trail rather than only
	// from the requests that happened to succeed.
	if in.CostCenterStamp != "" {
		if id := set.CostCenters.Lookup(in.CostCenterStamp); id != "" {
			d.CostCenterID = id
		} else {
			d.CostCenterStamp = in.CostCenterStamp
		}
	}

	route, found := set.Route(in.Route)
	if found {
		d.APIs = set.RouteAPIs(route)
	}
	if !found || !route.Available(in, teams) {
		d.APIs = nil
		d.Refused = true
		d.RefusedReason = RefusedUnknownRoute
		if found {
			// Recorded differently and said identically. The operator's log
			// says a condition excluded them; the engineer is told the name is
			// not one they can use, and is offered the ones that are.
			d.RefusedReason = RefusedNotAvailable
		}
		// The list is taken at the caller's current budget state, so a refusal
		// never offers a name that the next request would also be stopped on.
		state, _, _ := e.budgetState(set, in, teams)
		d.RefusedMessage = refusedMessage(in.Route, in.API, set.AvailableAt(in, teams, state))
		return d
	}

	state, budget, spend := e.budgetState(set, in, teams)
	d.Budget = state
	if budget != nil {
		d.BudgetID = budget.ID
	}

	if state == StateOverHard {
		d.Blocked = true
		d.BlockReason = BlockedBudgetExhausted
		d.BlockMessage = blockMessage(*budget, spend, in.Now)
		return d
	}

	if !route.ServesAt(state) {
		// The route exists for this caller and their budget has passed its soft
		// limit, which is exactly what an administrator marked this route as
		// not surviving. Stopped rather than rerouted: swapping the model here
		// would change what an engineer is working with without telling them,
		// and there is no way to tell a harness mid-session. So they are told,
		// and offered the routes that are still served.
		d.Blocked = true
		d.BlockReason = BlockedOverSoft
		d.BlockMessage = softStopMessage(route.Name, *budget, spend, in.Now,
			set.AvailableAt(in, teams, state))
		return d
	}

	if route.Target.IsZero() {
		// A route that admits a request and has nowhere to send it. The write
		// path will not create one, but the migration that moved targets onto
		// routes could only carry across what was there - a route that had no
		// default rule arrives here with no target - so this is reachable, and
		// it is the deployment's fault rather than the engineer's.
		d.Refused = true
		d.RefusedReason = RefusedNoTarget
		d.RefusedMessage = noTargetMessage(route.Name)
		return d
	}

	provider, serving := set.Providers[route.Target.Provider]
	if !serving || !provider.Serving() {
		// A route to a provider that is disabled, was never configured, or
		// failed to build. Disabling a provider a route uses is refused at the
		// write path, so this is the same shape of fault as a route with no
		// target: reachable, and the deployment's.
		d.Refused = true
		d.RefusedReason = RefusedProviderUnavailable
		d.RefusedMessage = providerUnavailableMessage(route.Name)
		return d
	}

	if !set.speaks(route, in.API) {
		d.Refused = true
		d.RefusedReason = RefusedWrongAPI
		d.RefusedMessage = wrongAPIMessage(route.Name, in.API, d.APIs)
		return d
	}

	d.Target = route.Target
	d.Provider = provider.Instance.Provider

	if state == StateOverSoft {
		d.Warning = warnMessage(*budget, spend, in.Now)
	}

	return d
}

// EvaluateTool decides whether a tool the gateway serves itself - a web search
// - may run for this caller.
//
// A tool call names no route, so the route list has nothing to say about it:
// whether the tool is offered at all is the tool's own switch, and what is
// left is money. A hard limit stops a search exactly as it stops a model
// request, because a budget that a search could spend past would not be a
// limit. A soft limit does not: it stops only the routes an administrator
// marked as not surviving one, and a search is not a route. It warns.
//
// No storage is touched, for the same reason as Evaluate.
func (e *Engine) EvaluateTool(in Input) Decision {
	if e == nil {
		return Decision{}
	}

	set := e.snap.Load()
	if in.Now.IsZero() {
		in.Now = e.now()
	}
	teams := set.teamsFor(in.UserID, in.TeamID)

	var d Decision
	if in.CostCenterStamp != "" {
		if id := set.CostCenters.Lookup(in.CostCenterStamp); id != "" {
			d.CostCenterID = id
		} else {
			d.CostCenterStamp = in.CostCenterStamp
		}
	}

	state, budget, spend := e.budgetState(set, in, teams)
	d.Budget = state
	if budget != nil {
		d.BudgetID = budget.ID
	}
	switch state {
	case StateOverHard:
		d.Blocked = true
		d.BlockReason = BlockedBudgetExhausted
		d.BlockMessage = blockMessage(*budget, spend, in.Now)
	case StateOverSoft:
		d.Warning = warnMessage(*budget, spend, in.Now)
	}
	return d
}

// budgetState finds the worst state across every budget governing a request,
// and the budget that produced it.
//
// Ties break toward the most specific scope: if a user and the organization are
// both over, the message names the user's own budget, because that is the one
// whose owner can most directly do something about it.
func (e *Engine) budgetState(set *Set, in Input, teams []string) (State, *Budget, Spend) {
	var (
		worst    = StateNone
		chosen   *Budget
		chosenAt Spend
		rank     int
	)

	specificity := map[string]int{ScopeOrg: 1, ScopeTeam: 2, ScopeUser: 3}

	for i := range set.Budgets {
		b := &set.Budgets[i]
		if !b.Enabled || !b.Governs(in.UserID, teams) {
			continue
		}

		start, _ := b.Period.Window(in.Now)
		spend := e.ledger.Spend(b.Key(), start)
		state := b.State(spend)

		better := state.severity() > worst.severity() ||
			(state.severity() == worst.severity() && specificity[b.Scope] > rank)
		if better {
			worst, chosen, chosenAt, rank = state, b, spend, specificity[b.Scope]
		}
	}

	return worst, chosen, chosenAt
}

// Status is one budget's current standing, for the console and the CLI.
type Status struct {
	Budget Budget
	Spend  Spend
	State  State
	// WindowStart and WindowEnd bound the period the spend was measured over.
	WindowStart time.Time
	WindowEnd   time.Time
	// Percent is how far through the hard limit the spend is, or -1 when there
	// is no money limit to be a percentage of.
	Percent int
}

// Status reports every budget's standing.
//
// This is an aggregate view by construction: a budget is a scope, and the
// smallest scope is one person looking at their own. Nothing here ranks one
// named person against another, and no caller can ask it to.
func (e *Engine) Status() []Status {
	if e == nil {
		return nil
	}

	set := e.Snapshot()
	now := e.now().UTC()

	out := make([]Status, 0, len(set.Budgets))
	for _, b := range set.Budgets {
		start, end := b.Period.Window(now)
		spend := e.ledger.Spend(b.Key(), start)
		out = append(out, Status{
			Budget:      b,
			Spend:       spend,
			State:       b.State(spend),
			WindowStart: start,
			WindowEnd:   end,
			Percent:     percentOf(spend.Cost, b.HardLimit),
		})
	}
	return out
}
