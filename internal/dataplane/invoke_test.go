// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/websearch"
)

const testModel = "claude-sonnet-4-5"

// resolvedModel is deliberately different from what the client asks for: the
// Bedrock inference-profile requirement means the two are never the same in
// practice, and the tests should exercise that.
const resolvedModel = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

func TestMessagesRequiresAToken(t *testing.T) {
	h := newFixture(t, &fakeProvider{})

	for _, tt := range []struct {
		name    string
		headers map[string]string
	}{
		{name: "no credentials"},
		{name: "unknown token", headers: map[string]string{"Authorization": "Bearer " + mustToken(t)}},
		{name: "a real anthropic key", headers: map[string]string{"x-api-key": "sk-ant-api03-" + strings.Repeat("a", 32)}},
		{name: "wrong scheme", headers: map[string]string{"Authorization": "Basic " + mustToken(t)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}

			var body apiError
			decode(t, rec.Body.Bytes(), &body)
			if body.Type != "error" || body.Error.Type != errTypeAuthentication {
				t.Errorf("body = %+v, want an Anthropic authentication_error envelope", body)
			}
			if body.Error.Message == "" {
				t.Error("the error carries no message; an engineer cannot act on it")
			}
		})
	}

	// The provider must never be reached without a valid token.
	if h.provider.invocations != 0 {
		t.Errorf("provider was invoked %d times for unauthenticated requests", h.provider.invocations)
	}
}

func TestMessagesRefusesANameNobodyRouted(t *testing.T) {
	h := newFixture(t, &fakeProvider{})

	rec := h.do(t, `{"model":"gpt-4o","max_tokens":16,"messages":[]}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	var body apiError
	decode(t, rec.Body.Bytes(), &body)
	if body.Error.Type != errTypePermission {
		t.Errorf("error type = %q, want %q", body.Error.Type, errTypePermission)
	}
	// The message has to say what was refused, which routes this person can use
	// instead, and who decides - the refusal is part of the product, not an
	// error string. A model id is exactly the kind of thing a client sends when
	// it has not been pointed at a route, so it is the case worth naming.
	for _, want := range []string{"gpt-4o", testModel, "administrator"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Errorf("message %q does not mention %q", body.Error.Message, want)
		}
	}

	if h.provider.invocations != 0 {
		t.Error("a refused model still reached the provider")
	}

	h.drain()
	got := h.latest(t)
	if got.Decision != store.DecisionRefused {
		t.Errorf("decision = %q, want %q", got.Decision, store.DecisionRefused)
	}
	if got.RequestedModel != "gpt-4o" {
		t.Errorf("requested model = %q, want it recorded even though it was refused", got.RequestedModel)
	}
}

func TestMessagesNonStreaming(t *testing.T) {
	provider := &fakeProvider{
		response: &providers.Response{
			Body: []byte(`{"id":"msg_1","content":[{"type":"text","text":"OK."}]}`),
			Usage: providers.Usage{
				InputTokens: 10, OutputTokens: 5, CacheReadTokens: 900, CacheWriteTokens: 40,
			},
		},
	}
	h := newFixture(t, provider)

	rec := h.do(t, `{"model":"`+testModel+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"msg_1"`) {
		t.Errorf("body = %s, want the provider response relayed verbatim", got)
	}

	// The provider must be asked for the resolved id, not what the client sent.
	if provider.lastModel != resolvedModel {
		t.Errorf("provider invoked with %q, want the resolved %q", provider.lastModel, resolvedModel)
	}

	h.drain()
	got := h.latest(t)
	if got.StatusCode != 200 {
		t.Errorf("recorded status = %d, want 200", got.StatusCode)
	}
	if got.RequestedModel != testModel || got.ResolvedModel != resolvedModel {
		t.Errorf("recorded models = %q → %q, want %q → %q",
			got.RequestedModel, got.ResolvedModel, testModel, resolvedModel)
	}
	if got.Decision != store.DecisionAllowed {
		t.Errorf("decision = %q, want %q: the route sent this request where it sends traffic by default, "+
			"and a column that said \"routed\" for every request would answer nothing",
			got.Decision, store.DecisionAllowed)
	}
	if got.InputTokens != 10 || got.OutputTokens != 5 || got.CacheReadTokens != 900 || got.CacheWriteTokens != 40 {
		t.Errorf("recorded tokens = %d/%d/%d/%d, want 10/5/900/40",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheWriteTokens)
	}
	if got.Tool != "claude-code" {
		t.Errorf("tool = %q, want claude-code from the user agent", got.Tool)
	}
	if got.Streaming {
		t.Error("recorded as streaming, want false")
	}
}

// TestMessagesStreamingFlushesEveryEvent is the test the proxy exists for.
// A proxy that accumulates events before writing them is indistinguishable from
// a working one in a response-body assertion, and useless in practice.
func TestMessagesStreamingFlushesEveryEvent(t *testing.T) {
	provider := &fakeProvider{
		events: [][]byte{
			[]byte(`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":800,"cache_creation_input_tokens":30}}}`),
			[]byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`),
			[]byte(`{"type":"message_delta","usage":{"output_tokens":7}}`),
			[]byte(`{"type":"message_stop"}`),
		},
		usage: providers.Usage{InputTokens: 12, OutputTokens: 7, CacheReadTokens: 800, CacheWriteTokens: 30},
	}
	h := newFixture(t, provider)

	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := h.request(`{"model":"` + testModel + `","max_tokens":16,"stream":true,"messages":[]}`)
	h.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := w.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no - an intermediary would otherwise buffer the stream", got)
	}

	// Every event must be followed by a flush before the next one is written.
	// Anything else means output sat in a buffer while the client waited.
	writes := 0
	for i, op := range w.ops {
		if op == "write" {
			writes++
			if i+1 >= len(w.ops) || w.ops[i+1] != "flush" {
				t.Errorf("write %d was not followed by a flush; the stream is being buffered", writes)
			}
		}
	}
	if writes < len(provider.events) {
		t.Errorf("%d writes for %d events; events are being coalesced", writes, len(provider.events))
	}

	body := w.Body.String()
	for _, want := range []string{
		"event: message_start\ndata: {",
		"event: content_block_delta\ndata: {",
		"event: message_delta\ndata: {",
		"event: message_stop\ndata: {",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %q\n---\n%s", want, body)
		}
	}

	h.drain()
	got := h.latest(t)
	if !got.Streaming {
		t.Error("recorded as non-streaming")
	}
	if got.InputTokens != 12 || got.OutputTokens != 7 || got.CacheReadTokens != 800 || got.CacheWriteTokens != 30 {
		t.Errorf("recorded tokens = %d/%d/%d/%d, want 12/7/800/30",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheWriteTokens)
	}
	if got.TimeToFirstToken <= 0 {
		t.Error("time to first token was not measured; the latency budget is unenforceable without it")
	}
}

func TestMessagesStreamingReportsAMidStreamFailureInBand(t *testing.T) {
	provider := &fakeProvider{
		events: [][]byte{[]byte(`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`)},
		streamErr: &providers.Error{
			Kind: providers.ErrKindThrottled, StatusCode: http.StatusTooManyRequests,
			Message: "throttled",
		},
	}
	h := newFixture(t, provider)

	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.mux.ServeHTTP(w, h.request(`{"model":"`+testModel+`","max_tokens":16,"stream":true,"messages":[]}`))

	// The status is committed the moment streaming starts, so a later failure
	// cannot change it - it has to arrive as an event.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want the already-committed 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "event: error") {
		t.Errorf("no in-band error event in the stream:\n%s", w.Body.String())
	}

	h.drain()
	if got := h.latest(t); got.ErrorKind != string(providers.ErrKindThrottled) {
		t.Errorf("error kind = %q, want %q", got.ErrorKind, providers.ErrKindThrottled)
	}
}

func TestMessagesMapsProviderFailures(t *testing.T) {
	tests := []struct {
		kind       providers.ErrorKind
		wantStatus int
		wantType   string
	}{
		{providers.ErrKindThrottled, http.StatusTooManyRequests, errTypeRateLimit},
		{providers.ErrKindUnavailable, http.StatusServiceUnavailable, errTypeOverloaded},
		{providers.ErrKindInvalidRequest, http.StatusBadRequest, errTypeInvalidRequest},
		{providers.ErrKindForbidden, http.StatusBadGateway, errTypeAPI},
		{providers.ErrKindNotFound, http.StatusBadGateway, errTypeAPI},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			h := newFixture(t, &fakeProvider{
				invokeErr: &providers.Error{Kind: tt.kind, StatusCode: tt.wantStatus, Message: "upstream detail"},
			})

			rec := h.do(t, `{"model":"`+testModel+`","max_tokens":16,"messages":[]}`)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			var body apiError
			decode(t, rec.Body.Bytes(), &body)
			if body.Error.Type != tt.wantType {
				t.Errorf("error type = %q, want %q", body.Error.Type, tt.wantType)
			}
			// Provider text leaks account and model detail and tells the
			// client nothing it can act on.
			if strings.Contains(body.Error.Message, "upstream detail") {
				t.Errorf("the provider's own message leaked to the client: %q", body.Error.Message)
			}

			h.drain()
			if got := h.latest(t); got.Decision != store.DecisionError {
				t.Errorf("decision = %q, want %q", got.Decision, store.DecisionError)
			}
		})
	}
}

func TestMessagesRejectsAMalformedBody(t *testing.T) {
	h := newFixture(t, &fakeProvider{})

	rec := h.do(t, `not json at all`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if h.provider.invocations != 0 {
		t.Error("a malformed body reached the provider")
	}
}

func TestMessagesEnforcesTheBodyCeiling(t *testing.T) {
	h := newFixture(t, &fakeProvider{})
	h.dp.maxBodyBytes = 64

	rec := h.do(t, `{"model":"`+testModel+`","padding":"`+strings.Repeat("x", 500)+`"}`)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if h.provider.invocations != 0 {
		t.Error("an oversized body reached the provider")
	}
}

// TestNoRequestContentIsPersisted guards the product's central promise from the
// storage side: whatever else the metering path records, it must not be
// possible for prompt text to reach the database.
func TestNoRequestContentIsPersisted(t *testing.T) {
	const secret = "SECRET-PROMPT-TEXT-THAT-MUST-NOT-PERSIST"

	h := newFixture(t, &fakeProvider{
		response: &providers.Response{Body: []byte(`{"content":[{"type":"text","text":"` + secret + `"}]}`)},
	})

	rec := h.do(t, `{"model":"`+testModel+`","max_tokens":16,"messages":[{"role":"user","content":"`+secret+`"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	h.drain()

	// Sweep every text column of every table rather than asserting about the
	// columns we happen to know: a future migration that adds a body column
	// should fail this test.
	rows, err := h.db.QueryContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, table := range tables {
		// Table names come from sqlite_master, not from any input.
		dump, err := h.db.QueryContext(t.Context(), "SELECT * FROM "+table)
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		cols, err := dump.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for dump.Next() {
			cells := make([]any, len(cols))
			for i := range cells {
				cells[i] = new(any)
			}
			if serr := dump.Scan(cells...); serr != nil {
				t.Fatal(serr)
			}
			for i, c := range cells {
				if v, ok := (*c.(*any)).(string); ok && strings.Contains(v, secret) {
					t.Fatalf("prompt text was persisted in %s.%s", table, cols[i])
				}
			}
		}
		if err := dump.Err(); err != nil {
			t.Fatal(err)
		}
		dump.Close()
	}
}

// --- fixture ---------------------------------------------------------------

type fixture struct {
	dp       *DataPlane
	db       *store.DB
	mux      *http.ServeMux
	provider *fakeProvider
	token    string
	user     *store.User
	engine   *policy.Engine
}

func newFixture(t *testing.T, provider *fakeProvider) *fixture {
	t.Helper()
	return newPricedFixture(t, provider, testPrices())
}

// fixtureConfig varies the parts of the data plane a test cares about. The
// zero value is the default fixture.
type fixtureConfig struct {
	provider *fakeProvider
	prices   *pricing.Table
	// set overrides the policy in force. Nil allows one route, testModel,
	// pointing at resolvedModel - which is every test that is not about
	// routing.
	//
	// There is no "no policy" option, because there is no such deployment: the
	// routes are the allowlist, so an engine with none refuses everything.
	set         *policy.Set
	attribution *attribution.Extractor
	// apis is what every model on the fixture's provider speaks. Nil speaks all
	// three, which is every test that is not about which surface a route is
	// served on.
	apis []providers.API
	// webSearch is the backend /mcp searches with. Nil serves no search.
	webSearch *websearch.Service
}

// testPrices prices the model the fixture resolves to, at Claude Haiku 4.5's
// published regional rates.
func testPrices() *pricing.Table {
	model, scope := pricing.ParseModel(resolvedModel)
	return pricing.NewTable([]pricing.Entry{{
		Key: pricing.Key{Provider: "bedrock", Model: model, Scope: scope},
		Rates: pricing.Rates{
			Input:        1_100_000_000,
			Output:       5_500_000_000,
			CacheRead:    110_000_000,
			CacheWrite:   1_375_000_000,
			CacheWrite1h: 2_200_000_000,
		},
		Currency: pricing.CurrencyUSD,
		Source:   pricing.SourceSeed,
	}})
}

func newPricedFixture(t *testing.T, provider *fakeProvider, prices *pricing.Table) *fixture {
	t.Helper()
	return newFixtureWith(t, fixtureConfig{provider: provider, prices: prices})
}

func newFixtureWith(t *testing.T, c fixtureConfig) *fixture {
	t.Helper()

	provider, prices := c.provider, c.prices
	if prices == nil {
		prices = testPrices()
	}
	set := policy.Set{Routes: []policy.Route{{
		Name: testModel, Target: bedrock(resolvedModel), ServesOverSoft: true,
	}}}
	if c.set != nil {
		set = *c.set
	}
	if set.Providers == nil {
		apis := c.apis
		if apis == nil {
			apis = providers.APIs()
		}
		set.Providers = servingProvider(provider, apis)
	}

	engine := policy.New(policy.Options{
		Repository:     &stubPolicyRepo{set: set},
		ReloadInterval: -1, // one-shot: no background loop to race
	})
	if err := engine.Load(t.Context()); err != nil {
		t.Fatalf("load policy: %v", err)
	}

	cfg := config.Default()
	cfg.Database.DSN = "file:" + migratedDatabaseFile(t)

	db, err := store.Open(t.Context(), cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", store.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	minted, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateGatewayToken(t.Context(), user.ID, "test", minted.Hash, minted.DisplayPrefix, nil); err != nil {
		t.Fatal(err)
	}

	dp := New(Options{
		Store:       db,
		Prices:      prices,
		Policy:      engine,
		Attribution: c.attribution,
		WebSearch:   c.webSearch,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	mux := http.NewServeMux()
	for pattern, handler := range dp.Routes() {
		mux.Handle(pattern, handler)
	}

	return &fixture{dp: dp, db: db, mux: mux, provider: provider,
		token: minted.Plaintext, user: user, engine: engine}
}

func (f *fixture) request(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("User-Agent", "claude-cli/1.2.3 (external)")
	return req
}

func (f *fixture) do(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, f.request(body))
	return rec
}

func (f *fixture) drain() { f.dp.Drain() }

func (f *fixture) latest(t *testing.T) *store.RequestRecord {
	t.Helper()
	rec, err := f.db.LatestRequest(t.Context())
	if err != nil {
		t.Fatalf("LatestRequest: %v", err)
	}
	return rec
}

// bedrock names a model on the bedrock provider, which is the kind the fixture's
// fake provider is registered as.
func bedrock(model string) providers.ModelRef {
	return providers.ModelRef{Provider: providers.KindBedrock, Model: model}
}

// servingProvider puts a fake provider in the snapshot as a built, serving
// bedrock provider whose every model speaks apis.
func servingProvider(p *fakeProvider, apis []providers.API) map[providers.Kind]providers.State {
	settings := providers.Settings{Kind: providers.KindBedrock, Enabled: true, Region: "us-east-1"}
	instance := &providers.Instance{
		Settings: settings,
		Provider: p,
		APIs:     func(string) []providers.API { return apis },
	}
	return map[providers.Kind]providers.State{
		providers.KindBedrock: {Settings: settings, Instance: instance},
	}
}

// --- fake provider ---------------------------------------------------------

type fakeProvider struct {
	response  *providers.Response
	invokeErr error
	// failFor fails only for particular provider models.
	failFor map[string]error

	events    [][]byte
	streamErr error
	usage     providers.Usage

	invocations int
	lastModel   string
	// lastRequest is what the provider was handed, so a test can read the API
	// and the top-level rewrites a surface made.
	lastRequest *providers.Request
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Invoke(_ context.Context, req *providers.Request) (*providers.Response, error) {
	p.invocations++
	p.lastModel = req.Model
	p.lastRequest = req
	if err := p.failure(req.Model); err != nil {
		return nil, err
	}
	if p.response == nil {
		return &providers.Response{Body: []byte(`{}`)}, nil
	}
	return p.response, nil
}

func (p *fakeProvider) InvokeStream(_ context.Context, req *providers.Request) (providers.EventStream, error) {
	p.invocations++
	p.lastModel = req.Model
	p.lastRequest = req
	if err := p.failure(req.Model); err != nil {
		return nil, err
	}
	return &fakeStream{events: p.events, err: p.streamErr, usage: p.usage}, nil
}

// failure reports whether this invocation should fail: unconditionally, or for
// this model alone.
func (p *fakeProvider) failure(model string) error {
	if p.invokeErr != nil {
		return p.invokeErr
	}
	return p.failFor[model]
}

type fakeStream struct {
	events [][]byte
	i      int
	err    error
	usage  providers.Usage
}

func (s *fakeStream) Next() ([]byte, error) {
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, nil
	}
	if s.err != nil {
		err := s.err
		s.err = nil
		return nil, err
	}
	return nil, io.EOF
}

func (s *fakeStream) Usage() providers.Usage { return s.usage }
func (s *fakeStream) Close() error           { return nil }

// --- helpers ---------------------------------------------------------------

// flushRecorder records the interleaving of writes and flushes, which is the
// only way to tell a streaming proxy from a buffering one.
type flushRecorder struct {
	*httptest.ResponseRecorder
	ops []string
}

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.ops = append(f.ops, "write")
	return f.ResponseRecorder.Write(b)
}

func (f *flushRecorder) Flush() {
	f.ops = append(f.ops, "flush")
	f.ResponseRecorder.Flush()
}

func mustToken(t *testing.T) string {
	t.Helper()
	tok, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return tok.Plaintext
}

func decode(t *testing.T, b []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

var _ = errors.Is
