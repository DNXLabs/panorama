// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers/bedrock"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/websearch"
)

const testGateway = "https://gw-1.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp"

type checkedBackend struct{ err error }

func (checkedBackend) Kind() websearch.Kind { return websearch.KindAgentCore }
func (b checkedBackend) Check(context.Context) error {
	return b.err
}
func (checkedBackend) Search(context.Context, websearch.Query) ([]websearch.Result, error) {
	return nil, nil
}

// newSearchConsole is a console whose web search service reads the fixture's
// own database, as serve wires it, with a backend that answers checks with
// checkErr.
func newSearchConsole(t *testing.T, checkErr error) *consoleFixture {
	t.Helper()
	return newSearchConsoleFor(t, checkErr, websearch.Deployment{})
}

// newSearchConsoleFor is newSearchConsole on a deployment that may name its
// own gateway.
func newSearchConsoleFor(t *testing.T, checkErr error, dep websearch.Deployment) *consoleFixture {
	t.Helper()
	var db *store.DB
	svc := websearch.NewService(
		func(ctx context.Context) (websearch.Config, bool, error) {
			s, found, err := db.EffectiveWebSearch(ctx, dep)
			if err != nil || !found {
				return websearch.Config{}, false, err
			}
			return s.Config(), true, nil
		},
		func(context.Context, websearch.Config) (websearch.Backend, error) {
			return checkedBackend{err: checkErr}, nil
		}, 0, discardLogger())
	f := newConsole(t, func(o *ConsoleOptions) {
		o.WebSearch = svc
		o.WebSearchKind = bedrock.WebSearchDescriptor()
		o.WebSearchDeployed = dep
	})
	db = f.db
	return f
}

func TestWebSearchIsConfiguredByAnAdministratorOnly(t *testing.T) {
	f := newSearchConsole(t, nil)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/web-search"},
		{http.MethodPut, "/api/v1/web-search"},
		{http.MethodPost, "/api/v1/web-search/check"},
	} {
		rec, _ := f.send(t, c.method, c.path, f.devToken, map[string]any{"enabled": true, "gateway_url": testGateway})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a developer: %d", c.method, c.path, rec.Code)
		}
	}

	rec, body := f.send(t, http.MethodGet, "/api/v1/web-search", f.adminToken, nil)
	if rec.Code != http.StatusOK || body["configured"] != false || body["serving"] != false {
		t.Fatalf("unconfigured: %d %v", rec.Code, body)
	}
	mcpURL, _ := body["mcp_url"].(string)
	if !strings.HasSuffix(mcpURL, "/mcp") || body["role_policy"] == nil {
		t.Errorf("the screen needs the endpoint and the policy: %v", body)
	}
}

func TestOnlyAGatewayURLIsAccepted(t *testing.T) {
	f := newSearchConsole(t, nil)
	for _, bad := range []string{"", "https://evil.example/mcp", "http://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp"} {
		rec, _ := f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken, map[string]any{"enabled": true, "gateway_url": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", bad, rec.Code)
		}
	}
	rec, _ := f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken,
		map[string]any{"enabled": true, "gateway_url": testGateway, "price_per_search": "seven"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a price that is not a number: %d", rec.Code)
	}
	if _, found, _ := f.db.WebSearch(t.Context()); found {
		t.Error("a refused save wrote something")
	}
}

func TestASavedGatewayServesAtOnce(t *testing.T) {
	f := newSearchConsole(t, nil)
	rec, body := f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken,
		map[string]any{"enabled": true, "gateway_url": " " + testGateway + "/ "})
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if body["serving"] != true || body["gateway_id"] != "gw-1" || body["region"] != "us-east-1" || body["gateway_url"] != testGateway {
		t.Errorf("saved: %v", body)
	}
	if body["price_per_search"] != websearch.PublishedPrice.USD() || body["price_source"] != "aws-published" {
		t.Errorf("price: %v %v", body["price_per_search"], body["price_source"])
	}

	_, body = f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken,
		map[string]any{"enabled": true, "gateway_url": testGateway, "price_per_search": "$0.005"})
	if body["price_per_search"] != "0.005000000" || body["price_source"] != "config" {
		t.Errorf("override: %v %v", body["price_per_search"], body["price_source"])
	}

	_, body = f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken,
		map[string]any{"enabled": false, "gateway_url": testGateway})
	if body["serving"] != false {
		t.Errorf("switched off and still serving: %v", body)
	}
}

func TestACheckIsRecordedEitherWay(t *testing.T) {
	f := newSearchConsole(t, nil)
	rec, _ := f.send(t, http.MethodPost, "/api/v1/web-search/check", f.adminToken, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a check with nothing saved: %d", rec.Code)
	}
	f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken, map[string]any{"enabled": true, "gateway_url": testGateway})
	_, body := f.send(t, http.MethodPost, "/api/v1/web-search/check", f.adminToken, nil)
	if body["ok"] != true || body["verified_at"] == nil {
		t.Errorf("a passing check: %v", body)
	}

	failing := newSearchConsole(t, &websearch.Error{Kind: websearch.ErrKindForbidden, Message: "needs bedrock-agentcore:InvokeGateway"})
	failing.send(t, http.MethodPut, "/api/v1/web-search", failing.adminToken, map[string]any{"enabled": true, "gateway_url": testGateway})
	_, body = failing.send(t, http.MethodPost, "/api/v1/web-search/check", failing.adminToken, nil)
	checkErr, _ := body["error"].(string)
	if body["ok"] != false || body["verified_at"] != nil || !strings.Contains(checkErr, "InvokeGateway") {
		t.Errorf("a failing check: %v", body)
	}
}

func TestMySetupAddsTheMCPServerWhileSearchIsOn(t *testing.T) {
	f := newSearchConsole(t, nil)
	tools := func() (map[string]map[string]any, map[string]any) {
		_, body := f.send(t, http.MethodGet, "/api/v1/setup/tools", f.devToken, nil)
		byID := map[string]map[string]any{}
		list, _ := body["tools"].([]any)
		for _, tl := range list {
			row, _ := tl.(map[string]any)
			id, _ := row["id"].(string)
			byID[id] = row
		}
		return byID, body
	}

	config := func(byID map[string]map[string]any, id string) string {
		text, _ := byID[id]["config"].(string)
		return text
	}
	byID, body := tools()
	if body["web_search"] != false || byID["claude-code"]["mcp"] != nil ||
		strings.Contains(config(byID, "opencode"), "/mcp") ||
		strings.Contains(config(byID, "codex-cli"), "mcp_servers") {
		t.Fatal("search is off and a tool was told about the MCP server")
	}

	f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken, map[string]any{"enabled": true, "gateway_url": testGateway})
	byID, body = tools()
	if body["web_search"] != true {
		t.Fatal("search is on and the screen was not told")
	}

	claude, _ := byID["claude-code"]["mcp"].(map[string]any)
	command, _ := claude["command"].(string)
	if !strings.Contains(command, "claude mcp add --transport http --scope user panorama ") ||
		!strings.Contains(command, "/mcp") || !strings.Contains(command, "Bearer "+tokenPlaceholder) {
		t.Errorf("claude code command: %q", command)
	}
	if !strings.Contains(mustJSON(t, claude["settings"]), `"deny":["WebSearch"]`) {
		t.Errorf("Claude Code's own WebSearch must be denied: %v", claude["settings"])
	}

	opencode := config(byID, "opencode")
	for _, want := range []string{`"mcp"`, `"type": "remote"`, `/mcp"`, `"Authorization": "Bearer ` + tokenPlaceholder} {
		if !strings.Contains(opencode, want) {
			t.Errorf("opencode config lacks %q:\n%s", want, opencode)
		}
	}

	codex := config(byID, "codex-cli")
	for _, want := range []string{"[mcp_servers.panorama]", `/mcp"`, `bearer_token_env_var = "PANORAMA_TOKEN"`} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex config lacks %q:\n%s", want, codex)
		}
	}
}

func TestABackendThatCannotBeBuiltIsOnTheScreen(t *testing.T) {
	var db *store.DB
	svc := websearch.NewService(
		func(ctx context.Context) (websearch.Config, bool, error) {
			s, found, err := db.WebSearch(ctx)
			if err != nil || !found {
				return websearch.Config{}, false, err
			}
			return s.Config(), true, nil
		},
		func(context.Context, websearch.Config) (websearch.Backend, error) {
			return nil, errors.New("web search: load aws config: no region")
		}, 0, discardLogger())
	f := newConsole(t, func(o *ConsoleOptions) {
		o.WebSearch = svc
		o.WebSearchKind = bedrock.WebSearchDescriptor()
	})
	db = f.db

	rec, body := f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken, map[string]any{"enabled": true, "gateway_url": testGateway})
	buildErr, _ := body["error"].(string)
	if rec.Code != http.StatusOK || body["configured"] != true || body["serving"] != false || !strings.Contains(buildErr, "no region") {
		t.Errorf("saved and failed: %d %v", rec.Code, body)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestADeployedGatewayServesWithNothingSaved(t *testing.T) {
	const other = "https://gw-old.gateway.bedrock-agentcore.us-east-1.amazonaws.com/mcp"
	dep := websearch.Deployment{Kind: websearch.KindAgentCore, GatewayURL: testGateway}
	f := newSearchConsoleFor(t, nil, dep)
	if err := f.db.SaveWebSearch(t.Context(), store.WebSearchSettings{
		Kind: websearch.KindAgentCore, Enabled: true, GatewayURL: other, Price: ptrMoney(1),
		ConfiguredBy: "admin@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	rec, body := f.send(t, http.MethodGet, "/api/v1/web-search", f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: %d", rec.Code)
	}
	if body["deployed"] != true || body["gateway_url"] != testGateway || body["enabled"] != true {
		t.Errorf("the deployment's gateway is the configuration: %v", body)
	}
	if body["price_source"] != "aws-published" {
		t.Errorf("a price saved for another gateway was served: %v", body["price_source"])
	}

	for _, req := range []map[string]any{
		{"enabled": true, "gateway_url": other},
		{"enabled": true, "price_per_search": "0.001"},
	} {
		if r, _ := f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken, req); r.Code != http.StatusBadRequest {
			t.Errorf("%v on a deployed gateway: %d", req, r.Code)
		}
	}

	rec, body = f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken, map[string]any{"enabled": false})
	if rec.Code != http.StatusOK || body["enabled"] != false || body["serving"] != false || body["gateway_url"] != testGateway {
		t.Fatalf("switching it off: %d %v", rec.Code, body)
	}
	_, body = f.send(t, http.MethodPut, "/api/v1/web-search", f.adminToken, map[string]any{"enabled": true})
	if body["serving"] != true {
		t.Errorf("switching it back on: %v", body)
	}

	rec, body = f.send(t, http.MethodPost, "/api/v1/web-search/check", f.adminToken, map[string]any{})
	if rec.Code != http.StatusOK || body["ok"] != true || body["verified_at"] == nil || body["state"] != "connected" {
		t.Errorf("a check of the deployed gateway is recorded: %d %v", rec.Code, body)
	}
}

func ptrMoney(m pricing.Money) *pricing.Money { return &m }
