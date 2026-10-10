// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package tools

import "testing"

func TestFromUserAgent(t *testing.T) {
	tests := map[string]string{
		"claude-cli/2.1.232 (external)":      ClaudeCode,
		"claude-code/1.0":                    ClaudeCode,
		"opencode (panorama)":                OpenCode,
		"opencode/1.14.18":                   OpenCode,
		"codex-cli/0.4":                      CodexCLI,
		"Cline/3.2 (vscode)":                 Cline,
		"continue/0.9":                       Continue,
		"gemini-cli/1.0":                     GeminiCLI,
		"anthropic-sdk-python/0.40":          AnthropicSDK,
		"":                                   Unknown,
		"curl/8.4.0":                         Unknown,
		"Mozilla/5.0 (definitely a browser)": Unknown,
	}

	for ua, want := range tests {
		if got := FromUserAgent(ua); got != want {
			t.Errorf("FromUserAgent(%q) = %q, want %q", ua, got, want)
		}
	}
}

// Every value FromUserAgent can return has to be nameable as a route
// condition. A condition naming something the data plane never produces would
// match no request ever and look like a route that had quietly stopped working.
func TestEveryIdentifiedToolIsNameable(t *testing.T) {
	agents := []string{
		"claude-cli/2.1", "opencode (panorama)", "codex-cli/0.4", "Cline/3.2",
		"continue/0.9", "gemini-cli/1.0", "anthropic-sdk-python/0.40", "curl/8.4.0",
	}
	for _, ua := range agents {
		if !Valid(FromUserAgent(ua)) {
			t.Errorf("%q is identified as %q, which no condition can name", ua, FromUserAgent(ua))
		}
	}

	if Valid("copilot") {
		t.Error("a tool no request can be identified as is offered as a condition")
	}
	// Case is not a permission: a client that capitalises itself differently
	// tomorrow must not lose access to the route that names it.
	if !Valid("Claude-Code") {
		t.Error("Valid is case-sensitive")
	}
}

// opencode reaches Bedrock through the Vercel AI SDK, so a request from it can
// carry both names. The specific one is the useful one, and is what the
// generated configuration sets.
func TestOpenCodeBeatsTheSDKItIsBuiltOn(t *testing.T) {
	if got := FromUserAgent("opencode (panorama) anthropic-sdk-typescript/0.30"); got != OpenCode {
		t.Errorf("FromUserAgent = %q, want %q: a request that says both is opencode", got, OpenCode)
	}
}
