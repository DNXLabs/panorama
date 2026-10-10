// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/websearch"
)

const claudeCodeUA = "claude-cli/2.1.0 (external, cli)"

// fakeSearch is a backend that records what it was asked and answers from a
// fixed list.
type fakeSearch struct {
	results []websearch.Result
	err     error
	asked   []websearch.Query
}

func (f *fakeSearch) Kind() websearch.Kind { return websearch.KindAgentCore }
func (f *fakeSearch) Check(context.Context) error {
	return nil
}
func (f *fakeSearch) Search(_ context.Context, q websearch.Query) ([]websearch.Result, error) {
	f.asked = append(f.asked, q)
	return f.results, f.err
}

func searchService(t *testing.T, backend websearch.Backend, enabled bool, price *pricing.Money) *websearch.Service {
	t.Helper()
	svc := websearch.NewService(
		func(context.Context) (websearch.Config, bool, error) {
			c := websearch.Config{
				Kind: websearch.KindAgentCore, Enabled: enabled,
				GatewayURL: "https://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp",
				Price:      websearch.PublishedPrice, PriceSource: "aws-published",
			}
			if price != nil {
				c.Price, c.PriceSource = *price, "config"
			}
			return c, true, nil
		},
		func(context.Context, websearch.Config) (websearch.Backend, error) { return backend, nil },
		0, nil)
	if err := svc.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	return svc
}

func newSearchFixture(t *testing.T, backend websearch.Backend, enabled bool, set *policy.Set) *fixture {
	t.Helper()
	return newFixtureWith(t, fixtureConfig{
		provider: &fakeProvider{}, set: set,
		webSearch: searchService(t, backend, enabled, nil),
	})
}

type rpcReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcErrorBody   `json:"error"`
}

func (f *fixture) rpc(t *testing.T, method string, params any) (*httptest.ResponseRecorder, rpcReply) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	rec := f.post(t, "/mcp", claudeCodeUA, string(body))
	var reply rpcReply
	if rec.Code == http.StatusOK {
		decode(t, rec.Body.Bytes(), &reply)
		if string(reply.ID) != "7" {
			t.Fatalf("reply id = %s, want the request's", reply.ID)
		}
	}
	return rec, reply
}

type callResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (f *fixture) search(t *testing.T, args map[string]any) callResult {
	t.Helper()
	rec, reply := f.rpc(t, "tools/call", map[string]any{"name": "web_search", "arguments": args})
	if rec.Code != http.StatusOK || reply.Error != nil {
		t.Fatalf("tools/call: %d %s", rec.Code, rec.Body)
	}
	var res callResult
	decode(t, reply.Result, &res)
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("a tool result is one text block: %s", reply.Result)
	}
	return res
}

func TestTheMCPEndpointRequiresAToken(t *testing.T) {
	f := newSearchFixture(t, &fakeSearch{}, true, nil)
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s /mcp without a token: %d, want 401", method, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "My setup") {
			t.Errorf("the refusal should say where the token comes from: %s", rec.Body)
		}
	}
}

func TestTheMCPEndpointServesPOSTOnly(t *testing.T) {
	f := newSearchFixture(t, &fakeSearch{}, true, nil)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestInitializeNegotiatesTheProtocol(t *testing.T) {
	f := newSearchFixture(t, &fakeSearch{}, true, nil)
	for requested, want := range map[string]string{
		"2025-06-18": "2025-06-18",
		"2025-03-26": "2025-03-26",
		"2099-01-01": mcpProtocolVersions[0],
	} {
		_, reply := f.rpc(t, "initialize", map[string]any{
			"protocolVersion": requested, "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "claude-code", "version": "2.1.0"},
		})
		var res struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
		}
		decode(t, reply.Result, &res)
		if res.ProtocolVersion != want || res.Capabilities["tools"] == nil {
			t.Errorf("asked for %s: %s", requested, reply.Result)
		}
	}

	rec := f.post(t, "/mcp", claudeCodeUA, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Errorf("a notification is accepted with no body: %d %q", rec.Code, rec.Body)
	}
}

func TestSearchIsListedOnlyWhileItIsOn(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		f := newSearchFixture(t, &fakeSearch{}, enabled, nil)
		_, reply := f.rpc(t, "tools/list", map[string]any{})
		var res struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		decode(t, reply.Result, &res)
		if got := len(res.Tools); (enabled && (got != 1 || res.Tools[0].Name != "web_search")) || (!enabled && got != 0) {
			t.Errorf("enabled=%v: %s", enabled, reply.Result)
		}
	}

	// A deployment with no web search at all lists nothing, rather than
	// failing the handshake a client makes for every configured server.
	f := newFixtureWith(t, fixtureConfig{provider: &fakeProvider{}})
	_, reply := f.rpc(t, "tools/list", map[string]any{})
	if string(reply.Result) != `{"tools":[]}` {
		t.Errorf("no search configured: %s", reply.Result)
	}
}

func TestASearchIsRunPricedAndRecorded(t *testing.T) {
	backend := &fakeSearch{results: []websearch.Result{
		{Title: "Go 1.25 Release Notes", URL: "https://go.dev/doc/go1.25", Published: "2025-08-12", Snippet: "Go 1.25 arrives\nsix months after 1.24."},
		{URL: "https://tip.golang.org/doc/next"},
	}}
	f := newSearchFixture(t, backend, true, nil)

	res := f.search(t, map[string]any{
		"query": "  go 1.25 release notes ", "max_results": 2,
		"allowed_domains": []string{"https://Go.dev/doc", "go.dev"},
		"published_after": "2025-01-01", "published_before": "2025-12-31",
	})
	if res.IsError {
		t.Fatalf("search failed: %s", res.Content[0].Text)
	}
	text := res.Content[0].Text
	for _, want := range []string{"1. Go 1.25 Release Notes", "https://go.dev/doc/go1.25", "Published 2025-08-12", "   six months after 1.24.", "2. https://tip.golang.org/doc/next"} {
		if !strings.Contains(text, want) {
			t.Errorf("result text is missing %q:\n%s", want, text)
		}
	}

	if len(backend.asked) != 1 {
		t.Fatalf("backend asked %d times", len(backend.asked))
	}
	q := backend.asked[0]
	if q.Text != "go 1.25 release notes" || q.MaxResults != 2 || len(q.Include) != 1 || q.Include[0] != "go.dev" {
		t.Errorf("query: %+v", q)
	}
	if !q.PublishedAfter.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) || q.PublishedBefore.Format("2006-01-02") != "2025-12-31" || q.PublishedBefore.Hour() != 23 {
		t.Errorf("dates: %v .. %v", q.PublishedAfter, q.PublishedBefore)
	}

	f.drain()
	row := f.latest(t)
	if row.Surface != SurfaceMCP || row.RequestedModel != "web_search" || row.ResolvedModel != "web-search" || row.Provider != "bedrock-agentcore" {
		t.Errorf("row identity: %+v", row)
	}
	if row.Decision != store.DecisionAllowed || row.Attempts != 1 || row.WebSearches != 1 || row.UserID != f.user.ID || row.Tool != "claude-code" {
		t.Errorf("row outcome: %+v", row)
	}
	if row.Cost == nil || row.Cost.Total != websearch.PublishedPrice || row.Cost.WebSearch != websearch.PublishedPrice || row.CostSource != "aws-published" {
		t.Errorf("row cost: %+v %q", row.Cost, row.CostSource)
	}
}

func TestTheDefaultIsFiveResults(t *testing.T) {
	backend := &fakeSearch{}
	f := newSearchFixture(t, backend, true, nil)
	res := f.search(t, map[string]any{"query": "nothing matches this"})
	if res.IsError || !strings.Contains(res.Content[0].Text, "No results") {
		t.Errorf("empty result: %+v", res)
	}
	if backend.asked[0].MaxResults != defaultSearchResults {
		t.Errorf("max results = %d", backend.asked[0].MaxResults)
	}
}

// A query is prompt content, written by a model from somebody's conversation,
// and the audit row keeps nothing of it.
func TestNoQueryOrResultIsPersisted(t *testing.T) {
	const secret = "acme-internal-codename-zebra"
	backend := &fakeSearch{results: []websearch.Result{{Title: secret, URL: "https://example.com/" + secret, Snippet: secret}}}
	f := newSearchFixture(t, backend, true, nil)
	f.search(t, map[string]any{"query": secret, "allowed_domains": []string{secret + ".com"}})
	f.drain()

	page, err := f.db.ListRequests(t.Context(), store.RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("a search's text reached the audit trail: %s", raw)
	}
}

func TestArgumentsTheBackendWouldRefuseAreRefusedHereWithoutASearch(t *testing.T) {
	backend := &fakeSearch{}
	f := newSearchFixture(t, backend, true, nil)
	for name, args := range map[string]map[string]any{
		"no query":      {},
		"long query":    {"query": strings.Repeat("x", websearch.MaxQueryChars+1)},
		"bad date":      {"query": "q", "published_after": "last week"},
		"unknown field": {"query": "q", "site": "go.dev"},
		"zero results":  {"query": "q", "max_results": 0},
		"too many":      {"query": "q", "max_results": 26},
		"dates crossed": {"query": "q", "published_after": "2025-02-01", "published_before": "2025-01-01"},
	} {
		res := f.search(t, args)
		if !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(backend.asked) != 0 {
		t.Errorf("the backend was asked %d times for arguments that were refused", len(backend.asked))
	}
	f.drain()
	if row := f.latest(t); row.Decision != store.DecisionRefused || row.Attempts != 0 || row.Cost != nil {
		t.Errorf("a refused search: %+v", row)
	}
}

func TestAnUnknownToolIsAProtocolError(t *testing.T) {
	f := newSearchFixture(t, &fakeSearch{}, true, nil)
	_, reply := f.rpc(t, "tools/call", map[string]any{"name": "fetch", "arguments": map[string]any{}})
	if reply.Error == nil || reply.Error.Code != rpcInvalidParams {
		t.Errorf("got %+v", reply)
	}
	_, reply = f.rpc(t, "resources/list", map[string]any{})
	if reply.Error == nil || reply.Error.Code != rpcMethodNotFound {
		t.Errorf("got %+v", reply)
	}
}

func TestASearchWhileOffIsRefusedWithoutASearch(t *testing.T) {
	backend := &fakeSearch{}
	f := newSearchFixture(t, backend, false, nil)
	res := f.search(t, map[string]any{"query": "q"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "switched off") || len(backend.asked) != 0 {
		t.Errorf("got %+v", res)
	}
}

func TestAHardLimitStopsASearch(t *testing.T) {
	hard := pricing.Money(1_000_000_000)
	set := policy.Set{
		Routes: []policy.Route{defaultRoute()},
		Budgets: []policy.Budget{{ID: "bdg_1", Scope: policy.ScopeOrg, Period: policy.PeriodDay,
			Name: "deployment", Enabled: true, HardLimit: &hard}},
	}
	backend := &fakeSearch{}
	f := newSearchFixture(t, backend, true, &set)
	f.engine.Record(policy.Completed{UserID: f.user.ID, At: time.Now().UTC(), Cost: &pricing.Cost{Total: 2_000_000_000}, UsedTokens: true})

	res := f.search(t, map[string]any{"query": "q"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "$1.00") || len(backend.asked) != 0 {
		t.Errorf("got %+v, asked %d", res, len(backend.asked))
	}
	f.drain()
	if row := f.latest(t); row.Decision != store.DecisionBlocked || row.Attempts != 0 || row.BudgetState != string(policy.StateOverHard) {
		t.Errorf("row: %+v", row)
	}
}

func TestASearchCountsAgainstTheBudget(t *testing.T) {
	soft := pricing.Money(1_000_000_000)
	set := policy.Set{
		Routes: []policy.Route{defaultRoute()},
		Budgets: []policy.Budget{{ID: "bdg_1", Scope: policy.ScopeOrg, Period: policy.PeriodDay,
			Name: "deployment", Enabled: true, SoftLimit: &soft}},
	}
	f := newSearchFixture(t, &fakeSearch{}, true, &set)
	f.search(t, map[string]any{"query": "q"})
	f.drain()

	status := f.engine.Status()
	if len(status) != 1 || status[0].Spend.Cost != websearch.PublishedPrice || status[0].Spend.Requests != 1 {
		t.Errorf("the ledger did not see the search: %+v", status)
	}
}

func TestAFailedSearchTellsTheModelWhatItCanDo(t *testing.T) {
	for kind, want := range map[websearch.ErrorKind]string{
		websearch.ErrKindRejected:  "refused: too long",
		websearch.ErrKindThrottled: "rate limited",
		websearch.ErrKindForbidden: "unavailable on this Panorama deployment",
	} {
		backend := &fakeSearch{err: &websearch.Error{Kind: kind, Message: "too long"}}
		f := newSearchFixture(t, backend, true, nil)
		res := f.search(t, map[string]any{"query": "q"})
		if !res.IsError || !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("%s: %q", kind, res.Content[0].Text)
		}
		if kind == websearch.ErrKindForbidden && strings.Contains(res.Content[0].Text, "too long") {
			t.Errorf("an operator's failure leaked the backend's words to the model: %q", res.Content[0].Text)
		}
		f.drain()
		row := f.latest(t)
		if row.Decision != store.DecisionError || row.ErrorKind != "web_search_"+string(kind) || row.Cost != nil || row.WebSearches != 0 {
			t.Errorf("%s row: %+v", kind, row)
		}
	}
}
