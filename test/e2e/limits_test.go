// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dnxlabs/panorama/internal/fakebedrock"
)

// TestAHardLimitBlocks covers the ceiling: past it nothing runs until somebody
// raises it.
func TestAHardLimitBlocks(t *testing.T) {
	d := newDeployment(t)

	// Spend something first, so the limit set below is already behind us. A
	// budget is compared against spend as of the last completed request, which
	// is the stated consequence of never touching the database on the request
	// path.
	if status, body := d.send(claudeCodeUA, messagesBody(routeStandard, false, "spend a little")); status != http.StatusOK {
		t.Fatalf("the first request was not served: %d\n%s", status, body)
	}
	first := d.latest()
	if !first.Priced || first.Cost.TotalNanos == 0 {
		t.Fatal("the first request recorded no cost; nothing would ever cross a limit")
	}

	d.setBudget("month", "", "0.0001")

	status, body := d.send(claudeCodeUA, messagesBody(routeStandard, false, "spend some more"))

	// 403 and not 429 on purpose: a rate-limit status invites a client to retry
	// with backoff, and a monthly budget would have it retrying into a wall for
	// weeks.
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403\n%s", status, body)
	}

	message := errorMessage(t, body)
	requireContains(t, message, "budget", "the refusal")

	got := d.latest()
	if got.Decision != "blocked" {
		t.Errorf("decision = %q, want blocked", got.Decision)
	}
	if got.BudgetState == "" {
		t.Error("no budget state was recorded; the row cannot say which line was crossed")
	}

	// A blocked request never reached a provider, so it has no cost - which is
	// not the same absence as "unpriced", and is why it must never count
	// against the budget that blocked it.
	if got.Priced {
		t.Error("a blocked request was priced; it would count against the budget that blocked it")
	}
	if got.Tokens.Input != 0 || got.Tokens.Output != 0 {
		t.Errorf("a blocked request recorded tokens (%d in, %d out)",
			got.Tokens.Input, got.Tokens.Output)
	}

	// And the provider must not have been asked. The count is the honest check:
	// the record saying "blocked" is written by the same code path that would
	// be wrong.
	if invocations := len(d.Fake.Received()); invocations != 1 {
		t.Errorf("the provider was invoked %d times; the blocked request should not have reached it", invocations)
	}
}

// TestASoftLimitStopsOnlyTheRoutesMarkedToStop covers the middle state, which
// is the one people get wrong: a soft limit warns, and stops only the routes an
// administrator marked as not serving past it.
func TestASoftLimitStopsOnlyTheRoutesMarkedToStop(t *testing.T) {
	d := newDeployment(t)

	if status, body := d.send(claudeCodeUA, messagesBody(routeStandard, false, "spend a little")); status != http.StatusOK {
		t.Fatalf("the first request was not served: %d\n%s", status, body)
	}

	// A soft limit and no hard one: nothing is exhausted, but the expensive
	// route stops.
	d.setBudget("month", "0.0001", "")

	t.Run("the marked route stops", func(t *testing.T) {
		status, body := d.send(claudeCodeUA, messagesBody(routeThrifty, false, "the expensive one"))
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403\n%s", status, body)
		}

		message := errorMessage(t, body)
		requireContains(t, message, "soft limit", "the refusal")

		// The half that matters: the engineer is told where to go. A refusal
		// that named no alternative would leave them with nothing to do.
		requireContains(t, message, "Routes still served", "the refusal")
		requireContains(t, message, routeStandard, "the refusal")

		got := d.latest()
		if got.Decision != "blocked" {
			t.Errorf("decision = %q, want blocked", got.Decision)
		}
	})

	t.Run("every other route carries on", func(t *testing.T) {
		status, body := d.send(claudeCodeUA, messagesBody(routeStandard, false, "the ordinary one"))
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 - a soft limit must not stop a serving route\n%s", status, body)
		}

		got := d.latest()
		if got.BudgetState == "" {
			t.Error("a request served past a soft limit recorded no budget state")
		}
	})
}

// TestAThrottleReachesTheClientAsA429 exercises a real SDK throttle, end to end.
//
// A route is one model, so there is nowhere else to send a throttled request:
// it reaches the client as a 429, which every client this gateway serves backs
// off on and retries. The request was attempted - it reached a provider, so a
// budget sees it - and it is not priced, because nothing ran.
//
// Always rather than a single use: the AWS SDK retries a throttle by itself
// before the provider sees it, so a fault armed once is consumed by the first
// attempt and the retry succeeds.
func TestAThrottleReachesTheClientAsA429(t *testing.T) {
	d := newDeployment(t)
	d.Fake.Arm(fakebedrock.TargetSonnet5US, fakebedrock.FaultThrottling, fakebedrock.Always)

	status, body := d.send(claudeCodeUA, messagesBody(routeStandard, true, "throttle me"))
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429\n%s", status, body)
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Error.Type != "rate_limit_error" {
		t.Errorf("error = %s, want a rate_limit_error the client retries", body)
	}

	got := d.latest()
	if got.Decision != "error" || got.ErrorKind != "throttled" {
		t.Errorf("recorded %q / %q, want error / throttled", got.Decision, got.ErrorKind)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 - the request reached a provider once", got.Attempts)
	}
	if got.Priced {
		t.Error("a throttled request was priced")
	}
	// The SDK's own retries happen underneath the provider, and every one of
	// them went to the same model: nothing else was asked.
	for _, r := range d.Fake.Received() {
		if r.Model != fakebedrock.TargetSonnet5US {
			t.Errorf("a throttled route reached %q as well", r.Model)
		}
	}
}

// TestAnInvalidRequestIsReportedAsOne. A request the provider refuses as
// malformed is the client's to fix, and is said so.
func TestAnInvalidRequestIsReportedAsOne(t *testing.T) {
	d := newDeployment(t)

	d.Fake.Arm(fakebedrock.TargetSonnet5US, fakebedrock.FaultValidation, fakebedrock.Always)

	status, _ := d.send(claudeCodeUA, messagesBody(routeStandard, false, "malformed, allegedly"))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}

	got := d.latest()
	if got.ErrorKind != "invalid_request" {
		t.Errorf("error_kind = %q, want invalid_request", got.ErrorKind)
	}
	if n := len(d.Fake.Received()); n != 1 {
		t.Errorf("the provider was asked %d times, want once", n)
	}
}
