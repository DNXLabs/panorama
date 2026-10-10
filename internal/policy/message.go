// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"fmt"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

// The messages in this file are part of the product, not error strings.
//
// An engineer who hits a budget is in the middle of something, and the gateway
// is the thing that just stopped them. Four questions have to be answered
// without them going to ask anyone: what happened, how much of which limit is
// used, when it comes back, and who can change it. A message that answers three
// of those generates a support conversation, which is the outcome the whole
// feature exists to avoid.

// blockMessage explains a budget that has stopped a request.
func blockMessage(b Budget, s Spend, now time.Time) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Panorama stopped this request: the %s budget is used up.\n\n", b.Label())
	sb.WriteString(usageSentence(b, s, true))
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "The limit resets %s.\n", resetPhrase(b.Period, now))

	if s.Unpriced > 0 {
		sb.WriteString(unpricedSentence(s) + "\n")
	}

	sb.WriteString("\nA Panorama administrator can raise the limit.")
	return sb.String()
}

// softStopMessage explains a route that is not served past a soft limit.
//
// This is the message the whole change is for, so it has more work to do than
// the others. An engineer meets it mid-task, usually mid-turn, and the thing
// that makes it a thirty-second interruption rather than a support conversation
// is that the next action is in the message: which name to switch to. That list
// is filtered to what is actually served right now - offering a route the same
// budget state would also stop teaches an engineer that the list is a guess.
//
// It says what the alternative costs them in nothing but words: which routes,
// how to select one, and that an administrator can lift this if the answer is
// that they should not have to.
func softStopMessage(route string, b Budget, s Spend, now time.Time, available []string) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Panorama is not serving %s: the %s budget is past its soft limit.\n\n",
		quoteModel(route), b.Label())
	sb.WriteString(usageSentence(b, s, false))
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "The limit resets %s.\n", resetPhrase(b.Period, now))

	if s.Unpriced > 0 {
		sb.WriteString(unpricedSentence(s) + "\n")
	}

	sb.WriteString("\n")
	others := without(available, route)
	if len(others) == 0 {
		// Nothing to switch to. Said plainly rather than dressed up as a
		// choice: the only way out is somebody changing the configuration, and
		// pretending otherwise sends an engineer looking for a route that is
		// not there.
		sb.WriteString(
			"No other route on this deployment is served past this limit, so there is nothing " +
				"to switch to. A Panorama administrator can raise the limit, or allow a route past it.")
		return sb.String()
	}

	sb.WriteString("Routes still served: " + strings.Join(others, ", ") + ".\n\n")
	sb.WriteString(
		"Switch to one of them and carry on - /model in Claude Code, or the model list in " +
			"opencode; both already have every route you can use. " +
			"A Panorama administrator can raise the limit instead.")
	return sb.String()
}

// without drops one name from a list, for a message that must not offer the
// route it is refusing.
//
// A route marked as not surviving a soft limit never appears in the list it is
// filtered from, so this removes nothing today. It is here because the list and
// the refusal are produced by two different rules, and the day those disagree
// the failure would be a message telling somebody to switch to the route they
// are already on.
func without(names []string, drop string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}

// warnMessage explains a soft limit that has been crossed but has not stopped
// anything. It is one line, because it travels in a response header as well as
// a log.
func warnMessage(b Budget, s Spend, now time.Time) string {
	var sb strings.Builder

	sb.WriteString(usageSentence(b, s, false))
	fmt.Fprintf(&sb, " Requests are still being served; the limit resets %s.", resetPhrase(b.Period, now))
	if s.Unpriced > 0 {
		sb.WriteString(" " + unpricedSentence(s))
	}
	return headerSafe(sb.String())
}

// refusedMessage explains a name the gateway will not serve.
//
// One message for two situations, and that is deliberate. A route whose
// conditions exclude somebody does not exist for them, so telling them it
// exists but is not theirs would contradict the model and leak which routes
// other teams have. The log says which of the two it was; the engineer is told
// what they can do instead, which is the only part they can act on.
func refusedMessage(name string, api providers.API, available []string) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Panorama does not serve %s.\n\n", quoteModel(name))
	if len(available) == 0 {
		sb.WriteString(
			"There is no route you can use on this deployment yet. " +
				"A Panorama administrator creates routes and decides which model each one runs on.")
		return sb.String()
	}

	sb.WriteString("Routes you can use: " + strings.Join(available, ", ") + ".\n\n")
	setting := "ANTHROPIC_MODEL for Claude Code"
	if api == providers.APIOpenAIChat || api == providers.APIOpenAIResponses {
		setting = "the model in your tool's provider configuration"
	}
	sb.WriteString(
		"Set the route name as your model - " + setting + ". " +
			"You do not name a model here: a Panorama administrator decides which model each route runs on.")
	return sb.String()
}

// providerUnavailableMessage explains a route whose provider is not serving.
// The engineer can do nothing about it, so it says so.
func providerUnavailableMessage(name string) string {
	return fmt.Sprintf(
		"Panorama could not serve %s: the model provider this route runs on is not available on this "+
			"deployment right now.\n\n"+
			"That is a configuration error on this deployment rather than something you did. "+
			"Quote the request id to a Panorama administrator.", quoteModel(name))
}

// wrongAPIMessage tells an engineer the route exists and is spoken at another
// endpoint.
//
// Said plainly rather than refused as an unknown name: the route is one this
// caller may use, so naming it gives nothing away, and the fix is theirs - a
// base URL in their tool's configuration.
func wrongAPIMessage(name string, api providers.API, speaks []providers.API) string {
	if len(speaks) == 0 {
		return fmt.Sprintf(
			"Panorama cannot serve %s: its model speaks no API this gateway serves.\n\n"+
				"That is a configuration error on this deployment rather than something you did. "+
				"Quote the request id to a Panorama administrator.", quoteModel(name))
	}
	endpoints := make([]string, 0, len(speaks))
	for _, a := range speaks {
		endpoints = append(endpoints, a.Label()+" at "+a.Endpoint())
	}
	return fmt.Sprintf(
		"The route %s is not served over the %s API. Its model speaks %s.\n\n"+
			"Point your tool at that endpoint on this gateway, or use a route whose model speaks %s.",
		quoteModel(name), api.Label(), strings.Join(endpoints, " and "), api.Label())
}

// noTargetMessage explains a route that admitted a request and had nowhere to
// send it.
//
// The write path refuses to create a route without a default, so reaching this
// means the database says something the console cannot express. That is the
// deployment's problem and the message says so rather than implying the
// engineer asked for something wrong.
func noTargetMessage(name string) string {
	return fmt.Sprintf(
		"Panorama could not serve %s: the route exists but has no model to route to.\n\n"+
			"That is a configuration error on this deployment rather than something you did. "+
			"Quote the request id to a Panorama administrator.", quoteModel(name))
}

// usageSentence states what has been used against what.
func usageSentence(b Budget, s Spend, hard bool) string {
	limit := b.SoftLimit
	if hard {
		limit = b.HardLimit
	}

	// The threshold is unbreached only when this is called speculatively; say
	// what has been used rather than inventing a limit.
	what := fmt.Sprintf("%s across %s", s.Cost.Display(), plural(s.Requests, "request"))
	if limit != nil && s.Cost >= *limit {
		what = fmt.Sprintf("%s of the %s %s limit",
			s.Cost.Display(), b.Period.Adjective(), limit.Display())
	}

	return fmt.Sprintf("%s used %s.", subjectHas(b), what)
}

// unpricedSentence states the gap in the figure just quoted.
//
// Enforcing against a number while knowing it is incomplete, and not saying so,
// is the same failure as reporting attributed spend as though it were all of
// it.
func unpricedSentence(s Spend) string {
	return fmt.Sprintf("%s in this period could not be priced, so the real figure is higher.",
		capitalise(plural(s.Unpriced, "request")))
}

// subjectHas renders who the budget governs, with its verb.
func subjectHas(b Budget) string {
	switch b.Scope {
	case ScopeOrg:
		return "This deployment has"
	case ScopeTeam:
		return "Your team has"
	case ScopeUser:
		return "You have"
	default:
		return "This scope has"
	}
}

// resetPhrase says when the window rolls over, in both absolute and relative
// terms. Absolute alone makes someone do arithmetic; relative alone is useless
// to anyone planning around it.
func resetPhrase(p Period, now time.Time) string {
	_, end := p.Window(now)
	if end.IsZero() {
		return "at the end of the period"
	}

	return fmt.Sprintf("at %s (%s)", end.Format("2006-01-02 15:04 UTC"), humanUntil(end.Sub(now)))
}

// humanUntil renders a duration the way someone waiting for it would say it.
func humanUntil(d time.Duration) string {
	switch {
	case d <= 0:
		return "any moment now"
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return "in " + plural(int64(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return "in " + plural(int64(d.Hours()), "hour")
	default:
		return "in " + plural(int64(d.Hours()/24), "day")
	}
}

// plural renders a count with its noun, pluralised.
func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// quoteModel renders a model name for a message, without quotes around an empty
// one.
func quoteModel(model string) string {
	if model == "" {
		return "that model"
	}
	return `"` + model + `"`
}

// percentOf reports how far through a limit an amount is, for logs and the API.
// It returns -1 when there is no limit to be a percentage of.
func percentOf(amount pricing.Money, limit *pricing.Money) int {
	if limit == nil || *limit <= 0 {
		return -1
	}
	return int(int64(amount) * 100 / int64(*limit))
}
