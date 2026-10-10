// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"regexp"
	"strings"
)

// What a model will accept and produce is a table this package maintains,
// because Bedrock does not publish either number.
//
// That was checked against the API rather than assumed. Every field
// ListFoundationModels returns is here - arn, id, name, provider, input and
// output modalities, customizations, inference types, streaming, lifecycle -
// and none of them is a context length or an output cap. GetFoundationModel
// returns the same shape, ListInferenceProfiles returns profile arns and
// regions, and the only MaxTokens fields anywhere in the service belong to
// imported models, fine-tuning hyperparameters and evaluation jobs. The
// numbers the Bedrock console shows beside a model are editorial content
// shipped in its frontend, not something the account can query.
//
// So this is a hand-maintained list, and it will go stale. It is written to
// fail in the ways a stale list should:
//
//   - A model absent from it reports nothing rather than a guess, and every
//     caller distinguishes "not recorded" from a number. A wrong number is
//     worse than no number here, because a route refusal and a coding agent's
//     token accounting are both derived from it.
//   - Matching is by family rather than by exact id. Bedrock revises a model
//     in place - anthropic.claude-haiku-4-5-20251001-v1:0 today, a new date
//     tomorrow - and an exact-id table would silently lose a model on a
//     revision that changed nothing about its limits.
//   - It is overridable per provider through the context windows in
//     Settings, Model providers, for the same reason the unsupported beta
//     list is: an operator who finds this wrong should not have to wait for a
//     release.
//
// Sources are AWS's model documentation and Anthropic's, which agree. Where a
// model takes a larger window only behind a beta flag, the number here is the
// one it serves without being asked, because that is what a client gets by
// default. Output is the model's default maximum rather than its ceiling: the
// larger figure is itself gated behind a flag on several models, and a client
// told the ceiling would ask for more than the request will grant.
const (
	window200K = 200_000
	window1M   = 1_000_000
)

// modelLimits is what this build knows about a model that the provider will
// not say.
type modelLimits struct {
	// Context is how many tokens the model accepts in one request.
	Context int
	// Output is how many tokens it will produce by default.
	Output int
}

// defaultModelLimits maps a model family to its limits.
//
// Keys are families, produced by modelFamily below: the provider prefix and
// the model name, with the revision date and version suffix removed. The
// entries cover the Anthropic catalogue on both endpoints, and the models
// bedrock-mantle serves over the OpenAI APIs. An unlisted model reports nothing
// rather than a default - a default here would be a guess wearing a number's
// clothes.
var defaultModelLimits = map[string]modelLimits{
	"anthropic.claude-opus-5":     {Context: window1M, Output: 64_000},
	"anthropic.claude-sonnet-5":   {Context: window1M, Output: 64_000},
	"anthropic.claude-opus-4-8":   {Context: window1M, Output: 64_000},
	"anthropic.claude-opus-4-7":   {Context: window1M, Output: 64_000},
	"anthropic.claude-opus-4-6":   {Context: window200K, Output: 64_000},
	"anthropic.claude-opus-4-5":   {Context: window200K, Output: 32_000},
	"anthropic.claude-opus-4-1":   {Context: window200K, Output: 32_000},
	"anthropic.claude-opus-4":     {Context: window200K, Output: 32_000},
	"anthropic.claude-sonnet-4-6": {Context: window200K, Output: 32_000},
	"anthropic.claude-sonnet-4-5": {Context: window200K, Output: 32_000},
	"anthropic.claude-sonnet-4":   {Context: window200K, Output: 32_000},
	"anthropic.claude-haiku-4-5":  {Context: window200K, Output: 32_000},
	"anthropic.claude-3-7-sonnet": {Context: window200K, Output: 8_192},
	"anthropic.claude-3-5-sonnet": {Context: window200K, Output: 8_192},
	"anthropic.claude-3-5-haiku":  {Context: window200K, Output: 8_192},
	"anthropic.claude-3-opus":     {Context: window200K, Output: 4_096},
	"anthropic.claude-3-sonnet":   {Context: window200K, Output: 4_096},
	"anthropic.claude-3-haiku":    {Context: window200K, Output: 4_096},

	// The open-weight and third-party models bedrock-mantle serves, from each
	// model's AWS model card (docs.aws.amazon.com/bedrock/latest/userguide/
	// model-card-*.html). The cards quote "128K", and it
	// is read as 128,000: a window recorded a little low compacts a session a
	// little early, and one recorded high is a request refused as too long.
	//
	// A model whose card publishes a window and no output cap is left out
	// entirely - Gemma 4 and Grok 4.3 today - rather than recorded with half of
	// what a client needs: opencode's limit takes both numbers or neither, and
	// an entry with one is a row every reader has to special-case.
	"openai.gpt-oss-120b":                   {Context: 128_000, Output: 16_000},
	"openai.gpt-oss-20b":                    {Context: 128_000, Output: 16_000},
	"openai.gpt-oss-safeguard-120b":         {Context: 128_000, Output: 16_000},
	"openai.gpt-oss-safeguard-20b":          {Context: 128_000, Output: 16_000},
	"qwen.qwen3-coder-480b-a35b-instruct":   {Context: 128_000, Output: 16_000},
	"qwen.qwen3-coder-30b-a3b-instruct":     {Context: 256_000, Output: 16_000},
	"qwen.qwen3-coder-next":                 {Context: 256_000, Output: 16_000},
	"qwen.qwen3-235b-a22b-2507":             {Context: 256_000, Output: 8_000},
	"qwen.qwen3-next-80b-a3b-instruct":      {Context: 256_000, Output: 8_000},
	"qwen.qwen3-32b":                        {Context: 32_000, Output: 8_000},
	"moonshotai.kimi-k2.5":                  {Context: 256_000, Output: 16_000},
	"moonshotai.kimi-k2-thinking":           {Context: 256_000, Output: 16_000},
	"deepseek.v3.1":                         {Context: 128_000, Output: 8_000},
	"deepseek.v3.2":                         {Context: 164_000, Output: 8_000},
	"zai.glm-4.7":                           {Context: 203_000, Output: 4_000},
	"zai.glm-4.7-flash":                     {Context: 203_000, Output: 4_000},
	"zai.glm-5":                             {Context: 200_000, Output: 128_000},
	"mistral.devstral-2-123b":               {Context: 256_000, Output: 32_000},
	"mistral.mistral-large-3-675b-instruct": {Context: 256_000, Output: 32_000},
	"mistral.magistral-small-2509":          {Context: 128_000, Output: 40_000},
	"minimax.minimax-m2.1":                  {Context: 196_000, Output: 8_000},
	"minimax.minimax-m2.5":                  {Context: 196_000, Output: 8_000},
	"nvidia.nemotron-super-3-120b":          {Context: 256_000, Output: 32_000},
	"nvidia.nemotron-nano-3-30b":            {Context: 256_000, Output: 8_000},
	"google.gemma-3-27b-it":                 {Context: 128_000, Output: 8_000},
}

// Nothing before Claude 3 is listed, and the omission is deliberate rather
// than an oversight. In `anthropic.claude-v2:1` the `-v2` is part of the
// model's name, while in `anthropic.claude-3-haiku-20240307-v1:0` the `-v1` is
// a revision - the same characters meaning opposite things, in the only two
// families where it happens. Rather than teach modelFamily a special case for
// models Bedrock has retired, they are left out: an unlisted model reports
// nothing, which is the right answer about a model nobody can invoke.

// revisionSuffix is the date-and-version tail Bedrock appends to a model id:
// "-20251001-v1:0", or "-v1:0" on a model that carries no date.
//
// Removed before lookup so a model that is revised in place keeps its limits.
// It cannot tell that tail from a model whose name genuinely ends in a version
// - anthropic.claude-v2:1 reduces to "anthropic.claude" - which is why the
// table lists nothing from that era. See above.
var revisionSuffix = regexp.MustCompile(`(?:-\d{8})?(?:-v\d+(?::\d+)?)?$`)

// providerPrefixes are the leading components of a model id that name a model
// provider rather than a geography, so modelFamily does not mistake one for
// the other.
var providerPrefixes = map[string]struct{}{
	"anthropic":  {},
	"amazon":     {},
	"meta":       {},
	"mistral":    {},
	"cohere":     {},
	"ai21":       {},
	"deepseek":   {},
	"writer":     {},
	"luma":       {},
	"stability":  {},
	"twelvelabs": {},
	"qwen":       {},
	"openai":     {},
	// Vendors bedrock-mantle names. Without them "moonshotai.kimi-k2.5" would
	// have its vendor stripped as though it were a geography.
	"moonshot":   {},
	"moonshotai": {},
	"zai":        {},
	"google":     {},
	"xai":        {},
	"minimax":    {},
	"nvidia":     {},
}

// modelFamily reduces a model id to the key this table is written against.
//
// The inference-profile prefix goes first - a geography decides where a
// request is processed and what it costs, and never how much context the model
// takes, so us., au. and global. share one entry rather than three that could
// drift apart. Then the revision suffix, for the reason above.
func modelFamily(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))

	// Strip a geography prefix: "us.anthropic.claude-sonnet-5" leaves the rest.
	// Only a first component that is not itself a provider name, which is what
	// distinguishes "us." from "anthropic.".
	if first, rest, ok := strings.Cut(id, "."); ok && rest != "" {
		if _, isProvider := providerPrefixes[first]; !isProvider {
			id = rest
		}
	}

	return revisionSuffix.ReplaceAllString(id, "")
}

// limitsFor reports what a model accepts and produces, and whether anything is
// known about it.
//
// The boolean is the point. A model this table has never heard of returns
// (zero, false), and callers say "not recorded" rather than printing a zero or
// substituting a default - the console shows nothing, the route check declines
// to compare, the generated client configuration omits the field. Guessing
// here would put a number in front of an administrator that nothing stands
// behind, and the number decides whether somebody's session breaks.
//
// An operator's override carries the context window only. That is the number
// they can observe going wrong - a session that compacts early, or a request
// refused as too long - and inviting them to hand-maintain an output cap as
// well would be asking for a second number to get wrong in exchange for
// nothing they were trying to fix.
func limitsFor(overrides map[string]int, id string) (modelLimits, bool) {
	family := modelFamily(id)
	limits, known := defaultModelLimits[family]

	for _, key := range []string{family, strings.ToLower(strings.TrimSpace(id))} {
		if n, ok := overrides[key]; ok {
			if n <= 0 {
				// Zero is how an operator says "stop claiming to know this",
				// which has to erase the built-in entry rather than fall back
				// to it.
				return modelLimits{}, false
			}
			limits.Context = n
			return limits, true
		}
	}

	return limits, known
}
