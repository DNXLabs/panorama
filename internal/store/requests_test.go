// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"
)

func TestRecordRequest(t *testing.T) {
	db := migratedSQLite(t)
	ctx := t.Context()

	user, err := db.CreateUser(ctx, "frank@example.com", "Frank", RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateGatewayToken(ctx, user.ID, "laptop", "hash-rec", "pnt_recd", nil)
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC().Truncate(time.Millisecond)
	want := RequestRecord{
		RequestID:        "abc123",
		UserID:           user.ID,
		TokenID:          tok.ID,
		StartedAt:        started,
		Duration:         1500 * time.Millisecond,
		TimeToFirstToken: 240 * time.Millisecond,
		Surface:          "anthropic",
		Provider:         "bedrock",
		RequestedModel:   "claude-sonnet-4-5",
		ResolvedModel:    "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Streaming:        true,
		StatusCode:       200,
		Decision:         DecisionAllowed,
		InputTokens:      1200,
		OutputTokens:     340,
		CacheReadTokens:  9800,
		CacheWriteTokens: 450,
		Tool:             "claude-code",
		UserAgent:        "claude-cli/1.0",
		ClientIP:         "127.0.0.1",
	}

	if rerr := db.RecordRequest(ctx, want); rerr != nil {
		t.Fatalf("RecordRequest: %v", rerr)
	}

	got, err := db.LatestRequest(ctx)
	if err != nil {
		t.Fatalf("LatestRequest: %v", err)
	}

	if got.RequestID != want.RequestID {
		t.Errorf("RequestID = %q, want %q", got.RequestID, want.RequestID)
	}
	if got.UserID != want.UserID || got.TokenID != want.TokenID {
		t.Errorf("identity = %q/%q, want %q/%q", got.UserID, got.TokenID, want.UserID, want.TokenID)
	}
	if got.RequestedModel != want.RequestedModel || got.ResolvedModel != want.ResolvedModel {
		t.Errorf("models = %q → %q, want %q → %q",
			got.RequestedModel, got.ResolvedModel, want.RequestedModel, want.ResolvedModel)
	}
	if !got.Streaming {
		t.Error("Streaming = false, want true")
	}
	if got.Decision != DecisionAllowed {
		t.Errorf("Decision = %q, want %q", got.Decision, DecisionAllowed)
	}

	// All four token classes must survive the round trip. Losing the cache
	// columns is exactly the failure that would overstate coding-agent cost.
	if got.InputTokens != want.InputTokens ||
		got.OutputTokens != want.OutputTokens ||
		got.CacheReadTokens != want.CacheReadTokens ||
		got.CacheWriteTokens != want.CacheWriteTokens {
		t.Errorf("tokens = in %d/out %d/cache-read %d/cache-write %d, want %d/%d/%d/%d",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheWriteTokens,
			want.InputTokens, want.OutputTokens, want.CacheReadTokens, want.CacheWriteTokens)
	}

	if got.Duration != want.Duration {
		t.Errorf("Duration = %v, want %v", got.Duration, want.Duration)
	}
	if got.TimeToFirstToken != want.TimeToFirstToken {
		t.Errorf("TimeToFirstToken = %v, want %v", got.TimeToFirstToken, want.TimeToFirstToken)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v - timestamps must survive the driver round trip", got.StartedAt, started)
	}
}

func TestRecordRequestWithoutIdentity(t *testing.T) {
	db := migratedSQLite(t)
	ctx := t.Context()

	// A request that failed before authenticating still has to be recorded.
	// An empty user id must become null rather than violate the foreign key.
	err := db.RecordRequest(ctx, RequestRecord{
		RequestID:  "anon",
		StartedAt:  time.Now().UTC(),
		Surface:    "anthropic",
		Provider:   "bedrock",
		StatusCode: 401,
		Decision:   DecisionError,
		ErrorKind:  "unauthenticated",
	})
	if err != nil {
		t.Fatalf("RecordRequest without a principal: %v", err)
	}

	n, err := db.CountRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("CountRequests = %d, want 1", n)
	}
}

func TestRecordRequestRejectsAnUnknownDecision(t *testing.T) {
	db := migratedSQLite(t)

	err := db.RecordRequest(t.Context(), RequestRecord{
		RequestID: "bad",
		StartedAt: time.Now().UTC(),
		Surface:   "anthropic",
		Decision:  "vibes",
	})
	if err == nil {
		t.Fatal("an unrecognised decision must be refused by the schema")
	}
}
