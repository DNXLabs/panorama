// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The CLI and its manual agree, and that is asserted rather than remembered.
//
// WHY THIS TEST EXISTS. Everything else about keeping the documentation true is
// mechanical: the configuration and policy reference pages are generated from
// config.example.yaml and policy.example.yaml on every docs build, so a key
// cannot be added without documenting it. The command list is the one surface
// with no generator, because a command's prose - when to reach for it, and the
// trap it exists to avoid - is written by somebody who understands it, not
// derived from a switch statement.
//
// So the prose stays written and only its coverage is checked. Adding a
// command and forgetting the page fails here, at the desk of the person who
// added it, rather than becoming a command nobody outside this repository
// knows about.
//
// It deliberately does not check the reverse - a command documented but no
// longer implemented - because a removed command should keep a line in the
// manual saying it was removed, and a test cannot tell that from a stale
// entry.
func TestEveryCommandIsDocumented(t *testing.T) {
	const page = "../../docs/src/content/docs/reference/cli.md"

	body, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("reading the CLI reference: %v", err)
	}
	documented := string(body)

	commands := dispatchedCommands(t)
	if len(commands) == 0 {
		// The switches are found by shape, so a refactor that renamed the
		// `command` variable would silently leave this test asserting nothing.
		t.Fatal("found no command dispatch: this test can no longer see the CLI, " +
			"which means it is passing without checking anything")
	}

	var missing []string
	for _, name := range commands {
		// `panorama token` as a word, not as a substring of another: matching
		// bare would let "tokens" in a sentence satisfy the `token` command.
		pattern := regexp.MustCompile(`(?m)panorama ` + regexp.QuoteMeta(name) + `\b`)
		if !pattern.MatchString(documented) {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		t.Errorf("these commands are dispatched but absent from %s:\n  %s\n\n"+
			"Add each one to the reference with the line a reader needs: what it is for,\n"+
			"and whether it runs inside the container or on a laptop.",
			page, strings.Join(missing, "\n  "))
	}
}

// Every string a `switch command` or `switch name` in this package dispatches
// on. Found by walking the AST rather than by matching text, so a case added
// on the same line as another is still seen.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package: %v", err)
	}

	seen := map[string]bool{}
	fset := gotoken.NewFileSet()

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}

		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}

		ast.Inspect(parsed, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			tag, ok := sw.Tag.(*ast.Ident)
			// Only the top-level dispatches. A subcommand switch inside one of
			// them runs on `args[0]` and is not an *ast.Ident, so it is
			// skipped: this asserts that `panorama sso` is documented, and
			// leaves which of its subcommands are worth a line to the writer.
			if !ok || (tag.Name != "command" && tag.Name != "name") {
				return true
			}
			for _, clause := range sw.Body.List {
				cc, ok := clause.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range cc.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != gotoken.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err == nil && value != "" {
						seen[value] = true
					}
				}
			}
			return true
		})
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
