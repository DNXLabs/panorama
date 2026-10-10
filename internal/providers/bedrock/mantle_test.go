// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/providers"
)

// newTestMantle builds the real provider against a test server, through the
// environment variable the end-to-end suite uses.
func newTestMantle(t *testing.T, handler http.Handler) *Mantle {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv(mantleEndpointEnv, srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "fake")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	m, err := NewMantle(t.Context(), Config{Region: "us-west-2"})
	if err != nil {
		t.Fatalf("NewMantle: %v", err)
	}
	return m
}

// Each API goes to its own path, the model rides in the body, and the request
// is signed for bedrock-mantle in the provider's region - which is the one
// thing that would otherwise only show up as an AccessDenied from real AWS.
func TestMantleSendsEachAPIWhereMantleServesIt(t *testing.T) {
	type seen struct {
		path, model, version, beta, scope string
		stream                            bool
	}
	var got []seen
	m := newTestMantle(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		auth := r.Header.Get("Authorization")
		_, credential, _ := strings.Cut(auth, "Credential=")
		got = append(got, seen{
			path: r.URL.Path, model: body.Model, stream: body.Stream,
			version: r.Header.Get("anthropic-version"), beta: r.Header.Get("anthropic-beta"),
			scope: credential,
		})
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))

	cases := []struct {
		api   providers.API
		model string
		path  string
	}{
		{providers.APIAnthropicMessages, "anthropic.claude-sonnet-5", "/anthropic/v1/messages"},
		{providers.APIOpenAIChat, "openai.gpt-oss-120b", "/v1/chat/completions"},
		{providers.APIOpenAIResponses, "openai.gpt-oss-120b", "/v1/responses"},
		{providers.APIOpenAIChat, "openai.gpt-5.5", "/openai/v1/chat/completions"},
		{providers.APIOpenAIResponses, "xai.grok-4.3", "/openai/v1/responses"},
		{providers.APIOpenAIChat, "qwen.qwen3-coder-480b-a35b-instruct", "/v1/chat/completions"},
	}
	for _, tc := range cases {
		if _, err := m.Invoke(t.Context(), &providers.Request{
			API: tc.api, Model: tc.model, Body: []byte(`{"model":"route-name","messages":[]}`),
			Betas: []string{"context-1m-2025-08-07", "redact-thinking-2026-02-12"},
		}); err != nil {
			t.Fatalf("%s %s: %v", tc.api, tc.model, err)
		}
	}

	for i, tc := range cases {
		s := got[i]
		if s.path != tc.path {
			t.Errorf("%s %s went to %s, want %s", tc.api, tc.model, s.path, tc.path)
		}
		if s.model != tc.model {
			t.Errorf("%s: the body named %q, want the provider-native id %q", tc.api, s.model, tc.model)
		}
		if s.stream {
			t.Errorf("%s: a non-streaming invocation asked for a stream", tc.api)
		}
		if !strings.Contains(s.scope, "/us-west-2/bedrock-mantle/aws4_request") {
			t.Errorf("%s: signed for %q, want the bedrock-mantle service in us-west-2", tc.api, s.scope)
		}
	}
	// The Anthropic path takes the first-party version header, and the betas
	// the provider is configured to drop are dropped.
	if got[0].version != mantleAnthropicVersion {
		t.Errorf("anthropic-version = %q", got[0].version)
	}
	if got[0].beta != "context-1m-2025-08-07" {
		t.Errorf("anthropic-beta = %q, want the unsupported flag dropped", got[0].beta)
	}
	if got[1].beta != "" {
		t.Errorf("an OpenAI request carried an anthropic-beta header: %q", got[1].beta)
	}
}

// GPT-5.x refuses max_tokens on Chat Completions and reads the same cap as
// max_completion_tokens; opencode's OpenAI-compatible provider sends only the
// first. Every other family, and every other API, is sent what the client sent.
func TestMantleSpellsTheChatCapTheWayTheModelReadsIt(t *testing.T) {
	var sent map[string]json.RawMessage
	m := newTestMantle(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		sent = nil
		_ = json.Unmarshal(raw, &sent)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))

	cases := []struct {
		name  string
		api   providers.API
		model string
		body  string
		// want is each field's value as sent, and "" for a field that must be
		// absent.
		want map[string]string
	}{
		{"renamed for GPT-5.x", providers.APIOpenAIChat, "openai.gpt-5.6-terra",
			`{"max_tokens":32000,"messages":[]}`,
			map[string]string{"max_tokens": "", "max_completion_tokens": "32000"}},
		{"renamed for GPT-6", providers.APIOpenAIChat, "openai.gpt-6",
			`{"max_tokens":8,"messages":[]}`,
			map[string]string{"max_tokens": "", "max_completion_tokens": "8"}},
		{"the client's own max_completion_tokens wins", providers.APIOpenAIChat, "openai.gpt-5.5",
			`{"max_tokens":100,"max_completion_tokens":50,"messages":[]}`,
			map[string]string{"max_tokens": "", "max_completion_tokens": "50"}},
		{"a null cap is no cap", providers.APIOpenAIChat, "openai.gpt-5.5",
			`{"max_tokens":null,"messages":[]}`,
			map[string]string{"max_tokens": "", "max_completion_tokens": ""}},
		{"no cap, nothing added", providers.APIOpenAIChat, "openai.gpt-5.5",
			`{"messages":[]}`,
			map[string]string{"max_tokens": "", "max_completion_tokens": ""}},
		{"gpt-oss is sent what it was given", providers.APIOpenAIChat, "openai.gpt-oss-120b",
			`{"max_tokens":32000,"messages":[]}`,
			map[string]string{"max_tokens": "32000", "max_completion_tokens": ""}},
		{"other families are sent what they were given", providers.APIOpenAIChat, "qwen.qwen3-coder-480b-a35b-instruct",
			`{"max_tokens":32000,"messages":[]}`,
			map[string]string{"max_tokens": "32000", "max_completion_tokens": ""}},
		{"Responses is not Chat Completions", providers.APIOpenAIResponses, "openai.gpt-5.5",
			`{"max_output_tokens":64,"input":"hi"}`,
			map[string]string{"max_output_tokens": "64", "max_completion_tokens": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.Invoke(t.Context(), &providers.Request{
				API: tc.api, Model: tc.model, Body: []byte(tc.body),
			}); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			for field, want := range tc.want {
				got, present := sent[field]
				switch {
				case want == "" && present:
					t.Errorf("%s was sent as %s, want it absent", field, got)
				case want != "" && string(got) != want:
					t.Errorf("%s = %q, want %s", field, got, want)
				}
			}
		})
	}
}

// A model asked for over an API it does not speak is refused before a request
// is made, rather than sent somewhere it will fail.
func TestMantleRefusesAnAPIAModelDoesNotSpeak(t *testing.T) {
	called := false
	m := newTestMantle(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

	for _, tc := range []struct {
		api   providers.API
		model string
	}{
		{providers.APIOpenAIChat, "anthropic.claude-sonnet-5"},
		{providers.APIAnthropicMessages, "openai.gpt-oss-120b"},
		{providers.APIOpenAIResponses, "qwen.qwen3-32b"},
		{providers.APIOpenAIChat, "acme.nobody-has-heard-of-this"},
	} {
		_, err := m.Invoke(t.Context(), &providers.Request{API: tc.api, Model: tc.model, Body: []byte(`{}`)})
		pe, ok := providers.AsError(err)
		if !ok || pe.Kind != providers.ErrKindInvalidRequest {
			t.Errorf("%s over %s: error = %v, want an invalid request", tc.model, tc.api, err)
		}
	}
	if called {
		t.Error("a request the model cannot serve was sent")
	}
}

// Mantle has been seen holding a Chat Completions response open after [DONE].
// The stream ends at the marker, not at the body, or every request waits for
// the connection to time out.
func TestMantleStreamsEndAtTheirMarker(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	m := newTestMantle(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, line := range []string{
			`data: {"choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
			`data: {"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":1000}}}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, line+"\n\n")
		}
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))

	stream, err := m.InvokeStream(t.Context(), &providers.Request{
		API: providers.APIOpenAIChat, Model: "openai.gpt-oss-120b", Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	done := make(chan int)
	go func() {
		n := 0
		for {
			if _, err := stream.Next(); err != nil {
				if !errors.Is(err, io.EOF) {
					t.Errorf("stream ended with %v, want io.EOF", err)
				}
				done <- n
				return
			}
			n++
		}
	}()

	select {
	case n := <-done:
		if n != 3 {
			t.Errorf("%d events relayed, want 3 - [DONE] is the marker, not an event", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream waited for the body to close after [DONE]")
	}

	// Cached tokens are inside prompt_tokens in the OpenAI APIs; counted as
	// input as well, every cache read would be billed twice.
	u := stream.Usage()
	if u.InputTokens != 200 || u.CacheReadTokens != 1000 || u.OutputTokens != 30 {
		t.Errorf("usage = %+v, want 200 uncached in, 1000 cache read, 30 out", u)
	}
}

// A body that closes before the stream's own end is a truncated response, told
// in band rather than relayed as though it were whole.
func TestMantleReportsAStreamCutShort(t *testing.T) {
	m := newTestMantle(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n")
	}))

	stream, err := m.InvokeStream(t.Context(), &providers.Request{
		API: providers.APIAnthropicMessages, Model: "anthropic.claude-sonnet-5", Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	if _, nerr := stream.Next(); nerr != nil {
		t.Fatalf("first event: %v", nerr)
	}
	_, err = stream.Next()
	pe, ok := providers.AsError(err)
	if !ok || pe.Kind != providers.ErrKindUpstream {
		t.Errorf("a stream cut short ended with %v, want an upstream error", err)
	}
	if u := stream.Usage(); u.InputTokens != 5 {
		t.Errorf("usage observed before the cut = %+v", u)
	}
}

// Errors map onto the six kinds from the status and the body, in each of the
// shapes Mantle returns them in.
func TestClassifyMantle(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   providers.ErrorKind
	}{
		{429, `{"error":{"type":"rate_limit_error","message":"slow"}}`, providers.ErrKindThrottled},
		{503, `{"message":"capacity"}`, providers.ErrKindUnavailable},
		{529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, providers.ErrKindUnavailable},
		{400, `{"error":{"code":"validation_error","type":"invalid_request_error","message":"bad"}}`, providers.ErrKindInvalidRequest},
		{403, `{"message":"User is not authorized to perform: bedrock-mantle:CreateInference"}`, providers.ErrKindForbidden},
		{404, `{"error":{"type":"not_found_error"}}`, providers.ErrKindNotFound},
		{500, `not json`, providers.ErrKindUpstream},
	} {
		pe, ok := providers.AsError(classifyMantle(tc.status, []byte(tc.body)))
		if !ok || pe.Kind != tc.want {
			t.Errorf("%d %s = %v, want %s", tc.status, tc.body, pe, tc.want)
		}
		if ok && strings.Contains(pe.Message, "bedrock-mantle:CreateInference") {
			t.Errorf("the provider's own text reached the client-safe message: %q", pe.Message)
		}
	}
}

// The Responses API reports cache writes inside input_tokens too, for the
// families that report them at all.
func TestOpenAIUsageSubtractsWhatIsInsideTheInputFigure(t *testing.T) {
	u := openAIUsage{InputTokens: 10_000, OutputTokens: 500}
	u.InputTokensDetails = &struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	}{CachedTokens: 6_000, CacheWriteTokens: 3_000}

	got := u.usage()
	if got.InputTokens != 1_000 || got.CacheReadTokens != 6_000 || got.CacheWriteTokens != 3_000 || got.OutputTokens != 500 {
		t.Errorf("usage = %+v", got)
	}
}

func TestReadEventJoinsDataLines(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("event: response.created\ndata: {\"a\":\ndata: 1}\n\n: keepalive\n\ndata: [DONE]\n\n"))
	event, data, err := readEvent(r)
	if err != nil || event != "response.created" || string(data) != "{\"a\":\n1}" {
		t.Fatalf("first event = %q %q %v", event, data, err)
	}
	// A comment line - a keep-alive - is not an event, and is read past.
	_, data, err = readEvent(r)
	if err != nil || string(data) != "[DONE]" {
		t.Fatalf("after a keep-alive = %q %v, want the marker", data, err)
	}
	if _, _, err = readEvent(r); !errors.Is(err, io.EOF) {
		t.Errorf("after the last event = %v, want io.EOF", err)
	}
}

// The family table: which API a model speaks and where Mantle serves it, with
// an operator's override winning and an unknown model speaking nothing.
func TestMantleAPIs(t *testing.T) {
	apis := MantleAPIs(map[string][]providers.API{
		"acme.mystery-1":     {providers.APIOpenAIChat},
		"openai.gpt-oss-20b": {},
	})
	for model, want := range map[string][]providers.API{
		"anthropic.claude-haiku-4-5":            {providers.APIAnthropicMessages},
		"openai.gpt-oss-120b":                   {providers.APIOpenAIChat, providers.APIOpenAIResponses},
		"moonshotai.kimi-k2.5":                  {providers.APIOpenAIChat},
		"google.gemma-4-31b":                    {providers.APIOpenAIChat, providers.APIOpenAIResponses},
		"acme.mystery-1":                        {providers.APIOpenAIChat},
		"acme.mystery-2":                        nil,
		"openai.gpt-oss-20b":                    {},
		"mistral.mistral-large-3-675b-instruct": {providers.APIOpenAIChat},
	} {
		if got := apis(model); !slices.Equal(got, want) {
			t.Errorf("%s speaks %v, want %v", model, got, want)
		}
	}
	if base := mantleOpenAIBase("acme.mystery-1"); base != "/v1" {
		t.Errorf("an overridden unknown model goes to %s, want /v1", base)
	}
}

// bedrock-runtime serves the Messages body to Claude and nothing else, and its
// options cannot say otherwise.
func TestRuntimeAPIs(t *testing.T) {
	if got := RuntimeAPIs("global.anthropic.claude-sonnet-5"); !slices.Equal(got, []providers.API{providers.APIAnthropicMessages}) {
		t.Errorf("a Claude profile speaks %v", got)
	}
	if got := RuntimeAPIs("meta.llama3-70b-instruct-v1:0"); len(got) != 0 {
		t.Errorf("a Llama model on runtime speaks %v, want nothing", got)
	}
	if err := validateRuntimeOptions(providers.Options{APIs: map[string][]providers.API{
		"openai.gpt-oss-120b": {providers.APIOpenAIChat},
	}}); err == nil {
		t.Error("the runtime provider accepted an API it cannot serve")
	}
}

func TestParseMantleUsageType(t *testing.T) {
	for usage, want := range map[string]struct {
		model string
		class tokenClass
		ok    bool
	}{
		"USE1-openai.gpt-oss-120b-mantle-input-tokens-standard":                 {"openai.gpt-oss-120b", classInput, true},
		"APS2-moonshotai.kimi-k2.5-mantle-output-tokens-standard":               {"moonshotai.kimi-k2.5", classOutput, true},
		"USE1-anthropic.claude-haiku-4-5-mantle-cache-write-tokens-1h-standard": {"anthropic.claude-haiku-4-5", classCacheWrite1h, true},
		"USE1-zai.glm5-mantle-input-tokens-standard":                            {"zai.glm-5", classInput, true},
		"USE1-openai.gpt-oss-120b-mantle-input-tokens-priority":                 {ok: false},
		"USE1-openai.gpt-oss-120b-mantle-input-tokens-batch":                    {ok: false},
		"USE1-gpt-oss-120b-input-tokens":                                        {ok: false},
		"USE1-openai.gpt-oss-120b-mantle-reasoning-tokens-standard":             {ok: false},
	} {
		model, class, ok := parseMantleUsageType(usage)
		if ok != want.ok || (ok && (model != want.model || class != want.class)) {
			t.Errorf("%s = (%q, %v, %v), want (%q, %v, %v)", usage, model, class, ok, want.model, want.class, want.ok)
		}
	}
}

// The region is a hostname component, so its shape is an allowlist.
func TestARegionMustLookLikeOne(t *testing.T) {
	for _, bad := range []string{"", "moon", "us-east-1.evil.com", "us east 1", "US-EAST-1"} {
		if err := validRegion(bad); err == nil {
			t.Errorf("%q was accepted as a region", bad)
		}
	}
	for _, good := range []string{"us-east-1", "us-gov-west-1", "ap-southeast-4", "eu-central-2"} {
		if err := validRegion(good); err != nil {
			t.Errorf("%q was refused: %v", good, err)
		}
	}
}
