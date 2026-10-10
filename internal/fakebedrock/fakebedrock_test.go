// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package fakebedrock_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/dnxlabs/panorama/internal/fakebedrock"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/providers/bedrock"
)

// start brings up the fake and points the AWS SDK at it.
//
// The environment is the whole seam. LoadDefaultConfig copies these into
// cfg.ConfigSources and the SDK's own service-specific endpoint resolution
// reads them from there, so a provider built after this call talks to the fake
// with nothing in internal/providers/bedrock knowing that it does.
func start(t *testing.T, opts ...fakebedrock.Option) *fakebedrock.Server {
	t.Helper()

	fake := fakebedrock.New(opts...)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", srv.URL)
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK", srv.URL)
	// The SDK still signs; nothing here verifies the signature. Any credential
	// will do, and none of these reach anything real.
	t.Setenv("AWS_ACCESS_KEY_ID", "fake")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")
	// A shared config file on the machine running the tests must not decide
	// what these talk to.
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	return fake
}

func newProvider(t *testing.T) *bedrock.Provider {
	t.Helper()
	p, err := bedrock.New(t.Context(), bedrock.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	return p
}

func body(t *testing.T, betas ...string) []byte {
	t.Helper()
	req := map[string]any{
		"model":      "a-route-name",
		"stream":     true,
		"max_tokens": 64,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}
	if len(betas) > 0 {
		req["anthropic_beta"] = betas
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encoding the request body: %v", err)
	}
	return raw
}

// TestStreamingThroughTheRealProvider is the test the whole harness rests on.
//
// If the endpoint override or the event-stream framing were wrong, it would be
// wrong here rather than three layers up in a CI matrix.
func TestStreamingThroughTheRealProvider(t *testing.T) {
	start(t)
	p := newProvider(t)

	stream, err := p.InvokeStream(t.Context(), &providers.Request{
		Model: fakebedrock.TargetSonnet5US,
		Body:  body(t),
	})
	if err != nil {
		t.Fatalf("InvokeStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var events [][]byte
	for {
		payload, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		events = append(events, payload)
	}

	if len(events) != 6 {
		t.Fatalf("got %d events, want 6", len(events))
	}

	// The payload must arrive as the raw Anthropic event, because that is what
	// the data plane relays byte for byte into an SSE frame.
	var first struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(events[0], &first); err != nil {
		t.Fatalf("first event is not JSON: %v", err)
	}
	if first.Type != "message_start" {
		t.Errorf("first event is %q, want message_start", first.Type)
	}

	want := providers.Usage{
		InputTokens:        1000,
		OutputTokens:       500,
		CacheReadTokens:    800,
		CacheWriteTokens:   300,
		CacheWrite1hTokens: 100,
	}
	if got := stream.Usage(); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestNonStreamingThroughTheRealProvider(t *testing.T) {
	start(t)
	p := newProvider(t)

	resp, err := p.Invoke(t.Context(), &providers.Request{
		Model: fakebedrock.TargetHaiku45US,
		Body:  body(t),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if resp.Usage.InputTokens != 1000 || resp.Usage.OutputTokens != 500 {
		t.Errorf("usage = %+v, want the default accounting", resp.Usage)
	}
	if resp.Usage.CacheWriteTokens != 300 || resp.Usage.CacheWrite1hTokens != 100 {
		t.Errorf("cache write split = %d/%d, want 300/100",
			resp.Usage.CacheWriteTokens, resp.Usage.CacheWrite1hTokens)
	}
}

// TestTheModelReachesTheFakeUnescaped guards the one place a model id can be
// mangled: it travels as a path segment, and Haiku's id carries a colon.
func TestTheModelReachesTheFakeUnescaped(t *testing.T) {
	fake := start(t)
	p := newProvider(t)

	if _, err := p.Invoke(t.Context(), &providers.Request{
		Model: fakebedrock.TargetHaiku45US,
		Body:  body(t),
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	got, ok := fake.Last()
	if !ok {
		t.Fatal("the fake recorded no request")
	}
	if got.Model != fakebedrock.TargetHaiku45US {
		t.Errorf("model = %q, want %q", got.Model, fakebedrock.TargetHaiku45US)
	}
}

// TestPrepareBodyIsExercised is why the fake sits at the wire. The body it
// records is what the provider sent, after the translation between dialects.
func TestPrepareBodyIsExercised(t *testing.T) {
	fake := start(t)
	p := newProvider(t)

	// One flag Bedrock accepts, and one it refuses outright.
	sent := []string{"context-1m-2025-08-07", "prompt-caching-2024-07-31"}
	if _, err := p.Invoke(t.Context(), &providers.Request{
		Model: fakebedrock.TargetSonnet5US,
		Body:  body(t),
		Betas: sent,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	got, ok := fake.Last()
	if !ok {
		t.Fatal("the fake recorded no request")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &fields); err != nil {
		t.Fatalf("decoding the recorded body: %v", err)
	}

	if _, present := fields["model"]; present {
		t.Error("model reached Bedrock in the body; it belongs in the API call")
	}
	if _, present := fields["stream"]; present {
		t.Error("stream reached Bedrock in the body; it belongs in the API call")
	}

	var version string
	if err := json.Unmarshal(fields["anthropic_version"], &version); err != nil {
		t.Fatalf("anthropic_version is missing or not a string: %v", err)
	}
	if version != "bedrock-2023-05-31" {
		t.Errorf("anthropic_version = %q", version)
	}

	var betas []string
	if err := json.Unmarshal(fields["anthropic_beta"], &betas); err != nil {
		t.Fatalf("anthropic_beta is missing or not a list: %v", err)
	}
	if len(betas) != 1 || betas[0] != "context-1m-2025-08-07" {
		t.Errorf("anthropic_beta = %v, want the unsupported flag dropped", betas)
	}
}

// TestFaultsClassify walks each fault through the SDK and into classify. The
// kind is what decides the status and error type the client is sent - a
// throttle is a 429 it retries, a denial is not - so getting it from the real
// error rather than from a hand-built one is the point.
func TestFaultsClassify(t *testing.T) {
	cases := []struct {
		fault     fakebedrock.Fault
		kind      providers.ErrorKind
		retryable bool
	}{
		{fakebedrock.FaultThrottling, providers.ErrKindThrottled, true},
		{fakebedrock.FaultUnavailable, providers.ErrKindUnavailable, true},
		{fakebedrock.FaultAccessDenied, providers.ErrKindForbidden, false},
		{fakebedrock.FaultValidation, providers.ErrKindInvalidRequest, false},
		{fakebedrock.FaultResourceMissing, providers.ErrKindNotFound, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.fault), func(t *testing.T) {
			fake := start(t)
			p := newProvider(t)
			fake.Arm("", tc.fault, fakebedrock.Always)

			_, err := p.Invoke(t.Context(), &providers.Request{
				Model: fakebedrock.TargetSonnet5US,
				Body:  body(t),
			})
			if err == nil {
				t.Fatal("Invoke succeeded; want the armed fault")
			}

			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error is %T, want a *providers.Error: %v", err, err)
			}
			if pe.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", pe.Kind, tc.kind)
			}

			// The SDK retries a retryable failure before the provider sees it.
			// Asserting the attempt count records that, because it is what
			// makes a fault armed for a single use useless.
			attempts := len(fake.Received())
			if tc.retryable && attempts < 2 {
				t.Errorf("the SDK made %d attempt(s); a retryable fault should have been retried", attempts)
			}
			if !tc.retryable && attempts != 1 {
				t.Errorf("the SDK made %d attempts; a terminal fault should not be retried", attempts)
			}
		})
	}
}

// TestAFaultIsArmedForOneModel arms a fault for one model and leaves the rest
// healthy.
func TestAFaultIsArmedForOneModel(t *testing.T) {
	fake := start(t)
	p := newProvider(t)
	fake.Arm(fakebedrock.TargetSonnet5US, fakebedrock.FaultThrottling, fakebedrock.Always)

	if _, err := p.Invoke(t.Context(), &providers.Request{
		Model: fakebedrock.TargetSonnet5US, Body: body(t),
	}); err == nil {
		t.Fatal("the armed model was served; want it throttled")
	}

	if _, err := p.Invoke(t.Context(), &providers.Request{
		Model: fakebedrock.TargetHaiku45US, Body: body(t),
	}); err != nil {
		t.Fatalf("a model with no fault armed was refused: %v", err)
	}
}

// TestMidStreamFailure covers a failure after the response has begun, which can
// only be reported in band.
func TestMidStreamFailure(t *testing.T) {
	fake := start(t)
	p := newProvider(t)
	fake.Arm("", fakebedrock.FaultMidStream, fakebedrock.Always)

	stream, err := p.InvokeStream(t.Context(), &providers.Request{
		Model: fakebedrock.TargetSonnet5US, Body: body(t),
	})
	if err != nil {
		t.Fatalf("InvokeStream failed before the first byte: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, first := stream.Next(); first != nil {
		t.Fatalf("the opening event did not arrive: %v", first)
	}

	_, err = stream.Next()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("the stream ended cleanly; want the mid-stream failure, got %v", err)
	}
}

// TestCatalogueThroughTheRealSource exercises discovery, including the manual
// NextToken loop over inference profiles.
func TestCatalogueThroughTheRealSource(t *testing.T) {
	start(t)

	cat, err := bedrock.NewCatalogue(t.Context(), bedrock.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("building the catalogue source: %v", err)
	}

	entries, err := cat.Catalogue(t.Context())
	if err != nil {
		t.Fatalf("Catalogue: %v", err)
	}

	byID := make(map[string]providers.CatalogueEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}

	// Every profile in the fixture must survive discovery. Five of them across
	// pages of two is what proves the paging loop ran to the end.
	for _, want := range []string{
		fakebedrock.TargetSonnet5US,
		fakebedrock.TargetSonnet5Global,
		fakebedrock.TargetHaiku45US,
		fakebedrock.TargetHaiku45Global,
		fakebedrock.TargetOpus5US,
	} {
		if _, ok := byID[want]; !ok {
			t.Errorf("%s is missing from the catalogue", want)
		}
	}

	sonnet, ok := byID[fakebedrock.TargetSonnet5US]
	if !ok {
		t.Fatal("the us Sonnet profile is missing")
	}
	if sonnet.Geography != "us" {
		t.Errorf("geography = %q, want us", sonnet.Geography)
	}
	if sonnet.FoundationModelID != fakebedrock.FoundationSonnet5 {
		t.Errorf("foundation model = %q", sonnet.FoundationModelID)
	}
	if !sonnet.Streaming {
		t.Error("Sonnet is not marked streaming")
	}
	if len(sonnet.Regions) == 0 {
		t.Error("no regions; residency cannot be answered")
	}

	// The bare id is offered only where AWS reports ON_DEMAND, and in this
	// fixture - as in us-east-1 - the one model that does is LEGACY.
	legacy, ok := byID[fakebedrock.FoundationLegacy]
	if !ok {
		t.Fatal("the on-demand model is missing from the catalogue")
	}
	if legacy.Lifecycle != "LEGACY" {
		t.Errorf("lifecycle = %q, want LEGACY", legacy.Lifecycle)
	}
	if legacy.RequiresProfile {
		t.Error("the on-demand model is marked as requiring a profile")
	}
}

// TestTheConnectionIsTraced covers the diagnostics attached to a streamed
// invocation, which exist for a failure nobody can reproduce on demand.
//
// A trace that quietly reported nothing would look exactly like a trace that
// reported everything was fine, and the one occasion it is read is the one
// occasion nobody can go back and check. So it is asserted on a run that works:
// the first stream dials, a later one is served over the connection it left
// behind, and the transport says so.
//
// What is not asserted is that any particular invocation reuses a connection.
// Reuse is the transport's own decision and net/http promises it nowhere: a
// request that arrives before the last connection has been handed back queues
// for a dial and gets a new socket, and a connection the read loop declines to
// park is closed rather than pooled. Both are documented behaviour, and this
// test read either of them as a broken trace - which failed CI twice on a
// provider that had done nothing wrong. So every invocation is checked against
// what the fake watched arrive, and the claim is made on one that really was
// served over a pooled connection.
func TestTheConnectionIsTraced(t *testing.T) {
	fake := start(t)
	p := newProvider(t)

	drain := func(t *testing.T) []slog.Attr {
		t.Helper()

		stream, err := p.InvokeStream(t.Context(), &providers.Request{
			Model: fakebedrock.TargetSonnet5US,
			Body:  body(t),
		})
		if err != nil {
			t.Fatalf("InvokeStream: %v", err)
		}
		defer func() { _ = stream.Close() }()

		for {
			if _, rerr := stream.Next(); errors.Is(rerr, io.EOF) {
				break
			} else if rerr != nil {
				t.Fatalf("reading the stream: %v", rerr)
			}
		}

		diag, ok := stream.(providers.Diagnostic)
		if !ok {
			t.Fatal("a Bedrock event stream no longer reports its connection")
		}
		return diag.Diagnostics()
	}

	lookup := func(attrs []slog.Attr, key string) (slog.Value, bool) {
		for _, a := range attrs {
			if a.Key == key {
				return a.Value, true
			}
		}
		return slog.Value{}, false
	}
	flag := func(t *testing.T, attrs []slog.Attr, key string) bool {
		t.Helper()
		v, ok := lookup(attrs, key)
		if !ok {
			t.Fatalf("no %q in the trace: %v", key, attrs)
		}
		return v.Bool()
	}

	// pooledStreams is how many streamed invocations the fake has seen arrive
	// on a connection that had already carried one. It keys that on the
	// client's own address, so it is the transport's decision observed from
	// the other end of it - the ground truth this trace is checked against.
	pooledStreams := func() int {
		_, reused, _ := fake.Reuse()
		return reused
	}

	first := drain(t)
	// Present only when no connection was handed over, which is what a trace
	// whose hooks never fired would report.
	if _, missing := lookup(first, "conn_never_acquired"); missing {
		t.Errorf("the first stream reports no connection was acquired: %v", first)
	}
	if flag(t, first, "conn_reused") {
		t.Errorf("the first stream on a new provider reports a reused connection: %v", first)
	}

	// The invocations after it are the assertion that the hooks are live rather
	// than returning zero values: a fresh connection and a pooled one differ
	// here, and a trace that reported false for both would pass the check above
	// and mean nothing.
	//
	// Bounded at five, because a transport that opens a new connection for six
	// consecutive streams to one host is a finding rather than weather.
	const attempts = 5
	traces := [][]slog.Attr{first}
	for i := range attempts {
		before := pooledStreams()
		next := drain(t)
		traces = append(traces, next)

		if pooledStreams() == before {
			// The transport opened a connection rather than taking the one
			// waiting for it, so conn_reused being false is the truth and
			// there is nothing here to assert. Ask again.
			continue
		}

		// From here the fake watched this stream arrive on a connection it had
		// already served, so the trace denying it is a hook that is not firing
		// - which is the whole failure this test exists to catch.
		if !flag(t, next, "conn_reused") {
			t.Fatalf("the fake served stream %d on a connection it had already used, and the trace denies it: %v", i+2, next)
		}
		if flag(t, next, "conn_was_idle") {
			return
		}
		// Reused and never idle: the transport handed this connection straight
		// to a request already queued for a dial, which reports no idle time
		// and is a real path rather than a broken hook. Ask again for one that
		// waited in the pool.
	}
	t.Errorf("no stream was reported as served over a pooled connection in %d attempts: %v", attempts+1, traces)
}
