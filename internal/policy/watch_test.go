// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// runWatching starts the background loop with a reload interval no test will
// reach, so anything it picks up it picked up by watching.
func runWatching(t *testing.T, repo *fakeRepo) *Engine {
	t.Helper()

	e := New(Options{
		Repository:     repo,
		ReloadInterval: time.Hour,
		WatchInterval:  5 * time.Millisecond,
	})
	if err := e.Load(t.Context()); err != nil {
		t.Fatalf("load: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return e
}

func hasRoute(set Set, name string) bool {
	for _, r := range set.Routes {
		if r.Name == name {
			return true
		}
	}
	return false
}

// TestARouteSavedElsewhereIsServedWithoutWaitingForTheReload is the deploy
// test's finding: a route applied from a one-off task was refused as "no route
// you can use" for about twenty seconds, until the running gateway's next
// reload.
func TestARouteSavedElsewhereIsServedWithoutWaitingForTheReload(t *testing.T) {
	repo := &fakeRepo{set: Set{Routes: []Route{plain("default", "us.anthropic.sonnet")}}}
	e := runWatching(t, repo)

	// Nothing written, nothing loaded: the watch reads a counter and stops.
	time.Sleep(50 * time.Millisecond)
	if got := repo.loadCount(); got != 1 {
		t.Fatalf("policy loaded %d times with nothing written, want 1", got)
	}

	repo.write(func(s *Set) {
		s.Routes = []Route{plain("default", "us.anthropic.sonnet"), plain("fast", "us.anthropic.haiku")}
	})

	deadline := time.Now().Add(2 * time.Second)
	for !hasRoute(e.Snapshot(), "fast") {
		if time.Now().After(deadline) {
			t.Fatal("a route written by another process was not served within two seconds")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A reload that keeps failing is left to the reload interval, rather than
// retried, and logged, on every poll.
func TestAFailedReloadIsNotRetriedEveryPoll(t *testing.T) {
	repo := &fakeRepo{set: Set{Routes: []Route{plain("default", "us.anthropic.sonnet")}}}
	e := runWatching(t, repo)

	repo.mu.Lock()
	repo.err = errors.New("database unavailable")
	repo.generation++
	repo.mu.Unlock()

	time.Sleep(100 * time.Millisecond)
	if got := repo.loadCount(); got != 2 {
		t.Errorf("policy loaded %d times after one change that failed to load, want 2", got)
	}
	if !hasRoute(e.Snapshot(), "default") {
		t.Error("the snapshot in force was dropped when a reload failed")
	}
}
