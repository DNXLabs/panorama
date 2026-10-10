// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"encoding/json"
	"testing"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

// TestProxiedRequestIsPriced is the end-to-end shape of M2: a request goes
// through the gateway and its cost lands in the audit trail, broken down by
// class.
func TestProxiedRequestIsPriced(t *testing.T) {
	// The usage shape a real coding-agent turn has: a handful of fresh input
	// tokens against tens of thousands read back out of the cache.
	body, err := json.Marshal(map[string]any{
		"usage": map[string]any{
			"input_tokens":                13,
			"output_tokens":               4,
			"cache_read_input_tokens":     75_069,
			"cache_creation_input_tokens": 174,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	h := newFixture(t, &fakeProvider{
		response: &providers.Response{
			Body: body,
			Usage: providers.Usage{
				InputTokens: 13, OutputTokens: 4,
				CacheReadTokens: 75_069, CacheWriteTokens: 174,
			},
		},
	})

	if rec := h.do(t, `{"model":"`+testModel+`","messages":[]}`); rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	h.drain()

	got := h.latest(t)
	if got.Cost == nil {
		t.Fatal("the request was recorded unpriced")
	}
	if got.CostSource != pricing.SourceSeed {
		t.Errorf("CostSource = %q, want it recorded", got.CostSource)
	}

	// 75,069 cache reads at $0.11 per million.
	if got.Cost.CacheRead.USD() != "0.008257590" {
		t.Errorf("cache read cost = %s, want 0.008257590", got.Cost.CacheRead.USD())
	}
	// 13 input tokens at $1.10 per million.
	if got.Cost.Input.USD() != "0.000014300" {
		t.Errorf("input cost = %s, want 0.000014300", got.Cost.Input.USD())
	}
	if sum := got.Cost.Input + got.Cost.Output + got.Cost.CacheRead + got.Cost.CacheWrite; sum != got.Cost.Total {
		t.Errorf("the classes sum to %s but the total is %s", sum, got.Cost.Total)
	}

	// The point of tracking classes separately: pricing those cached tokens as
	// fresh input would report ten times the cost.
	asInput, err := pricing.Rate(1_100_000_000).Cost(75_069)
	if err != nil {
		t.Fatal(err)
	}
	if asInput <= got.Cost.Total*5 {
		t.Errorf("expected a large gap between cache-read and input pricing: %s vs %s", got.Cost.Total, asInput)
	}
}

// TestStreamedRequestIsPricedWithTheCacheTTLSplit covers the path that carries
// nearly all real traffic, and the per-TTL split that only the streaming
// message_start event reports.
func TestStreamedRequestIsPricedWithTheCacheTTLSplit(t *testing.T) {
	h := newFixture(t, &fakeProvider{
		events: [][]byte{
			[]byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,` +
				`"cache_creation_input_tokens":1000,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":600,"ephemeral_1h_input_tokens":400}}}}`),
			[]byte(`{"type":"message_delta","usage":{"output_tokens":20}}`),
		},
		usage: providers.Usage{
			InputTokens: 10, OutputTokens: 20,
			CacheWriteTokens: 1000, CacheWrite1hTokens: 400,
		},
	})

	if rec := h.do(t, `{"model":"`+testModel+`","stream":true,"messages":[]}`); rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	h.drain()

	got := h.latest(t)
	if got.CacheWrite1hTokens != 400 {
		t.Errorf("CacheWrite1hTokens = %d, want 400 - the TTL split must survive to the record",
			got.CacheWrite1hTokens)
	}
	if got.Cost == nil {
		t.Fatal("the streamed request was recorded unpriced")
	}

	// 600 at $1.375/M plus 400 at $2.20/M.
	if got.Cost.CacheWrite.USD() != "0.001705000" {
		t.Errorf("cache write cost = %s, want 0.001705000", got.Cost.CacheWrite.USD())
	}

	// Charging all 1000 at the five-minute rate would understate it, which is
	// what a single cache-write rate would do.
	flat, err := pricing.Rate(1_375_000_000).Cost(1000)
	if err != nil {
		t.Fatal(err)
	}
	if flat >= got.Cost.CacheWrite {
		t.Errorf("the TTL split should cost more than a flat rate: %s vs %s", got.Cost.CacheWrite, flat)
	}
}

// TestUnpricedModelRecordsNullNotZero is the honesty rule on the request path.
func TestUnpricedModelRecordsNullNotZero(t *testing.T) {
	// A table that prices something else entirely.
	empty := pricing.NewTable([]pricing.Entry{{
		Key:      pricing.Key{Provider: "bedrock", Model: "anthropic.something-else", Scope: pricing.ScopeRegional},
		Rates:    pricing.Rates{Input: 1, Output: 2},
		Currency: pricing.CurrencyUSD,
		Source:   pricing.SourceSeed,
	}})

	h := newPricedFixture(t, &fakeProvider{
		response: &providers.Response{
			Body:  []byte(`{}`),
			Usage: providers.Usage{InputTokens: 100, OutputTokens: 50},
		},
	}, empty)

	if rec := h.do(t, `{"model":"`+testModel+`","messages":[]}`); rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	h.drain()

	got := h.latest(t)
	if got.Cost != nil {
		t.Errorf("an unpriced model recorded a cost of %v; it must be null so the gap stays visible", got.Cost)
	}
	if got.CostSource != "" {
		t.Errorf("CostSource = %q, want empty for an unpriced request", got.CostSource)
	}
	// The tokens are still recorded, so the cost can be backfilled once a rate
	// exists.
	if got.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want the usage recorded even when unpriced", got.InputTokens)
	}
}

// TestRefusedRequestHasNoCostRatherThanAnUnknownOne separates the two kinds of
// zero: a request that never reached a model has no cost, which is not the same
// as a cost nobody could compute.
func TestRefusedRequestHasNoCostRatherThanAnUnknownOne(t *testing.T) {
	h := newFixture(t, &fakeProvider{})

	rec := h.do(t, `{"model":"some-other-model","messages":[]}`)
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	h.drain()

	got := h.latest(t)
	if got.Cost != nil {
		t.Errorf("a refused request recorded a cost of %v", got.Cost)
	}
	if got.InputTokens != 0 {
		t.Errorf("a refused request recorded %d input tokens", got.InputTokens)
	}
}

// TestPricingDoesNotRunOnTheRequestPath keeps the latency promise: costing
// happens after the response has been delivered, never between the model and
// the client.
func TestPricingDoesNotRunOnTheRequestPath(t *testing.T) {
	h := newFixture(t, &fakeProvider{
		response: &providers.Response{
			Body:  []byte(`{}`),
			Usage: providers.Usage{InputTokens: 10, OutputTokens: 5},
		},
	})

	if rec := h.do(t, `{"model":"`+testModel+`","messages":[]}`); rec.Code != 200 {
		t.Fatal("request failed")
	}

	// Before draining, the response is already complete but the audit row need
	// not exist yet - which is the proof that costing is off the hot path.
	h.drain()

	if got := h.latest(t); got.Cost == nil {
		t.Error("the cost never landed after draining")
	}
}
