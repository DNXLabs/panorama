// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/dnxlabs/panorama/internal/providers"
)

// A route is a name a client may ask for, and the whole of what this
// deployment will serve under it.
//
// Nobody names a model. A coding tool is pointed at a route - Claude Code's
// ANTHROPIC_MODEL holds the route's name - and the route decides which
// model that becomes. That indirection is the product: which model serves
// "standard", and which country it is processed in, changes in a console in ten
// seconds without an engineer touching a laptop.
//
// A route carries three decisions and they must not be confused:
//
//   - Conditions say who the route exists for. A request from outside them is
//     refused exactly as though the name had never been created, because that
//     is what it means for a route not to exist for you.
//   - Target says which model it becomes. One model, with no chain tried when
//     the provider throttles it: a throttle reaches the client as a 429, which
//     every client this product serves retries.
//   - ServesOverSoft says whether it is still served once a budget governing
//     the caller has passed its soft limit.
//
// Budgets are not here. A route respects the budgets set on the person and
// their teams, and cannot set, raise, or lower one - otherwise "what may I
// spend" would have two answers that disagree the first time somebody edited
// only one of them.
type Route struct {
	Name        string
	Description string

	// Users, Teams and Tools are the conditions. Empty means unrestricted,
	// which is what a route is created with: everyone, every team, every tool.
	//
	// Within one condition the values are alternatives - any of these users.
	// Across conditions they are requirements: a route naming a user and a team
	// admits that user when they are in that team, and nobody else. That is the
	// reading that makes a route narrower as an administrator adds to it, which
	// is what somebody adding a condition means, and it is the one that fails
	// closed when they mean the other.
	Users []string
	Teams []string
	Tools []string

	// Target is the model to invoke: which provider, and the exact string that
	// provider is given, inference-profile prefix included. It is chosen from
	// the discovered catalogue rather than typed, because a typed identifier is
	// a typo waiting to be found by whoever's request fails at 3am.
	//
	// A reference rather than a bare id, because two providers can report the
	// same string - `anthropic.claude-haiku-4-5` is valid on both Bedrock
	// endpoints - and they route, bill and authorise differently.
	Target providers.ModelRef

	// ServesOverSoft says whether this route is still served once a budget
	// governing the caller has passed its soft limit.
	//
	// True is the ordinary route and the default: a soft limit warns, a hard
	// limit blocks, and nothing in between stops anybody. False is how an
	// administrator says "past the warning line, not this one" - the engineer
	// is refused, told which budget stopped them, and offered the routes that
	// are still served.
	//
	// It is a property of the route rather than a rule that swaps its model,
	// and the difference is who finds out. A swap changes the model underneath
	// a running coding agent without telling it, and it cannot be told: a route
	// is configured into a harness once, with its context window and output cap
	// beside the name, and no harness renegotiates those mid-session. A refusal
	// reaches the engineer, who already has every route they may use declared
	// in their own configuration and can pick another one.
	ServesOverSoft bool
}

// ServesAt reports whether this route is served in a given budget state.
//
// Only the soft limit is asked about here. A hard limit blocks before any route
// is consulted, so a route that claimed to serve past one would be describing
// something that cannot happen - the comparison is written against severity
// anyway, so a state worse than over_soft can never accidentally read as
// permitted.
func (r Route) ServesAt(state State) bool {
	if r.ServesOverSoft {
		return true
	}
	return state.severity() < StateOverSoft.severity()
}

// reservedRouteNames are names a Claude Code client cannot ask for, because it
// resolves them itself before a request is made.
//
// "default" is the one that matters and the one everybody reaches for first. It
// is not an alias for a model - it is Claude Code's instruction to clear any
// override and fall back to the runtime default for the account, so a route
// called "default" is never requested by that name. What arrives instead is
// whatever the client resolved to, which is a model id nobody routed, and the
// gateway refuses it correctly and unhelpfully: the engineer sees a name they
// never typed refused, and a list of routes containing the one they thought
// they were using. Everything visible points away from the cause.
//
// The rest of the list is the alias set, which resolves the same way. Refused
// at the write path because the time to find this out is while naming the
// route, not from a 403 on somebody's laptop two days later.
//
// Matched case-insensitively. Whether a client lowercases before resolving is
// its business, not something to depend on, and a route named "Default" is a
// trap either way.
var reservedRouteNames = map[string]struct{}{
	"default": {}, "best": {}, "fable": {}, "sonnet": {}, "opus": {},
	"haiku": {}, "opusplan": {}, "sonnet[1m]": {}, "opus[1m]": {},
}

// Validate rejects a route that would refuse the requests it admits.
func (r Route) Validate() error {
	var errs []error

	name := strings.TrimSpace(r.Name)
	switch {
	case name == "":
		errs = append(errs, errors.New("a route needs a name; it is what a client asks for"))
	case strings.ContainsAny(name, "*?"):
		// A wildcard would make the allowlist not one. Refused here as well as
		// at the API, because this is the function every write path passes
		// through.
		errs = append(errs, fmt.Errorf("a route name cannot contain a wildcard: %q", name))
	case strings.ContainsAny(name, " \t\n"):
		errs = append(errs, fmt.Errorf("a route name cannot contain whitespace: %q", name))
	default:
		if _, reserved := reservedRouteNames[strings.ToLower(name)]; reserved {
			errs = append(errs, fmt.Errorf(
				"%q is a name Claude Code resolves before it sends a request, so a route called that "+
					"is never asked for - it would send a model id instead and be refused. Pick another name", name))
		}
	}

	switch {
	case r.Target.IsZero() || strings.TrimSpace(r.Target.Model) == "":
		// A route with no model would admit a request and then have nowhere to
		// send it. Refused here rather than discovered by whoever's request
		// reaches it.
		errs = append(errs, errors.New("a route needs a model to route to"))
	case !r.Target.Provider.Valid():
		errs = append(errs, fmt.Errorf("a route's model names the provider %q, which this build does not serve",
			r.Target.Provider))
	}

	for _, condition := range []struct {
		name   string
		values []string
	}{
		{"user", r.Users}, {"team", r.Teams}, {"tool", r.Tools},
	} {
		for i, v := range condition.values {
			if strings.TrimSpace(v) == "" {
				errs = append(errs, fmt.Errorf("%s condition %d is empty", condition.name, i+1))
			}
		}
	}

	return errors.Join(errs...)
}

// RouteMinimum is a per-model limit - the context window, or the output cap -
// for the model this route becomes. Zero means unknown, and a client is told
// nothing rather than a number.
//
// It is the only place the answer is derived, because it is what reaches a
// laptop: the console, the CLI and the generated client configuration all read
// it, and two derivations of "what may this route tell a client" would drift.
func RouteMinimum(limits map[providers.ModelRef]int, r Route) int {
	limit, known := limits[r.Target]
	if !known || limit <= 0 {
		return 0
	}
	return limit
}

// Available reports whether this route exists for a given request.
//
// "Exists" rather than "is permitted", and the difference is the whole point of
// the conditions: a request from outside them is refused with the same words as
// a name nobody ever created, and the list of names offered back is filtered
// the same way. A route somebody may not use is not a route they can see.
//
// Budget state is deliberately not asked about here. This answers a permanent
// question - who is this route for - and it is what the setup screens render a
// harness configuration from. Folding a temporary state into it would write
// today's budget into a file that lives for months: an engineer who regenerated
// their opencode configuration while over a soft limit would lose the route
// from it entirely, and get it back only by regenerating again after the reset.
// See ServesAt for the question that changes by the hour.
func (r Route) Available(in Input, teams []string) bool {
	if len(r.Users) > 0 {
		// A service token has no user id, so a route restricted to named people
		// is not one a pipeline can reach. That is the right answer rather than
		// an oversight: the condition names people.
		if in.UserID == "" || !slices.Contains(r.Users, in.UserID) {
			return false
		}
	}
	if len(r.Teams) > 0 && !containsAny(r.Teams, teams) {
		return false
	}
	if len(r.Tools) > 0 && !containsFold(r.Tools, in.Tool) {
		return false
	}
	return true
}

// Restricted reports whether any condition narrows this route.
func (r Route) Restricted() bool {
	return len(r.Users) > 0 || len(r.Teams) > 0 || len(r.Tools) > 0
}

// containsAny reports whether the two lists share a value.
func containsAny(want, have []string) bool {
	for _, v := range have {
		if slices.Contains(want, v) {
			return true
		}
	}
	return false
}

// containsFold reports whether want holds value, ignoring case. Tool names come
// from a user agent, and an allowlist that was case-sensitive would refuse a
// route the day a client capitalised itself differently.
func containsFold(want []string, value string) bool {
	for _, v := range want {
		if strings.EqualFold(v, value) {
			return true
		}
	}
	return false
}
