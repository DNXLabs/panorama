// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"slices"
	"strings"

	"github.com/dnxlabs/panorama/internal/providers"
)

// Which client API a model speaks is a table this package maintains, for the
// same reason the context windows are: neither endpoint publishes it.
//
// bedrock-mantle's /v1/models returns an id, an owner, a status and the
// retention modes a model allows - nothing about which of the three request
// shapes it accepts. The answer lives in each model's AWS model card, and the
// cards disagree with AWS's own compatibility table in places. So this is a list written from the cards, it fails the way a stale
// list should - a model it has not heard of speaks nothing and is listed as
// browse-only rather than served on a guess - and a deployment corrects it per
// provider through the `apis` option without waiting for a release.
//
// The live suite probes each claim here, because a row that says a model speaks
// Responses and a Mantle that refuses it is a route failing at somebody's desk.

// mantleFamily is what this build knows about one group of Mantle models.
type mantleFamily struct {
	// prefix matches the start of a model id.
	prefix string
	apis   []providers.API
	// openAIBase is where the OpenAI-shaped APIs live for these models. Mantle
	// serves some families at /v1 and others at /openai/v1, and a model sent to
	// the other one is refused as not found - the cards for gpt-oss say "not
	// /openai/v1" and the cards for GPT-5.x say "not /v1", in those words.
	openAIBase string
	// completionTokens is a family that reads a Chat Completions output cap
	// as max_completion_tokens and refuses max_tokens outright. See
	// spellCompletionCap.
	completionTokens bool
}

const (
	baseV1       = "/v1"
	baseOpenAIV1 = "/openai/v1"
)

var (
	chatOnly         = []providers.API{providers.APIOpenAIChat}
	chatAndResponses = []providers.API{providers.APIOpenAIChat, providers.APIOpenAIResponses}
	messagesOnly     = []providers.API{providers.APIAnthropicMessages}
)

// mantleFamilies is matched in order, longest prefix first where two overlap.
var mantleFamilies = []mantleFamily{
	// Claude is served over the Anthropic Messages API alone.
	{prefix: "anthropic.", apis: messagesOnly},

	// OpenAI's open-weight models answer at /v1; the GPT-5.x and later families
	// at /openai/v1, where they are OpenAI's reasoning models and take their
	// Chat Completions cap as max_completion_tokens alone.
	{prefix: "openai.gpt-oss-", apis: chatAndResponses, openAIBase: baseV1},
	{prefix: "openai.gpt-5", apis: chatAndResponses, openAIBase: baseOpenAIV1, completionTokens: true},
	{prefix: "openai.gpt-6", apis: chatAndResponses, openAIBase: baseOpenAIV1, completionTokens: true},
	{prefix: "openai.gpt-daybreak-", apis: chatAndResponses, openAIBase: baseOpenAIV1, completionTokens: true},

	{prefix: "google.gemma-4-", apis: chatAndResponses, openAIBase: baseOpenAIV1},
	{prefix: "google.gemma-3-", apis: chatOnly, openAIBase: baseV1},
	{prefix: "xai.grok-", apis: chatAndResponses, openAIBase: baseOpenAIV1},

	{prefix: "qwen.", apis: chatOnly, openAIBase: baseV1},
	{prefix: "moonshotai.", apis: chatOnly, openAIBase: baseV1},
	{prefix: "deepseek.", apis: chatOnly, openAIBase: baseV1},
	{prefix: "zai.", apis: chatOnly, openAIBase: baseV1},
	{prefix: "mistral.", apis: chatOnly, openAIBase: baseV1},
	{prefix: "minimax.", apis: chatOnly, openAIBase: baseV1},
	{prefix: "nvidia.", apis: chatOnly, openAIBase: baseV1},
	{prefix: "writer.", apis: chatOnly, openAIBase: baseV1},
}

// mantleFamilyOf finds the entry for a model id, and whether there is one.
func mantleFamilyOf(model string) (mantleFamily, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	for _, f := range mantleFamilies {
		if strings.HasPrefix(id, f.prefix) {
			return f, true
		}
	}
	return mantleFamily{}, false
}

// overrideFor looks an id up in an operator's override map, by family first
// and then by exact id - the same order the context-window override uses.
func overrideFor[T any](overrides map[string]T, model string) (T, bool) {
	var zero T
	if len(overrides) == 0 {
		return zero, false
	}
	for _, key := range []string{modelFamily(model), strings.ToLower(strings.TrimSpace(model))} {
		if v, ok := overrides[key]; ok {
			return v, true
		}
	}
	return zero, false
}

// MantleAPIs reports what a Mantle model speaks, with an operator's overrides
// applied.
func MantleAPIs(overrides map[string][]providers.API) func(model string) []providers.API {
	return func(model string) []providers.API {
		if list, ok := overrideFor(overrides, model); ok {
			return slices.Clone(list)
		}
		if f, ok := mantleFamilyOf(model); ok {
			return slices.Clone(f.apis)
		}
		return nil
	}
}

// mantleOpenAIBase is where a model's OpenAI-shaped requests go.
//
// A model the table does not know, served only because an operator's override
// says it speaks Chat Completions, goes to /v1: that is the path AWS's own Chat
// Completions page documents, and the one most families use.
func mantleOpenAIBase(model string) string {
	if f, ok := mantleFamilyOf(model); ok && f.openAIBase != "" {
		return f.openAIBase
	}
	return baseV1
}

// mantleCompletionTokens reports whether a model's Chat Completions output cap
// must be spelled max_completion_tokens.
func mantleCompletionTokens(model string) bool {
	f, ok := mantleFamilyOf(model)
	return ok && f.completionTokens
}

// RuntimeAPIs reports what a bedrock-runtime model speaks.
//
// The runtime provider sends the Anthropic Messages body through InvokeModel,
// and that body means nothing to any other vendor's model - so an Anthropic
// model speaks Messages and everything else is browse-only here. Runtime also
// serves an OpenAI-compatible path now; serving it is a later change to this
// provider, not an override an operator can switch on, which is why the
// runtime options refuse one (see validateRuntimeOptions).
func RuntimeAPIs(model string) []providers.API {
	if vendorOf(model) == "anthropic" {
		return slices.Clone(messagesOnly)
	}
	return nil
}

// vendorOf reads the vendor segment of an id, past any geography prefix:
// us.anthropic.claude-sonnet-5 and anthropic.claude-sonnet-5 are both
// "anthropic".
func vendorOf(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if first, rest, ok := strings.Cut(id, "."); ok && rest != "" {
		if _, isGeo := knownGeographies[first]; isGeo {
			id = rest
		}
	}
	vendor, _, _ := strings.Cut(id, ".")
	return vendor
}
