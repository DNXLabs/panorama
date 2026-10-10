// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/integrations"
	"github.com/dnxlabs/panorama/internal/store"
)

func newDB(t *testing.T) *store.DB {
	t.Helper()

	dsn := "file:" + filepath.Join(t.TempDir(), "panorama.db") +
		"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"

	db, err := store.Open(t.Context(), config.Database{
		Driver: config.DriverSQLite, DSN: dsn, MaxOpenConns: 1,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// recordBranchWork puts one attributed session with spend into the database,
// which is what makes its work item worth asking about.
func recordBranchWork(t *testing.T, db *store.DB, repo, branch string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()

	user, err := db.EnsureUser(ctx, "dev@example.com", "Dev", store.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}

	session, err := db.AttributeSession(ctx, store.Observation{
		Fingerprint: "fp-" + branch, Method: "conversation-prefix",
		UserID: user.ID, Tool: "claude-code", At: now,
		Signals: attribution.Signals{
			Repo:   attribution.Observation{Value: repo, Rung: attribution.RungStructural, Evidence: "git remote"},
			Branch: attribution.Observation{Value: branch, Rung: attribution.RungStructural, Evidence: "environment block"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.RecordRequest(ctx, store.RequestRecord{
		RequestID: "req-" + branch, UserID: user.ID, StartedAt: now,
		Surface: "anthropic", Provider: "bedrock",
		RequestedModel: "claude-haiku-4-5", ResolvedModel: "us.anthropic.claude-haiku-4-5",
		Tool: "claude-code", StatusCode: 200, Decision: store.DecisionAllowed,
		InputTokens: 100, OutputTokens: 20, Attempts: 1, SessionID: session.ID,
	}); err != nil {
		t.Fatal(err)
	}
}

// recordingSource is a Source whose answers and failures a test controls.
type recordingSource struct {
	kind     integrations.Kind
	checkErr error
	lookErr  error
	items    []integrations.WorkItem

	asked [][]integrations.Ref
}

func (s *recordingSource) Kind() integrations.Kind { return s.kind }
func (s *recordingSource) Provider() string        { return string(s.kind) }
func (s *recordingSource) Describe() string        { return string(s.kind) + " under test" }
func (s *recordingSource) Check(context.Context) error {
	return s.checkErr
}

func (s *recordingSource) Lookup(_ context.Context, refs []integrations.Ref) ([]integrations.WorkItem, error) {
	s.asked = append(s.asked, refs)
	if s.lookErr != nil {
		return nil, s.lookErr
	}
	return s.items, nil
}

func TestRunOnceConfirmsAndRecords(t *testing.T) {
	db := newDB(t)
	recordBranchWork(t, db, "acme/panorama", "feat/tokenizer")

	source := &recordingSource{
		kind: integrations.KindGitHub,
		items: []integrations.WorkItem{{
			Kind: "branch", Key: "acme/panorama@feat/tokenizer",
			Title: "feat: streaming tokenizer", Outcome: integrations.OutcomeOpen,
		}},
	}

	c := NewConfirmer(ConfirmerOptions{DB: db, Resolver: integrations.Static(source)})
	if err := c.Register(t.Context()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := c.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(source.asked) != 1 || len(source.asked[0]) != 1 {
		t.Fatalf("the source was asked %v, want one batch of one reference", source.asked)
	}

	item, err := db.WorkItem(t.Context(), "branch", "acme/panorama@feat/tokenizer")
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "feat: streaming tokenizer" {
		t.Errorf("title = %q, want the confirmed one", item.Title)
	}

	states, err := db.ListIntegrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Status != store.IntegrationOK || states[0].LastConfirmedItems != 1 {
		t.Errorf("integration state = %+v, want ok with one confirmation", states)
	}
}

// TestAnUnreachableSourceConfirmsNothing is the rule about fallbacks applied to
// an integration. Carrying on past a failed health check would write "not
// found" against every reference and turn one unreachable API into a
// deployment-wide claim that none of the work exists.
func TestAnUnreachableSourceConfirmsNothing(t *testing.T) {
	db := newDB(t)
	recordBranchWork(t, db, "acme/panorama", "feat/tokenizer")

	source := &recordingSource{kind: integrations.KindGitHub, checkErr: errors.New("connection refused")}
	c := NewConfirmer(ConfirmerOptions{DB: db, Resolver: integrations.Static(source)})
	if err := c.Register(t.Context()); err != nil {
		t.Fatal(err)
	}

	err := c.RunOnce(t.Context())
	if err == nil {
		t.Fatal("RunOnce hid an unreachable source")
	}
	if len(source.asked) != 0 {
		t.Error("a source that failed its health check was asked for work items anyway")
	}

	item, err := db.WorkItem(t.Context(), "branch", "acme/panorama@feat/tokenizer")
	if err != nil {
		t.Fatal(err)
	}
	if item.Checked() {
		t.Error("a work item was marked checked despite nothing having been asked")
	}

	states, err := db.ListIntegrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Status != store.IntegrationError || states[0].LastError == "" {
		t.Errorf("integration state = %+v, want the failure recorded for the console", states)
	}
}

// TestOneBrokenSourceDoesNotStopTheOther. GitHub being down is not a reason for
// tickets to stop being confirmed.
func TestOneBrokenSourceDoesNotStopTheOther(t *testing.T) {
	db := newDB(t)
	recordBranchWork(t, db, "acme/panorama", "feat/tokenizer")

	// A ticket worth asking about, reached the only way a ticket with no direct
	// session is: named by something else.
	gh := &recordingSource{
		kind: integrations.KindGitHub,
		items: []integrations.WorkItem{{
			Kind: "branch", Key: "acme/panorama@feat/tokenizer",
			Links: []integrations.Link{{Kind: "ticket", Key: "PAY-1183"}},
		}},
	}
	c := NewConfirmer(ConfirmerOptions{DB: db, Resolver: integrations.Static(gh)})
	if err := c.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Fresh work for GitHub to be unreachable about. Without it there would be
	// nothing to ask, and a pass with nothing to ask is not a pass that failed.
	recordBranchWork(t, db, "acme/panorama", "fix/backoff")

	broken := &recordingSource{kind: integrations.KindGitHub, checkErr: errors.New("connection refused")}
	jira := &recordingSource{
		kind: integrations.KindJira,
		items: []integrations.WorkItem{{
			Kind: "ticket", Key: "PAY-1183",
			Title: "Streaming tokenizer for card capture", Outcome: integrations.OutcomeLanded,
		}},
	}
	c = NewConfirmer(ConfirmerOptions{DB: db, Resolver: integrations.Static(broken, jira)})
	if err := c.Register(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The pass reports the failure, and still does the half it could.
	if err := c.RunOnce(t.Context()); err == nil {
		t.Error("RunOnce hid a broken source because another one worked")
	}

	ticket, err := db.WorkItem(t.Context(), "ticket", "PAY-1183")
	if err != nil {
		t.Fatal(err)
	}
	if !ticket.Confirmed() || ticket.Title == "" {
		t.Errorf("ticket = %+v, want it confirmed by Jira despite GitHub being down", ticket)
	}
}

// TestNothingConfiguredIsNotAnError. The supported shape of a fresh deployment
// is no integrations at all.
func TestNothingConfiguredIsNotAnError(t *testing.T) {
	db := newDB(t)
	recordBranchWork(t, db, "acme/panorama", "feat/tokenizer")

	c := NewConfirmer(ConfirmerOptions{DB: db, Resolver: integrations.Static()})
	if err := c.Register(t.Context()); err != nil {
		t.Fatalf("Register with nothing configured: %v", err)
	}
	if err := c.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce with nothing configured: %v", err)
	}
}

// TestRunKeepsWatchingWhenNothingIsConnectedYet: the loop does not return on
// an unconfigured deployment.
//
// An administrator connects GitHub from a screen ten minutes after boot, so a
// loop that exited at startup would never notice - confirmation would stay
// dead until somebody restarted the process, with nothing saying why.
func TestRunKeepsWatchingWhenNothingIsConnectedYet(t *testing.T) {
	db := newDB(t)

	c := NewConfirmer(ConfirmerOptions{
		DB: db, Resolver: integrations.Static(), Interval: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()

	// Still running after several intervals, rather than having given up.
	select {
	case <-done:
		t.Fatal("Run returned with nothing connected; a connection made later would never be picked up")
	case <-time.After(100 * time.Millisecond):
	}

	// And it still stops when it is told to.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Run did not stop when its context was cancelled")
	}
}

// TestHealthIsCheckedWithNothingToConfirm is what a platform engineer sees in
// the first ten minutes.
//
// They configure GitHub, restart, and open the Integrations screen on a
// deployment with no traffic yet. Skipping the pass entirely would leave the
// status "unknown" until somebody's work happened to produce a work item -
// which is the wrong moment to find out the private key is wrong.
func TestHealthIsCheckedWithNothingToConfirm(t *testing.T) {
	db := newDB(t)

	source := &recordingSource{kind: integrations.KindGitHub}
	c := NewConfirmer(ConfirmerOptions{DB: db, Resolver: integrations.Static(source)})
	if err := c.Register(t.Context()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := c.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	states, err := db.ListIntegrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Status != store.IntegrationOK {
		t.Fatalf("integration state = %+v, want ok after a pass with nothing to confirm", states)
	}
	if len(source.asked) != 0 {
		t.Errorf("the source was asked about %v; there was nothing to confirm", source.asked)
	}
}

// TestAnUnreachableSourceWithNothingToConfirmIsRecordedNotReturned. There is no
// work to abandon on this path, so a broken connection is a red badge rather
// than a job that reports failure every interval on an idle deployment.
func TestAnUnreachableSourceWithNothingToConfirmIsRecordedNotReturned(t *testing.T) {
	db := newDB(t)

	source := &recordingSource{kind: integrations.KindGitHub, checkErr: errors.New("bad credentials")}
	c := NewConfirmer(ConfirmerOptions{DB: db, Resolver: integrations.Static(source)})
	if err := c.Register(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := c.RunOnce(t.Context()); err != nil {
		t.Errorf("RunOnce = %v, want nil - there was no work to fail at", err)
	}

	states, err := db.ListIntegrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Status != store.IntegrationError {
		t.Fatalf("integration state = %+v, want the failure recorded", states)
	}
	if !strings.Contains(states[0].LastError, "bad credentials") {
		t.Errorf("last error = %q, want the reason an operator can act on", states[0].LastError)
	}
}
