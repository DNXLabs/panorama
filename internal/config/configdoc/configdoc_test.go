// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package configdoc_test

import (
	"os"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/config/configdoc"
)

// The published configuration reference is what the source says it is.
//
// WHY THE DATA IS COMMITTED AND CHECKED RATHER THAN GENERATED AT BUILD TIME.
// The docs build runs in a Node container with no Go toolchain, and adding one
// to it would put a Go build in front of every heading change. So the derived
// table is committed and this test is what stops it going stale: change a
// default, rename a key, add one, and this fails - at the desk of whoever
// changed it, rather than in a reader's browser six weeks later.
//
// It is the same arrangement as docs/csp-hashes.json, for the same reason.
func TestTheConfigurationReferenceIsCurrent(t *testing.T) {
	const generated = "../../../docs/src/data/config-keys.json"

	keys, err := configdoc.Collect("..", config.Default())
	if err != nil {
		t.Fatalf("collecting the configuration keys: %v", err)
	}
	if len(keys) == 0 {
		// The tree is walked from the Config struct by name, so a rename would
		// otherwise leave this test asserting that nothing equals nothing.
		t.Fatal("collected no keys: this test can no longer see the configuration")
	}

	want, err := configdoc.Marshal(keys)
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}

	got, err := os.ReadFile(generated)
	if err != nil {
		t.Fatalf("reading %s: %v", generated, err)
	}

	if string(got) != string(want) {
		t.Errorf("%s is out of date with internal/config.\n\n"+
			"Regenerate it:\n"+
			"  go run ./internal/config/configdoc/generate -o %s\n\n"+
			"It carries %d keys; the source now describes %d.",
			generated, generated, countKeys(got), len(keys))
	}
}

// Every key reaches a reader by at least one route, which is the claim the
// reference makes by having an Env column at all.
//
// A key with neither a path nor a variable is one somebody can only set by
// editing the struct, and publishing a row for it would be documenting a
// setting nobody can use.
func TestEveryKeyCanActuallyBeSet(t *testing.T) {
	keys, err := configdoc.Collect("..", config.Default())
	if err != nil {
		t.Fatalf("collecting the configuration keys: %v", err)
	}

	for _, k := range keys {
		if k.Path == "" {
			t.Errorf("a key has no file path: %+v", k)
		}
		if k.Env == "" {
			t.Errorf("%s has no environment variable; if that is deliberate, the reference "+
				"should say so rather than leaving the column empty", k.Path)
		}
		if k.Doc == "" {
			t.Errorf("%s has no doc comment, so the reference has nothing to say about it", k.Path)
		}
	}
}

// The variable name is the key, mechanically, and the reference says so.
//
// The page states the rule once - PANORAMA_ plus the path upper-cased with
// dots as underscores - instead of asking a reader to check forty-three rows
// for an exception. That sentence is only safe while it is true, so this is
// what keeps it true: add a key whose variable is spelled any other way and
// the claim has to change with it.
func TestEveryVariableNameFollowsTheRule(t *testing.T) {
	keys, err := configdoc.Collect("..", config.Default())
	if err != nil {
		t.Fatalf("collecting the configuration keys: %v", err)
	}

	for _, k := range keys {
		want := "PANORAMA_" + strings.ToUpper(strings.ReplaceAll(k.Path, ".", "_"))
		if k.Env != want {
			t.Errorf("%s is set by %s, not %s.\n"+
				"The configuration reference states the rule once rather than listing exceptions.\n"+
				"Either spell the variable the mechanical way, or change that sentence in\n"+
				"docs/scripts/generate-reference.mjs to stop claiming there are none.",
				k.Path, k.Env, want)
		}
	}
}

func countKeys(body []byte) int {
	var n int
	for i := 0; i+8 < len(body); i++ {
		if string(body[i:i+8]) == `"path": ` {
			n++
		}
	}
	return n
}
