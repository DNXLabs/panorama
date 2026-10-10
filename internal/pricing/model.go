// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import "strings"

// Scope is how a Bedrock inference profile routes a request, which is a price
// input rather than a deployment detail.
//
// M1 discovered that current Anthropic models cannot be invoked by their bare
// foundation-model id and require a `us.`- or `global.`-prefixed inference
// profile. M2 discovered what that prefix costs: AWS publishes two rates for
// every current Claude model, and the global one is 10% cheaper. Claude Sonnet
// 5 input tokens are $2.20 per million through `us.` and $2.00 through
// `global.`; the same 10% holds for output and both cache classes.
//
// So the prefix cannot be normalised away. A gateway that strips it to find a
// foundation model id would mis-price every request by 10% in whichever
// direction the operator happened to configure.
type Scope string

// The two scopes AWS prices separately.
const (
	// ScopeRegional covers `us.`, `eu.`, `apac.` and friends, and a bare
	// foundation-model id invoked directly in its own region.
	ScopeRegional Scope = "regional"
	// ScopeGlobal covers the `global.` profiles, which route anywhere and bill
	// about 10% less.
	ScopeGlobal Scope = "global"
)

// globalPrefix is the one profile prefix that changes the price.
const globalPrefix = "global"

// regionalScopes are the geographic inference-profile prefixes AWS publishes.
//
// This is an allowlist, and the failure mode when it is out of date is the safe
// one. An unrecognised prefix leaves the id unchanged, no price-table row
// matches it, and the request is recorded unpriced with a log line naming the
// exact id. The alternative - treating any leading segment as a scope - would
// silently attach one model's price to another model's traffic.
var regionalScopes = map[string]struct{}{
	"us":     {},
	"us-gov": {},
	"eu":     {},
	"apac":   {},
	"jp":     {},
	"au":     {},
	"ca":     {},
	"sa":     {},
	"in":     {},
	"il":     {},
	"mx":     {},
	"kr":     {},
}

// ParseModel splits a provider-native model identifier into the foundation
// model id that carries the price and the routing scope that selects which of
// its two rate sets applies.
//
//	us.anthropic.claude-sonnet-5     → anthropic.claude-sonnet-5, regional
//	global.anthropic.claude-sonnet-5 → anthropic.claude-sonnet-5, global
//	anthropic.claude-sonnet-5        → anthropic.claude-sonnet-5, regional
//
// An inference-profile ARN is reduced to its id first, because a client may be
// configured with either form.
func ParseModel(id string) (model string, scope Scope) {
	model = id

	// arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.…
	if strings.HasPrefix(model, "arn:") {
		if slash := strings.LastIndex(model, "/"); slash >= 0 {
			model = model[slash+1:]
		}
	}

	prefix, rest, found := strings.Cut(model, ".")
	if !found || rest == "" {
		return model, ScopeRegional
	}

	if prefix == globalPrefix {
		return rest, ScopeGlobal
	}
	if _, ok := regionalScopes[prefix]; ok {
		return rest, ScopeRegional
	}

	// A bare foundation-model id: anthropic.claude-3-haiku-20240307-v1:0.
	// Invoked in its own region, so it bills at the regional rate.
	return model, ScopeRegional
}
