// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"testing"

	"github.com/dnxlabs/panorama/internal/costcenters"
)

func stampEngine(t *testing.T) *Engine {
	t.Helper()
	return newEngine(t, &fakeRepo{set: Set{
		Routes:      []Route{plain("work", "us.anthropic.claude-sonnet-5")},
		CostCenters: costcenters.Stampable{"cc-100": "cct-payments"},
	}}, "2026-09-05T10:00:00Z")
}

func TestAStampResolvesToItsLine(t *testing.T) {
	d := stampEngine(t).Evaluate(Input{
		UserID: "u1", Route: "work", CostCenterStamp: "CC-100",
	})

	if d.CostCenterID != "cct-payments" {
		t.Errorf("cost center = %q, want cct-payments", d.CostCenterID)
	}
	if d.CostCenterStamp != "" {
		t.Errorf("a stamp that resolved was also recorded raw: %q", d.CostCenterStamp)
	}
	if d.Refused || d.Blocked {
		t.Error("a stamp changed whether the request was served")
	}
}

func TestAStampThatNamesNothingIsKeptRatherThanDropped(t *testing.T) {
	// Both halves matter. The request proceeds - breaking a coding session over
	// an accounting label would be absurd - and the string survives, because
	// from the client's side a bad stamp looks exactly like success while the
	// money goes somewhere else.
	d := stampEngine(t).Evaluate(Input{
		UserID: "u1", Route: "work", CostCenterStamp: "CC-999",
	})

	if d.CostCenterID != "" {
		t.Errorf("an unknown code resolved to %q", d.CostCenterID)
	}
	if d.CostCenterStamp != "CC-999" {
		t.Errorf("raw stamp = %q, want CC-999 kept for the worklist", d.CostCenterStamp)
	}
	if d.Refused || d.Blocked {
		t.Error("an unknown cost center stopped the request")
	}
}

func TestARefusedRequestStillRecordsWhatItWasStampedWith(t *testing.T) {
	// A deployment whose stamps are all misspelt should be visible from the
	// audit trail, not only from the requests that happened to succeed.
	d := stampEngine(t).Evaluate(Input{
		UserID: "u1", Route: "no-such-route", CostCenterStamp: "CC-999",
	})

	if !d.Refused {
		t.Fatal("an unknown route was not refused")
	}
	if d.CostCenterStamp != "CC-999" {
		t.Errorf("raw stamp = %q on a refused request, want CC-999", d.CostCenterStamp)
	}
}

func TestNoStampIsNotAnEmptyStamp(t *testing.T) {
	d := stampEngine(t).Evaluate(Input{UserID: "u1", Route: "work"})
	if d.CostCenterID != "" || d.CostCenterStamp != "" {
		t.Errorf("unstamped request carried %q / %q", d.CostCenterID, d.CostCenterStamp)
	}
}
