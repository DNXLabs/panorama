// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// TestPricingRatesKeepTheirDecimalText is the reason ModelRates holds strings.
// YAML would parse 0.30 into a float64 that is not 0.30, and a rate that
// disagrees with the invoice by a rounding error is a rate that gets argued
// with instead of used.
func TestPricingRatesKeepTheirDecimalText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Written unquoted, exactly as an operator would.
	body := `
pricing:
  refresh_interval: 12h
  models:
    bedrock/us.anthropic.claude-sonnet-5:
      input: 2.20
      output: 11.00
      cache_read: 0.22
      cache_write: 2.75
      cache_write_1h: 4.40
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Pricing.RefreshInterval != 12*time.Hour {
		t.Errorf("refresh_interval = %s, want 12h", cfg.Pricing.RefreshInterval)
	}

	configured, ok := cfg.Pricing.Models["bedrock/us.anthropic.claude-sonnet-5"]
	if !ok {
		t.Fatal("the configured model is missing")
	}
	if configured.Input != "2.20" {
		t.Errorf("input = %q, want the literal text 2.20", configured.Input)
	}

	card, err := configured.Card()
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	want := pricing.Rates{
		Input: 2_200_000_000, Output: 11_000_000_000, CacheRead: 220_000_000,
		CacheWrite: 2_750_000_000, CacheWrite1h: 4_400_000_000,
	}
	if card.Rates != want {
		t.Errorf("rates = %+v, want %+v", card.Rates, want)
	}
}

// A rate says which provider it prices, and a key that does not is refused.
//
// The ARN is the case worth keeping: it carries a slash of its own, so
// "does this key name a provider" cannot be answered by looking for one. The
// segment before the first slash has to be a provider kind, which
// "arn:aws:bedrock:us-east-1:1:inference-profile" is not.
func TestAPricingKeyThatNamesNoProviderIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want string
	}{
		{
			name: "a bare model id",
			key:  "us.anthropic.claude-sonnet-5",
			want: "bedrock/us.anthropic.claude-sonnet-5",
		},
		{
			name: "an ARN, whose own slash names no provider",
			key:  "arn:aws:bedrock:us-east-1:1:inference-profile/us.anthropic.claude-haiku-4-5",
			want: "bedrock/arn:aws:bedrock:us-east-1:1:inference-profile/us.anthropic.claude-haiku-4-5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Pricing.Models = map[string]ModelRates{tc.key: {Input: "2.20", Output: "11.00"}}

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate = nil, want %q refused", tc.key)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to give the spelling %q", err, tc.want)
			}
		})
	}

	// Either provider's own prefix is accepted.
	cfg := Default()
	cfg.Pricing.Models = map[string]ModelRates{
		"bedrock/us.anthropic.claude-sonnet-5": {Input: "2.20", Output: "11.00"},
		"bedrock-mantle/openai.gpt-oss-120b":   {Input: "0.15", Output: "0.60"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate = %v, want both prefixed references accepted", err)
	}
}

func TestPricingModelsFromTheEnvironment(t *testing.T) {
	cfg := Default()
	env := mapEnv{
		"PANORAMA_PRICING_REFRESH_INTERVAL": "6h",
		"PANORAMA_PRICING_MODELS": "us.anthropic.claude-sonnet-5=2.20/11.00/0.22/2.75/4.40," +
			"global.anthropic.claude-sonnet-5=2.00/10.00/0.20/2.50/4.00",
	}

	if err := applyEnv(&cfg, env.get); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}

	if cfg.Pricing.RefreshInterval != 6*time.Hour {
		t.Errorf("refresh_interval = %s, want 6h", cfg.Pricing.RefreshInterval)
	}
	if len(cfg.Pricing.Models) != 2 {
		t.Fatalf("parsed %d models, want 2", len(cfg.Pricing.Models))
	}

	global, err := cfg.Pricing.Models["global.anthropic.claude-sonnet-5"].Card()
	if err != nil {
		t.Fatal(err)
	}
	if global.Rates.Input != 2_000_000_000 {
		t.Errorf("global input = %d, want 2000000000", global.Rates.Input)
	}
}

// A long-context price is part of an override, in the file and in the
// environment, or an override of GPT-5.x would cost every long session at the
// short-context rate.
func TestAnOverrideCarriesItsTiers(t *testing.T) {
	cfg := Default()
	env := mapEnv{
		"PANORAMA_PRICING_MODELS": "bedrock-mantle/openai.gpt-5.6-luna=0.22/1.32/0.022/0.275/" +
			"@272000=0.44/1.98/0.044/0.55/@800000=0.88/3.96/0.088/1.10/",
	}
	if err := applyEnv(&cfg, env.get); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	card, err := cfg.Pricing.Models["bedrock-mantle/openai.gpt-5.6-luna"].Card()
	if err != nil {
		t.Fatal(err)
	}
	if card.Rates.Input != 220_000_000 || card.Rates.CacheWrite1h != 0 {
		t.Errorf("base = %+v", card.Rates)
	}
	if len(card.Tiers) != 2 || card.Tiers[0].AboveInputTokens != 272_000 ||
		card.Tiers[0].Rates.Output != 1_980_000_000 || card.Tiers[1].AboveInputTokens != 800_000 {
		t.Errorf("tiers = %+v", card.Tiers)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, []byte(`
pricing:
  models:
    bedrock-mantle/openai.gpt-5.6-terra:
      input: "2.20"
      output: "13.20"
      cache_read: "0.22"
      cache_write: "2.75"
      tiers:
        - above_input_tokens: 272000
          input: "4.40"
          output: "19.80"
          cache_read: "0.44"
          cache_write: "5.50"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	yamlCfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	terra, err := yamlCfg.Pricing.Models["bedrock-mantle/openai.gpt-5.6-terra"].Card()
	if err != nil {
		t.Fatal(err)
	}
	if len(terra.Tiers) != 1 || terra.Tiers[0].Rates.Input != 4_400_000_000 {
		t.Errorf("YAML tiers = %+v", terra.Tiers)
	}
}

// A tier that leaves out input or output, or thresholds out of order, would
// price some requests at nothing or at the wrong tier; both are refused when
// the configuration loads.
func TestABadTierIsRefused(t *testing.T) {
	for name, value := range map[string]string{
		"tier without output":   "bedrock-mantle/m=1/2///@1000=3////",
		"descending thresholds": "bedrock-mantle/m=1/2///@2000=3/4///@1000=5/6///",
		"zero threshold":        "bedrock-mantle/m=1/2///@0=3/4///",
		"threshold not number":  "bedrock-mantle/m=1/2///@many=3/4///",
		"tier with few rates":   "bedrock-mantle/m=1/2///@1000=3/4",
	} {
		cfg := Default()
		env := mapEnv{"PANORAMA_PRICING_MODELS": value}
		if err := applyEnv(&cfg, env.get); err != nil {
			continue
		}
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: %q was accepted", name, value)
		}
	}
}

func TestPricingModelsFromTheEnvironmentRejectsBadForms(t *testing.T) {
	for name, value := range map[string]string{
		"no rates":     "us.anthropic.claude-sonnet-5",
		"too few":      "us.anthropic.claude-sonnet-5=2.20/11.00",
		"too many":     "us.anthropic.claude-sonnet-5=1/2/3/4/5/6",
		"no model":     "=1/2/3/4/5",
		"not a number": "m=one/2/3/4/5",
	} {
		cfg := Default()
		env := mapEnv{"PANORAMA_PRICING_MODELS": value}

		err := applyEnv(&cfg, env.get)
		if err == nil {
			// A rate that fails to parse is caught by validation instead.
			if verr := cfg.Validate(); verr == nil {
				t.Errorf("%s: %q was accepted", name, value)
			}
			continue
		}
	}
}

// TestPartialRateOverrideIsRefused catches the footgun in the override
// feature: because a configured rate replaces the published one outright,
// naming only some classes would silently leave the rest unpriced.
func TestPartialRateOverrideIsRefused(t *testing.T) {
	cfg := Default()
	cfg.Pricing.Models = map[string]ModelRates{
		"bedrock/us.anthropic.claude-sonnet-5": {CacheRead: "0.22"},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a partial rate override must be refused")
	}
	if !strings.Contains(err.Error(), "input and output are required") {
		t.Errorf("error = %v, want it to explain what is missing", err)
	}
}

func TestPricingValidatesRefreshInterval(t *testing.T) {
	cfg := Default()

	cfg.Pricing.RefreshInterval = -time.Hour
	if err := cfg.Validate(); err == nil {
		t.Error("a negative refresh interval must be refused")
	}

	// Hammering the pricing API achieves nothing: published rates move on the
	// order of months.
	cfg.Pricing.RefreshInterval = time.Second
	if err := cfg.Validate(); err == nil {
		t.Error("a one-second refresh interval must be refused")
	}

	// Zero is the documented way to turn the refresh off.
	cfg.Pricing.RefreshInterval = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero must be allowed as \"off\": %v", err)
	}
}

func TestPricingRejectsUnparsableRates(t *testing.T) {
	cfg := Default()
	cfg.Pricing.Models = map[string]ModelRates{
		"m": {Input: "free", Output: "1.00"},
	}

	if err := cfg.Validate(); err == nil {
		t.Error("an unparsable rate must fail at startup, not at the first request")
	}
}
