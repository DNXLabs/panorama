// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/providers"
)

// TestAProviderKeepsItsLatestRefusal.
//
// One row per provider, replaced by the next refusal, and cleared only by a
// check that invoked the same model in the same region.
func TestAProviderKeepsItsLatestRefusal(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
		first := ProviderFailure{
			Kind: providers.KindBedrock, Region: "us-east-1", Model: "global.anthropic.claude-haiku-5-5",
			ErrorKind: "forbidden", Message: "AccessDeniedException: not available for this account",
			RequestID: "req_1", At: at,
		}
		if err := db.RecordProviderFailure(t.Context(), first); err != nil {
			t.Fatalf("record: %v", err)
		}
		second := first
		second.Model, second.ErrorKind, second.RequestID = "us.anthropic.claude-haiku-4-5-20251001-v1:0", "not_found", "req_2"
		second.Message = strings.Repeat("é", 2000)
		second.At = at.Add(time.Minute)
		if err := db.RecordProviderFailure(t.Context(), second); err != nil {
			t.Fatalf("record again: %v", err)
		}

		got, err := db.ProviderFailures(t.Context())
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d rows, want the one latest refusal", len(got))
		}
		f := got[providers.KindBedrock]
		if f.Model != second.Model || f.RequestID != "req_2" || !f.At.Equal(second.At) {
			t.Errorf("kept %+v, want the second refusal", f)
		}
		if len(f.Message) > maxFailureMessage || !strings.HasPrefix(second.Message, f.Message) {
			t.Errorf("message kept as %d bytes, want it cut on a rune boundary under %d", len(f.Message), maxFailureMessage)
		}

		// A check that passed on another model says nothing about this one.
		if err := db.ClearProviderFailure(t.Context(), providers.KindBedrock, "us-east-1", first.Model); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if got, _ := db.ProviderFailures(t.Context()); len(got) != 1 {
			t.Error("a check on another model cleared the refusal")
		}
		if err := db.ClearProviderFailure(t.Context(), providers.KindBedrock, "us-east-1", second.Model); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if got, _ := db.ProviderFailures(t.Context()); len(got) != 0 {
			t.Error("a check on the refused model did not clear it")
		}
	})
}

func TestProvidersRoundTrip(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		betas := []string{"redact-thinking-2026-02-12"}
		mantle := providers.Settings{
			Kind: providers.KindBedrockMantle, Enabled: true, Region: "us-west-2",
			Options: providers.Options{
				UnsupportedBetas: &betas,
				ContextWindows:   map[string]int{"openai.gpt-oss-120b": 128_000},
				APIs:             map[string][]providers.API{"acme.mystery-1": {providers.APIOpenAIChat}},
			},
			ConfiguredBy: "usr_admin",
		}
		changed, err := db.SaveProvider(t.Context(), mantle)
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if !changed {
			t.Error("a new provider did not report its region as changed")
		}
		if _, serr := db.SaveProvider(t.Context(), providers.Settings{
			Kind: providers.KindBedrock, Enabled: false, Region: "ap-southeast-2",
		}); serr != nil {
			t.Fatalf("save bedrock: %v", serr)
		}

		list, err := db.ListProviders(t.Context())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 2 || list[0].Kind != providers.KindBedrock || list[1].Kind != providers.KindBedrockMantle {
			t.Fatalf("providers = %+v, want both, bedrock first", list)
		}
		got := list[1]
		if got.Region != "us-west-2" || !got.Enabled || got.ConfiguredBy != "usr_admin" {
			t.Errorf("mantle came back as %+v", got)
		}
		if got.Options.UnsupportedBetas == nil || (*got.Options.UnsupportedBetas)[0] != betas[0] {
			t.Errorf("unsupported betas did not round-trip: %+v", got.Options)
		}
		if got.Options.ContextWindows["openai.gpt-oss-120b"] != 128_000 {
			t.Errorf("context windows did not round-trip: %+v", got.Options)
		}
		if list[0].Enabled {
			t.Error("a disabled provider came back enabled")
		}
	})
}

// A verification is a statement about one region. Changing the region clears
// it, and a check that raced a region change cannot mark the new region as
// verified by a call made to the old one.
func TestAVerificationBelongsToOneRegion(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		s := providers.Settings{Kind: providers.KindBedrock, Enabled: true, Region: "us-east-1"}
		if _, err := db.SaveProvider(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		if err := db.SetProviderVerified(t.Context(), s.Kind, "us-east-1", "us.anthropic.claude-haiku-4-5",
			time.Now()); err != nil {
			t.Fatal(err)
		}

		// Saving without changing the region keeps it.
		s.Options.ContextWindows = map[string]int{"x": 1}
		changed, err := db.SaveProvider(t.Context(), s)
		if err != nil || changed {
			t.Fatalf("an options change reported a region change (%v, %v)", changed, err)
		}
		got, _, _ := db.Provider(t.Context(), s.Kind)
		if got.VerifiedAt == nil || got.VerifiedModel == "" {
			t.Fatal("an options change cleared the verification")
		}

		s.Region = "eu-west-1"
		changed, err = db.SaveProvider(t.Context(), s)
		if err != nil || !changed {
			t.Fatalf("a region change was not reported (%v, %v)", changed, err)
		}
		got, _, _ = db.Provider(t.Context(), s.Kind)
		if got.VerifiedAt != nil {
			t.Error("a region change kept a verification made in another region")
		}

		// The race: a check that ran against us-east-1 lands after the change.
		if err := db.SetProviderVerified(t.Context(), s.Kind, "us-east-1", "m", time.Now()); err != nil {
			t.Fatal(err)
		}
		got, _, _ = db.Provider(t.Context(), s.Kind)
		if got.VerifiedAt != nil {
			t.Error("a check made in the old region verified the new one")
		}
	})
}

// The file seeds the row once; after that the row is the configuration.
func TestSeedingAProviderHappensOnce(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		seed := providers.Settings{Kind: providers.KindBedrock, Enabled: true, Region: "us-east-1",
			ConfiguredBy: "configuration file"}
		created, stored, err := db.SeedProvider(t.Context(), seed)
		if err != nil || !created {
			t.Fatalf("first seed = (%v, %v), want created", created, err)
		}

		// An administrator changes the region; the file still says us-east-1.
		stored.Region = "ap-southeast-2"
		if _, serr := db.SaveProvider(t.Context(), stored); serr != nil {
			t.Fatal(serr)
		}
		created, stored, err = db.SeedProvider(t.Context(), seed)
		if err != nil || created {
			t.Fatalf("second seed = (%v, %v), want nothing created", created, err)
		}
		if stored.Region != "ap-southeast-2" {
			t.Errorf("the file revised the administrator's region to %q", stored.Region)
		}
	})
}

// A route names the provider it runs on, and turning that provider off is
// refused by naming the routes - which is this list.
func TestRoutesOnAProvider(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		for _, r := range []Route{
			route("standard", "us.anthropic.claude-sonnet-5"),
			{Route: route("oss", "x").Route},
		} {
			if r.Name == "oss" {
				r.Target = providers.MustModelRef("bedrock-mantle/openai.gpt-oss-120b")
			}
			if err := db.SaveRoute(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}
		onMantle, err := db.RoutesOnProvider(t.Context(), providers.KindBedrockMantle)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(onMantle, ",") != "oss" {
			t.Errorf("routes on bedrock-mantle = %v, want [oss]", onMantle)
		}
	})
}
