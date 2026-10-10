// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// client returns an HTTP client with no redirect following and a bounded wait.
//
// A streamed response is read incrementally, so the timeout is on the whole
// exchange rather than on the headers alone; a test that hangs should fail
// rather than sit until the suite's own deadline.
func (d *deployment) client() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

// api performs an authenticated console API call.
//
// The credential is a gateway token rather than a session cookie, which is why
// no CSRF header appears anywhere in this suite: cookies are CSRF-gated and
// bearer tokens are not, and a script holding a token is a supported caller.
func (d *deployment) api(method, path string, body any) *http.Response {
	d.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			d.t.Fatalf("encoding the request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(d.t.Context(), method, d.baseURL+path, reader)
	if err != nil {
		d.t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.client().Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// getJSON calls the API and decodes a successful response.
func (d *deployment) getJSON(path string, out any) {
	d.t.Helper()

	resp := d.api(http.MethodGet, path, nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		d.t.Fatalf("GET %s: %s\n%s", path, resp.Status, raw)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		d.t.Fatalf("decoding %s: %v", path, err)
	}
}

// putJSON writes and fails on anything but success.
func (d *deployment) putJSON(method, path string, body any) {
	d.t.Helper()

	resp := d.api(method, path, body)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		d.t.Fatalf("%s %s: %s\n%s", method, path, resp.Status, raw)
	}
}

// messagesBody builds a Messages API request naming a route.
func messagesBody(route string, stream bool, text string) map[string]any {
	return map[string]any{
		"model":      route,
		"max_tokens": 64,
		"stream":     stream,
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": text}},
			},
		},
	}
}

// messages calls the gateway's only data-plane route as a coding tool would.
func (d *deployment) messages(userAgent string, body map[string]any, headers ...string) *http.Response {
	d.t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		d.t.Fatalf("encoding the messages body: %v", err)
	}

	req, err := http.NewRequestWithContext(d.t.Context(), http.MethodPost,
		d.baseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		d.t.Fatalf("building the messages request: %v", err)
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
		d.t.Fatalf("POST /v1/messages: %v", err)
	}
	return resp
}

// send makes one request and drains it, returning the status and the body.
func (d *deployment) send(userAgent string, body map[string]any, headers ...string) (int, string) {
	d.t.Helper()

	resp := d.messages(userAgent, body, headers...)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		d.t.Fatalf("reading the response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// loggedRequest is one row of the audit trail, as the console API renders it.
type loggedRequest struct {
	RequestID      string  `json:"request_id"`
	Surface        string  `json:"surface"`
	Provider       string  `json:"provider"`
	RequestedModel string  `json:"requested_model"`
	ResolvedModel  string  `json:"resolved_model"`
	Streaming      bool    `json:"streaming"`
	StatusCode     int     `json:"status_code"`
	Decision       string  `json:"decision"`
	ErrorKind      string  `json:"error_kind"`
	BudgetState    string  `json:"budget_state"`
	Attempts       int     `json:"attempts"`
	Tool           string  `json:"tool"`
	UserEmail      string  `json:"user_email"`
	SessionID      string  `json:"session_id"`
	Repo           string  `json:"repo"`
	Branch         string  `json:"branch"`
	WorkItem       string  `json:"work_item"`
	Rung           string  `json:"rung"`
	Priced         bool    `json:"priced"`
	TTFTMillis     float64 `json:"ttft_ms"`
	Tokens         struct {
		Input        int64 `json:"input"`
		Output       int64 `json:"output"`
		CacheRead    int64 `json:"cache_read"`
		CacheWrite   int64 `json:"cache_write"`
		CacheWrite1h int64 `json:"cache_write_1h"`
	} `json:"tokens"`
	Cost struct {
		TotalNanos int64  `json:"total_nanos"`
		TotalUSD   string `json:"total_usd"`
		ByClass    struct {
			Input struct {
				Nanos int64 `json:"nanos"`
			} `json:"input"`
			Output struct {
				Nanos int64 `json:"nanos"`
			} `json:"output"`
			CacheRead struct {
				Nanos int64 `json:"nanos"`
			} `json:"cache_read"`
			CacheWrite struct {
				Nanos int64 `json:"nanos"`
			} `json:"cache_write"`
		} `json:"by_class"`
	} `json:"cost"`
}

// requests reads the audit trail, newest first.
//
// The metadata write happens off the request path, so this polls rather than
// reading once: a row that has not landed yet is the normal state immediately
// after a response, not a failure.
func (d *deployment) requests() []loggedRequest {
	d.t.Helper()

	var page struct {
		Requests []loggedRequest `json:"requests"`
	}
	d.getJSON("/api/v1/requests?limit=100", &page)
	return page.Requests
}

// waitForRequests polls until at least n rows have been recorded.
func (d *deployment) waitForRequests(n int) []loggedRequest {
	d.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	var last []loggedRequest
	for time.Now().Before(deadline) {
		last = d.requests()
		if len(last) >= n {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	d.t.Fatalf("only %d request(s) were recorded, want %d", len(last), n)
	return nil
}

// latest returns the request this deployment most recently sent.
//
// It waits for as many rows as have been sent, not for one, because the trail is
// ordered newest first and a row that has not landed yet would hand back the
// previous test's request.
func (d *deployment) latest() loggedRequest {
	d.t.Helper()

	rows := d.waitForRequests(int(d.sent.Load()))
	return rows[0]
}

// onboarding is what GET /api/v1/setup/onboarding reports.
type onboarding struct {
	HasToken   bool `json:"has_token"`
	HasRequest bool `json:"has_request"`
	Detected   struct {
		Tool       string `json:"tool"`
		Route      string `json:"route"`
		Model      string `json:"model"`
		Repo       string `json:"repo"`
		Branch     string `json:"branch"`
		Attributed bool   `json:"attributed"`
		Describe   string `json:"describe"`
		WorkItem   struct {
			Key        string `json:"key"`
			Kind       string `json:"kind"`
			Rung       string `json:"rung"`
			Confidence string `json:"confidence"`
			Evidence   string `json:"evidence"`
		} `json:"work_item"`
	} `json:"detected"`
}

func (d *deployment) onboarding() onboarding {
	d.t.Helper()

	var out onboarding
	d.getJSON("/api/v1/setup/onboarding", &out)
	return out
}

// setBudget writes an organization budget for a period.
func (d *deployment) setBudget(period string, soft, hard string) {
	d.t.Helper()

	body := map[string]any{
		"scope":  "org",
		"period": period,
		"name":   "e2e",
	}
	if soft != "" {
		body["soft_limit"] = soft
	}
	if hard != "" {
		body["hard_limit"] = hard
	}
	d.putJSON(http.MethodPut, "/api/v1/budgets", body)

	// The policy engine reads a snapshot reloaded on an interval, so a budget
	// written here does not bind until the next cycle. Waiting a little over
	// one is the honest way to test it: a request admitted a moment after this
	// call is enforcement working as designed, not a flake.
	time.Sleep(2 * time.Second)
}

// requireServed fails with the recorded failure rather than with its
// consequences.
//
// A request whose upstream stream broke still returns 200 - the response has
// begun, so a mid-stream failure is reported in band - and records whatever
// usage was observed before it stopped. Asserted on token counts alone that
// arrives as "output = 0, want 500", which sends a reader looking at the
// accounting instead of at the connection that dropped.
func (d *deployment) requireServed(t *testing.T, got loggedRequest) {
	t.Helper()

	if got.ErrorKind != "" {
		// Which side lost the bytes. If the fake wrote every stream in full and
		// the gateway still saw a short one, the loss is in transit rather than
		// in either program - and that is the difference between a bug here and
		// an afternoon spent reading the wrong code.
		completed, failures := d.Fake.Streams()
		t.Fatalf("the request failed upstream (%s); the fake completed %d stream(s) and failed %d: %v",
			got.ErrorKind, completed, len(failures), failures)
	}
	if got.Decision != "allowed" {
		t.Fatalf("decision = %q, want allowed", got.Decision)
	}
}

// errorMessage pulls the message out of an Anthropic-shaped error envelope.
func errorMessage(t *testing.T, body string) string {
	t.Helper()

	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("the error body is not an Anthropic error envelope: %v\n%s", err, body)
	}
	if env.Type != "error" {
		t.Errorf("envelope type = %q, want error", env.Type)
	}
	return env.Error.Message
}

// requireContains fails with the whole subject, which is what makes a refusal
// message worth asserting on readable when it changes.
func requireContains(t *testing.T, subject, want, what string) {
	t.Helper()
	if !strings.Contains(subject, want) {
		t.Errorf("%s does not mention %q:\n%s", what, want, subject)
	}
}
