// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// createLine is the two calls every test below opens with: a line, and a rule
// that fills it.
func createLine(t *testing.T, f *consoleFixture, code, pattern string) string {
	t.Helper()

	rec, body := f.send(t, http.MethodPost, "/api/v1/cost-centers", f.adminToken,
		map[string]any{"code": code, "name": code + " line", "plan": "1000.00"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create cost center = %d %s", rec.Code, rec.Body.String())
	}
	id, _ := body["id"].(string)

	rec, _ = f.send(t, http.MethodPost, "/api/v1/cost-centers/rules", f.adminToken,
		map[string]any{"cost_center_id": id, "match_on": "repo", "pattern": pattern})
	if rec.Code != http.StatusOK {
		t.Fatalf("create rule = %d %s", rec.Code, rec.Body.String())
	}
	return id
}

// TestADeveloperIsServedNoChartOfAccounts. The sidebar hiding a screen is not
// what enforces this - a cost center is somebody else's line by definition, and
// a developer's own numbers live on the personal screens.
func TestADeveloperIsServedNoChartOfAccounts(t *testing.T) {
	f := newConsole(t)

	for _, path := range []string{"/api/v1/cost-centers", "/api/v1/allocation"} {
		rec, _ := f.send(t, http.MethodGet, path, f.devToken, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as a developer = %d, want 403", path, rec.Code)
		}
	}
}

// TestOnlyAnAdministratorWritesTheChartOfAccounts. Reading is a leader's;
// writing is not.
func TestOnlyAnAdministratorWritesTheChartOfAccounts(t *testing.T) {
	f := newConsole(t)

	rec, _ := f.send(t, http.MethodPost, "/api/v1/cost-centers", f.devToken,
		map[string]any{"code": "CC-100", "name": "Payments"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a developer creating a cost center = %d, want 403", rec.Code)
	}

	rec, _ = f.send(t, http.MethodPost, "/api/v1/cost-centers/reallocate", f.devToken, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a developer restating every allocation = %d, want 403", rec.Code)
	}
}

// TestTheGapIsPartOfTheAnswer. Unallocated spend is a row of the same response
// as the totals, never a second call somebody could forget to make.
func TestTheGapIsPartOfTheAnswer(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	createLine(t, f, "CC-100", "acme/payments-*")

	placed := f.attribute(t, "fp-1", f.developer.ID, "feature/PAY-1", "PAY-1", now)
	f.recordIn(t, f.developer.ID, "allocated", placed.ID, 3_000_000, now)
	f.recordIn(t, f.developer.ID, "orphan", "", 7_000_000, now)

	rec, body := f.send(t, http.MethodGet, "/api/v1/allocation", f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("allocation = %d %s", rec.Code, rec.Body.String())
	}

	totals, ok := body["totals"].(map[string]any)
	if !ok {
		t.Fatalf("no totals in %v", body)
	}
	total, _ := totals["cost_nanos"].(float64)
	allocated, _ := totals["allocated_nanos"].(float64)
	unallocated, _ := totals["unallocated_nanos"].(float64)

	// The property the whole feature rests on: the parts are the whole.
	if allocated+unallocated != total {
		t.Errorf("allocated %v + unallocated %v != total %v", allocated, unallocated, total)
	}
	if unallocated == 0 {
		t.Error("spend no rule could reach was reported as fully allocated")
	}
	if _, present := body["unallocated"]; !present {
		t.Error("the gap has no breakdown; it has to be a worklist, not a figure")
	}
	// Said on the wire so no client has to assume it.
	if enforced, _ := body["enforced"].(bool); enforced {
		t.Error("the API claims allocation is enforced; it warns and stops nobody")
	}
}

// TestACostCenterFilterOnlyEverNarrows. filterFrom reads it before it pins the
// scope, so no combination of filters is a way to see somebody else's traffic.
func TestACostCenterFilterOnlyEverNarrows(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	id := createLine(t, f, "CC-100", "acme/payments-*")

	other, _ := f.newUser(t, "colleague@example.com", "developer")
	session := f.attribute(t, "fp-2", other.ID, "feature/PAY-9", "PAY-9", now)
	f.recordIn(t, other.ID, "theirs", session.ID, 5_000_000, now)

	rec, body := f.send(t, http.MethodGet,
		"/api/v1/requests?cost_center="+id, f.devToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered requests = %d %s", rec.Code, rec.Body.String())
	}

	rows, _ := body["requests"].([]any)
	if len(rows) != 0 {
		t.Errorf("a developer filtering by cost center saw %d of a colleague's rows", len(rows))
	}
}

// TestTheTesterAnswersBeforeTheReportDoes. The price of an ordered rule list,
// paid rather than argued away.
func TestTheTesterAnswersBeforeTheReportDoes(t *testing.T) {
	f := newConsole(t)
	id := createLine(t, f, "CC-100", "acme/payments-*")

	rec, body := f.send(t, http.MethodPost, "/api/v1/cost-centers/test", f.adminToken,
		map[string]any{"kind": "branch", "repo": "acme/payments-api"})
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	if allocated, _ := body["allocated"].(bool); !allocated {
		t.Fatalf("the tester found no rule for a repository one claims: %v", body)
	}
	if got, _ := body["cost_center_id"].(string); got != id {
		t.Errorf("tester resolved to %q, want %q", got, id)
	}

	// The other half, and the one that makes the worklist honest: no match is
	// an answer, not an error.
	rec, body = f.send(t, http.MethodPost, "/api/v1/cost-centers/test", f.adminToken,
		map[string]any{"kind": "branch", "repo": "acme/website"})
	if rec.Code != http.StatusOK {
		t.Fatalf("test of an unmatched repository = %d", rec.Code)
	}
	if allocated, _ := body["allocated"].(bool); allocated {
		t.Error("an unmatched repository was reported as allocated")
	}
}

// TestALineWithSpendIsArchivedRatherThanDeleted.
func TestALineWithSpendIsArchivedRatherThanDeleted(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	id := createLine(t, f, "CC-100", "*")
	session := f.attribute(t, "fp-1", f.developer.ID, "feature/PAY-1", "PAY-1", now)
	f.recordIn(t, f.developer.ID, "allocated", session.ID, 3_000_000, now)

	rec, _ := f.send(t, http.MethodDelete, "/api/v1/cost-centers/"+id, f.adminToken, nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("deleting a line with spend = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Archive") {
		t.Error("the refusal does not name the way out")
	}
}

// TestTheAllocationExportCarriesTheUnallocatedRow. The same commitment the work
// export makes, in the file that actually leaves the product.
func TestTheAllocationExportCarriesTheUnallocatedRow(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	createLine(t, f, "CC-100", "acme/payments-*")
	f.recordIn(t, f.developer.ID, "orphan", "", 7_000_000, now)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/export/allocation.csv", nil)
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	rec := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "(unallocated)") {
		t.Errorf("the allocation export dropped the unallocated row:\n%s", body)
	}
	// Keyed on the finance system's own string, because the join happens in
	// somebody else's ledger.
	if !strings.Contains(body, "code,name,owner") {
		t.Errorf("the export is not keyed on the code:\n%s", body)
	}
}
