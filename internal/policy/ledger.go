// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"sync"
	"time"
)

// Ledger tracks spend per budget window, in memory.
//
// It exists so that enforcing a budget costs a map lookup rather than a
// database round trip. Streaming is sacred; a SELECT on the request path would
// put the database's latency between a keystroke and the first token.
//
// Each window holds two numbers. The base is what the database said at the last
// reconcile, and the local delta is what this process has recorded since. Their
// sum is what a budget is compared against.
//
// The split is what makes more than one replica work at all. A single replica
// could simply count its own traffic; three replicas each counting their own
// would enforce three times the budget. Reconciling against shared storage
// gives every replica the whole picture, and the local delta keeps enforcement
// exact between reconciles rather than lagging a whole interval behind.
//
// The imprecision that remains is bounded and worth naming: a request whose
// database write straddles a reconcile query can be counted twice, or missed
// until the next reconcile. It is at most a few requests per interval, in a
// design where a request is priced only after it completes anyway.
type Ledger struct {
	mu      sync.RWMutex
	windows map[Key]*ledgerWindow
}

type ledgerWindow struct {
	// start identifies which window these numbers belong to. When the clock
	// moves past it, the counters are not decayed - they are dropped, because a
	// budget period starts at zero.
	start time.Time
	base  Spend
	local Spend
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{windows: make(map[Key]*ledgerWindow)}
}

// Spend reports what has been used in the window beginning at start.
//
// A window that has rolled over reads as zero rather than as the previous
// period's total: a budget resets, and a stale counter would block everyone on
// the first day of the month.
func (l *Ledger) Spend(k Key, start time.Time) Spend {
	if l == nil {
		return Spend{}
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	w, ok := l.windows[k]
	if !ok || !w.start.Equal(start) {
		return Spend{}
	}
	return w.base.add(w.local)
}

// Add records one completed request against a window.
func (l *Ledger) Add(k Key, start time.Time, s Spend) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	w := l.window(k, start)
	w.local = w.local.add(s)
}

// window returns the entry for a key, resetting it if the period has rolled.
// The caller holds the write lock.
func (l *Ledger) window(k Key, start time.Time) *ledgerWindow {
	w, ok := l.windows[k]
	if !ok {
		w = &ledgerWindow{start: start}
		l.windows[k] = w
		return w
	}
	if !w.start.Equal(start) {
		*w = ledgerWindow{start: start}
	}
	return w
}

// Mark captures a window's local delta before a reconcile query runs.
//
// Reconcile subtracts it afterwards, so requests recorded while the query was
// in flight survive rather than being erased by a result that predates them.
type Mark struct {
	start time.Time
	local Spend
}

// Mark captures the state a reconcile should be applied against. Call it
// immediately before querying storage.
func (l *Ledger) Mark(k Key, start time.Time) Mark {
	if l == nil {
		return Mark{start: start}
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	w, ok := l.windows[k]
	if !ok || !w.start.Equal(start) {
		return Mark{start: start}
	}
	return Mark{start: start, local: w.local}
}

// Reconcile installs an authoritative figure from storage.
//
// A reconcile for a window that has since rolled over is discarded: it answers
// a question about a period that has ended.
func (l *Ledger) Reconcile(k Key, m Mark, base Spend) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Checked before touching the entry, not after. Reaching for the window
	// first would reset a live counter to the stale mark's period and then
	// discard the reconcile - losing the current window's spend on every query
	// that happened to straddle midnight.
	w, ok := l.windows[k]
	if ok && !w.start.Equal(m.start) {
		return
	}
	if !ok {
		w = &ledgerWindow{start: m.start}
		l.windows[k] = w
	}

	w.base = base
	w.local = w.local.sub(m.local)
}

// Forget drops windows for keys that no longer have a budget, so a deployment
// that deletes budgets does not keep counting into memory forever.
func (l *Ledger) Forget(keep map[Key]struct{}) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	for k := range l.windows {
		if _, ok := keep[k]; !ok {
			delete(l.windows, k)
		}
	}
}

// Len reports how many windows are being tracked. For tests and diagnostics.
func (l *Ledger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.windows)
}
