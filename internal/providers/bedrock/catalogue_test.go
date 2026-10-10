// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"slices"
	"testing"

	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
)

// TestRegionsComeFromARNsNotProse. The profile's description says "Routes
// requests to Claude Haiku 4.5 in ap-southeast-2, ap-southeast-4." - the same
// information in English. Reading a compliance answer out of prose means it
// changes the day AWS rewords the sentence, so the region field of the ARN is
// what is read instead.
func TestRegionsComeFromARNsNotProse(t *testing.T) {
	model, regions := profileTargets([]bedrocktypes.InferenceProfileModel{
		{ModelArn: strPtr("arn:aws:bedrock:ap-southeast-2::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0")},
		{ModelArn: strPtr("arn:aws:bedrock:ap-southeast-4::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0")},
	})

	if model != "anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Errorf("model = %q", model)
	}
	if want := []string{"ap-southeast-2", "ap-southeast-4"}; !slices.Equal(regions, want) {
		t.Errorf("regions = %v, want %v", regions, want)
	}
}

// TestAGlobalProfileClaimsNoRegions. Its ARN carries an empty region -
// arn:aws:bedrock:::… - which is how "anywhere" arrives. Turning that into a
// list would be inventing a residency guarantee that does not exist.
func TestAGlobalProfileClaimsNoRegions(t *testing.T) {
	_, regions := profileTargets([]bedrocktypes.InferenceProfileModel{
		{ModelArn: strPtr("arn:aws:bedrock:::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0")},
		{ModelArn: strPtr("arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-haiku-4-5-20251001-v1:0")},
	})

	// us-east-1 is still listed, because it is genuinely one of the places a
	// global profile may land. What matters is that the empty region does not
	// become a phantom entry, and that the caller treats `global` as
	// unrestricted from its geography rather than from this list.
	if slices.Contains(regions, "") {
		t.Errorf("regions = %v, want no empty entry", regions)
	}
}

func TestGeographyIsAnAllowlist(t *testing.T) {
	for id, want := range map[string]string{
		"us.anthropic.claude-sonnet-5":                 "us",
		"au.anthropic.claude-sonnet-5":                 "au",
		"global.anthropic.claude-sonnet-5":             "global",
		"apac.anthropic.claude-sonnet-4-20250514-v1:0": "apac",
		// A bare foundation-model id. `anthropic` is not a geography, and
		// treating any leading segment as one would invent a scope for every
		// model in the catalogue.
		"anthropic.claude-3-haiku-20240307-v1:0": "",
		"amazon.nova-pro-v1:0":                   "",
		// Something AWS has not published yet. Unrecognised leaves it blank
		// rather than guessing, which is the same posture pricing takes.
		"mars.anthropic.claude-sonnet-5": "",
	} {
		if got := geographyOf(id); got != want {
			t.Errorf("geographyOf(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestTheGeographyListMatchesPricing. A prefix this recognised but pricing did
// not would put a model in the console priced at nothing; one pricing
// recognised and this did not would show a geography of "-" against a real
// profile. They have to be the same set.
func TestTheGeographyListMatchesPricing(t *testing.T) {
	// The prefixes pricing.ParseModel treats as regional, plus global.
	for _, prefix := range []string{
		"global", "us", "us-gov", "eu", "apac", "jp", "au", "ca", "sa", "in", "il", "mx", "kr",
	} {
		if _, ok := knownGeographies[prefix]; !ok {
			t.Errorf("pricing knows the %q prefix and the catalogue does not", prefix)
		}
	}
	if len(knownGeographies) != 13 {
		t.Errorf("%d geographies known here; keep this in step with pricing's own list",
			len(knownGeographies))
	}
}

func TestParseModelARN(t *testing.T) {
	region, id := parseModelARN(
		"arn:aws:bedrock:us-west-2::foundation-model/anthropic.claude-opus-4-6-v1")
	if region != "us-west-2" || id != "anthropic.claude-opus-4-6-v1" {
		t.Errorf("parseModelARN = %q, %q", region, id)
	}

	if region, id := parseModelARN("not-an-arn"); region != "" || id != "" {
		t.Errorf("a malformed ARN produced %q, %q - want both empty", region, id)
	}
}
