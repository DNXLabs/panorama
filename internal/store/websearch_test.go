// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/websearch"
)

const (
	gatewayA = "https://a.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp"
	gatewayB = "https://b.gateway.bedrock-agentcore.eu-west-1.amazonaws.com/mcp"
)

func TestWebSearchIsUnconfiguredUntilSaved(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		_, found, err := db.WebSearch(t.Context())
		if err != nil || found {
			t.Fatalf("found=%v err=%v, want nothing configured", found, err)
		}
	})
}

func TestChangingTheGatewayForgetsTheCheck(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		s := WebSearchSettings{Kind: websearch.KindAgentCore, Enabled: true, GatewayURL: gatewayA, ConfiguredBy: "u1"}
		if serr := db.SaveWebSearch(ctx, s); serr != nil {
			t.Fatal(serr)
		}
		if verr := db.SetWebSearchVerified(ctx, websearch.KindAgentCore, gatewayA, time.Now()); verr != nil {
			t.Fatal(verr)
		}

		// A save that keeps the gateway keeps the check: switching search off
		// and on again does not make it unverified.
		s.Enabled = false
		if err := db.SaveWebSearch(ctx, s); err != nil {
			t.Fatal(err)
		}
		got, _, err := db.WebSearch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.VerifiedAt == nil || got.Enabled {
			t.Fatalf("got %+v, want disabled and still verified", got)
		}

		s.GatewayURL = gatewayB
		if serr := db.SaveWebSearch(ctx, s); serr != nil {
			t.Fatal(serr)
		}
		// A check that raced the change reports on the old gateway, and must
		// not mark the new one verified.
		if verr := db.SetWebSearchVerified(ctx, websearch.KindAgentCore, gatewayA, time.Now()); verr != nil {
			t.Fatal(verr)
		}
		got, _, err = db.WebSearch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.VerifiedAt != nil || got.GatewayURL != gatewayB {
			t.Fatalf("got %+v, want the new gateway unverified", got)
		}
	})
}

func TestASearchIsPricedAtThePublishedRateUnlessOverridden(t *testing.T) {
	s := WebSearchSettings{Kind: websearch.KindAgentCore}
	if price, source := s.PricePerSearch(); price != websearch.PublishedPrice || source != "aws-published" {
		t.Errorf("default: %v %s", price, source)
	}
	override := pricing.Money(5_000_000)
	s.Price = &override
	if price, source := s.PricePerSearch(); price != override || source != "config" {
		t.Errorf("override: %v %s", price, source)
	}
}

func TestASearchIsInTheTotalsAndTheAuditTrail(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		u, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
		price := websearch.PublishedPrice
		records := []RequestRecord{
			{
				RequestID: "search", UserID: u.ID, StartedAt: at, Surface: "mcp",
				Provider: "bedrock-agentcore", RequestedModel: "web_search", ResolvedModel: "web-search",
				StatusCode: 200, Decision: DecisionAllowed, Attempts: 1, Tool: "claude-code",
				WebSearches: 1,
				Cost:        &pricing.Cost{WebSearch: price, Total: price}, CostSource: "aws-published",
			},
			{
				// A search nothing could price is billable and unpriced, like
				// a request whose tokens have no rate.
				RequestID: "unpriced-search", UserID: u.ID, StartedAt: at.Add(time.Second), Surface: "mcp",
				Provider: "bedrock-agentcore", RequestedModel: "web_search", ResolvedModel: "web-search",
				StatusCode: 200, Decision: DecisionAllowed, Attempts: 1, WebSearches: 1,
			},
		}
		for _, r := range records {
			if rerr := db.RecordRequest(ctx, r); rerr != nil {
				t.Fatal(rerr)
			}
		}

		sum, err := db.Summarize(ctx, RequestFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if sum.Totals.WebSearches != 2 || sum.Totals.Cost.WebSearch != price || sum.Totals.Cost.Total != price {
			t.Errorf("totals: %+v", sum.Totals)
		}
		if sum.Totals.UnpricedBillable != 1 {
			t.Errorf("UnpricedBillable = %d, want the unpriced search counted", sum.Totals.UnpricedBillable)
		}

		page, err := db.ListRequests(ctx, RequestFilter{})
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, r := range page.Requests {
			if r.RequestID != "search" {
				continue
			}
			found = true
			if r.WebSearches != 1 || r.Cost == nil || r.Cost.WebSearch != price || r.Cost.Total != price {
				t.Errorf("search read back as %+v, cost %+v", r.RequestRecord, r.Cost)
			}
		}
		if !found {
			t.Fatal("the search is not in the audit trail")
		}
	})
}

func TestADeployedGatewayIsOnAndKeepsOnlyTheSwitch(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		dep := websearch.Deployment{Kind: websearch.KindAgentCore, GatewayURL: gatewayA}

		got, found, err := db.EffectiveWebSearch(ctx, dep)
		if err != nil || !found || !got.Enabled || !got.Deployed || got.GatewayURL != gatewayA {
			t.Fatalf("nothing saved: %+v found=%v err=%v, want the deployment's gateway, on", got, found, err)
		}

		// A gateway and a price saved before the stack made its own are not
		// served; the switch is.
		price := pricing.Money(1)
		if serr := db.SaveWebSearch(ctx, WebSearchSettings{
			Kind: websearch.KindAgentCore, GatewayURL: gatewayB, Price: &price, ConfiguredBy: "u1",
		}); serr != nil {
			t.Fatal(serr)
		}
		if verr := db.SetWebSearchVerified(ctx, websearch.KindAgentCore, gatewayB, time.Now()); verr != nil {
			t.Fatal(verr)
		}
		got, _, err = db.EffectiveWebSearch(ctx, dep)
		if err != nil {
			t.Fatal(err)
		}
		if got.GatewayURL != gatewayA || got.Price != nil || got.Enabled || got.VerifiedAt != nil {
			t.Fatalf("got %+v, want the deployment's gateway at the published price, off, unverified", got)
		}

		if _, _, err := db.EffectiveWebSearch(ctx, websearch.Deployment{}); err != nil {
			t.Fatal(err)
		}
		row, _, _ := db.EffectiveWebSearch(ctx, websearch.Deployment{})
		if row.Deployed || row.GatewayURL != gatewayB {
			t.Errorf("without a deployment the row is the configuration: %+v", row)
		}
	})
}
