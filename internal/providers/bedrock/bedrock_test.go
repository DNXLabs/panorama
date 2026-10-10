// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/providers"
)

func TestPrepareBody(t *testing.T) {
	const clientBody = `{
		"model": "claude-sonnet-4-5",
		"stream": true,
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "hello"}],
		"system": "be terse"
	}`

	out, err := prepareBody(&providers.Request{Body: []byte(clientBody)}, []string{"context-management-2025-06-27"})
	if err != nil {
		t.Fatalf("prepareBody: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not a JSON object: %v", err)
	}

	// Bedrock takes the model in the URL and rejects it in the body; streaming
	// is chosen by the operation.
	if _, present := got["model"]; present {
		t.Error("model survived into the body; Bedrock rejects it there")
	}
	if _, present := got["stream"]; present {
		t.Error("stream survived into the body; the operation selects streaming")
	}

	if string(got["anthropic_version"]) != `"`+anthropicVersion+`"` {
		t.Errorf("anthropic_version = %s, want %q", got["anthropic_version"], anthropicVersion)
	}
	if string(got["anthropic_beta"]) != `["context-management-2025-06-27"]` {
		t.Errorf("anthropic_beta = %s; the header has to become a body field or Bedrock "+
			"rejects the parameters it enables", got["anthropic_beta"])
	}

	// Everything the gateway has no business touching must survive untouched.
	if string(got["max_tokens"]) != "1024" {
		t.Errorf("max_tokens = %s, want 1024", got["max_tokens"])
	}
	if !strings.Contains(string(got["messages"]), "hello") {
		t.Errorf("messages were altered: %s", got["messages"])
	}
	if string(got["system"]) != `"be terse"` {
		t.Errorf("system = %s, want it preserved", got["system"])
	}
}

func TestPrepareBodyLeavesAnExplicitBetaListAlone(t *testing.T) {
	body := `{"model":"m","anthropic_beta":["client-chose-this"],"messages":[]}`

	out, err := prepareBody(&providers.Request{Body: []byte(body)}, []string{"header-supplied"})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["anthropic_beta"]) != `["client-chose-this"]` {
		t.Errorf("anthropic_beta = %s, want the client's explicit list to win", got["anthropic_beta"])
	}
}

func TestPrepareBodyKeepsAnExplicitAnthropicVersion(t *testing.T) {
	body := `{"model":"m","anthropic_version":"bedrock-2099-01-01","messages":[]}`

	out, err := prepareBody(&providers.Request{Body: []byte(body)}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["anthropic_version"]) != `"bedrock-2099-01-01"` {
		t.Errorf("anthropic_version = %s, want the client's value preserved", got["anthropic_version"])
	}
}

func TestPrepareBodyRejectsNonObjectBodies(t *testing.T) {
	for _, body := range []string{`[1,2,3]`, `"a string"`, `not json`, ``} {
		if _, err := prepareBody(&providers.Request{Body: []byte(body)}, nil); err == nil {
			t.Errorf("prepareBody(%q) succeeded; want an invalid-request error", body)
		} else if pe, ok := providers.AsError(err); !ok || pe.Kind != providers.ErrKindInvalidRequest {
			t.Errorf("prepareBody(%q) error = %v, want an invalid_request provider error", body, err)
		}
	}
}

// TestFilterBetas covers the translation that makes Claude Code work at all:
// Bedrock fails the entire request on one flag it does not recognise, so every
// flag the client sends has to be checked against the list before it goes.
func TestFilterBetas(t *testing.T) {
	// The flags a current Claude Code actually sends, captured from a live
	// request's headers.
	claudeCodeBetas := []string{
		"claude-code-20250219",
		"interleaved-thinking-2025-05-14",
		"redact-thinking-2026-02-12",
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"mid-conversation-system-2026-04-07",
		"advisor-tool-2026-03-01",
		"effort-2025-11-24",
		"afk-mode-2026-01-31",
	}

	p := &Provider{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		unsupported: map[string]struct{}{},
	}
	for _, b := range DefaultUnsupportedBetas {
		p.unsupported[b] = struct{}{}
	}

	got := p.filterBetas(t.Context(), claudeCodeBetas)

	// Count what should have gone rather than assuming the drop list and this
	// capture overlap entirely: the list also covers flags older clients send
	// that a current Claude Code does not.
	var expectDropped int
	for _, dropped := range DefaultUnsupportedBetas {
		if contains(claudeCodeBetas, dropped) {
			expectDropped++
		}
		for _, kept := range got {
			if kept == dropped {
				t.Errorf("%q survived the filter; Bedrock rejects the whole request on it", dropped)
			}
		}
	}
	if want := len(claudeCodeBetas) - expectDropped; len(got) != want {
		t.Errorf("kept %d flags, want %d", len(got), want)
	}
	// Order matters for nothing functionally, but dropping extras would.
	for _, want := range []string{"claude-code-20250219", "mid-conversation-system-2026-04-07", "effort-2025-11-24"} {
		if !contains(got, want) {
			t.Errorf("%q was dropped but Bedrock accepts it", want)
		}
	}
}

func TestFilterBetasCanBeDisabled(t *testing.T) {
	// An empty list forwards everything, which is how an operator finds out
	// what Bedrock has started supporting.
	p := &Provider{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		unsupported: map[string]struct{}{},
	}

	in := []string{"advisor-tool-2026-03-01", "anything-else"}
	if got := p.filterBetas(t.Context(), in); len(got) != len(in) {
		t.Errorf("filtered %v down to %v with an empty unsupported set", in, got)
	}
}

func TestNewRequiresARegion(t *testing.T) {
	if _, err := New(t.Context(), Config{}); err == nil {
		t.Fatal("New with no region must fail")
	}
}

func TestUsageFromResponseReadsAllFourClasses(t *testing.T) {
	// The shape Bedrock actually returns, including the per-TTL cache_creation
	// breakdown that M2's pricing will need.
	body := []byte(`{"usage":{"input_tokens":13,"output_tokens":4,
		"cache_read_input_tokens":75069,"cache_creation_input_tokens":174,
		"cache_creation":{"ephemeral_5m_input_tokens":174,"ephemeral_1h_input_tokens":0}}}`)

	got := usageFromResponse(body)

	want := providers.Usage{InputTokens: 13, OutputTokens: 4, CacheReadTokens: 75069, CacheWriteTokens: 174}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageFromResponseToleratesAMissingUsage(t *testing.T) {
	for _, body := range []string{`{}`, `{"usage":null}`, `garbage`} {
		if got := usageFromResponse([]byte(body)); got != (providers.Usage{}) {
			t.Errorf("usageFromResponse(%q) = %+v, want the zero value", body, got)
		}
	}
}

func TestEventStreamObservesUsageAcrossEvents(t *testing.T) {
	s := &eventStream{}

	// Input accounting arrives once, up front.
	s.observe([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":2,` +
		`"cache_read_input_tokens":75069,"cache_creation_input_tokens":174}}}`))
	// Output accounting is restated as generation proceeds; the last wins.
	s.observe([]byte(`{"type":"message_delta","usage":{"output_tokens":3}}`))
	s.observe([]byte(`{"type":"message_delta","usage":{"output_tokens":9}}`))
	// Events that carry no accounting must not disturb it.
	s.observe([]byte(`{"type":"content_block_delta","delta":{"text":"hi"}}`))
	s.observe([]byte(`not json`))

	want := providers.Usage{InputTokens: 2, OutputTokens: 9, CacheReadTokens: 75069, CacheWriteTokens: 174}
	if got := s.Usage(); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
