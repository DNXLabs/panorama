// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/providers"
)

// stubResolver answers the references a document makes.
type stubResolver struct {
	users map[string]string
	teams map[string]string
}

func (s stubResolver) UserIDByEmail(_ context.Context, email string) (string, error) {
	if id, ok := s.users[email]; ok {
		return id, nil
	}
	return "", errors.New("no such user")
}

func (s stubResolver) TeamIDBySlug(_ context.Context, slug string) (string, error) {
	if id, ok := s.teams[slug]; ok {
		return id, nil
	}
	return "", errors.New("no such team")
}

func testResolver() stubResolver {
	return stubResolver{
		users: map[string]string{"alice@example.com": "usr_alice", "oncall@example.com": "usr_oncall"},
		teams: map[string]string{"platform": "tm_platform"},
	}
}

func compile(t *testing.T, doc string) (Set, error) {
	t.Helper()

	parsed, err := ParseDocument(strings.NewReader(doc))
	if err != nil {
		return Set{}, err
	}
	return parsed.Compile(t.Context(), testResolver())
}

const exampleDocument = `
budgets:
  - scope: org
    period: month
    name: whole deployment
    soft_limit: 800.00
    hard_limit: 1000.00

  - scope: user
    id: alice@example.com
    period: day
    hard_limit: "20.00"

  - scope: team
    id: platform
    period: month
    soft_limit: 300.00

routes:
  - name: standard
    description: What every engineer gets
    model: bedrock/us.anthropic.claude-sonnet-5
    serves_over_soft: false

  - name: economy
    description: Where people go once the soft limit is crossed
    model: bedrock/us.anthropic.claude-haiku-4-5

  - name: heavy
    description: For the platform team, when Sonnet is not enough
    teams: [platform]
    users: [alice@example.com]
    tools: [claude-code]
    model: bedrock-mantle/anthropic.claude-opus-4-6
`

// An unknown key is refused rather than dropped. A misspelled condition that
// was silently ignored would leave a route admitting everybody, and a key the
// document does not have (a second model on a route, a limit counted in
// requests) would be applied as though it were not written.
func TestAnUnknownKeyIsRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"route": `
routes:
  - name: standard
    model: bedrock/us.anthropic.claude-sonnet-5
    fallbacks: [bedrock/global.anthropic.claude-sonnet-5]
`,
		"budget": `
budgets:
  - scope: org
    period: day
    hard_requests: 50000
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compile(t, doc)
			if err == nil {
				t.Fatal("a document with an unknown key was accepted")
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Errorf("the refusal does not name the unknown key:\n%v", err)
			}
		})
	}
}

func TestCompileExampleDocument(t *testing.T) {
	set, err := compile(t, exampleDocument)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if len(set.Budgets) != 3 {
		t.Fatalf("%d budgets, want 3", len(set.Budgets))
	}
	if len(set.Routes) != 3 {
		t.Fatalf("%d routes, want 3", len(set.Routes))
	}

	// References are resolved to identifiers.
	var userBudget *Budget
	for i := range set.Budgets {
		if set.Budgets[i].Scope == ScopeUser {
			userBudget = &set.Budgets[i]
		}
	}
	if userBudget == nil || userBudget.ScopeID != "usr_alice" {
		t.Fatalf("the user budget did not resolve its email: %+v", userBudget)
	}
	if userBudget.HardLimit == nil || *userBudget.HardLimit != 20_000_000_000 {
		t.Errorf("hard limit = %v, want exactly $20 in nanodollars", userBudget.HardLimit)
	}

	def, ok := set.Route("standard")
	if !ok {
		t.Fatal("the standard route did not compile")
	}
	if def.Target != providers.MustModelRef("bedrock/us.anthropic.claude-sonnet-5") {
		t.Errorf("the standard route goes to %q", def.Target)
	}
	if mantle, _ := set.Route("heavy"); mantle.Target.Provider != providers.KindBedrockMantle ||
		mantle.Target.Model != "anthropic.claude-opus-4-6" {
		t.Errorf("the heavy route goes to %q, want its provider kept", mantle.Target)
	}
	if def.ServesOverSoft {
		t.Error("serves_over_soft: false was written and did not survive compiling")
	}

	// Omitted means served, so the ordinary route is silent in the file and
	// stopping is the thing somebody has to write down.
	economy, ok := set.Route("economy")
	if !ok {
		t.Fatal("the economy route did not compile")
	}
	if !economy.ServesOverSoft {
		t.Error("a route that says nothing about soft limits stopped serving past one")
	}

	heavy, ok := set.Route("heavy")
	if !ok {
		t.Fatal("the heavy route did not compile")
	}
	if len(heavy.Teams) != 1 || heavy.Teams[0] != "tm_platform" {
		t.Errorf("teams = %v, want the resolved id", heavy.Teams)
	}
	if len(heavy.Users) != 1 || heavy.Users[0] != "usr_alice" {
		t.Errorf("users = %v, want the resolved id", heavy.Users)
	}
	if len(heavy.Tools) != 1 || heavy.Tools[0] != "claude-code" {
		t.Errorf("tools = %v, want the literal name", heavy.Tools)
	}
}

// TestAmountsAreExact is the same commitment the price table makes: 0.30 typed
// is 0.30 stored, not the nearest float to it.
func TestAmountsAreExact(t *testing.T) {
	set, err := compile(t, `
budgets:
  - scope: org
    period: day
    hard_limit: 0.30
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if got := int64(*set.Budgets[0].HardLimit); got != 300_000_000 {
		t.Errorf("0.30 became %d nanodollars, want 300000000", got)
	}
}

func TestUnknownKeysAreRefused(t *testing.T) {
	_, err := compile(t, `
routes:
  - name: typo
    model_id: us.anthropic.claude-sonnet-5
`)
	if err == nil {
		t.Fatal("a misspelled key was accepted, so the route would have had no model at all")
	}
	if !strings.Contains(err.Error(), "model_id") {
		t.Errorf("the error does not name the offending key: %v", err)
	}
}

func TestUnresolvableReferencesAreRefused(t *testing.T) {
	_, err := compile(t, `
budgets:
  - scope: user
    id: nobody@example.com
    period: day
    hard_limit: 5.00
`)
	if err == nil {
		t.Fatal("a budget on a user who does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "nobody@example.com") {
		t.Errorf("the error does not name the address: %v", err)
	}
}

// A condition naming somebody who is not here matches nothing, so the route
// would silently serve nobody.
func TestAConditionNamingNobodyIsRefused(t *testing.T) {
	_, err := compile(t, `
routes:
  - name: standard
    model: bedrock/us.anthropic.claude-sonnet-5
    users: [ghost@example.com]
`)
	if err == nil {
		t.Fatal("a route restricted to a user who does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "ghost@example.com") {
		t.Errorf("the error does not name the address: %v", err)
	}
}

func TestDuplicateRouteNamesAreRefused(t *testing.T) {
	_, err := compile(t, `
routes:
  - name: same
    model: bedrock/a
  - name: same
    model: bedrock/b
`)
	if err == nil {
		t.Fatal("two routes with one name were accepted; the name is the route's identity")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

func TestDuplicateBudgetScopesAreRefused(t *testing.T) {
	_, err := compile(t, `
budgets:
  - scope: org
    period: month
    hard_limit: 10.00
  - scope: org
    period: month
    hard_limit: 20.00
`)
	if err == nil {
		t.Fatal("two budgets on one scope and period were accepted")
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	_, err := compile(t, `
budgets:
  - scope: user
    id: nobody@example.com
    period: day
    hard_limit: 5.00
routes:
  - name: broken
`)
	if err == nil {
		t.Fatal("expected errors")
	}

	msg := err.Error()
	if !strings.Contains(msg, "nobody@example.com") || !strings.Contains(msg, "needs a model") {
		t.Errorf("only some problems were reported, so fixing them takes one pass each:\n%s", msg)
	}
}

func TestAnEmptyDocumentIsValid(t *testing.T) {
	// Applying it removes everything, which is a real operation and not a
	// mistake to guess around.
	set, err := compile(t, "")
	if err != nil {
		t.Fatalf("an empty document: %v", err)
	}
	if len(set.Routes) != 0 || len(set.Budgets) != 0 {
		t.Errorf("an empty document produced %d routes and %d budgets", len(set.Routes), len(set.Budgets))
	}
}

// A bare model id is a guess about where a prompt is processed, so it is
// refused - and the refusal carries the reference to write instead, because the
// fix is one prefix.
func TestABareModelIDIsRefusedWithTheReferenceToWrite(t *testing.T) {
	_, err := compile(t, `
routes:
  - name: standard
    model: us.anthropic.claude-sonnet-5
`)
	if err == nil {
		t.Fatal("a route naming no provider was accepted")
	}
	for _, want := range []string{"names no provider", "bedrock/us.anthropic.claude-sonnet-5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}

// A provider this build does not serve is refused by name rather than read as
// part of a model id.
func TestAnUnknownProviderIsRefused(t *testing.T) {
	_, err := compile(t, `
routes:
  - name: standard
    model: openrouter/anthropic/claude-sonnet-5
`)
	if err == nil || !strings.Contains(err.Error(), "openrouter") {
		t.Fatalf("an unknown provider was not refused by name: %v", err)
	}
}

// A route with no model admits a request and then has nowhere to send it.
func TestARouteWithoutAModelIsRefused(t *testing.T) {
	_, err := compile(t, `
routes:
  - name: standard
    description: nowhere to go
`)
	if err == nil {
		t.Fatal("a route with no model was accepted")
	}
	if !strings.Contains(err.Error(), "needs a model") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

// TestTheShippedExampleApplies checks the file operators actually copy.
//
// config.example.yaml and integrations-fixtures.example.yaml are each loaded by
// a test in their own package, and this is the same guard for the policy
// example: a key the document no longer has would make `panorama policy apply
// -f policy.example.yaml` refuse the example the product ships.
//
// The resolver here answers whatever the file references, because what is under
// test is the document, not whether this machine happens to have those accounts.
func TestTheShippedExampleApplies(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "policy.example.yaml"))
	if err != nil {
		t.Fatalf("reading the shipped example: %v", err)
	}

	set, cerr := compile(t, string(raw))
	if cerr != nil {
		t.Fatalf("the shipped policy.example.yaml does not apply:\n%v", cerr)
	}

	if len(set.Routes) == 0 {
		t.Error("the example declares no routes")
	}
	if len(set.Budgets) == 0 {
		t.Error("the example declares no budgets")
	}
}
