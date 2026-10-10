// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package attribution

import (
	"strings"
	"testing"
	"time"
)

// TestFingerprintIsStableAsTheConversationGrows is the property the whole
// grouping rests on. A coding agent re-sends everything every turn, and if the
// fingerprint moved with the conversation, every turn would be its own session
// and nothing would ever be attributed twice to the same work.
func TestFingerprintIsStableAsTheConversationGrows(t *testing.T) {
	e := newExtractor(t, "")

	turns := []string{"Add cache-aware pricing"}
	first, method := e.Fingerprint("usr_1", claudeCodeRequest{
		system: currentSystemPrompt, messages: turns,
	}.body(t))

	if first == "" {
		t.Fatal("no fingerprint from a normal request")
	}
	if method != "conversation-prefix" {
		t.Errorf("method = %q, want conversation-prefix", method)
	}

	for range 20 {
		turns = append(turns, "…and then?", "Here is the next step.")
		got, _ := e.Fingerprint("usr_1", claudeCodeRequest{
			system: currentSystemPrompt, messages: turns,
		}.body(t))
		if got != first {
			t.Fatalf("the fingerprint changed after %d turns; every turn would be its own session", len(turns))
		}
	}
}

// TestShortOpeningDoesNotDrift is the specific way the above breaks. Taking a
// fixed number of bytes after the first message spills into the messages after
// it, and those grow every turn.
func TestShortOpeningDoesNotDrift(t *testing.T) {
	e := newExtractor(t, "")

	short := []string{"hi"}
	first, _ := e.Fingerprint("usr_1", claudeCodeRequest{system: "You are Claude Code.", messages: short}.body(t))

	short = append(short, "Hello - what would you like to do?", "refactor the price table")
	second, _ := e.Fingerprint("usr_1", claudeCodeRequest{system: "You are Claude Code.", messages: short}.body(t))

	if first != second {
		t.Error("a two-character opening message drifted once the conversation continued")
	}
}

func TestFingerprintSeparatesUsers(t *testing.T) {
	e := newExtractor(t, "")
	body := claudeCodeRequest{system: currentSystemPrompt, messages: []string{"same opening"}}.body(t)

	alice, _ := e.Fingerprint("usr_alice", body)
	bob, _ := e.Fingerprint("usr_bob", body)

	if alice == bob {
		// Two people opening a conversation the same way would share a session,
		// and one would inherit the other's attribution.
		t.Error("two users sharing an opening line got the same fingerprint")
	}
}

// TestFingerprintSeparatesRepositories: the same person running the same
// opening line in two checkouts is doing two different pieces of work.
func TestFingerprintSeparatesRepositories(t *testing.T) {
	e := newExtractor(t, "")

	here := claudeCodeRequest{system: currentSystemPrompt, messages: []string{"fix the tests"}}.body(t)
	there := claudeCodeRequest{
		system:   strings.Replace(currentSystemPrompt, "/Users/dev/src/panorama", "/Users/dev/src/billing", 1),
		messages: []string{"fix the tests"},
	}.body(t)

	a, _ := e.Fingerprint("usr_1", here)
	b, _ := e.Fingerprint("usr_1", there)

	if a == b {
		t.Error("the same opening in two working directories produced one session")
	}
}

// TestClientSessionIdIsPreferred: exact beats inferred, and it is also immune
// to a client compacting its own history.
func TestClientSessionIdIsPreferred(t *testing.T) {
	e := newExtractor(t, "")

	withMetadata := func(opening string) []byte {
		return claudeCodeRequest{
			system:   currentSystemPrompt,
			messages: []string{opening},
			metadata: "user_9f2a_account_1b7c-4d2e_session_0198f2a1-77bd-4c0e-9a12-5f3b6c8d9e01",
		}.body(t)
	}

	first, method := e.Fingerprint("usr_1", withMetadata("start here"))
	if method != "client-session-id" {
		t.Fatalf("method = %q, want client-session-id", method)
	}

	// The opening changes - as it would after the client compacted its history
	// - and the session survives, which the inferred method cannot manage.
	second, _ := e.Fingerprint("usr_1", withMetadata("a completely different opening"))
	if first != second {
		t.Error("the session did not survive a rewritten opening despite an explicit session id")
	}

	// Still scoped to the user.
	other, _ := e.Fingerprint("usr_2", withMetadata("start here"))
	if other == first {
		t.Error("a client-supplied session id let one user join another's session")
	}
}

func TestNoConversationMeansNoSession(t *testing.T) {
	e := newExtractor(t, "")

	fp, method := e.Fingerprint("usr_1", []byte(`{"model":"claude-sonnet-5","max_tokens":16}`))
	if fp != "" || method != "" {
		// Inventing one would give the request a session to inherit an
		// attribution from, and it has nothing to do with any of them.
		t.Errorf("a request with no messages got fingerprint %q (%s)", fp, method)
	}
}

func TestSessionCacheDecidesWhenToScan(t *testing.T) {
	c := NewSessions(0)

	if !c.NeedsExtraction("unknown") {
		t.Error("an unseen conversation was not scanned")
	}
	if c.NeedsExtraction("") {
		t.Error("a request with no fingerprint was scanned; there is nothing to attach the result to")
	}

	// Attributed from a mention only: keep looking for a fact.
	c.Remember(Known{ID: "ses_1", Fingerprint: "fp", Rung: RungContent, LastSeen: time.Now()})
	if !c.NeedsExtraction("fp") {
		t.Error("a session attributed only from conversation text stopped looking for structure")
	}

	// Structural: settled.
	c.Remember(Known{ID: "ses_1", Fingerprint: "fp", Rung: RungStructural, LastSeen: time.Now()})
	if c.NeedsExtraction("fp") {
		t.Error("a session with structural attribution is still being re-scanned every turn")
	}

	c.Remember(Known{ID: "ses_1", Fingerprint: "fp", Rung: RungStamp, LastSeen: time.Now()})
	if c.NeedsExtraction("fp") {
		t.Error("a stamped session is still being re-scanned")
	}
}

func TestSessionCacheEvicts(t *testing.T) {
	c := NewSessions(50)

	base := time.Now()
	for i := range 500 {
		c.Remember(Known{
			ID:          "ses",
			Fingerprint: string(rune('a'+i%26)) + strings.Repeat("x", i%7) + itoa(i),
			Rung:        RungStructural,
			LastSeen:    base.Add(time.Duration(i) * time.Second),
		})
	}

	if got := c.Len(); got > 60 {
		t.Errorf("cache holds %d entries with a capacity of 50; it is not evicting", got)
	}
	if c.Len() == 0 {
		t.Error("the cache evicted everything")
	}
}

func TestNilSessionCacheIsSafe(t *testing.T) {
	var c *Sessions

	// Without a cache every turn is an unseen one, so every turn scans. That is
	// the expensive answer and the right one: the alternative is a deployment
	// that never attributes anything, and a missing cache should cost latency
	// rather than the feature.
	if !c.NeedsExtraction("fp") {
		t.Error("a nil cache stopped attributing rather than stopping caching")
	}
	c.Remember(Known{Fingerprint: "fp"})
	if _, ok := c.Lookup("fp"); ok {
		t.Error("a nil cache returned an entry")
	}
	if c.Len() != 0 {
		t.Error("a nil cache reported entries")
	}
}

// itoa avoids pulling strconv in for one call in a test helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
