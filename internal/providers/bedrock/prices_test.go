// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// liveRegionEnv names a Bedrock region to run the live price fetch against.
// Without it that test skips: the suite must stay runnable with nothing but a
// Go toolchain, and the Price List API needs credentials.
const liveRegionEnv = "PANORAMA_TEST_AWS_REGION"

func TestUsageSuffix(t *testing.T) {
	cases := map[string]string{
		"USE1-MP:USE1_InputTokenCount-Units":                       "InputTokenCount",
		"USE1-MP:USE1_InputTokenCount_Global-Units":                "InputTokenCount_Global",
		"APS2-MP:APS2_input_tokens_standard-Units":                 "input_tokens_standard",
		"EUC1-MP:EUC1_cache_write_tokens_1h_global_standard-Units": "cache_write_tokens_1h_global_standard",
		"USW2-MP:USW2_Reserved_1Month_InputTPM_Geo-Units":          "Reserved_1Month_InputTPM_Geo",
		// Shapes that carry no usage type at all must not resolve to one.
		"USE1-InputTokenCount": "",
		"":                     "",
	}
	for in, want := range cases {
		if got := usageSuffix(in); got != want {
			t.Errorf("usageSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUsageTypesExcludeNonOnDemand is the guard against the expensive mistake:
// batch tokens are priced at half the on-demand rate, so a batch usage type
// leaking into the allowlist would halve every reported bill.
func TestUsageTypesExcludeNonOnDemand(t *testing.T) {
	// Every non-on-demand usage type the live catalogue publishes for Claude.
	for _, forbidden := range []string{
		"InputTokenCount_Batch", "InputTokenCount_Global_Batch",
		"OutputTokenCount_Batch", "OutputTokenCount_Global_Batch",
		"input_tokens_batch", "input_tokens_global_batch",
		"output_tokens_batch", "output_tokens_global_batch",
		"MillionBatchInputTokens", "MillionBatchOutputTokens",
		"InputTokenCount_LatencyOptimized", "OutputTokenCount_LatencyOptimized",
		"CacheReadInputTokenCount_LatencyOptimized", "CacheWriteInputTokenCount_LatencyOptimized",
		"Reserved_1Month_InputTPM_Geo", "Reserved_3Month_OutputTPM_Global",
		"ProvisionedThroughput_1MonthCommit_ModelUnits_Usage",
		"CustomModel_ProvisionedThroughput_NoCommit",
		"CustomizationTokenCount", "Customization_Storage", "ModelStorage1Month",
	} {
		if kind, ok := usageTypes[forbidden]; ok {
			t.Errorf("%q is in the on-demand allowlist as class %d; it is not an on-demand token rate",
				forbidden, kind.class)
		}
	}
}

func TestRateFromRespectsTheUnit(t *testing.T) {
	// The same number means three different things depending on the unit, and
	// the catalogue uses more than one.
	perMillion, err := rateFrom("2.2000000000", "1M tokens")
	if err != nil {
		t.Fatal(err)
	}
	perThousand, err := rateFrom("0.0022000000", "1K tokens")
	if err != nil {
		t.Fatal(err)
	}
	perToken, err := rateFrom("0.0000022", "tokens")
	if err != nil {
		t.Fatal(err)
	}

	if perMillion != perThousand || perMillion != perToken {
		t.Errorf("units disagree: 1M=%s 1K=%s token=%s", perMillion, perThousand, perToken)
	}
	if want := pricing.Rate(2_200_000_000); perMillion != want {
		t.Errorf("rate = %d, want %d ($2.20 per million)", perMillion, want)
	}

	if _, err := rateFrom("1.00", "1M TPM Hour"); err == nil {
		t.Error("a throughput unit must be refused, not read as a token price")
	}
}

func TestNormalizeName(t *testing.T) {
	// The two AWS catalogues disagree about case and spacing for the same model.
	if a, b := normalizeName("Claude Sonnet 5"), normalizeName("claude  sonnet 5"); a != b {
		t.Errorf("%q != %q", a, b)
	}
}

// TestFetchAgainstLiveAWS verifies the parser against the real catalogue.
//
// It asserts the shape rather than specific prices - prices move, and a test
// that pins them would fail for the wrong reason - except for the one
// relationship the whole design rests on: the global rate is cheaper than the
// regional one for the same model.
func TestFetchAgainstLiveAWS(t *testing.T) {
	region := os.Getenv(liveRegionEnv)
	if region == "" {
		t.Skipf("set %s to run the live price fetch", liveRegionEnv)
	}

	src, err := NewPriceSource(t.Context(), Config{
		Region: region,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("NewPriceSource: %v", err)
	}

	entries, err := src.Fetch(t.Context())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no prices returned; the catalogue or the parser has moved")
	}

	table := pricing.NewTable(entries)

	var anthropic, withCache int
	byKey := map[pricing.Key]pricing.Entry{}
	for _, e := range entries {
		byKey[e.Key] = e
		if err := e.Validate(); err != nil {
			t.Errorf("invalid entry: %v", err)
		}
		if e.Rates.Input <= 0 || e.Rates.Output <= 0 {
			t.Errorf("%s has no input or output rate: %+v", e.Key, e.Rates)
		}
		if e.Rates.Output < e.Rates.Input {
			t.Errorf("%s prices output below input, which no model does: %+v", e.Key, e.Rates)
		}
		if strings.HasPrefix(e.Model, "anthropic.") {
			anthropic++
			if e.Rates.CacheRead > 0 && e.Rates.CacheWrite > 0 {
				withCache++
				if e.Rates.CacheRead >= e.Rates.Input {
					t.Errorf("%s prices cache reads at or above input, which defeats caching: %+v", e.Key, e.Rates)
				}
				if e.Rates.CacheWrite1h > 0 && e.Rates.CacheWrite1h <= e.Rates.CacheWrite {
					t.Errorf("%s prices the one-hour cache at or below the five-minute one: %+v", e.Key, e.Rates)
				}
			}
		}
	}

	if anthropic == 0 {
		t.Fatal("no Anthropic models priced; the model-name join has broken")
	}
	if withCache == 0 {
		t.Error("no Anthropic model has cache rates; the cache usage types have moved")
	}

	// This does not assert that some model publishes both a regional and a
	// global rate that differ, which is what makes the inference-profile
	// prefix a price input. Checking it needs a region whose catalogue carries
	// both for the same model, and this one does not: ap-southeast-1 prices
	// eleven models globally and seven regionally for disjoint sets, so
	// nothing pairs, and the assertion would fail on every run for a reason
	// that is about which models a region offers rather than about this code.
	//
	// The claim is not unguarded. TestSeedCarriesTheScopeSplit holds it against
	// the shipped rate table, deterministically and with no network. What is
	// not checked anywhere is AWS changing its mind - that needs a live run in
	// a region that publishes pairs, us-east-1 among them, and this suite is
	// deliberately single-region.

	// A price the table cannot find is a price nobody gets charged, so prove
	// the round trip from a profile id an operator would actually configure.
	for _, e := range entries {
		if e.Scope != pricing.ScopeGlobal {
			continue
		}
		if _, ok := table.Lookup(Name, "global."+e.Model); !ok {
			t.Errorf("global.%s does not resolve back to its own price", e.Model)
		}
		break
	}

	t.Logf("priced %d entries (%d Anthropic, %d with cache rates) in %s",
		len(entries), anthropic, withCache, region)
	if testing.Verbose() {
		out, _ := json.MarshalIndent(entries[:min(3, len(entries))], "", "  ")
		t.Logf("sample:\n%s", out)
	}

	if path := os.Getenv(writeSeedEnv); path != "" {
		writeSeed(t, path, region, entries)
	}
}

// writeSeedEnv names a path to rewrite the compiled-in seed from a live fetch.
//
// Regenerating the seed through the same parser that refreshes a running
// deployment is the point: there is one implementation of "read the AWS
// catalogue", and the seed is a captured output of it rather than a
// hand-maintained second copy that can quietly disagree.
//
//	PANORAMA_TEST_AWS_REGION=us-east-1 \
//	PANORAMA_WRITE_SEED=internal/pricing/seed.json \
//	go test ./internal/providers/bedrock/ -run TestFetchAgainstLiveAWS
const writeSeedEnv = "PANORAMA_WRITE_SEED"

// writeSeed renders fetched rates in the seed file's format.
func writeSeed(t *testing.T, path, region string, entries []pricing.Entry) {
	t.Helper()

	type seedEntry struct {
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		Scope        string `json:"scope"`
		Input        string `json:"input"`
		Output       string `json:"output"`
		CacheRead    string `json:"cache_read,omitempty"`
		CacheWrite   string `json:"cache_write,omitempty"`
		CacheWrite1h string `json:"cache_write_1h,omitempty"`
		AsOf         string `json:"as_of,omitempty"`
	}

	out := struct {
		Note       string      `json:"note"`
		CapturedAt string      `json:"captured_at"`
		Region     string      `json:"region"`
		Entries    []seedEntry `json:"entries"`
	}{
		Note: "Bedrock on-demand rates in US dollars per million tokens, read from the AWS Price " +
			"List API and regenerated by TestFetchAgainstLiveAWS. Every row is outranked by a " +
			"fetched or configured rate. Scope is which inference-profile prefix the rate applies " +
			"to: `regional` for us./eu./apac., `global` for global. The bedrock-mantle rows are the " +
			"bedrock rows' regional rates under Mantle's model ids: AWS states per-token prices are " +
			"identical on both endpoints for the same model at the in-region rate, and Mantle " +
			"publishes no row of its own for most Claude models.",
		Region: region,
	}

	var captured time.Time
	for _, e := range entries {
		// v1 serves Anthropic models. Seeding the rest would triple the file to
		// price traffic the gateway cannot yet carry; the refresh picks them up
		// when it can.
		if !strings.HasPrefix(e.Model, "anthropic.") {
			continue
		}
		if e.UpdatedAt.After(captured) {
			captured = e.UpdatedAt
		}

		row := seedEntry{
			Provider:     e.Provider,
			Model:        e.Model,
			Scope:        string(e.Scope),
			Input:        decimal(e.Rates.Input),
			Output:       decimal(e.Rates.Output),
			CacheRead:    decimal(e.Rates.CacheRead),
			CacheWrite:   decimal(e.Rates.CacheWrite),
			CacheWrite1h: decimal(e.Rates.CacheWrite1h),
		}
		if !e.AsOf.IsZero() {
			row.AsOf = e.AsOf.UTC().Format(time.RFC3339)
		}
		out.Entries = append(out.Entries, row)
	}

	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].Model != out.Entries[j].Model {
			return out.Entries[i].Model < out.Entries[j].Model
		}
		return out.Entries[i].Scope < out.Entries[j].Scope
	})

	// The same rates under bedrock-mantle's ids, derived the way the Mantle price
	// source derives them: one row per Claude family whose regional rate is not
	// ambiguous, keyed by the family, which is the id Mantle uses.
	families := map[string][]seedEntry{}
	for _, row := range out.Entries {
		// A variant suffix the family table does not strip - ":200k" - is a
		// second rate for one model, not a family.
		family := modelFamily(row.Model)
		if row.Scope != string(pricing.ScopeRegional) || strings.Contains(family, ":") {
			continue
		}
		families[family] = append(families[family], row)
	}
	var mantle []seedEntry
	for family, rows := range families {
		ambiguous := false
		for _, r := range rows[1:] {
			if r.Input != rows[0].Input || r.Output != rows[0].Output || r.CacheRead != rows[0].CacheRead ||
				r.CacheWrite != rows[0].CacheWrite || r.CacheWrite1h != rows[0].CacheWrite1h {
				ambiguous = true
			}
		}
		if ambiguous {
			continue
		}
		row := rows[0]
		row.Provider, row.Model = MantleName, family
		mantle = append(mantle, row)
	}
	sort.Slice(mantle, func(i, j int) bool { return mantle[i].Model < mantle[j].Model })
	out.Entries = append(out.Entries, mantle...)
	out.CapturedAt = captured.UTC().Truncate(time.Second).Format(time.RFC3339)

	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("render seed: %v", err)
	}
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	t.Logf("wrote %d seed rates to %s", len(out.Entries), path)
}

// decimal renders a rate as the shortest exact decimal, so the file can be read
// against a published price list. A zero rate renders empty and is omitted:
// "not published" is not "free".
func decimal(r pricing.Rate) string {
	if r == 0 {
		return ""
	}
	s := pricing.Money(r).USD()
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s += "00"
	}
	// Keep at least two decimal places, which is how prices are quoted.
	if dot := strings.IndexByte(s, '.'); dot >= 0 && len(s)-dot == 2 {
		s += "0"
	}
	return s
}
