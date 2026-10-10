// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/providers"
)

// sonnet5 in two geographies: the pair the whole catalogue design exists for.
func catalogueFixture() []CatalogueModel {
	return []CatalogueModel{
		{
			ID:                "au.anthropic.claude-sonnet-5",
			FoundationModelID: "anthropic.claude-sonnet-5",
			Vendor:            "Anthropic", DisplayName: "Claude Sonnet 5",
			Geography: "au", Regions: []string{"ap-southeast-2", "ap-southeast-4"},
			Streaming: true, Lifecycle: "ACTIVE", RequiresProfile: true,
		},
		{
			ID:                "global.anthropic.claude-sonnet-5",
			FoundationModelID: "anthropic.claude-sonnet-5",
			Vendor:            "Anthropic", DisplayName: "Claude Sonnet 5",
			Geography: "global",
			Streaming: true, Lifecycle: "ACTIVE", RequiresProfile: true,
		},
		{
			ID:                "anthropic.claude-3-haiku-20240307-v1:0",
			FoundationModelID: "anthropic.claude-3-haiku-20240307-v1:0",
			Vendor:            "Anthropic", DisplayName: "Claude 3 Haiku",
			Streaming: true, Lifecycle: "LEGACY",
		},
	}
}

func TestCatalogueRoundTrips(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		if err := db.ReplaceCatalogue(t.Context(), providers.KindBedrock, catalogueFixture()); err != nil {
			t.Fatalf("replace catalogue: %v", err)
		}

		got, err := db.ListCatalogue(t.Context())
		if err != nil {
			t.Fatalf("list catalogue: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("%d catalogue rows, want 3", len(got))
		}

		byID := map[string]CatalogueModel{}
		for _, m := range got {
			byID[m.ID] = m
		}

		au := byID["au.anthropic.claude-sonnet-5"]
		// The region list is the compliance answer and the one field that
		// cannot be derived from anything else in the row.
		if want := "ap-southeast-2, ap-southeast-4"; au.Residency() != want {
			t.Errorf("au residency = %q, want %q", au.Residency(), want)
		}
		if au.Global() {
			t.Error("the au profile reports itself as global")
		}

		global := byID["global.anthropic.claude-sonnet-5"]
		if !global.Global() || global.Residency() != "any supported AWS region" {
			t.Errorf("global residency = %q", global.Residency())
		}
		// Both geographies of one model must survive as separate rows, because
		// allowing one and refusing the other is the point.
		if au.FoundationModelID != global.FoundationModelID {
			t.Error("the two geographies do not share a foundation model id")
		}

		if !byID["anthropic.claude-3-haiku-20240307-v1:0"].Legacy() {
			t.Error("a LEGACY model does not report itself as legacy")
		}

		fetched, ferr := db.CatalogueFetchedAt(t.Context())
		if _, ok := fetched[providers.KindBedrock]; ferr != nil || !ok {
			t.Errorf("CatalogueFetchedAt after a write = %v err:%v", fetched, ferr)
		}
		if _, ok := fetched[providers.KindBedrockMantle]; ok {
			t.Error("a provider that was never discovered reports a fetch time")
		}
	})
}

// TestProfilesSortAheadOfTheBareModelID.
//
// Sorting on geography alone puts the empty string first, which leads every
// model with the one target almost nobody should pick. Of the fifteen Anthropic
// models in us-east-1, fourteen are invocable only through an inference profile
// and the one that still accepts its bare id is LEGACY, so the bare id is the
// exception and belongs last.
func TestProfilesSortAheadOfTheBareModelID(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		haiku := "anthropic.claude-3-haiku-20240307-v1:0"
		if err := db.ReplaceCatalogue(t.Context(), providers.KindBedrock, store3Haiku(haiku)); err != nil {
			t.Fatalf("seed: %v", err)
		}

		got, err := db.ListCatalogue(t.Context())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("%d rows, want 3", len(got))
		}
		if got[len(got)-1].Geography != "" {
			t.Errorf("last row geography = %q, want the bare model id sorted last", got[len(got)-1].Geography)
		}
		for _, m := range got[:2] {
			if m.Geography == "" {
				t.Error("a bare foundation-model id sorted ahead of an inference profile")
			}
		}
	})
}

// store3Haiku is one model in its three reachable forms: two profiles and the
// bare id AWS still accepts for it.
func store3Haiku(bare string) []CatalogueModel {
	common := CatalogueModel{
		FoundationModelID: bare, Vendor: "Anthropic",
		DisplayName: "Claude 3 Haiku", Streaming: true, Lifecycle: "LEGACY",
	}
	us, global, direct := common, common, common
	us.ID, us.Geography, us.RequiresProfile = "us."+bare, "us", true
	global.ID, global.Geography, global.RequiresProfile = "global."+bare, "global", true
	direct.ID = bare
	return []CatalogueModel{direct, us, global}
}

// TestReplacingTheCatalogueForgetsWithdrawnModels. The catalogue's correctness
// includes what is absent from it: a model AWS withdrew has to disappear, and an
// upsert would leave it behind looking exactly like one still on offer.
func TestReplacingTheCatalogueForgetsWithdrawnModels(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		if err := db.ReplaceCatalogue(t.Context(), providers.KindBedrock, catalogueFixture()); err != nil {
			t.Fatalf("seed catalogue: %v", err)
		}
		if err := db.ReplaceCatalogue(t.Context(), providers.KindBedrock,
			catalogueFixture()[:1]); err != nil {
			t.Fatalf("replace catalogue: %v", err)
		}

		got, err := db.ListCatalogue(t.Context())
		if err != nil {
			t.Fatalf("list catalogue: %v", err)
		}
		if len(got) != 1 || got[0].ID != "au.anthropic.claude-sonnet-5" {
			t.Fatalf("catalogue = %+v, want only the model still on offer", got)
		}
	})
}

// TestDiscoveringOneProviderLeavesTheOtherAlone. A bedrock-mantle row is not a
// stale bedrock row, and a refresh of one must not delete the other - even
// where the two report the same id, which is a key per provider.
func TestDiscoveringOneProviderLeavesTheOtherAlone(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		if err := db.ReplaceCatalogue(t.Context(), providers.KindBedrock, catalogueFixture()); err != nil {
			t.Fatalf("seed runtime: %v", err)
		}
		mantle := []CatalogueModel{
			{
				ID:                "anthropic.claude-3-haiku-20240307-v1:0",
				FoundationModelID: "anthropic.claude-3-haiku-20240307-v1:0",
				DisplayName:       "claude-3-haiku", Streaming: true, Region: "us-west-2",
				APIs: []providers.API{providers.APIAnthropicMessages},
			},
			{
				ID: "openai.gpt-oss-120b", FoundationModelID: "openai.gpt-oss-120b",
				Vendor: "OpenAI", DisplayName: "gpt-oss-120b", Streaming: true, Region: "us-west-2",
				APIs:      []providers.API{providers.APIOpenAIChat, providers.APIOpenAIResponses},
				Retention: []string{"none", "aws_review"},
			},
			{
				ID: "anthropic.claude-mythos-5", FoundationModelID: "anthropic.claude-mythos-5",
				Status: "unavailable", StatusReason: "requires aws_review retention",
			},
		}
		if err := db.ReplaceCatalogue(t.Context(), providers.KindBedrockMantle, mantle); err != nil {
			t.Fatalf("seed mantle: %v", err)
		}

		got, err := db.ListCatalogue(t.Context())
		if err != nil {
			t.Fatalf("list catalogue: %v", err)
		}
		if len(got) != 6 {
			t.Fatalf("%d rows after discovering two providers, want 6", len(got))
		}

		byRef := map[providers.ModelRef]CatalogueModel{}
		for _, m := range got {
			byRef[m.Ref()] = m
		}
		// The same id on both providers is two rows, told apart by provider.
		runtime := byRef[providers.MustModelRef("bedrock/anthropic.claude-3-haiku-20240307-v1:0")]
		onMantle := byRef[providers.MustModelRef("bedrock-mantle/anthropic.claude-3-haiku-20240307-v1:0")]
		if runtime.ID == "" || onMantle.ID == "" {
			t.Fatalf("an id reported by both providers did not survive as two rows: %+v", byRef)
		}
		if onMantle.Residency() != "us-west-2" {
			t.Errorf("a bare Mantle id's residency = %q, want the region it was discovered in", onMantle.Residency())
		}

		oss := byRef[providers.MustModelRef("bedrock-mantle/openai.gpt-oss-120b")]
		if !oss.Speaks(providers.APIOpenAIResponses) || oss.Speaks(providers.APIAnthropicMessages) {
			t.Errorf("apis = %v, want what was recorded", oss.APIs)
		}
		if len(oss.Retention) != 2 {
			t.Errorf("retention = %v, want both modes", oss.Retention)
		}
		mythos := byRef[providers.MustModelRef("bedrock-mantle/anthropic.claude-mythos-5")]
		if mythos.Available() || mythos.StatusReason == "" {
			t.Errorf("an unavailable model came back as %+v", mythos)
		}

		// And a refresh of one leaves the other's rows alone.
		if rerr := db.ReplaceCatalogue(t.Context(), providers.KindBedrock, nil); rerr != nil {
			t.Fatalf("empty runtime: %v", rerr)
		}
		left, err := db.ListCatalogue(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 3 {
			t.Errorf("%d rows after emptying bedrock, want mantle's 3 left alone", len(left))
		}
	})
}

// route builds an ordinary route: one name, one model, available to everybody,
// and served right up to the hard limit.
func route(name, target string) Route {
	return Route{
		Route: policy.Route{
			Name:           name,
			Target:         providers.ModelRef{Provider: providers.KindBedrock, Model: target},
			ServesOverSoft: true,
		},
		EnabledBy: "admin@example.com",
	}
}

// TestARouteIsTheAllowlist. Every askable name is a route, and a route carries
// everything that decides what a request naming it becomes: who it exists for,
// and which model each condition sends it to.
func TestARouteIsTheAllowlist(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		plain := route("standard", "global.anthropic.claude-sonnet-5")
		plain.Description = "What every engineer gets."

		restricted := route("fast", "au.anthropic.claude-haiku-4-5-20251001-v1:0")
		restricted.Description = "Cheap and quick, kept in Australia."
		restricted.Teams = []string{"tm_platform"}
		restricted.Tools = []string{"claude-code"}
		restricted.ServesOverSoft = false

		for _, r := range []Route{plain, restricted} {
			if err := db.SaveRoute(t.Context(), r); err != nil {
				t.Fatalf("save route %s: %v", r.Name, err)
			}
		}

		got, err := db.ListRoutes(t.Context())
		if err != nil {
			t.Fatalf("list routes: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("%d routes, want 2", len(got))
		}

		byName := map[string]Route{}
		for _, r := range got {
			byName[r.Name] = r
		}

		if byName["standard"].Restricted() {
			t.Error("a route with no conditions reports itself as restricted")
		}
		fast := byName["fast"]
		if !fast.Restricted() {
			t.Error("a route with a team condition does not report itself as restricted")
		}
		if len(fast.Teams) != 1 || fast.Teams[0] != "tm_platform" {
			t.Errorf("teams = %v, want the stored condition", fast.Teams)
		}
		if len(fast.Tools) != 1 || fast.Tools[0] != "claude-code" {
			t.Errorf("tools = %v, want the stored condition", fast.Tools)
		}
		if fast.Target != providers.MustModelRef("bedrock/au.anthropic.claude-haiku-4-5-20251001-v1:0") {
			t.Errorf("target = %v, want the stored reference", fast.Target)
		}
		if fast.ServesOverSoft {
			t.Error("a route stored as stopping at the soft limit came back serving past one")
		}
		// The default is the one that must survive a round trip unasked: a
		// route read back as stopping when nobody said so would turn a soft
		// limit into a hard one for everybody it admits.
		if !byName["standard"].ServesOverSoft {
			t.Error("an ordinary route came back stopping at the soft limit")
		}
	})
}

// TestRepointingARouteKeepsItsIdentity. Changing which model a route runs on is
// the ordinary operation, and the name is what every request already recorded
// against it resolves through, so a repoint must be an update rather than a
// replacement.
func TestRepointingARouteKeepsItsIdentity(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		r := route("standard", "global.anthropic.claude-sonnet-5")
		if err := db.SaveRoute(t.Context(), r); err != nil {
			t.Fatalf("save: %v", err)
		}

		before, err := db.ListRoutes(t.Context())
		if err != nil {
			t.Fatalf("list routes: %v", err)
		}
		created := before[0].CreatedAt

		r.Target = providers.MustModelRef("bedrock-mantle/anthropic.claude-sonnet-5")
		r.ServesOverSoft = false
		if serr := db.SaveRoute(t.Context(), r); serr != nil {
			t.Fatalf("repoint: %v", serr)
		}

		got, err := db.ListRoutes(t.Context())
		if err != nil {
			t.Fatalf("list routes: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("%d routes after a repoint, want 1: an edit replaced the route rather than changing it", len(got))
		}
		if got[0].Target != providers.MustModelRef("bedrock-mantle/anthropic.claude-sonnet-5") {
			t.Errorf("target = %q, want the new provider and model", got[0].Target)
		}
		if got[0].ServesOverSoft {
			t.Error("the repoint did not save the change to whether the route survives a soft limit")
		}
		if !got[0].CreatedAt.Equal(created) {
			t.Error("the repoint lost the route's creation time")
		}
	})
}

// A route with nowhere to send a request would admit one and then refuse it, so
// the write path will not create one.
func TestARouteNeedsAModel(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		if err := db.SaveRoute(t.Context(),
			Route{Route: policy.Route{Name: "nowhere"}}); err == nil {
			t.Error("a route with no model was accepted")
		}
		if err := db.SaveRoute(t.Context(), route("", "x")); err == nil {
			t.Error("a route with no name was accepted")
		}
	})
}

func TestDeletingARoute(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		if err := db.SaveRoute(t.Context(), route("fast", "x")); err != nil {
			t.Fatalf("save: %v", err)
		}
		removed, err := db.DeleteRoute(t.Context(), "fast")
		if err != nil || !removed {
			t.Fatalf("delete existing route = %v %v", removed, err)
		}

		left, err := db.ListRoutes(t.Context())
		if err != nil {
			t.Fatalf("list routes: %v", err)
		}
		if len(left) != 0 {
			t.Errorf("%d routes after deleting the only one", len(left))
		}

		removed, err = db.DeleteRoute(t.Context(), "fast")
		if err != nil || removed {
			t.Fatalf("delete missing route = %v %v, want false", removed, err)
		}
	})
}

// TestAnUndiscoveredCatalogueIsDistinguishable. "No models" and "never
// discovered" look identical in a list and mean opposite things.
func TestAnUndiscoveredCatalogueIsDistinguishable(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		fetched, err := db.CatalogueFetchedAt(t.Context())
		if err != nil {
			t.Fatalf("CatalogueFetchedAt: %v", err)
		}
		if len(fetched) != 0 {
			t.Error("a database that has never discovered reports a fetch time")
		}
	})
}
