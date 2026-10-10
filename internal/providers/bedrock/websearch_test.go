// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/websearch"
)

const testGatewayURL = "https://panorama-probe-a9ootz1ja4.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp"

// fakeGateway answers the way a live AgentCore gateway was seen to: tools/list
// with one connector tool, tools/call with the captured response.
type fakeGateway struct {
	t        *testing.T
	tools    []string
	call     func(args map[string]any) (int, string)
	lists    atomic.Int32
	lastAuth string
	lastArgs map[string]any
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.lastAuth = r.Header.Get("Authorization")
	var req struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		g.t.Fatalf("gateway got a body that is not JSON-RPC: %s", body)
	}
	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "tools/list":
		g.lists.Add(1)
		tools := make([]map[string]any, 0, len(g.tools))
		for _, name := range g.tools {
			tools = append(tools, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": tools}})
	case "tools/call":
		args, _ := req.Params["arguments"].(map[string]any)
		g.lastArgs = args
		name, _ := req.Params["name"].(string)
		if !contains(g.tools, name) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32602, "message": "Unknown tool: " + name}})
			return
		}
		status, payload := g.call(args)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	default:
		g.t.Fatalf("unexpected method %q", req.Method)
	}
}

func capturedCall(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/agentcore_tools_call.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func newTestWebSearch(t *testing.T, g *fakeGateway) *WebSearch {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	t.Setenv(agentCoreEndpointEnv, srv.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "fake")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	ws, err := NewWebSearch(context.Background(), testGatewayURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestAGatewayURLIsTheOnlyHostThatIsSigned(t *testing.T) {
	good := map[string]Gateway{
		testGatewayURL: {ID: "panorama-probe-a9ootz1ja4", Region: "us-east-1", URL: testGatewayURL},
		"https://GW-1.gateway.bedrock-agentcore.eu-west-1.amazonaws.com/mcp/": {
			ID: "gw-1", Region: "eu-west-1", URL: "https://gw-1.gateway.bedrock-agentcore.eu-west-1.amazonaws.com/mcp"},
	}
	for raw, want := range good {
		got, err := ParseGatewayURL(raw)
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", raw, got, want)
		}
	}

	for _, raw := range []string{
		"",
		"http://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp",
		"https://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com",
		"https://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com/other",
		"https://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com.evil.example/mcp",
		"https://evil.example/mcp",
		"https://user:pw@gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp",
		"https://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com:8443/mcp",
		"https://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp?x=1",
	} {
		if _, err := ParseGatewayURL(raw); err == nil {
			t.Errorf("%q was accepted", raw)
		}
	}
}

func TestASearchReturnsTheIndexResultsTrimmed(t *testing.T) {
	g := &fakeGateway{t: t, tools: []string{"web-search___WebSearch"},
		call: func(map[string]any) (int, string) { return http.StatusOK, capturedCall(t) }}
	ws := newTestWebSearch(t, g)

	results, err := ws.Search(context.Background(), websearch.Query{
		Text: "Go 1.25 release notes", MaxResults: 2,
		Include:        []string{"go.dev"},
		PublishedAfter: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].URL != "https://go.dev/doc/go1.25" || results[0].Title != "The Go Programming Language" {
		t.Errorf("first result: %+v", results[0])
	}
	if results[0].Published != "" {
		t.Errorf("an unknown date should be no date, got %q", results[0].Published)
	}
	if results[1].Published != "2000-01-27" {
		t.Errorf("the index's date should be reduced to the day, got %q", results[1].Published)
	}

	if !strings.HasPrefix(g.lastAuth, "AWS4-HMAC-SHA256 ") || !strings.Contains(g.lastAuth, "/us-east-1/bedrock-agentcore/aws4_request") {
		t.Errorf("request was not signed for bedrock-agentcore in the gateway's region: %q", g.lastAuth)
	}
	if g.lastArgs["query"] != "Go 1.25 release notes" || g.lastArgs["maxResults"] != float64(2) {
		t.Errorf("arguments: %v", g.lastArgs)
	}
	filters, _ := g.lastArgs["filters"].(map[string]any)
	if filters == nil || filters["domainFilter"] == nil || filters["publishedDateFilter"] == nil {
		t.Errorf("filters were not passed: %v", g.lastArgs)
	}

	if _, err := ws.Search(context.Background(), websearch.Query{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	if n := g.lists.Load(); n != 1 {
		t.Errorf("the tool name should be looked up once, was looked up %d times", n)
	}
}

func TestASnippetIsCutAtAWord(t *testing.T) {
	long := strings.Repeat("word ", 1000)
	got := trimSnippet(long, maxSnippetChars)
	if len([]rune(got)) > maxSnippetChars+4 || !strings.HasSuffix(got, " ...") || strings.HasSuffix(strings.TrimSuffix(got, " ..."), "wor") {
		t.Errorf("bad cut: %d chars, ends %q", len([]rune(got)), got[len(got)-12:])
	}
	if trimSnippet("short", 10) != "short" {
		t.Error("a short snippet should be left alone")
	}
}

func TestTheConnectorRefusingAQueryIsRejectedNotUnavailable(t *testing.T) {
	g := &fakeGateway{t: t, tools: []string{"web-search___WebSearch"}, call: func(map[string]any) (int, string) {
		return http.StatusOK, `{"jsonrpc":"2.0","id":6,"result":{"content":[{"type":"text","text":"DependencyFailedException - The web search tool received an invalid request: 1 validation error detected: Value at 'query' failed to satisfy constraint: Member must have length less than or equal to 200"}],"isError":true}}`
	}}
	ws := newTestWebSearch(t, g)
	_, err := ws.Search(context.Background(), websearch.Query{Text: "x"})
	if websearch.KindOf(err) != websearch.ErrKindRejected || !strings.Contains(err.Error(), "less than or equal to 200") {
		t.Fatalf("got %v", err)
	}
}

func TestARefusedRoleSaysWhichPermission(t *testing.T) {
	g := &fakeGateway{t: t, tools: []string{"web-search___WebSearch"}}
	ws := newTestWebSearch(t, g)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":8,"error":{"code":-32002,"message":"Authorization error - Insufficient permissions"}}`)
	}))
	defer srv.Close()
	ws.endpoint = srv.URL + "/mcp"

	err := ws.Check(context.Background())
	if websearch.KindOf(err) != websearch.ErrKindForbidden {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "bedrock-agentcore:InvokeGateway") {
		t.Errorf("the error should name the permission: %v", err)
	}
}

func TestAGatewayWithoutOneWebSearchTargetIsMisconfigured(t *testing.T) {
	for name, tools := range map[string][]string{
		"none": {"other___Thing"},
		"two":  {"a___WebSearch", "b___WebSearch"},
	} {
		t.Run(name, func(t *testing.T) {
			ws := newTestWebSearch(t, &fakeGateway{t: t, tools: tools})
			if err := ws.Check(context.Background()); websearch.KindOf(err) != websearch.ErrKindMisconfigured {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestARenamedTargetIsLookedUpAgain(t *testing.T) {
	g := &fakeGateway{t: t, tools: []string{"old___WebSearch"},
		call: func(map[string]any) (int, string) { return http.StatusOK, capturedCall(t) }}
	ws := newTestWebSearch(t, g)
	if _, err := ws.Search(context.Background(), websearch.Query{Text: "q"}); err != nil {
		t.Fatal(err)
	}

	g.tools = []string{"new___WebSearch"}
	_, err := ws.Search(context.Background(), websearch.Query{Text: "q"})
	var wsErr *websearch.Error
	if !errors.As(err, &wsErr) || wsErr.Kind != websearch.ErrKindMisconfigured {
		t.Fatalf("got %v", err)
	}
	if _, err := ws.Search(context.Background(), websearch.Query{Text: "q"}); err != nil {
		t.Fatalf("the next search should find the new name: %v", err)
	}
}

func TestAStreamedResponseIsRead(t *testing.T) {
	g := &fakeGateway{t: t, tools: []string{"web-search___WebSearch"}}
	ws := newTestWebSearch(t, g)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"t___WebSearch\"}]}}\n\n")
	}))
	defer srv.Close()
	ws.endpoint = srv.URL + "/mcp"
	if err := ws.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}
