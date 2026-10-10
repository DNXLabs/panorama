// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"errors"
	"math"
	"testing"
)

func TestParseUSDIsExact(t *testing.T) {
	cases := map[string]Money{
		"0":           0,
		"1":           1_000_000_000,
		"3.00":        3_000_000_000,
		"0.30":        300_000_000,
		"2.20":        2_200_000_000,
		"1.375":       1_375_000_000,
		"0.000000001": 1,
		"-0.50":       -500_000_000,
		"+2.5":        2_500_000_000,
		// Digits past a nanodollar round rather than vanish.
		"0.0000000004": 0,
		"0.0000000005": 1,
		"0.0000000016": 2,
	}
	for in, want := range cases {
		got, err := ParseUSD(in)
		if err != nil {
			t.Errorf("ParseUSD(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseUSD(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestParseUSDDoesNotGoThroughAFloat is the reason this parser exists at all.
// 0.1 and 0.3 have no exact binary representation, so a float-based parser
// returns values that do not sum to the number a person would write down.
func TestParseUSDDoesNotGoThroughAFloat(t *testing.T) {
	var sum Money
	for range 10 {
		m, err := ParseUSD("0.1")
		if err != nil {
			t.Fatal(err)
		}
		sum += m
	}

	if want := Money(1_000_000_000); sum != want {
		t.Errorf("ten times $0.10 = %s, want %s", sum, want)
	}
	if sum.USD() != "1.000000000" {
		t.Errorf("USD() = %q, want exactly 1.000000000", sum.USD())
	}

	// The same sum through float64 does not land on 1.0, which is the bug this
	// type is built to avoid.
	var viaFloat float64
	for range 10 {
		viaFloat += 0.1
	}
	if viaFloat == 1.0 {
		t.Skip("this platform's float64 sums 0.1 ten times to exactly 1.0; the comparison proves nothing here")
	}
}

func TestParseUSDRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "  ", "abc", "1.2.3", "1,000", "3.00USD", "-", "1e9", "0x10"} {
		if got, err := ParseUSD(in); err == nil {
			t.Errorf("ParseUSD(%q) = %d, want an error", in, got)
		}
	}
}

func TestParseUnitsAgree(t *testing.T) {
	// The same price quoted three ways must produce the same rate, because AWS
	// quotes all three depending on which catalogue a model is in.
	perMillion, err := ParseUSDPerMillion("2.20")
	if err != nil {
		t.Fatal(err)
	}
	perThousand, err := ParseUSDPerThousand("0.0022")
	if err != nil {
		t.Fatal(err)
	}
	perToken, err := ParseUSDPerToken("0.0000022")
	if err != nil {
		t.Fatal(err)
	}

	if perMillion != perThousand || perMillion != perToken {
		t.Errorf("units disagree: perMillion=%d perThousand=%d perToken=%d",
			perMillion, perThousand, perToken)
	}
}

func TestRateCost(t *testing.T) {
	// $3.00 per million.
	rate, err := ParseUSDPerMillion("3.00")
	if err != nil {
		t.Fatal(err)
	}

	cases := map[int64]string{
		0:         "0.000000000",
		1:         "0.000003000",
		1_000:     "0.003000000",
		1_000_000: "3.000000000",
		333_333:   "0.999999000",
	}
	for tokens, want := range cases {
		got, cerr := rate.Cost(tokens)
		if cerr != nil {
			t.Errorf("Cost(%d): %v", tokens, cerr)
			continue
		}
		if got.USD() != want {
			t.Errorf("Cost(%d) = %s, want %s", tokens, got.USD(), want)
		}
	}
}

// TestRateCostMatchesTheObservedCacheRead pins the arithmetic to a real
// measurement: one Claude Code turn recorded during M1 read 75,069 cached
// tokens, and the ten-fold gap between pricing those as cache reads and pricing
// them as input is the entire argument for tracking the classes separately.
func TestRateCostMatchesTheObservedCacheRead(t *testing.T) {
	const observed = 75_069

	inputRate, err := ParseUSDPerMillion("3.00")
	if err != nil {
		t.Fatal(err)
	}
	cacheReadRate, err := ParseUSDPerMillion("0.30")
	if err != nil {
		t.Fatal(err)
	}

	asInput, err := inputRate.Cost(observed)
	if err != nil {
		t.Fatal(err)
	}
	asCacheRead, err := cacheReadRate.Cost(observed)
	if err != nil {
		t.Fatal(err)
	}

	if asInput.USD() != "0.225207000" {
		t.Errorf("as input = %s, want 0.225207000", asInput.USD())
	}
	if asCacheRead.USD() != "0.022520700" {
		t.Errorf("as cache read = %s, want 0.022520700", asCacheRead.USD())
	}
	if asInput != asCacheRead*10 {
		t.Errorf("the two should differ by exactly 10x: %s vs %s", asInput, asCacheRead)
	}
}

// TestRateCostRefusesToWrapAround guards the failure that would be worst:
// silently reporting a negative cost.
func TestRateCostRefusesToWrapAround(t *testing.T) {
	rate := Rate(math.MaxInt64)

	got, err := rate.Cost(math.MaxInt64)
	if err == nil {
		t.Fatalf("Cost overflowed to %d without an error", got)
	}
	if !errors.Is(err, ErrCostOverflow) {
		t.Errorf("error = %v, want ErrCostOverflow", err)
	}

	if _, err := Rate(1).Cost(-5); !errors.Is(err, ErrNegativeTokens) {
		t.Errorf("negative tokens error = %v, want ErrNegativeTokens", err)
	}
}

func TestMoneyUSDRendersNegativesAndZero(t *testing.T) {
	cases := map[Money]string{
		0:              "0.000000000",
		1:              "0.000000001",
		-1:             "-0.000000001",
		1_000_000_000:  "1.000000000",
		-2_500_000_000: "-2.500000000",
		22_520_700:     "0.022520700",
	}
	for in, want := range cases {
		if got := in.USD(); got != want {
			t.Errorf("Money(%d).USD() = %q, want %q", int64(in), got, want)
		}
	}
}
