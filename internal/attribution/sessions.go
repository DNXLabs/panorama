// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package attribution

import (
	"sync"
	"time"
)

// DefaultCacheSize is how many live conversations are remembered in memory.
//
// Ten thousand is far more than a single-organization deployment has open at
// once, and costs a few megabytes. The cache is an optimisation, not a source
// of truth: a miss re-runs the extraction and reaches the same answer.
const DefaultCacheSize = 10_000

// Known is what the gateway already knows about a live conversation.
type Known struct {
	ID          string
	Fingerprint string
	// Rung is the provenance of the current attribution, empty when the session
	// has none yet.
	Rung Rung
	// Key is the work item currently attributed, for logging.
	Key      string
	LastSeen time.Time
}

// Sessions remembers live conversations so that attributing one is work done
// once rather than on every turn.
//
// A coding agent sends dozens or hundreds of turns per conversation, and the
// answer does not change between them: the branch someone is on is the branch
// they were on a minute ago. Scanning every body would spend the request path's
// budget re-deriving a fact already known.
type Sessions struct {
	mu       sync.Mutex
	entries  map[string]Known
	capacity int
}

// NewSessions builds a cache. A capacity of zero uses DefaultCacheSize.
func NewSessions(capacity int) *Sessions {
	if capacity <= 0 {
		capacity = DefaultCacheSize
	}
	return &Sessions{entries: make(map[string]Known), capacity: capacity}
}

// Lookup returns what is known about a conversation.
func (s *Sessions) Lookup(fingerprint string) (Known, bool) {
	if s == nil || fingerprint == "" {
		return Known{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	k, ok := s.entries[fingerprint]
	return k, ok
}

// NeedsExtraction reports whether the full body scan is worth running.
//
// The rule, made concrete: attribute once per session and
// inherit across turns rather than re-guessing per request. "Once" means once
// the answer is at least structural - a session attributed only from a mention
// in conversation keeps looking for a fact, because the whole ladder exists to
// prefer one.
//
// The cost of getting this wrong in the generous direction is a body scan; in
// the stingy direction it is a session stuck on its worst evidence. So an
// unknown fingerprint always scans.
func (s *Sessions) NeedsExtraction(fingerprint string) bool {
	if fingerprint == "" {
		// Nothing to remember it by. Scanning would produce an attribution with
		// no session to attach it to.
		return false
	}

	// A nil cache reports every conversation as unseen, so every turn scans.
	// That is the expensive answer and the right one: the alternative is a
	// deployment that quietly stops attributing.
	k, ok := s.Lookup(fingerprint)
	if !ok {
		return true
	}
	return k.Rung.Rank() < RungStructural.Rank()
}

// Remember records what a session is now attributed to.
func (s *Sessions) Remember(k Known) {
	if s == nil || k.Fingerprint == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[k.Fingerprint] = k
	if len(s.entries) > s.capacity {
		s.evict()
	}
}

// evict drops the least recently seen tenth of the cache.
//
// A tenth at a time rather than one at a time, so that a deployment sitting
// exactly at capacity does not pay a full scan of the map on every new session.
// The caller holds the lock.
func (s *Sessions) evict() {
	target := len(s.entries) - s.capacity + s.capacity/10

	// Two passes rather than a sort: find the cutoff time, then delete
	// everything at or before it. Approximate, which is the right accuracy for
	// a cache whose miss costs one extra scan.
	var oldest time.Time
	for _, k := range s.entries {
		if oldest.IsZero() || k.LastSeen.Before(oldest) {
			oldest = k.LastSeen
		}
	}

	cutoff := oldest
	for range 8 {
		removed := 0
		for fp, k := range s.entries {
			if !k.LastSeen.After(cutoff) {
				delete(s.entries, fp)
				removed++
				if removed >= target {
					return
				}
			}
		}
		if len(s.entries) <= s.capacity {
			return
		}
		// Widen the window and try again.
		cutoff = cutoff.Add(time.Minute)
	}
}

// Len reports how many conversations are being remembered.
func (s *Sessions) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
