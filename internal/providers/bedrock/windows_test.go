// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import "testing"

// The table is only useful if it survives the two things AWS does to a model
// id: prefixing it with a geography, and revising it in place with a new date.
func TestModelFamily(t *testing.T) {
	cases := map[string]string{
		"anthropic.claude-sonnet-5":                      "anthropic.claude-sonnet-5",
		"us.anthropic.claude-sonnet-5":                   "anthropic.claude-sonnet-5",
		"global.anthropic.claude-sonnet-5":               "anthropic.claude-sonnet-5",
		"au.anthropic.claude-sonnet-5":                   "anthropic.claude-sonnet-5",
		"anthropic.claude-haiku-4-5-20251001-v1:0":       "anthropic.claude-haiku-4-5",
		"us.anthropic.claude-haiku-4-5-20251001-v1:0":    "anthropic.claude-haiku-4-5",
		"anthropic.claude-3-haiku-20240307-v1:0":         "anthropic.claude-3-haiku",
		"US.Anthropic.Claude-Sonnet-5":                   "anthropic.claude-sonnet-5",
		"  anthropic.claude-opus-5  ":                    "anthropic.claude-opus-5",
		"anthropic.claude-sonnet-4-20250514-v1:0":        "anthropic.claude-sonnet-4",
		"apac.anthropic.claude-sonnet-4-5-20250929-v1:0": "anthropic.claude-sonnet-4-5",
	}

	for id, want := range cases {
		if got := modelFamily(id); got != want {
			t.Errorf("modelFamily(%q) = %q, want %q", id, got, want)
		}
	}
}

// A model whose name ends in a version cannot be told from a revision suffix,
// so nothing from that era is in the table and this records why rather than
// leaving the next person to rediscover it.
func TestPreClaude3ModelsAreNotListed(t *testing.T) {
	if got := modelFamily("anthropic.claude-v2:1"); got != "anthropic.claude" {
		t.Errorf("modelFamily(%q) = %q; the -v2 is this model's name rather than a revision, "+
			"which is the ambiguity that keeps its era out of the table", "anthropic.claude-v2:1", got)
	}
	if _, ok := limitsFor(nil, "anthropic.claude-v2:1"); ok {
		t.Error("a pre-Claude-3 model reported limits; it should be unrecorded")
	}
}

// A geography and a provider both sit in the first dotted component, and
// mistaking one for the other loses every model.
func TestModelFamilyKeepsProviderPrefix(t *testing.T) {
	if got := modelFamily("amazon.nova-pro-v1:0"); got != "amazon.nova-pro" {
		t.Errorf("modelFamily stripped a provider name: got %q", got)
	}
	if got := modelFamily("meta.llama3-70b-instruct-v1:0"); got != "meta.llama3-70b-instruct" {
		t.Errorf("modelFamily stripped a provider name: got %q", got)
	}
}

func TestLimitsFor(t *testing.T) {
	t.Run("a geography does not change the window", func(t *testing.T) {
		us, ok := limitsFor(nil, "us.anthropic.claude-sonnet-5")
		if !ok {
			t.Fatal("us.anthropic.claude-sonnet-5 is not in the table")
		}
		global, ok := limitsFor(nil, "global.anthropic.claude-sonnet-5")
		if !ok {
			t.Fatal("global.anthropic.claude-sonnet-5 is not in the table")
		}
		if us != global {
			t.Errorf("us %+v and global %+v disagree; a geography decides where a request is "+
				"processed and what it costs, never how much context the model takes", us, global)
		}
		if us.Context != window1M {
			t.Errorf("Sonnet 5 context = %d, want %d", us.Context, window1M)
		}
	})

	t.Run("an unknown model is absent rather than zero-or-default", func(t *testing.T) {
		limits, ok := limitsFor(nil, "anthropic.claude-model-nobody-has-shipped")
		if ok {
			t.Errorf("an unheard-of model reported limits %+v; it has to report nothing, because "+
				"a route refusal and a client's token budget are both derived from this", limits)
		}
	})

	t.Run("an override wins over the built-in table", func(t *testing.T) {
		limits, ok := limitsFor(map[string]int{"anthropic.claude-sonnet-5": 400_000},
			"us.anthropic.claude-sonnet-5")
		if !ok || limits.Context != 400_000 {
			t.Errorf("got (%+v, %v), want a 400000 window: an operator is looking at the "+
				"deployment and this build is not", limits, ok)
		}
		if limits.Output == 0 {
			t.Error("the override erased the output cap; it carries the context window only")
		}
	})

	t.Run("an override matches an exact id too", func(t *testing.T) {
		limits, ok := limitsFor(map[string]int{"us.anthropic.claude-sonnet-5": 300_000},
			"us.anthropic.claude-sonnet-5")
		if !ok || limits.Context != 300_000 {
			t.Errorf("got (%+v, %v), want a 300000 window", limits, ok)
		}
	})

	t.Run("zero is how an operator says stop claiming to know this", func(t *testing.T) {
		limits, ok := limitsFor(map[string]int{"anthropic.claude-sonnet-5": 0},
			"us.anthropic.claude-sonnet-5")
		if ok {
			t.Errorf("got %+v; zero has to erase the built-in entry rather than fall back to it, "+
				"or an operator who knows the table is wrong cannot say so", limits)
		}
	})

	t.Run("every entry carries both numbers", func(t *testing.T) {
		// opencode's provider block takes `limit` as one object and requires
		// both, so an entry with only a window cannot produce a valid one.
		for family, limits := range defaultModelLimits {
			if limits.Context <= 0 || limits.Output <= 0 {
				t.Errorf("%s has limits %+v; both numbers are required", family, limits)
			}
			if limits.Output >= limits.Context {
				t.Errorf("%s produces %d out of a %d window, which is not a model that exists",
					family, limits.Output, limits.Context)
			}
		}
	})
}
