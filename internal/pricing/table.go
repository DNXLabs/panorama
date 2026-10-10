// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"sort"
	"sync/atomic"
)

// Table is the in-memory price table the request path consults.
//
// It is replaced wholesale rather than mutated, so a refresh cannot be observed
// half-applied - a request never sees the new input rate with the old cache
// rate. Lookups take an atomic load and a map read, which is what keeps pricing
// off the critical path even though it runs per request.
type Table struct {
	entries atomic.Pointer[map[Key]Entry]
}

// NewTable builds a table. Later entries win over earlier ones with the same
// key, so callers can layer sources by appending the more authoritative last.
func NewTable(entries []Entry) *Table {
	t := &Table{}
	t.Replace(entries)
	return t
}

// Replace swaps in a new set of rates.
func (t *Table) Replace(entries []Entry) {
	m := make(map[Key]Entry, len(entries))
	for _, e := range entries {
		m[e.Key] = e
	}
	t.entries.Store(&m)
}

// Lookup finds the rates for a provider-native model identifier, parsing the
// inference-profile prefix to pick the regional or global rate set.
//
// A nil table reports nothing found rather than panicking: a deployment with no
// pricing configured records unpriced requests, which is a legitimate state and
// not a crash.
func (t *Table) Lookup(provider, modelID string) (Entry, bool) {
	if t == nil {
		return Entry{}, false
	}
	m := t.entries.Load()
	if m == nil {
		return Entry{}, false
	}
	e, ok := (*m)[KeyFor(provider, modelID)]
	return e, ok
}

// entry finds a row by its key.
func (t *Table) entry(k Key) (Entry, bool) {
	if t == nil {
		return Entry{}, false
	}
	m := t.entries.Load()
	if m == nil {
		return Entry{}, false
	}
	e, ok := (*m)[k]
	return e, ok
}

// Price computes the cost of one request's usage under this table, at the
// tier the request's prompt size selects.
//
// Every error means the request is recorded unpriced, and errors.Is with
// ErrRateMissing is true of all of them. ErrNoPrice is the table holding
// nothing for the model; any other error names the class that had no rate.
func (t *Table) Price(provider, modelID string, u Usage) (Cost, Entry, error) {
	entry, ok := t.Lookup(provider, modelID)
	if !ok {
		return Cost{}, Entry{}, ErrNoPrice
	}
	cost, err := Compute(u, entry.RatesFor(u))
	return cost, entry, err
}

// All returns every entry, ordered so output is stable for tests, exports, and
// the console.
func (t *Table) All() []Entry {
	if t == nil {
		return nil
	}
	m := t.entries.Load()
	if m == nil {
		return nil
	}

	out := make([]Entry, 0, len(*m))
	for _, e := range *m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// Len reports how many models are priced.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	m := t.entries.Load()
	if m == nil {
		return 0
	}
	return len(*m)
}
