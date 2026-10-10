// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package policy decides what happens to a request before it reaches a
// provider: whether the name it asked for exists for the person asking, which
// model that name becomes, and whether a budget allows it at all.
//
// # Routes, and nothing else
//
// A client never names a model. It names a route, and the route is the
// allowlist: a name absent from it is refused, and there is no wildcard, no
// flag and no configuration file that could add one. A route carries
// conditions saying who it exists for - users, teams, tools - the one model it
// becomes, on one provider, and whether it keeps serving once a budget has
// passed its soft limit.
//
// There is no fallback chain. A throttle reaches the client as a 429, which
// every client this gateway serves retries, and a route that should survive a
// throttle is a second route the engineer can switch to.
//
// A route is served only on the client APIs its model speaks. A request for a
// Chat Completions model arriving on the Anthropic surface is refused with the
// endpoint that would work, rather than translated.
//
// Budgets are set on people and teams, and a route reads them without being
// able to change them. A hard limit blocks everything. A soft limit warns, and
// stops the routes an administrator marked as not serving past one - the
// engineer is refused, told which budget stopped them and which routes still
// work, and switches. The gateway does not switch for them: a harness is told a
// route's context window once, when it is configured, and nothing renegotiates
// it mid-session.
//
// # Nothing here touches the database
//
// Streaming is sacred, and a budget check that ran a SELECT would put the
// database's latency between a keystroke and a token. So the whole decision is
// made against an in-memory snapshot: routes, budgets, and team membership are
// loaded on an interval, and spend is tracked in a Ledger this process
// increments as requests complete.
//
// That has a consequence worth stating plainly rather than discovering later.
// A request is priced only once it has finished, so enforcement is always
// against spend as of the last completed request - a single request can cross a
// limit, and the one after it is the one that is blocked. No design that does
// not hold the client's first token hostage to a write can do better than that,
// and holding the first token hostage is not on the table.
//
// # Money that could not be priced
//
// A request whose model has no rate is counted but its cost is not, because
// there is no honest number to add. Spend enforced against a budget is
// therefore a floor, not a total, and every message this package produces says
// so when it is true. This is the same commitment the product makes to
// unattributed spend, applied to enforcement.
package policy

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

// Scopes a budget can govern.
//
// v1 is single-organization, so ScopeOrg has exactly one subject: this
// deployment. Its budget rows carry an empty scope id.
const (
	ScopeOrg  = "org"
	ScopeTeam = "team"
	ScopeUser = "user"
)

// State is how a request stands against the budgets that govern it.
type State string

const (
	// StateNone means no budget governs this request at all. It is distinct
	// from StateOK, which means a budget looked and was satisfied.
	StateNone     State = ""
	StateOK       State = "ok"
	StateOverSoft State = "over_soft"
	StateOverHard State = "over_hard"
)

// severity orders states so the worst of several budgets wins.
func (s State) severity() int {
	switch s {
	case StateOverHard:
		return 3
	case StateOverSoft:
		return 2
	case StateOK:
		return 1
	default:
		return 0
	}
}

// Period is the window a budget's limits apply to.
type Period string

const (
	PeriodDay   Period = "day"
	PeriodWeek  Period = "week"
	PeriodMonth Period = "month"
)

// Window returns the UTC window containing now, as a half-open [start, end).
//
// Everything is UTC. A budget that reset at local midnight would reset at a
// different instant for each person it governs, and "when does this reset"
// would have no single answer to put in a block message.
func (p Period) Window(now time.Time) (start, end time.Time) {
	now = now.UTC()
	y, m, d := now.Date()

	switch p {
	case PeriodDay:
		start = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 0, 1)

	case PeriodWeek:
		// Weeks begin Monday, which is what a working week means to the people
		// whose spend this is. time.Weekday puts Sunday at zero.
		day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		offset := (int(day.Weekday()) + 6) % 7
		start = day.AddDate(0, 0, -offset)
		return start, start.AddDate(0, 0, 7)

	case PeriodMonth:
		start = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	}

	return time.Time{}, time.Time{}
}

// Valid reports whether p is a period this package knows.
func (p Period) Valid() bool {
	switch p {
	case PeriodDay, PeriodWeek, PeriodMonth:
		return true
	default:
		return false
	}
}

// Adjective renders a period as it reads in a sentence: "the daily limit".
func (p Period) Adjective() string {
	switch p {
	case PeriodDay:
		return "daily"
	case PeriodWeek:
		return "weekly"
	case PeriodMonth:
		return "monthly"
	default:
		return string(p)
	}
}

// Key identifies one budget's ledger window: a scope, its subject, and a
// period.
type Key struct {
	Scope   string
	ScopeID string
	Period  Period
}

// Spend is what one scope has used inside one window.
type Spend struct {
	Cost     pricing.Money
	Requests int64
	// Unpriced counts requests in the window whose cost is unknown. Their money
	// is missing from Cost, so a budget compared against Cost alone is being
	// compared against a floor. Every message that quotes a figure says so when
	// this is not zero.
	Unpriced int64
}

// add accumulates one completed request.
func (s Spend) add(other Spend) Spend {
	return Spend{
		Cost:     s.Cost + other.Cost,
		Requests: s.Requests + other.Requests,
		Unpriced: s.Unpriced + other.Unpriced,
	}
}

// sub removes a previously counted amount, flooring at zero so a reconcile that
// races a request cannot drive a counter negative.
func (s Spend) sub(other Spend) Spend {
	out := Spend{
		Cost:     s.Cost - other.Cost,
		Requests: s.Requests - other.Requests,
		Unpriced: s.Unpriced - other.Unpriced,
	}
	if out.Cost < 0 {
		out.Cost = 0
	}
	if out.Requests < 0 {
		out.Requests = 0
	}
	if out.Unpriced < 0 {
		out.Unpriced = 0
	}
	return out
}

// Budget is a spending limit on one scope, for one period.
//
// Every limit is a pointer because absent and zero are different: a nil hard
// limit means "no ceiling", and a zero one means "nothing may be spent". An
// operator who types 0 means the second.
type Budget struct {
	ID      string
	Scope   string
	ScopeID string
	Period  Period
	Name    string
	Enabled bool

	// SoftLimit warns, and stops the routes marked as not serving past one.
	// HardLimit blocks everything.
	SoftLimit *pricing.Money
	HardLimit *pricing.Money
}

// Key returns the ledger key this budget accumulates into.
func (b Budget) Key() Key {
	return Key{Scope: b.Scope, ScopeID: b.ScopeID, Period: b.Period}
}

// Subject renders who this budget governs, for a message an engineer reads.
func (b Budget) Subject() string {
	switch b.Scope {
	case ScopeOrg:
		return "this deployment"
	case ScopeTeam:
		return "your team"
	case ScopeUser:
		return "you"
	default:
		return b.Scope
	}
}

// Label is the budget's name, falling back to a description of its scope so a
// message never says "the  budget".
func (b Budget) Label() string {
	if b.Name != "" {
		return b.Name
	}
	return b.Period.Adjective() + " " + b.Scope
}

// Validate rejects a budget that would not enforce anything meaningful.
func (b Budget) Validate() error {
	var errs []error

	switch b.Scope {
	case ScopeOrg:
		if b.ScopeID != "" {
			errs = append(errs, errors.New("an org budget has no subject id: v1 is one deployment, one organization"))
		}
	case ScopeTeam, ScopeUser:
		if b.ScopeID == "" {
			errs = append(errs, fmt.Errorf("a %s budget needs the %s it applies to", b.Scope, b.Scope))
		}
	default:
		errs = append(errs, fmt.Errorf("scope %q is not one of %s, %s, %s", b.Scope, ScopeOrg, ScopeTeam, ScopeUser))
	}

	if !b.Period.Valid() {
		errs = append(errs, fmt.Errorf("period %q is not one of %s, %s, %s", b.Period, PeriodDay, PeriodWeek, PeriodMonth))
	}

	if b.SoftLimit == nil && b.HardLimit == nil {
		errs = append(errs, errors.New("no limit is set, so this budget would enforce nothing"))
	}
	for name, limit := range map[string]*pricing.Money{"soft": b.SoftLimit, "hard": b.HardLimit} {
		if limit != nil && *limit < 0 {
			errs = append(errs, fmt.Errorf("%s limit is negative", name))
		}
	}

	// A soft limit above the hard one never fires: the hard limit blocks first.
	// That is a configuration mistake rather than a preference, because the
	// warning it was meant to give is unreachable.
	if b.SoftLimit != nil && b.HardLimit != nil && *b.SoftLimit > *b.HardLimit {
		errs = append(errs, fmt.Errorf("soft limit %s is above the hard limit %s, so it would never warn",
			b.SoftLimit.USD(), b.HardLimit.USD()))
	}

	return errors.Join(errs...)
}

// State evaluates spend against this budget.
func (b Budget) State(s Spend) State {
	switch {
	case b.HardLimit != nil && s.Cost >= *b.HardLimit:
		return StateOverHard
	case b.SoftLimit != nil && s.Cost >= *b.SoftLimit:
		return StateOverSoft
	default:
		return StateOK
	}
}

// Input is what a request looks like to the policy engine.
type Input struct {
	UserID string
	// TeamID is set only when the request came in on a team's service token,
	// which has no user to reach a team through. It is empty for a person, and
	// deliberately so: people can be on several teams, and putting one of them
	// here would silently pick a winner for budgets and conditions alike.
	TeamID string
	// Tool is the coding tool, as identified from the user agent.
	Tool string
	// Surface is the client-facing API the request arrived on, as recorded.
	Surface string
	// API is the shape of the request. A route whose model does not speak it
	// is refused with the endpoint that would work, and every list of routes a
	// refusal offers is narrowed to the ones this API can reach. Empty asks no
	// question about shape, which is what the setup screens mean.
	API providers.API
	// Route is the name the client asked for. It is a route name, never a model
	// id: a client that sends a model identifier is asking for a route that
	// does not exist, and is refused like any other unknown name.
	Route string
	// CostCenterStamp is the raw x-panorama-cost-center header, if the client
	// sent one. It is carried through here rather than resolved in the data
	// plane for the same reason Set.Teams lives in the snapshot: resolving it
	// needs a lookup, and a lookup on the request path would be a query. It is
	// not a policy decision, and nothing in Evaluate branches on it.
	CostCenterStamp string
	// Now is injected by tests; zero means time.Now.
	Now time.Time
}

// Why a request was refused. Recorded and logged; the engineer is told the same
// thing for the first two, because a route somebody may not use is not one they
// should be able to detect.
const (
	// RefusedUnknownRoute: no route by that name.
	RefusedUnknownRoute = "unknown_route"
	// RefusedNotAvailable: the route exists and its conditions exclude this
	// request.
	RefusedNotAvailable = "route_not_available"
	// RefusedNoTarget: the route exists, admits the request, and names no
	// model. That is a deployment's mistake rather than an engineer's, so it is
	// said differently and logged at error level.
	RefusedNoTarget = "route_has_no_target"
	// RefusedProviderUnavailable: the route's model is on a provider that is
	// disabled, unconfigured, or failed to build. Also the deployment's
	// mistake, and handled the same way.
	RefusedProviderUnavailable = "provider_unavailable"
	// RefusedWrongAPI: the route exists for this caller and its model does not
	// speak the API the request arrived in. The engineer can fix this - point
	// the tool at the endpoint that does - so they are told which.
	RefusedWrongAPI = "route_wrong_api"
)

// Why a budget stopped a request. Both are a refusal the engineer can read and
// act on, and they are told apart because the two acts are different: one is
// out of money, the other is out of this particular model.
const (
	// BlockedBudgetExhausted: a hard limit. Nothing is served until the period
	// rolls over or somebody raises the limit.
	BlockedBudgetExhausted = "budget_exhausted"
	// BlockedOverSoft: a soft limit, and a route an administrator marked as not
	// surviving one. Other routes are still served, and the message names them.
	BlockedOverSoft = "route_over_soft"
)

// Decision is what the engine decided, and why.
type Decision struct {
	// Route is the name the request asked for and was served under.
	Route string
	// Target is the model to invoke. Zero only when the request was refused or
	// blocked.
	Target providers.ModelRef
	// Provider is what serves Target, resolved from the same snapshot the
	// route came from, so the data plane has nothing left to look up and no
	// moment at which the route and the provider could be two versions apart.
	Provider providers.Provider
	// APIs are the client APIs the route's model speaks, set on every decision
	// that found an available route.
	APIs []providers.API

	// Refused is set when no model was reached: the name does not exist for
	// this caller, or the route it names has nowhere to send them.
	Refused bool
	// RefusedReason is one of the constants above, for the log and the audit
	// trail rather than for the client.
	RefusedReason  string
	RefusedMessage string

	// Blocked is set when a budget stopped the request.
	Blocked bool
	// BlockReason tells the two kinds apart for the log and the audit trail:
	// out of money, or out of this model. The engineer gets a different message
	// for each, because they can act on one of them without anybody's help.
	BlockReason string
	// BlockMessage is written for the engineer who hit the limit: what
	// happened, how much of which limit is used, when it resets, what they may
	// use instead, and who can raise it.
	BlockMessage string

	// Warning is set when a soft limit has been crossed and this route serves
	// past one anyway. The request proceeds.
	Warning string

	// CostCenterID is the line a valid stamp named, empty otherwise. Passed to
	// the recorder, never acted on: allocation says who pays, and a cost center
	// cannot stop or route a request.
	CostCenterID string
	// CostCenterStamp is a stamp that resolved to nothing - an unknown code, or
	// a line that refuses stamps. Recorded rather than dropped, because both
	// look like success from the client's side while the money goes somewhere
	// else. Dropping it would be a fallback that masks an error.
	CostCenterStamp string

	// Budget is the worst state across every budget that governs this request.
	Budget State
	// BudgetID names the budget that produced that state, for the audit trail.
	BudgetID string
}

// Served reports whether the request reached a model.
func (d Decision) Served() bool { return !d.Refused && !d.Blocked && !d.Target.IsZero() }

// header renders a message safely for an HTTP header value: one line, no
// control characters.
func headerSafe(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}
