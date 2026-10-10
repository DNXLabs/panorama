// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
)

// post sends a request to one of the data plane's paths as a Codex or opencode
// client would: a bearer token, and a user agent naming the tool.
func (f *fixture) post(t *testing.T, path, userAgent, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("User-Agent", userAgent)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func decodeOpenAIError(t *testing.T, raw []byte) openAIErrorBody {
	t.Helper()
	var envelope openAIError
	decode(t, raw, &envelope)
	if envelope.Error.Message == "" {
		t.Fatalf("not an OpenAI error envelope: %s", raw)
	}
	return envelope.Error
}

// A Chat Completions request is served as one: handed to the provider in its
// own shape, relayed verbatim, and recorded on its own surface.
func TestChatCompletionsIsServedNatively(t *testing.T) {
	provider := &fakeProvider{
		response: &providers.Response{
			Body:  []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`),
			Usage: providers.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 400},
		},
	}
	f := newFixture(t, provider)

	rec := f.post(t, "/v1/chat/completions", "opencode (panorama)",
		`{"model":"`+testModel+`","messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "chatcmpl-1") {
		t.Errorf("the provider's response was not relayed: %s", rec.Body)
	}
	if provider.lastRequest.API != providers.APIOpenAIChat || provider.lastModel != resolvedModel {
		t.Errorf("provider was asked for %s on %q", provider.lastRequest.API, provider.lastModel)
	}

	f.drain()
	row := f.latest(t)
	if row.Surface != SurfaceOpenAIChat || row.Provider != "bedrock" || row.Tool != "opencode" {
		t.Errorf("recorded surface %q, provider %q, tool %q", row.Surface, row.Provider, row.Tool)
	}
	if row.CacheReadTokens != 400 || row.InputTokens != 100 {
		t.Errorf("recorded tokens %d in, %d cache read", row.InputTokens, row.CacheReadTokens)
	}
}

// A stream is metered only if the provider reports usage, and Chat Completions
// reports it only when asked. So the gateway asks, and a client that did not
// ask is not sent the chunk it did not ask for.
func TestChatStreamsAskForUsageAndWithholdIt(t *testing.T) {
	usageChunk := []byte(`{"id":"c","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	for _, asked := range []bool{false, true} {
		provider := &fakeProvider{
			events: [][]byte{
				[]byte(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}`),
				usageChunk,
			},
			usage: providers.Usage{InputTokens: 5, OutputTokens: 2},
		}
		f := newFixture(t, provider)

		body := `{"model":"` + testModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		if asked {
			body = `{"model":"` + testModel + `","stream":true,"stream_options":{"include_usage":true,"extra":1},"messages":[]}`
		}
		w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.token)
		f.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("asked=%v: status = %d: %s", asked, w.Code, w.Body)
		}

		options := map[string]json.RawMessage{}
		if err := json.Unmarshal(provider.lastRequest.Fields["stream_options"], &options); err != nil {
			t.Fatalf("asked=%v: stream_options were not an object: %s", asked, provider.lastRequest.Fields["stream_options"])
		}
		if string(options["include_usage"]) != "true" {
			t.Errorf("asked=%v: the provider was not asked for usage: %v", asked, options)
		}
		if asked && string(options["extra"]) != "1" {
			t.Errorf("the client's other stream options were dropped: %v", options)
		}

		out := w.Body.String()
		if strings.Contains(out, "event:") {
			t.Errorf("asked=%v: a Chat Completions stream carries no event names:\n%s", asked, out)
		}
		if !strings.HasSuffix(out, "data: [DONE]\n\n") {
			t.Errorf("asked=%v: the stream does not end with [DONE]:\n%s", asked, out)
		}
		if has := strings.Contains(out, `"usage":{"prompt_tokens"`); has != asked {
			t.Errorf("asked=%v: usage chunk relayed = %v\n%s", asked, has, out)
		}

		f.drain()
		if row := f.latest(t); row.InputTokens != 5 || row.OutputTokens != 2 {
			t.Errorf("asked=%v: a stream was not metered: %d in, %d out", asked, row.InputTokens, row.OutputTokens)
		}
	}
}

// A Responses request is kept stateless: store is forced off when the client
// leaves it unset, and a request that needs server-side storage is refused
// before a provider is invoked.
func TestResponsesAreKeptStateless(t *testing.T) {
	provider := &fakeProvider{response: &providers.Response{Body: []byte(`{"id":"resp_1","object":"response"}`)}}
	f := newFixture(t, provider)

	rec := f.post(t, "/v1/responses", "codex_cli_rs/0.40.0", `{"model":"`+testModel+`","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := string(provider.lastRequest.Fields["store"]); got != "false" {
		t.Errorf("store = %s, want false forced on a request that left it unset", got)
	}
	f.drain()
	if row := f.latest(t); row.Surface != SurfaceOpenAIResponses || row.Tool != "codex-cli" {
		t.Errorf("recorded surface %q, tool %q", row.Surface, row.Tool)
	}

	for name, body := range map[string]string{
		"store":                `{"model":"` + testModel + `","input":"hi","store":true}`,
		"previous_response_id": `{"model":"` + testModel + `","input":"hi","previous_response_id":"resp_0"}`,
		"background":           `{"model":"` + testModel + `","input":"hi","background":true}`,
		"service_tier":         `{"model":"` + testModel + `","input":"hi","service_tier":"priority"}`,
	} {
		before := provider.invocations
		rec := f.post(t, "/v1/responses", "codex_cli_rs/0.40.0", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
			continue
		}
		got := decodeOpenAIError(t, rec.Body.Bytes())
		if got.Type != "invalid_request_error" || !strings.Contains(got.Message, name) {
			t.Errorf("%s: error = %+v, want an invalid request naming the parameter", name, got)
		}
		if provider.invocations != before {
			t.Errorf("%s: a refused request reached the provider", name)
		}
	}

	// The default tier is the one that is priced, and is served.
	if rec := f.post(t, "/v1/responses", "codex", `{"model":"`+testModel+`","input":"hi","service_tier":"default"}`); rec.Code != http.StatusOK {
		t.Errorf("service_tier default: status = %d, want 200", rec.Code)
	}
}

// A Responses stream names each event after its type, the way Codex reads it,
// and ends without a [DONE] - which is Chat Completions' marker, not this API's.
func TestResponsesStreamsNameTheirEvents(t *testing.T) {
	provider := &fakeProvider{
		events: [][]byte{
			[]byte(`{"type":"response.created","response":{}}`),
			[]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":3}}}`),
		},
	}
	f := newFixture(t, provider)

	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+testModel+`","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+f.token)
	f.mux.ServeHTTP(w, req)

	out := w.Body.String()
	for _, want := range []string{"event: response.created\ndata: {", "event: response.completed\ndata: {"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("a Responses stream ended with Chat Completions' marker:\n%s", out)
	}
}

// Every failure on an OpenAI surface arrives in OpenAI's error envelope, and a
// missing token names the setting an OpenAI client reads.
func TestOpenAISurfacesSpeakOpenAIErrors(t *testing.T) {
	provider := &fakeProvider{
		invokeErr: &providers.Error{Kind: providers.ErrKindThrottled, StatusCode: 429, Message: "slow"},
	}
	f := newFixture(t, provider)

	unauthenticated := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, unauthenticated)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := decodeOpenAIError(t, rec.Body.Bytes()); !strings.Contains(got.Message, "OPENAI_API_KEY") ||
		got.Code == nil || *got.Code != "invalid_api_key" {
		t.Errorf("unauthenticated error = %+v", got)
	}

	rec = f.post(t, "/v1/chat/completions", "cline", `{"model":"`+testModel+`","messages":[]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := decodeOpenAIError(t, rec.Body.Bytes()); got.Type != "rate_limit_error" {
		t.Errorf("throttle error = %+v, want a rate_limit_error a client backs off on", got)
	}

	rec = f.post(t, "/v1/responses", "codex", `{"model":"nobody-routed-this","input":"hi"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	got := decodeOpenAIError(t, rec.Body.Bytes())
	if strings.Contains(got.Message, "ANTHROPIC_MODEL") {
		t.Errorf("an OpenAI client was told to set Claude Code's variable: %s", got.Message)
	}
}

// A refusal lists only the routes that would be served on the surface the
// request arrived at: offering a Codex user a Claude route would send them to
// switch to a name the next request refuses for another reason.
func TestARefusalListsOnlyRoutesThisSurfaceServes(t *testing.T) {
	provider := &fakeProvider{}
	oss := policy.Route{Name: "oss", Target: providers.MustModelRef("bedrock-mantle/openai.gpt-oss-120b"), ServesOverSoft: true}
	claude := policy.Route{Name: "claude", Target: bedrock(resolvedModel), ServesOverSoft: true}

	mantle := providers.Settings{Kind: providers.KindBedrockMantle, Enabled: true, Region: "us-west-2"}
	runtime := providers.Settings{Kind: providers.KindBedrock, Enabled: true, Region: "us-east-1"}
	set := policy.Set{
		Routes: []policy.Route{oss, claude},
		Providers: map[providers.Kind]providers.State{
			providers.KindBedrockMantle: {Settings: mantle, Instance: &providers.Instance{
				Settings: mantle, Provider: provider,
				APIs: func(string) []providers.API { return []providers.API{providers.APIOpenAIResponses} },
			}},
			providers.KindBedrock: {Settings: runtime, Instance: &providers.Instance{
				Settings: runtime, Provider: provider,
				APIs: func(string) []providers.API { return []providers.API{providers.APIAnthropicMessages} },
			}},
		},
	}
	f := newFixtureWith(t, fixtureConfig{provider: provider, set: &set})

	rec := f.post(t, "/v1/responses", "codex", `{"model":"gpt-4o","input":"hi"}`)
	message := decodeOpenAIError(t, rec.Body.Bytes()).Message
	if !strings.Contains(message, "oss") || strings.Contains(message, "claude") {
		t.Errorf("the refusal offered routes this surface cannot serve: %s", message)
	}

	// And the route on another provider is invoked on its own provider, and
	// recorded as that provider's.
	if rec := f.post(t, "/v1/responses", "codex", `{"model":"oss","input":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	f.drain()
	if row := f.latest(t); row.Provider != "bedrock-mantle" || row.ResolvedModel != "openai.gpt-oss-120b" {
		t.Errorf("recorded %s/%s", row.Provider, row.ResolvedModel)
	}
}

// GET /v1/models lists routes, not models, filtered the way a refusal is - and
// in whichever of the two shapes the client reads.
func TestModelsListsTheRoutesACallerMayUse(t *testing.T) {
	provider := &fakeProvider{}
	f := newFixtureWith(t, fixtureConfig{provider: provider, apis: []providers.API{providers.APIAnthropicMessages}})

	anthropic := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	anthropic.Header.Set("Authorization", "Bearer "+f.token)
	anthropic.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, anthropic)
	var anthropicList struct {
		Data []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	decode(t, rec.Body.Bytes(), &anthropicList)
	if len(anthropicList.Data) != 1 || anthropicList.Data[0].ID != testModel || anthropicList.Data[0].Type != "model" {
		t.Errorf("anthropic listing = %s", rec.Body)
	}

	// The same route speaks only Messages, so an OpenAI client is shown nothing.
	openai := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	openai.Header.Set("Authorization", "Bearer "+f.token)
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, openai)
	var openaiList struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	decode(t, rec.Body.Bytes(), &openaiList)
	if openaiList.Object != "list" || len(openaiList.Data) != 0 {
		t.Errorf("openai listing = %s, want an empty list", rec.Body)
	}

	// Unauthenticated, it is refused in the shape the client reads.
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated model list = %d, want 401", rec.Code)
	}
	decodeOpenAIError(t, rec.Body.Bytes())

	f.drain()
	if _, err := f.db.LatestRequest(t.Context()); err == nil {
		t.Error("a model listing was recorded as though it were an invocation")
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

// A Chat Completions conversation opens with the tool's system prompt, which is
// the same in every session it starts. Grouping by that would merge a
// repository's every conversation into one, so two conversations that differ
// only after the system prompt are two sessions.
func TestChatSessionsAreNotMergedByTheSystemPrompt(t *testing.T) {
	extractor, err := attribution.NewExtractor("")
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{response: &providers.Response{Body: []byte(`{}`), Usage: providers.Usage{InputTokens: 1}}}
	f := newFixtureWith(t, fixtureConfig{provider: provider, attribution: extractor})

	system := `{"role":"system","content":"You are opencode. Working directory: /Users/dev/src/panorama"}`
	for _, prompt := range []string{"fix the login bug", "write the release notes"} {
		body := `{"model":"` + testModel + `","messages":[` + system + `,{"role":"user","content":"` + prompt + `"}]}`
		if rec := f.post(t, "/v1/chat/completions", "opencode (panorama)", body); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		f.drain()
	}

	page, err := f.db.ListRequests(t.Context(), store.RequestFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 2 {
		t.Fatalf("%d requests recorded, want 2", len(page.Requests))
	}
	if page.Requests[0].SessionID == "" || page.Requests[0].SessionID == page.Requests[1].SessionID {
		t.Errorf("two conversations with one system prompt were grouped as one session (%q, %q)",
			page.Requests[0].SessionID, page.Requests[1].SessionID)
	}
	if page.Requests[0].Repo == "" && page.Requests[0].SessionID != "" {
		// The working directory is what places the work; the session reads it.
		sessions, _ := f.db.ListSessions(t.Context(), store.RequestFilter{Limit: 5})
		if len(sessions) == 0 {
			t.Error("no session was recorded for Chat Completions traffic")
		}
	}
}
