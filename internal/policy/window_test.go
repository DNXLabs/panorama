// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"testing"

	"github.com/dnxlabs/panorama/internal/providers"
)

// The windows a deployment might actually have, so the cases read as
// configurations somebody would write.
var testWindows = map[providers.ModelRef]int{
	bedrockRef("us.anthropic.claude-sonnet-5"):                            1_000_000,
	bedrockRef("us.anthropic.claude-haiku-4-5"):                           200_000,
	{Provider: providers.KindBedrockMantle, Model: "openai.gpt-oss-120b"}: 128_000,
}

func TestRouteMinimum(t *testing.T) {
	t.Run("a route reports its model's limit", func(t *testing.T) {
		got := RouteMinimum(testWindows, plain("standard", "us.anthropic.claude-sonnet-5"))
		if got != 1_000_000 {
			t.Errorf("RouteMinimum = %d, want 1000000", got)
		}
	})

	t.Run("an unrecorded model reports nothing", func(t *testing.T) {
		// A number nobody stands behind is worse here than an absence: a
		// client is configured with it once and managed against it for months.
		got := RouteMinimum(testWindows, plain("standard", "us.anthropic.claude-something-new"))
		if got != 0 {
			t.Errorf("RouteMinimum = %d, want 0", got)
		}
	})

	t.Run("the provider is part of the lookup", func(t *testing.T) {
		// The same id on another provider is another target; a limit recorded
		// for one says nothing about the other.
		route := Route{Name: "oss", Target: providers.ModelRef{Provider: providers.KindBedrock, Model: "openai.gpt-oss-120b"}}
		if got := RouteMinimum(testWindows, route); got != 0 {
			t.Errorf("RouteMinimum = %d, want 0 for a model recorded only on another provider", got)
		}
		route.Target.Provider = providers.KindBedrockMantle
		if got := RouteMinimum(testWindows, route); got != 128_000 {
			t.Errorf("RouteMinimum = %d, want 128000", got)
		}
	})
}
