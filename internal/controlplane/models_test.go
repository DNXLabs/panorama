// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
)

// seedCatalogue puts the pair the whole design exists for into the fixture:
// one model reachable in two geographies, at two prices, on the bedrock
// provider.
func seedCatalogue(t *testing.T, f *consoleFixture) {
	t.Helper()

	messages := []providers.API{providers.APIAnthropicMessages}
	if err := f.db.ReplaceCatalogue(t.Context(), providers.KindBedrock, []store.CatalogueModel{
		{
			ID:                "au.anthropic.claude-haiku-4-5-20251001-v1:0",
			FoundationModelID: "anthropic.claude-haiku-4-5-20251001-v1:0",
			Vendor:            "Anthropic", DisplayName: "Claude Haiku 4.5",
			Geography: "au", Regions: []string{"ap-southeast-2", "ap-southeast-4"},
			Streaming: true, Lifecycle: "ACTIVE", RequiresProfile: true, APIs: messages,
		},
		{
			ID:                "global.anthropic.claude-haiku-4-5-20251001-v1:0",
			FoundationModelID: "anthropic.claude-haiku-4-5-20251001-v1:0",
			Vendor:            "Anthropic", DisplayName: "Claude Haiku 4.5",
			Geography: "global",
			Streaming: true, Lifecycle: "ACTIVE", RequiresProfile: true, APIs: messages,
		},
	}); err != nil {
		t.Fatalf("seed catalogue: %v", err)
	}
}

const (
	auHaiku     = "bedrock/au.anthropic.claude-haiku-4-5-20251001-v1:0"
	globalHaiku = "bedrock/global.anthropic.claude-haiku-4-5-20251001-v1:0"
)

// TestTheModelsScreenSaysWhereTrafficIsProcessed. Residency is the reason
// geography is a row rather than a flag, so it has to reach the client - a
// prefix like "au" is not an answer anybody can act on.
func TestTheModelsScreenSaysWhereTrafficIsProcessed(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	rec, body := f.send(t, http.MethodGet, "/api/v1/models", f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /models = %d: %s", rec.Code, rec.Body)
	}

	models, _ := body["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("%d models, want both geographies", len(models))
	}

	found := map[string]map[string]any{}
	for _, raw := range models {
		m, _ := raw.(map[string]any)
		ref, _ := m["ref"].(string)
		found[ref] = m
	}

	if got, want := found[auHaiku]["residency"], "ap-southeast-2, ap-southeast-4"; got != want {
		t.Errorf("au residency = %q, want %q", got, want)
	}
	if got, want := found[globalHaiku]["residency"], "any supported AWS region"; got != want {
		t.Errorf("global residency = %q, want %q", got, want)
	}
	// The provider and the vendor are two words now, and the screen needs both.
	if found[auHaiku]["provider"] != "bedrock" || found[auHaiku]["vendor"] != "Anthropic" {
		t.Errorf("provider %v, vendor %v", found[auHaiku]["provider"], found[auHaiku]["vendor"])
	}
	if apis, _ := found[auHaiku]["apis"].([]any); len(apis) != 1 || apis[0] != "anthropic-messages" {
		t.Errorf("apis = %v, want the API the row records", found[auHaiku]["apis"])
	}
}

// TestAnUnpricedModelSaysSo. Unpriced is null rather than zero, and the screen
// where somebody permits a model is where they should learn its requests will
// record no cost - not the cost report a week later.
func TestAnUnpricedModelSaysSo(t *testing.T) {
	f := newConsole(t)
	if err := f.db.ReplaceCatalogue(t.Context(), providers.KindBedrock, []store.CatalogueModel{{
		ID:                "us.example.model-nobody-prices",
		FoundationModelID: "example.model-nobody-prices",
		DisplayName:       "Unpriced Model", Geography: "us", Streaming: true,
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, body := f.send(t, http.MethodGet, "/api/v1/models", f.adminToken, nil)
	models, _ := body["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("%d models, want 1", len(models))
	}
	m, _ := models[0].(map[string]any)
	if priced, _ := m["priced"].(bool); priced {
		t.Error("a model with no rate reports itself priced")
	}
	if _, hasRates := m["rates"]; hasRates {
		t.Error("an unpriced model carries a rates object, which would render as zero")
	}
}

// TestARouteMustNameAModelThisDeploymentHasSeen. A typo accepted here fails at
// the desk of whoever's request is refused, hours later.
func TestARouteMustNameAModelThisDeploymentHasSeen(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	for name, target := range map[string]string{
		"a misspelling":      "bedrock/us.anthropic.claude-hiaku-4-5",
		"no provider":        "au.anthropic.claude-haiku-4-5-20251001-v1:0",
		"the wrong provider": "bedrock-mantle/au.anthropic.claude-haiku-4-5-20251001-v1:0",
		"an unknown one":     "openrouter/anthropic/claude",
	} {
		rec, body := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
			"name": "fast", "target": target,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %q = %d, want 400", name, target, rec.Code)
		}
		if msg, _ := body["error"].(string); msg == "" {
			t.Errorf("%s: the refusal names no reason", name)
		}
	}

	rec, _ := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "fast", "target": auHaiku,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("a discovered target = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// A model this build can list and cannot serve - one that speaks no API this
// gateway serves - is refused as a route's model, because a route to it would
// admit requests and have nothing that could carry them.
func TestARouteModelMustSpeakAnAPI(t *testing.T) {
	f := newConsole(t)
	if err := f.db.ReplaceCatalogue(t.Context(), providers.KindBedrock, []store.CatalogueModel{{
		ID: "meta.llama3-70b-instruct-v1:0", FoundationModelID: "meta.llama3-70b-instruct-v1:0",
		Vendor: "Meta", DisplayName: "Llama 3 70B", Streaming: true,
	}}); err != nil {
		t.Fatal(err)
	}

	rec, body := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "llama", "target": "bedrock/meta.llama3-70b-instruct-v1:0",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a browse-only model = %d, want 400", rec.Code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "speaks no API") {
		t.Errorf("the refusal does not say why: %q", msg)
	}
}

// A route to a provider that is turned off would admit requests with nowhere to
// send them; the refusal says where to turn it on.
func TestARouteOnADisabledProviderIsRefused(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)
	if _, err := f.db.SaveProvider(t.Context(), providers.Settings{
		Kind: providers.KindBedrock, Enabled: false, Region: "us-east-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.policy.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	rec, body := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "fast", "target": auHaiku,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a route on a disabled provider = %d, want 400", rec.Code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "turned off") {
		t.Errorf("the refusal does not say why: %q", msg)
	}
}

// TestARouteCannotBeAWildcard.
//
// This is the only door. There is no second source of routes - no file, no
// environment variable - so what this refuses is refused everywhere, and
// "everything is permitted" is not a state this product can be in.
func TestARouteCannotBeAWildcard(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	for _, name := range []string{"*", "claude-*", "*-haiku"} {
		rec, body := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
			"name": name, "target": globalHaiku,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("a route named %q = %d, want 400", name, rec.Code)
		}
		if msg, _ := body["error"].(string); msg == "" {
			t.Errorf("refusing %q says nothing about why", name)
		}
	}
}

// TestARouteIsTheAllowlistAndReachesTheGateway. The point of the table: a name
// saved here is a name the resolver will accept after the next policy reload.
func TestARouteIsTheAllowlistAndReachesTheGateway(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	rec, _ := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "fast", "target": auHaiku,
		"description": "Cheap and quick, kept in Australia.",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /routes = %d: %s", rec.Code, rec.Body)
	}

	if err := f.policy.Load(t.Context()); err != nil {
		t.Fatalf("reload policy: %v", err)
	}
	route, ok := f.policy.Snapshot().Route("fast")
	if !ok {
		t.Fatal("the saved route is not in the policy snapshot, so the gateway would refuse the name")
	}
	if route.Target != providers.MustModelRef(auHaiku) {
		t.Errorf("the snapshot routes to %q, want the saved target", route.Target)
	}
	// A route saved without saying anything about soft limits serves past one,
	// which is what every route did before the flag existed. Defaulting the
	// other way would turn a warning into a block for everybody it admits.
	if !route.ServesOverSoft {
		t.Error("a route saved with no serves_over_soft field stops at the soft limit")
	}

	// And removing it takes the name away again.
	rec, _ = f.send(t, http.MethodDelete, "/api/v1/routes/fast", f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /routes/fast = %d: %s", rec.Code, rec.Body)
	}
	if err := f.policy.Load(t.Context()); err != nil {
		t.Fatalf("reload policy: %v", err)
	}
	if _, still := f.policy.Snapshot().Route("fast"); still {
		t.Error("a removed route is still in the snapshot")
	}
}

// TestARouteNeedsAModel. A route with nowhere to send a request would admit one
// and then refuse it, which is a failure the engineer cannot act on and the
// administrator never sees.
func TestARouteNeedsAModel(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	rec, body := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "platform-only", "description": "Nowhere to go.",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT /routes with no model = %d, want 400: %s", rec.Code, rec.Body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "model") {
		t.Errorf("the refusal does not say what is missing: %q", msg)
	}
}

// A route is one model. A body carrying a second one is refused as a field the
// server does not know, rather than dropped: dropping it would save a route
// that did not do what its editor showed.
func TestARouteWithAFallbackChainIsRefused(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	rec, _ := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "fast", "target": auHaiku, "fallbacks": []string{globalHaiku},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a route with a fallback chain = %d, want 400", rec.Code)
	}
}

// TestARouteCarriesItsConditionsAndItsModel. The two halves of a route reach
// the console together, because a screen that showed one without the other
// would show a restriction whose effect it could not explain.
func TestARouteCarriesItsConditionsAndItsModel(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	rec, saved := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name":             "fast",
		"target":           auHaiku,
		"tools":            []string{"claude-code"},
		"serves_over_soft": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /routes = %d: %s", rec.Code, rec.Body)
	}
	// The only route on this deployment stops at the soft limit, so everybody
	// it admits will be refused with nowhere to go. Said rather than refused -
	// stopping may be exactly what the administrator meant.
	if warning, _ := saved["warning"].(string); !strings.Contains(warning, "nothing to switch to") {
		t.Errorf("saving the only route as stopping at the soft limit warned %q", warning)
	}

	_, body := f.send(t, http.MethodGet, "/api/v1/models", f.adminToken, nil)
	routes, _ := body["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("%d routes, want 1", len(routes))
	}
	route, _ := routes[0].(map[string]any)

	if complete, _ := route["complete"].(bool); !complete {
		t.Error("a route with a model reports itself incomplete")
	}
	if restricted, _ := route["restricted"].(bool); !restricted {
		t.Error("a route with a tool condition does not report itself restricted")
	}
	if target, _ := route["target"].(string); target != auHaiku {
		t.Errorf("target = %q, want the saved model", target)
	}
	if route["provider"] != "bedrock" {
		t.Errorf("provider = %v, want bedrock", route["provider"])
	}
	if apis, _ := route["apis"].([]any); len(apis) != 1 || apis[0] != "anthropic-messages" {
		t.Errorf("apis = %v, want what the gateway would serve it on", route["apis"])
	}
	if serves, _ := route["serves_over_soft"].(bool); serves {
		t.Error("serves_over_soft: false did not survive the round trip")
	}
	if _, has := route["fallbacks"]; has {
		t.Error("a route still carries a fallback chain")
	}
	if _, has := route["model"]; !has {
		t.Error("a route naming a discovered model carries no model detail, so the screen cannot price it")
	}
}

// A warning about having nowhere to switch to is asked per person, not
// globally: conditions make "there is another route" and "there is another
// route for you" different questions, and a deployment can have an economy
// route that the stopped team is not admitted to.
func TestAStopIsWarnedAboutOnlyWhenNobodyCanSwitch(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	if rec, _ := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "economy", "target": globalHaiku,
	}); rec.Code != http.StatusOK {
		t.Fatalf("creating the economy route: %d: %s", rec.Code, rec.Body)
	}

	rec, saved := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, map[string]any{
		"name": "standard", "target": auHaiku,
		"serves_over_soft": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /routes = %d: %s", rec.Code, rec.Body)
	}
	if warning, has := saved["warning"]; has {
		t.Errorf("warned about a stop that has an unrestricted route to fall back to: %v", warning)
	}
}

// A route restricted to a team is not served to somebody outside it, and
// neither is the fact that it exists. The gateway refuses the name the same
// way; a console that listed it would contradict the refusal.
func TestADeveloperSeesOnlyTheRoutesTheyCanUse(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	team, err := f.db.CreateTeam(t.Context(), "Platform", "platform", "")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	for _, route := range []map[string]any{
		{"name": "standard", "target": globalHaiku},
		{"name": "platform", "target": auHaiku, "teams": []string{team.ID}},
	} {
		if rec, _ := f.send(t, http.MethodPut, "/api/v1/routes", f.adminToken, route); rec.Code != http.StatusOK {
			t.Fatalf("PUT /routes %v = %d", route["name"], rec.Code)
		}
	}

	_, body := f.send(t, http.MethodGet, "/api/v1/models", f.devToken, nil)
	routes, _ := body["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("a developer sees %d routes, want only the one they can use", len(routes))
	}
	route, _ := routes[0].(map[string]any)
	if name, _ := route["name"].(string); name != "standard" {
		t.Errorf("a developer sees %q", name)
	}
	// The conditions name people, and a developer is served the routes they can
	// use rather than the reasons somebody else cannot.
	if _, has := route["users"]; has {
		t.Error("a developer is served a route's conditions, which name colleagues")
	}

	_, body = f.send(t, http.MethodGet, "/api/v1/models", f.adminToken, nil)
	routes, _ = body["routes"].([]any)
	if len(routes) != 2 {
		t.Errorf("an administrator sees %d routes, want both", len(routes))
	}
}

// TestOnlyAnAdministratorChoosesModels.
//
// Reading the allowlist is open to anybody signed in - an engineer whose request
// was refused should be able to see what they may ask for instead, and Budgets
// and Integrations both render route names to whoever opens them. Changing it is
// not: a route decides what a company pays for and which country a prompt is
// processed in.
//
// The two halves are asserted together because the failure that matters is them
// disagreeing: a screen that offers a control the server then refuses reads as a
// bug, and one that hides a control the server would have allowed is a feature
// nobody finds.
func TestOnlyAnAdministratorChoosesModels(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	rec, read := f.send(t, http.MethodGet, "/api/v1/models", f.devToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a developer reading /models = %d, want 200: %s", rec.Code, rec.Body)
	}
	if canEdit, _ := read["can_edit"].(bool); canEdit {
		t.Error("the console offers a developer edits the server would refuse")
	}

	rec, _ = f.send(t, http.MethodPut, "/api/v1/routes", f.devToken, map[string]any{
		"name": "fast", "target": auHaiku,
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a developer creating a route = %d, want 403", rec.Code)
	}

	rec, _ = f.send(t, http.MethodDelete, "/api/v1/routes/fast", f.devToken, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a developer removing a route = %d, want 403", rec.Code)
	}

	_, read = f.send(t, http.MethodGet, "/api/v1/models", f.adminToken, nil)
	if canEdit, _ := read["can_edit"].(bool); !canEdit {
		t.Error("the console hides edits from an administrator, who may make them")
	}
}

// TestAnUndiscoveredDeploymentSaysSo. "No models" and "never discovered" look
// identical in a list and mean opposite things.
func TestAnUndiscoveredDeploymentSaysSo(t *testing.T) {
	f := newConsole(t)

	_, body := f.send(t, http.MethodGet, "/api/v1/models", f.adminToken, nil)
	if discovered, _ := body["discovered"].(bool); discovered {
		t.Error("a deployment that has never discovered reports that it has")
	}
	if _, has := body["discovered_at"]; has {
		t.Error("a deployment that has never discovered carries a fetch time")
	}
}

// TestDiscoveryNeedsAProvider. Reported as a refusal that names the fix rather
// than as an empty account, which is what it would otherwise look like.
func TestDiscoveryNeedsAProvider(t *testing.T) {
	f := newConsole(t, withoutProviders)

	rec, body := f.send(t, http.MethodPost, "/api/v1/models/discover", f.adminToken, map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("discover with no provider = %d, want 400", rec.Code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "Model providers") {
		t.Errorf("the refusal names no fix: %q", msg)
	}
}

// Discovery asks the serving provider and writes what it found, scoped to it.
func TestDiscoveryWritesTheProvidersCatalogue(t *testing.T) {
	f := newConsole(t)
	f.provider.setCatalogue([]providers.CatalogueEntry{{
		ID: "us.anthropic.claude-sonnet-5", FoundationModelID: "anthropic.claude-sonnet-5",
		Vendor: "Anthropic", DisplayName: "Claude Sonnet 5", Streaming: true,
		APIs: []providers.API{providers.APIAnthropicMessages},
	}})

	rec, body := f.send(t, http.MethodPost, "/api/v1/models/discover", f.adminToken, map[string]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("discover = %d: %s", rec.Code, rec.Body)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("discover reported failure: %v", body)
	}
	catalogue, err := f.db.ListCatalogue(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalogue) != 1 || catalogue[0].Provider != providers.KindBedrock {
		t.Errorf("catalogue = %+v, want the one row on bedrock", catalogue)
	}
}

// recordingProvider answers a live check and remembers what it was asked to
// invoke, and serves as the catalogue a test hands it.
type recordingProvider struct {
	mu        sync.Mutex
	invoked   []string
	apis      []providers.API
	err       error
	catalogue []providers.CatalogueEntry
	// catalogueErr is what discovery fails with, when a test wants it to.
	catalogueErr error
}

func (p *recordingProvider) Name() string { return "bedrock" }

func (p *recordingProvider) Invoke(_ context.Context, req *providers.Request) (*providers.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invoked = append(p.invoked, req.Model)
	p.apis = append(p.apis, req.API)
	if p.err != nil {
		return nil, p.err
	}
	return &providers.Response{Body: []byte(`{}`)}, nil
}

func (p *recordingProvider) InvokeStream(context.Context, *providers.Request) (providers.EventStream, error) {
	return nil, errors.New("the setup check never streams")
}

func (p *recordingProvider) Catalogue(context.Context) ([]providers.CatalogueEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.catalogueErr != nil {
		return nil, p.catalogueErr
	}
	return p.catalogue, nil
}

func (p *recordingProvider) setCatalogue(entries []providers.CatalogueEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.catalogue = entries
}

// testDescriptors registers both Bedrock kinds as the recording provider, with
// the API table reduced to its three cases: Claude speaks Messages, OpenAI's
// models speak both OpenAI APIs, and Qwen speaks Chat Completions alone.
func testDescriptors(p *recordingProvider) []providers.Descriptor {
	build := func(_ context.Context, s providers.Settings, _ *slog.Logger) (*providers.Instance, error) {
		if !strings.Contains(s.Region, "-") {
			return nil, errors.New("bedrock: \"" + s.Region + "\" is not an AWS region name, like us-east-1")
		}
		return &providers.Instance{
			Provider:  p,
			Catalogue: p,
			APIs: func(model string) []providers.API {
				switch {
				case strings.Contains(model, "anthropic."):
					return []providers.API{providers.APIAnthropicMessages}
				case strings.HasPrefix(model, "openai."):
					return []providers.API{providers.APIOpenAIChat, providers.APIOpenAIResponses}
				case strings.HasPrefix(model, "qwen."):
					return []providers.API{providers.APIOpenAIChat}
				default:
					return nil
				}
			},
		}, nil
	}
	hint := func(string) string { return "the hint for this failure" }
	return []providers.Descriptor{
		{Kind: providers.KindBedrock, Label: "Amazon Bedrock", Regions: []string{"us-east-1"},
			Build: build, Hint: hint, Policy: `{"Statement":[]}`,
			Endpoint: func(r string) string { return "bedrock-runtime." + r + ".amazonaws.com" }},
		{Kind: providers.KindBedrockMantle, Label: "Amazon Bedrock (Mantle)", Regions: []string{"us-east-1"},
			Build: build, Hint: hint, Policy: `{"Statement":[]}`},
	}
}

// withoutProviders is a deployment with no provider configured: the state
// before anybody has been through setup.
func withoutProviders(o *ConsoleOptions) {
	o.Providers = providers.NewRegistry(nil)
}

func setupStates(t *testing.T, f *consoleFixture) map[string]string {
	t.Helper()
	_, state := f.send(t, http.MethodGet, "/api/v1/setup", f.adminToken, nil)
	steps, _ := state["steps"].([]any)
	got := map[string]string{}
	for _, raw := range steps {
		row, _ := raw.(map[string]any)
		id, _ := row["id"].(string)
		st, _ := row["state"].(string)
		got[id] = st
	}
	return got
}

// TestTheProviderCheckNeedsNoRoute.
//
// The check answers "can this deployment reach its provider at all", and it has
// to answer that before any route exists - which is the one moment an operator
// most needs it.
func TestTheProviderCheckNeedsNoRoute(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	if routes, err := f.db.ListRoutes(t.Context()); err != nil {
		t.Fatal(err)
	} else if len(routes) != 0 {
		t.Fatalf("the fixture has %d routes; this test is about having none", len(routes))
	}
	if got := setupStates(t, f)["provider"]; got != stateTodo {
		t.Errorf("provider step = %q before any check, want todo: configured is not verified", got)
	}

	rec, body := f.send(t, http.MethodPost, "/api/v1/providers/bedrock/check", f.adminToken, map[string]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("check with no routes = %d, want 200: %s", rec.Code, rec.Body)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("check reported failure: %v", body)
	}
	if len(f.provider.invoked) != 1 {
		t.Fatalf("the provider was invoked %d times, want once", len(f.provider.invoked))
	}

	// The cheaper of the two geographies the fixture offers. Not a detail: the
	// check spends real money on somebody's account, and picking the pricier
	// profile to prove a connection is a charge nobody asked for.
	if got := f.provider.invoked[0]; got != "global.anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Errorf("verified against %q, want the cheapest discovered target", got)
	}
	if f.provider.apis[0] != providers.APIAnthropicMessages {
		t.Errorf("checked over %q, want the API the model speaks", f.provider.apis[0])
	}

	// Deterministic, so clicking check twice checks the same model twice.
	f.send(t, http.MethodPost, "/api/v1/providers/bedrock/check", f.adminToken, map[string]string{})
	if len(f.provider.invoked) != 2 || f.provider.invoked[0] != f.provider.invoked[1] {
		t.Errorf("two checks chose %v, want the same target twice", f.provider.invoked)
	}

	got := setupStates(t, f)
	if got["provider"] != stateDone {
		t.Errorf("provider = %q after a successful invocation, want done", got["provider"])
	}
	if got["models"] != stateTodo {
		t.Errorf("models = %q with no routes, want todo", got["models"])
	}
}

// The check sends a body in the API the model speaks, and will not invoke a
// model that speaks none, is legacy, cannot stream, or is listed as
// unavailable - each of which would report a perfectly good connection as
// broken, or pass today and fail on a date nobody has in their calendar.
//
// The provider reports the same four, because a check with nothing to pick
// from asks it: a catalogue only the database held would be replaced by an
// empty one, and the test would pass for having nothing in it at all.
func TestTheProviderCheckOnlyInvokesAModelItCanServe(t *testing.T) {
	f := newConsole(t)

	messages := []providers.API{providers.APIAnthropicMessages}
	f.provider.setCatalogue([]providers.CatalogueEntry{
		{ID: "amazon.titan-text-express-v1", Vendor: "Amazon", Streaming: true, Lifecycle: "ACTIVE"},
		{ID: "anthropic.claude-v2", Vendor: "Anthropic", Streaming: true, Lifecycle: "LEGACY", APIs: messages},
		{ID: "us.anthropic.claude-opus-4-1", Vendor: "Anthropic", Streaming: false, APIs: messages},
		{ID: "anthropic.claude-mythos-5", Vendor: "Anthropic", Streaming: true, Status: "unavailable", APIs: messages},
	})

	rec, body := f.send(t, http.MethodPost, "/api/v1/providers/bedrock/check", f.adminToken, map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("check with nothing eligible = %d, want 400: %s", rec.Code, rec.Body)
	}
	if len(f.provider.invoked) != 0 {
		t.Errorf("invoked %v; none of these can be served", f.provider.invoked)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "name a model identifier") {
		t.Errorf("error = %q, want it to name the fix", msg)
	}
	// It has just been refreshed, so sending somebody to refresh it is sending
	// them round a loop.
	if strings.Contains(msg, "Refresh the catalogue") {
		t.Errorf("error = %q, which asks for the refresh this check already made", msg)
	}
	if catalogue, err := f.db.ListCatalogue(t.Context()); err != nil {
		t.Fatal(err)
	} else if len(catalogue) != 4 {
		t.Errorf("the catalogue holds %d rows, want the four the provider reported", len(catalogue))
	}
}

// TestACheckWithNoCatalogueAsksTheProvider.
//
// A deployment's first discovery runs a few seconds after it starts, and a
// check pressed before then - or after that discovery failed and the fault was
// fixed - had nothing to pick from and refused, pointing at a refresh button on
// another screen. The check reads the catalogue itself instead, and carries on.
func TestACheckWithNoCatalogueAsksTheProvider(t *testing.T) {
	f := newConsole(t)
	f.provider.setCatalogue([]providers.CatalogueEntry{{
		ID: "global.anthropic.claude-haiku-4-5-20251001-v1:0", FoundationModelID: "anthropic.claude-haiku-4-5-20251001-v1:0",
		Vendor: "Anthropic", DisplayName: "Claude Haiku 4.5", Geography: "global",
		Streaming: true, Lifecycle: "ACTIVE", RequiresProfile: true,
		APIs: []providers.API{providers.APIAnthropicMessages},
	}})

	rec, body := f.send(t, http.MethodPost, "/api/v1/providers/bedrock/check", f.adminToken, map[string]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("check with an empty catalogue = %d, want 200: %s", rec.Code, rec.Body)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("check reported failure: %v", body)
	}
	if len(f.provider.invoked) != 1 || f.provider.invoked[0] != "global.anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Errorf("invoked %v, want the one target discovery found", f.provider.invoked)
	}
	if got := setupStates(t, f)["provider"]; got != stateDone {
		t.Errorf("provider = %q after a successful invocation, want done", got)
	}
}

// TestACheckThatCannotReadTheCatalogueSaysWhy.
//
// With expired AWS credentials, discovery fails at startup and the catalogue is
// empty, so a check that refused with "refresh the catalogue first" would say
// nothing about credentials - from the one control on the screen that claims
// to prove them. Credentials that cannot list models are the check's finding,
// reported as one: a 200 carrying the
// provider's own error and the hint, exactly as a failed invocation is.
func TestACheckThatCannotReadTheCatalogueSaysWhy(t *testing.T) {
	f := newConsole(t)
	f.provider.catalogueErr = errors.New("get credentials: failed to refresh cached credentials")

	rec, body := f.send(t, http.MethodPost, "/api/v1/providers/bedrock/check", f.adminToken, map[string]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("a check that could not read the catalogue = %d, want 200 carrying the failure: %s", rec.Code, rec.Body)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatal("a provider that could not be read reported success")
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "failed to refresh cached credentials") {
		t.Errorf("error = %q, want the provider's own words", msg)
	}
	if hint, _ := body["hint"].(string); hint == "" {
		t.Error("a failed check carries no hint")
	}
	if len(f.provider.invoked) != 0 {
		t.Errorf("invoked %v with nothing discovered to invoke", f.provider.invoked)
	}
	if got := setupStates(t, f)["provider"]; got != stateTodo {
		t.Errorf("provider = %q after a failed check, want todo", got)
	}
}

// The catalogue is a cache and the provider is the authority, so an operator
// who knows their model id can hand it over and find out what the provider
// says - in the API that model speaks.
func TestANamedTargetIsTriedAsGiven(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)

	rec, _ := f.send(t, http.MethodPost, "/api/v1/providers/bedrock/check", f.adminToken,
		map[string]string{"model": "openai.gpt-oss-120b"})
	if rec.Code != http.StatusOK {
		t.Fatalf("check a named target = %d, want 200: %s", rec.Code, rec.Body)
	}
	if len(f.provider.invoked) != 1 || f.provider.invoked[0] != "openai.gpt-oss-120b" {
		t.Errorf("invoked %v, want the identifier that was asked for", f.provider.invoked)
	}
	if f.provider.apis[0] != providers.APIOpenAIChat {
		t.Errorf("checked over %q, want the first API the model speaks", f.provider.apis[0])
	}
}

// A failed check says what failed and what to do about it, and does not mark
// the provider verified.
func TestAFailedCheckCarriesTheHint(t *testing.T) {
	f := newConsole(t)
	seedCatalogue(t, f)
	f.provider.err = errors.New("AccessDeniedException: not authorized")

	rec, body := f.send(t, http.MethodPost, "/api/v1/providers/bedrock/check", f.adminToken, map[string]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("a failed check = %d, want 200 carrying the failure", rec.Code)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatal("a failed invocation reported success")
	}
	if hint, _ := body["hint"].(string); hint == "" {
		t.Error("a failed check carries no hint")
	}
	if got := setupStates(t, f)["provider"]; got != stateTodo {
		t.Errorf("provider = %q after a failed check, want todo", got)
	}
}
