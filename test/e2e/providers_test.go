// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/fakebedrock"
	"github.com/dnxlabs/panorama/internal/pricing"
)

const codexUA = "codex_cli_rs/0.40.0 (Mac OS 15.6.0; arm64) iTerm.app/3.6.1"

// post sends one data-plane request to any surface and drains it.
func (d *deployment) post(path, userAgent string, body map[string]any, headers ...string) (int, string) {
	d.t.Helper()

	// Encoded the way the clients encode, without HTML escaping. Go's default
	// writes < as \u003c, which no coding tool sends - and Codex's environment
	// block is written in angle brackets.
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		d.t.Fatalf("encoding the body: %v", err)
	}
	req, err := http.NewRequestWithContext(d.t.Context(), http.MethodPost, d.baseURL+path, &raw)
	if err != nil {
		d.t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	d.sent.Add(1)
	resp, err := d.client().Do(req)
	if err != nil {
		d.t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		d.t.Fatalf("reading the response: %v", err)
	}
	return resp.StatusCode, string(out)
}

func chatBody(route string, stream bool, text string) map[string]any {
	return map[string]any{
		"model":  route,
		"stream": stream,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding agent. Working directory: /work/panorama-e2e"},
			map[string]any{"role": "user", "content": text},
		},
	}
}

func responsesBody(route string, stream bool, text string) map[string]any {
	return map[string]any{
		"model":        route,
		"stream":       stream,
		"instructions": "You are Codex.",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "<environment_context>\n  <cwd>/work/panorama-e2e</cwd>\n</environment_context>"},
			}},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": text},
			}},
		},
	}
}

// lastMantleOn is the last request the fake received on one Mantle path.
func (d *deployment) lastMantleOn(t *testing.T, path string) fakebedrock.Received {
	t.Helper()
	received := d.Fake.Received()
	for i := len(received) - 1; i >= 0; i-- {
		if received[i].Endpoint == fakebedrock.EndpointMantle && received[i].Path == path {
			return received[i]
		}
	}
	t.Fatalf("no request reached bedrock-mantle at %s", path)
	return fakebedrock.Received{}
}

// lastMantle is the last request the fake received on a Mantle path.
func (d *deployment) lastMantle(t *testing.T) fakebedrock.Received {
	t.Helper()
	received := d.Fake.Received()
	for i := len(received) - 1; i >= 0; i-- {
		if received[i].Endpoint == fakebedrock.EndpointMantle {
			return received[i]
		}
	}
	t.Fatal("no request reached bedrock-mantle")
	return fakebedrock.Received{}
}

// Claude on bedrock-mantle: the same Messages API a client already speaks,
// sent to a different endpoint, signed for a different service, with the model
// in the body and no inference profile - and metered and priced like Claude on
// runtime, because AWS bills the two identically.
func TestMantleServesClaudeOverTheMessagesAPI(t *testing.T) {
	d := newDeployment(t)

	status, body := d.send(claudeCodeUA, messagesBody(routeMantleSonnet, true, "hello from mantle"),
		"anthropic-beta", "context-1m-2025-08-07,redact-thinking-2026-02-12")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") {
		t.Errorf("the stream is not the Anthropic event stream:\n%s", body)
	}

	got := d.latest()
	d.requireServed(t, got)
	if got.Provider != "bedrock-mantle" || got.ResolvedModel != fakebedrock.MantleSonnet5 || got.Surface != "anthropic" {
		t.Errorf("recorded %s/%s on %s", got.Provider, got.ResolvedModel, got.Surface)
	}

	sent := d.lastMantle(t)
	if sent.Path != "/anthropic/v1/messages" {
		t.Errorf("sent to %s, want /anthropic/v1/messages", sent.Path)
	}
	if sent.SigningService != "bedrock-mantle" || sent.SigningRegion != "us-east-1" {
		t.Errorf("signed for %q in %q, want bedrock-mantle in us-east-1 - bedrock:InvokeModel does not authorise Mantle",
			sent.SigningService, sent.SigningRegion)
	}
	if sent.AnthropicVersion == "" {
		t.Error("no anthropic-version header was sent")
	}
	if sent.Model != fakebedrock.MantleSonnet5 {
		t.Errorf("the body named %q, want the Mantle model id", sent.Model)
	}
	if strings.Join(sent.Betas, ",") != "context-1m-2025-08-07" {
		t.Errorf("betas = %v, want the unsupported one dropped", sent.Betas)
	}

	u := fakebedrock.DefaultUsage
	if got.Tokens.Input != u.InputTokens || got.Tokens.CacheRead != u.CacheReadTokens ||
		got.Tokens.CacheWrite != u.Total() || got.Tokens.CacheWrite1h != u.CacheWrite1hTokens {
		t.Errorf("tokens = %+v, want every class the fake reported", got.Tokens)
	}

	entries, err := pricing.Seed()
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := pricing.NewTable(entries).Price("bedrock-mantle", fakebedrock.MantleSonnet5, pricing.Usage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens,
		CacheWriteTokens: u.Total(), CacheWrite1hTokens: u.CacheWrite1hTokens,
	})
	if err != nil {
		t.Fatalf("pricing the fixture usage: %v", err)
	}
	if !got.Priced || got.Cost.TotalNanos != int64(want.Total) {
		t.Errorf("cost = %d (priced %v), want %d at the in-region rate", got.Cost.TotalNanos, got.Priced, int64(want.Total))
	}
}

// bedrock-mantle has no SDK retry loop, so a throttle reaches the client after
// exactly one attempt at the wire - where runtime's reaches it after the SDK's
// own three. The asymmetry is documented, and this is what it looks like.
func TestAMantleThrottleReachesTheClientOnTheFirstAttempt(t *testing.T) {
	d := newDeployment(t)
	d.Fake.Arm(fakebedrock.MantleSonnet5, fakebedrock.FaultThrottling, fakebedrock.Always)

	status, body := d.send(claudeCodeUA, messagesBody(routeMantleSonnet, false, "throttle me"))
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429\n%s", status, body)
	}
	if n := len(d.Fake.Received()); n != 1 {
		t.Errorf("the fake saw %d attempts, want exactly one", n)
	}
	got := d.latest()
	if got.ErrorKind != "throttled" || got.Attempts != 1 || got.Priced {
		t.Errorf("recorded %q, %d attempts, priced %v", got.ErrorKind, got.Attempts, got.Priced)
	}
}

// Chat Completions end to end: opencode's OpenAI-compatible provider, Cline and
// Continue send this. The stream is relayed in its own shape and ends with
// [DONE]; the usage the gateway asked for on the client's behalf is metered and
// not relayed; and the request is attributed from the working directory in the
// system prompt without that system prompt becoming the session.
func TestChatCompletionsThroughMantle(t *testing.T) {
	d := newDeployment(t, withUsage(fakebedrock.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 800}))

	started := time.Now()
	status, body := d.post("/v1/chat/completions", "opencode (panorama)", chatBody(routeOSS, true, "fix the bug"))
	took := time.Since(started)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("the stream does not end with [DONE]:\n%s", body)
	}
	if strings.Contains(body, `"usage":{`) {
		t.Errorf("the usage chunk nobody asked for was relayed:\n%s", body)
	}
	// The fake holds the connection open after [DONE], as Mantle has been seen
	// to. A provider that waited for the body to close would take that long.
	if took > 2500*time.Millisecond {
		t.Errorf("the request took %s; the provider waited for the body after [DONE]", took)
	}

	sent := d.lastMantle(t)
	if sent.Path != "/v1/chat/completions" {
		t.Errorf("sent to %s, want /v1/chat/completions", sent.Path)
	}
	var sentBody struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	_ = json.Unmarshal(sent.Body, &sentBody)
	if !sentBody.StreamOptions.IncludeUsage {
		t.Error("the provider was not asked for usage, so the stream could not be metered")
	}

	got := d.latest()
	d.requireServed(t, got)
	if got.Surface != "openai-chat" || got.Provider != "bedrock-mantle" || got.Tool != "opencode" {
		t.Errorf("recorded surface %q, provider %q, tool %q", got.Surface, got.Provider, got.Tool)
	}
	// prompt_tokens includes the cached tokens; the gateway subtracts them.
	if got.Tokens.Input != 1000 || got.Tokens.CacheRead != 800 || got.Tokens.Output != 500 {
		t.Errorf("tokens = %+v, want 1000 in, 800 cache read, 500 out", got.Tokens)
	}
	// No rate is seeded for an open-weight model, and a fresh deployment says
	// so rather than pricing it at zero.
	if got.Priced {
		t.Error("an open-weight model with no seeded rate was priced")
	}
	if got.SessionID == "" || got.Repo != fixtureDirectory {
		t.Errorf("session %q, repo %q, want a session in %q", got.SessionID, got.Repo, fixtureDirectory)
	}
}

// GPT-5.x refuses max_tokens on Chat Completions, and max_tokens is the only
// spelling opencode's OpenAI-compatible provider has. The cap reaches the model
// as max_completion_tokens, and the client is served without knowing there was
// a difference. Found by opencode on a GPT-5.6 route, where every request was
// refused.
func TestChatCompletionsCapReachesGPTAsMaxCompletionTokens(t *testing.T) {
	d := newDeployment(t)

	req := chatBody(routeGPT, true, "fix the bug")
	req["max_tokens"] = 32000
	status, body := d.post("/v1/chat/completions", "opencode (panorama)", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}

	sent := d.lastMantle(t)
	if sent.Path != "/openai/v1/chat/completions" {
		t.Errorf("sent to %s, want /openai/v1/chat/completions", sent.Path)
	}
	var sentBody map[string]json.RawMessage
	if err := json.Unmarshal(sent.Body, &sentBody); err != nil {
		t.Fatalf("the body the fake received: %v", err)
	}
	if raw, ok := sentBody["max_tokens"]; ok {
		t.Errorf("max_tokens = %s was sent to a model that refuses it", raw)
	}
	if got := string(sentBody["max_completion_tokens"]); got != "32000" {
		t.Errorf("max_completion_tokens = %q, want the client's cap of 32000", got)
	}
	d.requireServed(t, d.latest())
}

// Responses end to end, which is all Codex speaks: sent to the base path its
// model family is served at, kept stateless, and attributed from Codex's own
// environment block rather than grouped by it.
func TestResponsesThroughMantle(t *testing.T) {
	d := newDeployment(t)

	status, body := d.post("/v1/responses", codexUA, responsesBody(routeGPT, true, "write the tests"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}
	for _, want := range []string{"event: response.created", "event: response.completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("the stream is missing %q:\n%s", want, body)
		}
	}

	sent := d.lastMantle(t)
	if sent.Path != "/openai/v1/responses" {
		t.Errorf("sent to %s, want /openai/v1/responses - GPT-5.x is not served at /v1", sent.Path)
	}
	var sentBody struct {
		Store *bool `json:"store"`
	}
	_ = json.Unmarshal(sent.Body, &sentBody)
	if sentBody.Store == nil || *sentBody.Store {
		t.Errorf("store = %v, want false forced on a request that left it unset", sentBody.Store)
	}

	got := d.latest()
	d.requireServed(t, got)
	if got.Surface != "openai-responses" || got.Tool != "codex-cli" {
		t.Errorf("recorded surface %q, tool %q", got.Surface, got.Tool)
	}
	if got.Tokens.Input != fakebedrock.DefaultUsage.InputTokens {
		t.Errorf("input tokens = %d, want the uncached input", got.Tokens.Input)
	}
	if got.Repo != fixtureDirectory {
		t.Errorf("repo = %q, want %q from Codex's environment block", got.Repo, fixtureDirectory)
	}

	// A second conversation in the same checkout is a second session: the
	// environment block is the same in both and must not be what groups them.
	first := got.SessionID
	if status, body := d.post("/v1/responses", codexUA, responsesBody(routeGPT, false, "a different task")); status != http.StatusOK {
		t.Fatalf("second conversation: %d\n%s", status, body)
	}
	if second := d.latest().SessionID; second == first {
		t.Error("two Codex conversations in one checkout were grouped as one session")
	}

	// And a request that needs server-side storage is refused before Mantle is
	// asked.
	before := len(d.Fake.Received())
	stored := responsesBody(routeGPT, false, "remember this")
	stored["store"] = true
	if status, _ := d.post("/v1/responses", codexUA, stored); status != http.StatusBadRequest {
		t.Errorf("store: true = %d, want 400", status)
	}
	if len(d.Fake.Received()) != before {
		t.Error("a refused Responses request reached the provider")
	}
}

// A route is served only on the APIs its model speaks, and the refusal names
// the endpoint that would work.
func TestARouteOnTheWrongSurfaceIsRefusedWithTheEndpoint(t *testing.T) {
	d := newDeployment(t)

	status, body := d.send(claudeCodeUA, messagesBody(routeOSS, false, "hello"))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", status, body)
	}
	message := errorMessage(t, body)
	requireContains(t, message, "/v1/chat/completions", "the refusal")

	status, body = d.post("/v1/chat/completions", "cline/3.0", chatBody(routeStandard, false, "hello"))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", status, body)
	}
	requireContains(t, body, "/v1/messages", "the refusal")
	if len(d.Fake.Received()) != 0 {
		t.Error("a request on the wrong surface reached a provider")
	}
}

// GET /v1/models lists routes, in whichever shape the client reads, narrowed
// to the routes that client's API can reach.
func TestTheModelListIsTheRoutesPerAPI(t *testing.T) {
	d := newDeployment(t)

	list := func(anthropic bool) []string {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, d.baseURL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+d.token)
		if anthropic {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		resp, err := d.client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range out.Data {
			names = append(names, m.ID)
		}
		return names
	}

	if got := strings.Join(list(true), ","); got != "fast,mantle-sonnet,standard,thrifty" {
		t.Errorf("Anthropic listing = %s", got)
	}
	if got := strings.Join(list(false), ","); got != "gpt,oss,qwen" {
		t.Errorf("OpenAI listing = %s", got)
	}
}

// The providers as the CLI and the console see them, and the live check run
// against Mantle through the same function both call.
func TestTheProvidersAreConfiguredAndChecked(t *testing.T) {
	d := newDeployment(t)

	out := d.run("providers", "list")
	for _, want := range []string{"bedrock ", "bedrock-mantle", "us-east-1"} {
		requireContains(t, out, want, "providers list")
	}
	policy := d.run("providers", "policy")
	requireContains(t, policy, "bedrock-mantle:CreateInference", "the printed IAM policy")

	var providers struct {
		Providers []struct {
			Kind      string `json:"kind"`
			State     string `json:"state"`
			Region    string `json:"region"`
			Catalogue struct {
				Models    int `json:"models"`
				Serveable int `json:"serveable"`
			} `json:"catalogue"`
		} `json:"providers"`
		IAMPolicy string `json:"iam_policy"`
	}
	d.getJSON("/api/v1/providers", &providers)
	if len(providers.Providers) != 2 {
		t.Fatalf("%d providers, want both", len(providers.Providers))
	}
	mantle := providers.Providers[1]
	if mantle.Kind != "bedrock-mantle" || mantle.State != "never checked" || mantle.Catalogue.Models != len(fakebedrock.DefaultMantleModels()) {
		t.Errorf("mantle = %+v", mantle)
	}
	// Two of the fake's models are not serveable: one speaks nothing this
	// build knows, one is listed as unavailable.
	if mantle.Catalogue.Serveable != mantle.Catalogue.Models-2 {
		t.Errorf("serveable = %d of %d, want all but the browse-only and the unavailable model",
			mantle.Catalogue.Serveable, mantle.Catalogue.Models)
	}

	var check struct {
		OK     bool   `json:"ok"`
		Target string `json:"target"`
		Error  string `json:"error"`
	}
	resp := d.api(http.MethodPost, "/api/v1/providers/bedrock-mantle/check", map[string]string{})
	_ = json.NewDecoder(resp.Body).Decode(&check)
	_ = resp.Body.Close()
	if !check.OK {
		t.Fatalf("the Mantle check failed: %s", check.Error)
	}
	if !strings.HasPrefix(check.Target, "anthropic.") {
		t.Errorf("checked %q, want a Claude model first", check.Target)
	}

	d.getJSON("/api/v1/providers", &providers)
	if providers.Providers[1].State != "connected" {
		t.Errorf("after a check the provider reads %q, want connected", providers.Providers[1].State)
	}
}

// TestCodexEndToEnd runs the real Codex CLI against the config.toml the
// deployment rendered.
func TestCodexEndToEnd(t *testing.T) {
	bin := toolPath(t, "codex")

	d := newDeployment(t)
	s := newSandbox(t, d)
	s.deviceLogin(t, d)

	config, _ := d.toolSetup().row(t, "codex-cli")["config"].(string)
	if !strings.Contains(config, `wire_api = "responses"`) {
		t.Fatalf("the deployment rendered no Codex configuration:\n%s", config)
	}
	dir := filepath.Join(s.home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	env := append(s.onPath(t, d), "PANORAMA_TOKEN="+s.mintedToken(t, d), "CODEX_HOME="+dir)
	out := runTool(t, env, s.repo, bin, "exec", "--skip-git-repo-check", "Reply with the single word: ok")
	t.Logf("codex: %s", strings.TrimSpace(out))

	got := d.latest()
	if got.Tool != "codex-cli" || got.Surface != "openai-responses" {
		t.Errorf("recorded tool %q on %q", got.Tool, got.Surface)
	}
	if got.Decision != "allowed" {
		t.Errorf("decision = %q, want allowed", got.Decision)
	}
	if got.Repo != fixtureDirectory {
		t.Errorf("repo = %q, want %q - Codex carries its working directory", got.Repo, fixtureDirectory)
	}
}

// TestOpenCodeOverTheOpenAIAPIs runs opencode against the OpenAI provider
// blocks, on routes whose models do not speak the Messages API.
//
// A route whose model speaks Responses is declared under OpenAI's own provider
// and sent there: GPT-5.x refuses function tools on Chat Completions while it
// is reasoning, and opencode sends tools on every request, so a GPT route on
// Chat Completions failed every time at somebody's desk. A model that speaks
// only Chat Completions is still sent Chat Completions, through the
// OpenAI-compatible provider.
func TestOpenCodeOverTheOpenAIAPIs(t *testing.T) {
	bin := toolPath(t, "opencode")

	d := newDeployment(t)
	s := newSandbox(t, d)
	s.deviceLogin(t, d)
	s.configure(t, d, "opencode", "-write")

	env := append(s.onPath(t, d), "PANORAMA_TOKEN="+s.mintedToken(t, d))
	for _, tc := range []struct{ selection, route, surface string }{
		{"panorama-openai/" + routeOSS, routeOSS, "openai-responses"},
		{"panorama-openai/" + routeGPT, routeGPT, "openai-responses"},
		{"panorama-chat/" + routeQwen, routeQwen, "openai-chat"},
	} {
		out := runTool(t, env, s.repo, bin, "run", "-m", tc.selection, "Reply with the single word: ok")
		t.Logf("opencode on %s: %s", tc.selection, strings.TrimSpace(out))

		got := d.latest()
		if got.Tool != "opencode" || got.Surface != tc.surface || got.RequestedModel != tc.route {
			t.Errorf("%s: recorded tool %q on %q for %q, want opencode on %s", tc.selection,
				got.Tool, got.Surface, got.RequestedModel, tc.surface)
		}
		if got.Decision != "allowed" || got.ErrorKind != "" {
			t.Errorf("%s: decision = %q, error %q, want allowed and served", tc.selection, got.Decision, got.ErrorKind)
		}
		if got.Repo != fixtureDirectory {
			t.Errorf("%s: repo = %q, want %q", tc.selection, got.Repo, fixtureDirectory)
		}
	}

	// Stateless, because that is the only kind of Responses request this
	// gateway serves: opencode says so itself for this provider package.
	sent := d.lastMantleOn(t, "/openai/v1/responses")
	var body struct {
		Store              *bool  `json:"store"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	if err := json.Unmarshal(sent.Body, &body); err != nil {
		t.Fatalf("the body the fake received: %v", err)
	}
	if body.Store == nil || *body.Store || body.PreviousResponseID != "" {
		t.Errorf("store = %v, previous_response_id = %q, want a stateless request", body.Store, body.PreviousResponseID)
	}
}
