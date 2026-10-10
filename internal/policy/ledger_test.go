// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import "testing"

func TestLedgerAccumulates(t *testing.T) {
	l := NewLedger()
	k := Key{Scope: ScopeUser, ScopeID: "usr_1", Period: PeriodDay}
	start := at(t, "2026-08-14T00:00:00Z")

	l.Add(k, start, Spend{Cost: 1_000, Requests: 1})
	l.Add(k, start, Spend{Cost: 2_500, Requests: 1})
	l.Add(k, start, Spend{Requests: 1, Unpriced: 1})

	got := l.Spend(k, start)
	if got.Cost != 3_500 {
		t.Errorf("cost = %d, want 3500", got.Cost)
	}
	if got.Requests != 3 {
		t.Errorf("requests = %d, want 3", got.Requests)
	}
	if got.Unpriced != 1 {
		t.Errorf("unpriced = %d, want 1", got.Unpriced)
	}
}

// TestLedgerResetsOnANewWindow is the first-of-the-month case: a budget starts
// at zero, and a counter that carried over would block everyone.
func TestLedgerResetsOnANewWindow(t *testing.T) {
	l := NewLedger()
	k := Key{Scope: ScopeOrg, Period: PeriodDay}

	august := at(t, "2026-08-14T00:00:00Z")
	l.Add(k, august, Spend{Cost: 9_000_000_000, Requests: 500})

	tomorrow := at(t, "2026-08-15T00:00:00Z")
	if got := l.Spend(k, tomorrow); got != (Spend{}) {
		t.Errorf("a new window reads %+v, want zero", got)
	}

	l.Add(k, tomorrow, Spend{Cost: 5, Requests: 1})
	if got := l.Spend(k, tomorrow).Cost; got != 5 {
		t.Errorf("after the roll, cost = %d, want 5", got)
	}
}

// TestReconcileKeepsWhatArrivedDuringTheQuery is the race the Mark/Reconcile
// pair exists for. A reconcile answers a question asked a moment ago; requests
// recorded since then must survive it.
func TestReconcileKeepsWhatArrivedDuringTheQuery(t *testing.T) {
	l := NewLedger()
	k := Key{Scope: ScopeOrg, Period: PeriodMonth}
	start := at(t, "2026-08-01T00:00:00Z")

	// Two requests this process has already counted, and which the database
	// therefore also knows about.
	l.Add(k, start, Spend{Cost: 100, Requests: 1})
	l.Add(k, start, Spend{Cost: 200, Requests: 1})

	mark := l.Mark(k, start)

	// A third arrives while the reconcile query is in flight.
	l.Add(k, start, Spend{Cost: 400, Requests: 1})

	// The database reports the first two.
	l.Reconcile(k, mark, Spend{Cost: 300, Requests: 2})

	got := l.Spend(k, start)
	if got.Cost != 700 {
		t.Errorf("cost = %d, want 700 - the in-flight request was lost or double counted", got.Cost)
	}
	if got.Requests != 3 {
		t.Errorf("requests = %d, want 3", got.Requests)
	}
}

// TestReconcileBringsInOtherReplicasSpend: the whole reason the base exists.
func TestReconcileBringsInOtherReplicasSpend(t *testing.T) {
	l := NewLedger()
	k := Key{Scope: ScopeTeam, ScopeID: "tm_1", Period: PeriodWeek}
	start := at(t, "2026-08-10T00:00:00Z")

	l.Add(k, start, Spend{Cost: 50, Requests: 1})

	mark := l.Mark(k, start)
	// The database sees this replica's one request and two hundred from others.
	l.Reconcile(k, mark, Spend{Cost: 20_050, Requests: 201})

	if got := l.Spend(k, start).Cost; got != 20_050 {
		t.Errorf("cost = %d, want 20050 - other replicas' spend was not picked up", got)
	}
}

// TestReconcileForAnEndedWindowIsDiscarded: a query that straddles midnight
// answers a question about yesterday.
func TestReconcileForAnEndedWindowIsDiscarded(t *testing.T) {
	l := NewLedger()
	k := Key{Scope: ScopeOrg, Period: PeriodDay}

	yesterday := at(t, "2026-08-13T00:00:00Z")
	today := at(t, "2026-08-14T00:00:00Z")

	mark := l.Mark(k, yesterday)
	l.Add(k, today, Spend{Cost: 7, Requests: 1})
	l.Reconcile(k, mark, Spend{Cost: 999_999, Requests: 5_000})

	if got := l.Spend(k, today).Cost; got != 7 {
		t.Errorf("today reads %d, want 7 - yesterday's total leaked into it", got)
	}
}

func TestLedgerForgetsDeletedBudgets(t *testing.T) {
	l := NewLedger()
	start := at(t, "2026-08-14T00:00:00Z")

	kept := Key{Scope: ScopeOrg, Period: PeriodDay}
	gone := Key{Scope: ScopeUser, ScopeID: "usr_1", Period: PeriodDay}

	l.Add(kept, start, Spend{Cost: 1, Requests: 1})
	l.Add(gone, start, Spend{Cost: 1, Requests: 1})
	if l.Len() != 2 {
		t.Fatalf("tracking %d windows, want 2", l.Len())
	}

	l.Forget(map[Key]struct{}{kept: {}})

	if l.Len() != 1 {
		t.Errorf("tracking %d windows after Forget, want 1", l.Len())
	}
	if got := l.Spend(gone, start); got != (Spend{}) {
		t.Errorf("a forgotten budget still reads %+v", got)
	}
}

// A nil ledger is usable, so no caller needs a nil check on the request path.
func TestNilLedgerIsSafe(t *testing.T) {
	var l *Ledger
	k := Key{Scope: ScopeOrg, Period: PeriodDay}
	start := at(t, "2026-08-14T00:00:00Z")

	l.Add(k, start, Spend{Cost: 1})
	l.Reconcile(k, l.Mark(k, start), Spend{Cost: 1})
	l.Forget(nil)

	if got := l.Spend(k, start); got != (Spend{}) {
		t.Errorf("a nil ledger reported %+v", got)
	}
	if l.Len() != 0 {
		t.Errorf("a nil ledger reported %d windows", l.Len())
	}
}

func TestSpendSubFloorsAtZero(t *testing.T) {
	// A reconcile that races a window roll could otherwise drive a counter
	// negative, and a negative spend reads as credit.
	got := Spend{Cost: 5, Requests: 1}.sub(Spend{Cost: 10, Requests: 4, Unpriced: 2})
	if got.Cost != 0 || got.Requests != 0 || got.Unpriced != 0 {
		t.Errorf("sub produced %+v, want zeroes", got)
	}
}
