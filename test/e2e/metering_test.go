// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/fakebedrock"
	"github.com/dnxlabs/panorama/internal/pricing"
)

// TestTheCostIsTheSeedRateAppliedToEveryClass proves the whole metering path,
// not the arithmetic.
//
// The expected figure comes from the same pricing package the gateway uses, so
// this is not a second implementation of the rate table. What it asserts is
// that all four token classes reached the pricer with the right numbers: an
// omitted cache class, a dropped 1h split, or a rate looked up under the wrong
// geography would each land here as a mismatched total.
func TestTheCostIsTheSeedRateAppliedToEveryClass(t *testing.T) {
	d := newDeployment(t)

	status, body := d.send(claudeCodeUA, messagesBody(routeStandard, true, "what did this cost"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}

	got := d.latest()
	d.requireServed(t, got)
	if !got.Priced {
		t.Fatal("the request was recorded unpriced; the seed table should carry this model")
	}

	entries, err := pricing.Seed()
	if err != nil {
		t.Fatalf("reading the seed rates: %v", err)
	}
	u := fakebedrock.DefaultUsage
	want, entry, err := pricing.NewTable(entries).Price("bedrock", fakebedrock.TargetSonnet5US, pricing.Usage{
		InputTokens:        u.InputTokens,
		OutputTokens:       u.OutputTokens,
		CacheReadTokens:    u.CacheReadTokens,
		CacheWriteTokens:   u.Total(),
		CacheWrite1hTokens: u.CacheWrite1hTokens,
	})
	if err != nil {
		t.Fatalf("pricing the fixture usage: %v", err)
	}
	t.Logf("priced against %s (%s)", entry.Key, entry.Source)

	if got.Cost.TotalNanos != int64(want.Total) {
		t.Errorf("total = %d nanodollars, want %d", got.Cost.TotalNanos, int64(want.Total))
	}

	// Every class is asserted separately. A total can match while a class is
	// wrong, and the breakdown is the evidence behind "caching saved you this
	// much" - the claim a reader is most likely to challenge.
	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		{"input", got.Cost.ByClass.Input.Nanos, int64(want.Input)},
		{"output", got.Cost.ByClass.Output.Nanos, int64(want.Output)},
		{"cache read", got.Cost.ByClass.CacheRead.Nanos, int64(want.CacheRead)},
		{"cache write", got.Cost.ByClass.CacheWrite.Nanos, int64(want.CacheWrite)},
	} {
		if c.got != c.want {
			t.Errorf("%s cost = %d nanodollars, want %d", c.name, c.got, c.want)
		}
	}

	// Every class must contribute, or the fixture would let a class be dropped
	// silently and still match.
	if want.CacheRead == 0 || want.CacheWrite == 0 {
		t.Error("the fixture usage prices a cache class at zero; it would prove nothing")
	}
}

// TestBetaFlagsAreTranslated reads the body the provider actually sent.
//
// Bedrock refuses a request carrying a flag it does not know, so this is the
// translation that keeps a current Claude Code working at all - and it is
// invisible from the client's side, which is why the assertion is made against
// what the fake was handed.
func TestBetaFlagsAreTranslated(t *testing.T) {
	d := newDeployment(t)

	// A realistic set: flags Bedrock accepts, plus the two this build knows it
	// refuses.
	sent := []string{
		"claude-code-20250219",
		"context-1m-2025-08-07",
		"fine-grained-tool-streaming-2025-05-14",
		"interleaved-thinking-2025-05-14",
		"prompt-caching-2024-07-31",
		"redact-thinking-2026-02-12",
	}

	status, body := d.send(claudeCodeUA, messagesBody(routeStandard, false, "translate my betas"),
		"anthropic-beta", strings.Join(sent, ","))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}

	received, ok := d.Fake.Last()
	if !ok {
		t.Fatal("the fake was handed no request")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(received.Body, &fields); err != nil {
		t.Fatalf("decoding the body the provider sent: %v", err)
	}

	// Bedrock reads these from the body; the client sent them as a header.
	var betas []string
	if err := json.Unmarshal(fields["anthropic_beta"], &betas); err != nil {
		t.Fatalf("anthropic_beta is missing from the body Bedrock was sent: %v", err)
	}

	for _, refused := range []string{"prompt-caching-2024-07-31", "redact-thinking-2026-02-12"} {
		if slicesContains(betas, refused) {
			t.Errorf("%q was forwarded; Bedrock refuses the whole request over it", refused)
		}
	}
	for _, kept := range []string{"claude-code-20250219", "context-1m-2025-08-07"} {
		if !slicesContains(betas, kept) {
			t.Errorf("%q was dropped; Bedrock accepts it and the feature it enables is lost", kept)
		}
	}

	// The two fields Bedrock rejects outright, and the one it requires.
	if _, present := fields["model"]; present {
		t.Error("model was left in the body; Bedrock refuses that")
	}
	if _, present := fields["stream"]; present {
		t.Error("stream was left in the body; Bedrock refuses that")
	}
	if _, present := fields["anthropic_version"]; !present {
		t.Error("anthropic_version is missing; Bedrock requires it")
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestAStreamIsNotBuffered watches the response arrive.
//
// Sub-10ms added latency to time-to-first-token is the promise; a proxy that
// accumulated the stream and wrote it at the end would satisfy every other test
// in this file and break the one thing the product is built around.
func TestAStreamIsNotBuffered(t *testing.T) {
	const frameDelay = 150 * time.Millisecond
	d := newDeployment(t, withFrameDelay(frameDelay))

	start := time.Now()
	resp := d.messages(claudeCodeUA, messagesBody(routeStandard, true, "stream to me"))
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content type = %q, want text/event-stream", ct)
	}

	var (
		firstEventAt time.Time
		events       int
	)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "event: ") {
			continue
		}
		events++
		if events == 1 {
			firstEventAt = time.Now()
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the stream: %v", err)
	}

	if events < 2 {
		t.Fatalf("%d SSE events arrived, want the whole sequence", events)
	}

	// The fake spaces its frames, so a buffering proxy could not deliver the
	// first one before it had sent them all. Half the total spacing is a wide
	// margin either way: it is far above the real cost of relaying one frame
	// and far below the time a buffered response would take.
	elapsed := firstEventAt.Sub(start)
	budget := frameDelay * time.Duration(events) / 2
	if elapsed > budget {
		t.Errorf("the first event took %s; a stream relayed as it arrives should beat %s",
			elapsed.Round(time.Millisecond), budget)
	}
	t.Logf("first of %d events after %s", events, elapsed.Round(time.Millisecond))
}

// TestTurnsShareASession is what makes attribution cheap: a conversation is
// fingerprinted once and the answer is inherited, rather than re-guessed on
// every turn.
func TestTurnsShareASession(t *testing.T) {
	d := newDeployment(t)

	const opening = "Refactor the pricing table so it reads rates by scope."

	first := messagesBody(routeStandard, false, opening)
	if status, body := d.send(claudeCodeUA, first); status != http.StatusOK {
		t.Fatalf("first turn: status = %d\n%s", status, body)
	}
	one := d.latest()

	// A second turn of the same conversation: the same opening message, with
	// more after it. That growth is the reason the fingerprint is taken from the
	// opening alone.
	second := messagesBody(routeStandard, false, opening)
	turns, ok := second["messages"].([]any)
	if !ok {
		t.Fatalf("the fixture body has no message list: %T", second["messages"])
	}
	second["messages"] = append(turns,
		map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": "Which rates?"}},
		},
		map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": "The cache-write ones."}},
		},
	)
	if status, body := d.send(claudeCodeUA, second); status != http.StatusOK {
		t.Fatalf("second turn: status = %d\n%s", status, body)
	}
	two := d.latest()

	if one.SessionID == "" {
		t.Fatal("the first turn recorded no session")
	}
	if one.SessionID != two.SessionID {
		t.Errorf("the turns are in different sessions (%q and %q); every turn would be its own conversation",
			one.SessionID, two.SessionID)
	}
}

// TestConcurrentStreamsAllComplete drives many streams at once and asserts that
// every one of them arrived whole.
//
// Streaming is the one path this product promises never to interfere with, and
// until now the suite proved that for a single relay at a time. A gateway that
// dropped one stream in a few hundred under load would pass every other test
// here: the ones that read a stream read exactly one, and a truncated response
// still carries a 200 and a usage record, so it fails as a wrong number
// somewhere else rather than as a broken stream.
//
// What it looks for is a response missing its terminal event. That is the shape
// a mid-stream failure takes on the wire - the status and the opening events
// have already gone out, so the only report left is in band - and it is the
// shape CI has intermittently produced on Linux.
func TestConcurrentStreamsAllComplete(t *testing.T) {
	d := newDeployment(t)

	const workers = 8
	const each = 25

	var wg sync.WaitGroup
	short := make(chan string, workers*each)

	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				resp := d.messages(claudeCodeUA,
					messagesBody(routeStandard, true, fmt.Sprintf("stream %d/%d", w, i)))
				raw, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					short <- fmt.Sprintf("worker %d turn %d: reading: %v", w, i, err)
					continue
				}
				if resp.StatusCode != http.StatusOK {
					short <- fmt.Sprintf("worker %d turn %d: status %d", w, i, resp.StatusCode)
					continue
				}
				// message_stop is the last event of the sequence. Its absence
				// is the whole assertion: everything before it can be present
				// in a stream that died.
				if !strings.Contains(string(raw), "event: message_stop") {
					short <- fmt.Sprintf("worker %d turn %d: %d bytes, no message_stop:\n%s",
						w, i, len(raw), raw)
				}
			}
		}(w)
	}
	wg.Wait()
	close(short)

	// Reported together rather than one Fatalf at the first, because whether one
	// stream broke or two hundred did is the difference between a race and a
	// gateway that cannot serve concurrent traffic at all.
	broken := 0
	for msg := range short {
		broken++
		if broken <= 3 {
			t.Error(msg)
		}
	}
	completed, failures := d.Fake.Streams()
	t.Logf("%d streams: %d short; the fake wrote %d in full and stopped early %d time(s) %v",
		workers*each, broken, completed, len(failures), failures)

	// The population the gateway's own trace cannot describe. It reports the
	// connection only for streams that failed, so "every failure was on a
	// reused connection" means one thing if reuse is rare here and nothing at
	// all if it is what almost every stream gets.
	total, reused, conns := d.Fake.Reuse()
	t.Logf("connections: %d of %d streamed invocations arrived on a reused connection, over %d connection(s)",
		reused, total, conns)
	if broken > 3 {
		t.Errorf("%d streams did not complete", broken)
	}
}

// TestAClientHangingUpIsNotAProviderFailure pins the difference between the two
// ways a stream ends early, because they are reported by the same code path and
// look alike in a log.
//
// When the caller goes away mid-stream its request context is cancelled, which
// closes the upstream connection under the read - so the provider error is the
// consequence and the disconnect is the cause. Told apart, the first is nothing
// to investigate and the second is an outage. The assertion is on the two
// fields that distinguish them, and it is also what stops those fields quietly
// becoming a constant: a client_gone that is never true reports nothing.
func TestAClientHangingUpIsNotAProviderFailure(t *testing.T) {
	// Spaced frames, so there is a middle of the stream to hang up in.
	d := newDeployment(t, withFrameDelay(80*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	raw, err := json.Marshal(messagesBody(routeStandard, true, "hang up on me"))
	if err != nil {
		t.Fatalf("encoding the body: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.baseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", claudeCodeUA)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}

	// One event, then hang up with the rest still to come.
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "event: ") {
			break
		}
	}
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		log := d.log.String()
		if strings.Contains(log, "client_gone=true") ||
			strings.Contains(log, "client_disconnected") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a client that hung up mid-stream was not recorded as one:\n%s", d.log.String())
}

// TestAStreamFailureReportsItsConnection covers the wiring between the provider
// and the log, which has a silent failure mode.
//
// The data plane reaches the connection trace through a type assertion to
// providers.Diagnostic, and an assertion that stops matching does not break a
// build or fail a test - it just leaves the fields out, at the one moment they
// are wanted. Nothing else here would notice, because the failure this exists
// to explain cannot be produced on demand.
func TestAStreamFailureReportsItsConnection(t *testing.T) {
	d := newDeployment(t)
	// A well-formed opening event and then an exception frame: the response has
	// begun, so the data plane reports in band and takes the path that carries
	// these fields.
	d.Fake.Arm("", fakebedrock.FaultMidStream, fakebedrock.Always)

	resp := d.messages(claudeCodeUA, messagesBody(routeStandard, true, "break mid-stream"))
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	fields := []string{
		"events_relayed",
		"client_gone",
		// The provider's half. conn_returned_to_pool is the one that matters:
		// a connection already back in the transport's idle pool at the moment
		// its stream failed was one this process had finished with while the
		// reader had not.
		"conn_reused",
		"conn_was_idle",
		"conn_returned_to_pool",
	}

	// Waited for rather than read once. The gateway is a subprocess and its log
	// reaches this buffer through a pipe, so a line written before the response
	// finished can still be half-copied when the response arrives - which shows
	// up as the tail of the line missing, and reads exactly like a field that
	// was never logged.
	missing := func() []string {
		log := d.log.String()
		var absent []string
		for _, f := range fields {
			if !strings.Contains(log, f) {
				absent = append(absent, f)
			}
		}
		return absent
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(missing()) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a failed stream was logged without %v:\n%s", missing(), d.log.String())
}
