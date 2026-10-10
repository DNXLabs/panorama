// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package tools identifies the coding tool behind a request.
//
// It is one list in one place because two parts of the product depend on the
// same strings meaning the same thing. The data plane derives a tool from a
// user agent and records it on every request; the console offers those names as
// a route condition - "this route is for Claude Code" - and a condition naming
// a string the data plane never produces would silently match nothing.
//
// Matching is an allowlist. Anything unrecognised is recorded as Unknown rather
// than guessed at, so analytics never invent a tool that isn't there.
package tools

import "strings"

// The tools a request can be identified as.
const (
	ClaudeCode   = "claude-code"
	OpenCode     = "opencode"
	CodexCLI     = "codex-cli"
	Cline        = "cline"
	Continue     = "continue"
	GeminiCLI    = "gemini-cli"
	AnthropicSDK = "anthropic-sdk"
	// Unknown is a real value rather than an absence: a request whose client
	// nobody recognises still happened, still cost money, and a route condition
	// may legitimately name it.
	Unknown = "unknown"
)

// known is every value FromUserAgent can return, in the order a picker should
// offer them: the tools this gateway actually serves first.
var known = []string{ClaudeCode, OpenCode, CodexCLI, Cline, Continue, GeminiCLI, AnthropicSDK, Unknown}

// Known lists every tool a request can be identified as.
//
// It is the allowlist behind a route's tool condition. A condition can only
// name something on this list, because a condition naming anything else would
// match no request ever and look like a route that had simply stopped working.
func Known() []string {
	out := make([]string, len(known))
	copy(out, known)
	return out
}

// Valid reports whether name is a tool this product can identify.
func Valid(name string) bool {
	for _, k := range known {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// FromUserAgent identifies the coding tool behind a request.
//
// The user agent is the only signal available before attribution runs, and
// attribution runs on the body - so this is what every metric and log line
// carries as `tool`.
func FromUserAgent(ua string) string {
	lower := strings.ToLower(ua)

	switch {
	case strings.Contains(lower, "claude-cli"), strings.Contains(lower, "claude-code"):
		return ClaudeCode
	// opencode reaches Bedrock through the Vercel AI SDK, whose Anthropic
	// provider sends no distinctive user agent of its own. So the generated
	// configuration sets one per model - opencode's provider block takes a
	// headers map - and this matches what that sets.
	//
	// Matched before the SDK cases below on purpose: a request that says both
	// is opencode, and the more specific name is the useful one. Client-set,
	// like every user agent, which is why nothing but identification is built
	// on it; a route's tool condition has always been this trusting.
	case strings.Contains(lower, "opencode"):
		return OpenCode
	case strings.Contains(lower, "codex"):
		return CodexCLI
	case strings.Contains(lower, "cline"):
		return Cline
	case strings.Contains(lower, "continue"):
		return Continue
	case strings.Contains(lower, "gemini"):
		return GeminiCLI
	case strings.Contains(lower, "anthropic-sdk"), strings.Contains(lower, "anthropic-python"),
		strings.Contains(lower, "anthropic-typescript"):
		return AnthropicSDK
	default:
		return Unknown
	}
}
