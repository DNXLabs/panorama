// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/providers"
)

func providerRows(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, row := range mapsOf(body["providers"]) {
		kind, _ := row["kind"].(string)
		out[kind] = row
	}
	return out
}

// Settings, Model providers says, for every kind this build can serve, whether
// it is configured and in which of five states - and serves the IAM policy the
// role needs beside the list, rendered from the value the template is tested
// against.
func TestTheProvidersScreenStatesEachProvider(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodGet, "/api/v1/providers", f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /providers = %d: %s", rec.Code, rec.Body)
	}
	rows := providerRows(t, body)
	if len(rows) != 2 {
		t.Fatalf("%d providers, want both kinds this build serves", len(rows))
	}
	if got := rows["bedrock"]["state"]; got != providerNeverChecked {
		t.Errorf("a configured, never-checked provider reads %q", got)
	}
	if got := rows["bedrock-mantle"]["state"]; got != providerNotConfigured {
		t.Errorf("an unconfigured provider reads %q", got)
	}
	if policy, _ := body["iam_policy"].(string); !strings.Contains(policy, "Statement") {
		t.Errorf("the IAM policy was not served: %q", body["iam_policy"])
	}
	if can, _ := body["can_configure"].(bool); !can {
		t.Error("an administrator is told they cannot configure providers")
	}

	// A check moves it to connected.
	seedCatalogue(t, f)
	f.send(t, http.MethodPost, "/api/v1/providers/bedrock/check", f.adminToken, map[string]string{})
	_, body = f.send(t, http.MethodGet, "/api/v1/providers", f.adminToken, nil)
	if got := providerRows(t, body)["bedrock"]["state"]; got != providerConnected {
		t.Errorf("after a successful check the provider reads %q, want connected", got)
	}
}

// Configuring a provider writes the row, builds it into the snapshot, and
// discovers what it can invoke - so the catalogue for a new region is there by
// the time the save returns.
func TestConfiguringAProviderDiscoversIt(t *testing.T) {
	f := newConsole(t)
	f.provider.setCatalogue([]providers.CatalogueEntry{{
		ID: "openai.gpt-oss-120b", FoundationModelID: "openai.gpt-oss-120b", Vendor: "OpenAI",
		Streaming: true, APIs: []providers.API{providers.APIOpenAIChat},
	}})

	rec, body := f.send(t, http.MethodPut, "/api/v1/providers/bedrock-mantle", f.adminToken,
		map[string]any{"region": "us-west-2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /providers/bedrock-mantle = %d: %s", rec.Code, rec.Body)
	}
	if discovered, _ := body["discovered"].(float64); discovered != 1 {
		t.Errorf("discovered = %v, want the one model the provider lists", body["discovered"])
	}

	stored, found, err := f.db.Provider(t.Context(), providers.KindBedrockMantle)
	if err != nil || !found || stored.Region != "us-west-2" || !stored.Enabled {
		t.Fatalf("stored = %+v (found %v, err %v)", stored, found, err)
	}
	if state := f.policy.Snapshot().Providers[providers.KindBedrockMantle]; !state.Serving() {
		t.Errorf("the saved provider is not serving in the snapshot: %+v", state)
	}
	catalogue, err := f.db.ListCatalogue(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalogue) != 1 || catalogue[0].Provider != providers.KindBedrockMantle {
		t.Errorf("catalogue = %+v, want the new provider's model", catalogue)
	}
}

// A configuration that cannot build is refused before anything is written, in
// words that say what is wrong.
func TestAProviderThatCannotBuildIsRefused(t *testing.T) {
	f := newConsole(t)

	rec, body := f.send(t, http.MethodPut, "/api/v1/providers/bedrock-mantle", f.adminToken,
		map[string]any{"region": "moon"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a region that is not a region = %d, want 400", rec.Code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "not an AWS region") {
		t.Errorf("error = %q", msg)
	}
	if _, found, _ := f.db.Provider(t.Context(), providers.KindBedrockMantle); found {
		t.Error("a configuration that could not build was stored")
	}

	for name, change := range map[string]map[string]any{
		"no region":         {},
		"an unknown kind":   {"region": "us-east-1"},
		"a negative window": {"region": "us-east-1", "options": map[string]any{"context_windows": map[string]int{"x": -1}}},
	} {
		path := "/api/v1/providers/bedrock-mantle"
		if name == "an unknown kind" {
			path = "/api/v1/providers/openrouter"
		}
		if rec, _ := f.send(t, http.MethodPut, path, f.adminToken, change); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, rec.Code)
		}
	}
}

// Turning off a provider that routes use would leave those routes admitting
// requests with nowhere to send them, so it is refused, naming the routes.
func TestAProviderInUseCannotBeTurnedOff(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)
	if rec, _ := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "fast", "target": auHaiku,
	}); rec.Code != http.StatusOK {
		t.Fatalf("create route = %d", rec.Code)
	}

	rec, body := f.send(t, http.MethodPut, "/api/v1/providers/bedrock", f.adminToken,
		map[string]any{"enabled": false})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disabling a provider in use = %d, want 400", rec.Code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "fast") {
		t.Errorf("the refusal does not name the route: %q", msg)
	}

	if rec, _ := f.send(t, http.MethodDelete, "/api/v1/routes/fast", f.adminToken, nil); rec.Code != http.StatusOK {
		t.Fatal("delete route")
	}
	if rec, _ := f.send(t, http.MethodPut, "/api/v1/providers/bedrock", f.adminToken,
		map[string]any{"enabled": false}); rec.Code != http.StatusOK {
		t.Errorf("disabling a provider nothing uses = %d, want 200", rec.Code)
	}
}

// Preflight answers what a candidate region offers and which routes it would
// strand, and writes nothing - so a region somebody is only considering never
// becomes the one the gateway serves from.
func TestPreflightStrandsNothingItDoesNotWrite(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)
	if rec, _ := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "fast", "target": auHaiku,
	}); rec.Code != http.StatusOK {
		t.Fatal("create route")
	}
	// The candidate region lists only the global profile.
	f.provider.setCatalogue([]providers.CatalogueEntry{{
		ID: "global.anthropic.claude-haiku-4-5-20251001-v1:0", Vendor: "Anthropic", Streaming: true,
		APIs: []providers.API{providers.APIAnthropicMessages},
	}})

	rec, body := f.send(t, http.MethodPost, "/api/v1/providers/bedrock/preflight", f.adminToken,
		map[string]any{"region": "eu-west-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight = %d: %s", rec.Code, rec.Body)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("preflight failed: %v", body)
	}
	if stranded, _ := body["stranded"].([]any); len(stranded) != 1 || stranded[0] != "fast" {
		t.Errorf("stranded = %v, want the route whose model the region does not list", body["stranded"])
	}

	stored, _, _ := f.db.Provider(t.Context(), providers.KindBedrock)
	if stored.Region != "us-east-1" {
		t.Errorf("preflight changed the region to %q", stored.Region)
	}
	catalogue, _ := f.db.ListCatalogue(t.Context())
	if len(catalogue) != 2 {
		t.Errorf("preflight rewrote the catalogue: %d rows", len(catalogue))
	}
}

// How a deployment reaches its providers is deployment configuration, which is
// what a setup session is for - so it may configure and check them. It may not
// read or choose routes, which are an administrator's.
func TestASetupSessionConfiguresProvidersAndNothingMore(t *testing.T) {
	f := newConsole(t)
	cookie, csrf := redeemSetupCode(t, f.server, f.db)

	if rec := getWith(t, f.server, "/api/v1/providers", withSetup(cookie, csrf)); rec.Code != http.StatusOK {
		t.Fatalf("a setup session reading providers = %d, want 200", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/bedrock-mantle",
		strings.NewReader(`{"region":"us-west-2"}`))
	req.Header.Set("Content-Type", "application/json")
	withSetup(cookie, csrf)(req)
	rec := httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a setup session configuring a provider = %d, want 200: %s", rec.Code, rec.Body)
	}

	// Without the CSRF header, a write is refused like every other.
	req = httptest.NewRequest(http.MethodPut, "/api/v1/providers/bedrock-mantle",
		strings.NewReader(`{"region":"us-east-1"}`))
	req.Header.Set("Content-Type", "application/json")
	withSetup(cookie, "")(req)
	rec = httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a provider write without CSRF = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/v1/routes", strings.NewReader(`{"name":"x","target":"bedrock/y"}`))
	req.Header.Set("Content-Type", "application/json")
	withSetup(cookie, csrf)(req)
	rec = httptest.NewRecorder()
	f.server.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a setup session writing a route = %d, want 403", rec.Code)
	}

	// A developer may do none of it.
	if rec, _ := f.send(t, http.MethodGet, "/api/v1/providers", f.devToken, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a developer reading providers = %d, want 403", rec.Code)
	}
	if _, found, _ := f.db.Provider(t.Context(), providers.KindBedrockMantle); !found {
		t.Error("the setup session's write did not land")
	}
}
