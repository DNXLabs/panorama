// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/dnxlabs/panorama/internal/fakebedrock"
)

const claudeCodeUA = "claude-cli/2.0.1 (external, cli)"

// TestARequestIsServedAndRecorded is the spine: a request goes in through the
// real binary, out through the real Bedrock provider to the fake, and lands in
// the audit trail with the metadata this product exists to produce.
func TestARequestIsServedAndRecorded(t *testing.T) {
	d := newDeployment(t)

	status, body := d.send(claudeCodeUA, messagesBody(routeStandard, false, "hello"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}

	got := d.latest()

	if got.Decision != "allowed" {
		t.Errorf("decision = %q, want allowed", got.Decision)
	}
	if got.StatusCode != http.StatusOK {
		t.Errorf("recorded status = %d, want 200", got.StatusCode)
	}
	if got.Surface != "anthropic" {
		t.Errorf("surface = %q, want anthropic", got.Surface)
	}
	if got.Provider != "bedrock" {
		t.Errorf("provider = %q, want bedrock", got.Provider)
	}
	if got.UserEmail != adminEmail {
		t.Errorf("user = %q, want %q", got.UserEmail, adminEmail)
	}

	// The client asks for a route name; the record keeps both that and what it
	// became. Conflating them would lose the only evidence of what was invoked.
	if got.RequestedModel != routeStandard {
		t.Errorf("requested_model = %q, want the route name %q", got.RequestedModel, routeStandard)
	}
	if got.ResolvedModel != fakebedrock.TargetSonnet5US {
		t.Errorf("resolved_model = %q, want %q", got.ResolvedModel, fakebedrock.TargetSonnet5US)
	}
}

// TestTheToolIsIdentified is the test that catches a dropped user-agent in the
// generated opencode block, and a Claude Code that renames itself.
func TestTheToolIsIdentified(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
		want      string
	}{
		{"claude code", claudeCodeUA, "claude-code"},
		// What the generated provider block sets, because the Vercel AI SDK
		// underneath opencode sends nothing distinctive of its own.
		{"opencode", "opencode (panorama)", "opencode"},
		{"an unknown client", "curl/8.4.0", "unknown"},
	}

	d := newDeployment(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := d.send(tc.userAgent, messagesBody(routeFast, false, "hello "+tc.name))
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200\n%s", status, body)
			}
			if got := d.latest().Tool; got != tc.want {
				t.Errorf("tool = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAllFourTokenClassesAreRecorded covers the cost model's headline
// commitment. Coding-agent economics live in prompt caching, so a total that
// omitted a cache class would understate spend several-fold.
func TestAllFourTokenClassesAreRecorded(t *testing.T) {
	d := newDeployment(t)

	status, body := d.send(claudeCodeUA, messagesBody(routeStandard, true, "count my tokens"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}

	got := d.latest()
	d.requireServed(t, got)
	want := fakebedrock.DefaultUsage

	if got.Tokens.Input != want.InputTokens {
		t.Errorf("input = %d, want %d", got.Tokens.Input, want.InputTokens)
	}
	if got.Tokens.Output != want.OutputTokens {
		t.Errorf("output = %d, want %d", got.Tokens.Output, want.OutputTokens)
	}
	if got.Tokens.CacheRead != want.CacheReadTokens {
		t.Errorf("cache read = %d, want %d", got.Tokens.CacheRead, want.CacheReadTokens)
	}
	// The flat total covers both TTLs, and the 1h figure is carried separately
	// because Bedrock charges them differently.
	if got.Tokens.CacheWrite != want.Total() {
		t.Errorf("cache write = %d, want %d", got.Tokens.CacheWrite, want.Total())
	}
	if got.Tokens.CacheWrite1h != want.CacheWrite1hTokens {
		t.Errorf("cache write 1h = %d, want %d", got.Tokens.CacheWrite1h, want.CacheWrite1hTokens)
	}

	if !got.Streaming {
		t.Error("the request is not recorded as streaming")
	}
	if got.TTFTMillis <= 0 {
		t.Error("no time to first token was recorded for a streamed request")
	}
}

// TestAnUnroutedModelIsRefused holds the allowlist. A client naming a model id
// is naming something nobody routed.
func TestAnUnroutedModelIsRefused(t *testing.T) {
	d := newDeployment(t)

	status, body := d.send(claudeCodeUA,
		messagesBody(fakebedrock.TargetSonnet5US, false, "let me pick my own model"))
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403\n%s", status, body)
	}

	message := errorMessage(t, body)
	requireContains(t, message, routeStandard, "the refusal")
	requireContains(t, message, "ANTHROPIC_MODEL", "the refusal")

	got := d.latest()
	if got.Decision != "refused" {
		t.Errorf("decision = %q, want refused", got.Decision)
	}
}

// TestNoRequestContentIsStored is the non-negotiable, checked against the
// artifact rather than against the code that writes it.
func TestNoRequestContentIsStored(t *testing.T) {
	d := newDeployment(t)

	const sentinel = "zorblattifying-quaxlepticon"
	status, _ := d.send(claudeCodeUA, messagesBody(routeStandard, false, sentinel))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	d.waitForRequests(1)

	if found := d.databaseContains(sentinel); found {
		t.Error("the prompt was found in the database; bodies are never stored")
	}
}
