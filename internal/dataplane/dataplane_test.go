// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"net/http"
	"strings"
	"testing"
)

func TestBetasFrom(t *testing.T) {
	t.Run("a comma-separated header is split", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-beta", "claude-code-20250219,context-1m-2025-08-07, effort-2025-11-24 ")

		got := betasFrom(h)
		want := []string{"claude-code-20250219", "context-1m-2025-08-07", "effort-2025-11-24"}

		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("betasFrom = %v, want %v", got, want)
		}
	})

	t.Run("repeated headers are all collected", func(t *testing.T) {
		h := http.Header{}
		h.Add("anthropic-beta", "one")
		h.Add("anthropic-beta", "two,three")

		if got := betasFrom(h); len(got) != 3 {
			t.Errorf("betasFrom = %v, want three flags", got)
		}
	})

	t.Run("absent means nil", func(t *testing.T) {
		if got := betasFrom(http.Header{}); got != nil {
			t.Errorf("betasFrom = %v, want nil", got)
		}
	})
}
