// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/store"
)

// The schema is applied once per package run and copied per test.
//
// Applying it costs about 1.7 seconds under the race detector - twenty
// migrations, each opening a transaction against a fresh file - and every
// fixture in this package was paying it. At around a hundred fixtures a run
// that was most of what this package's tests spent their time on, and none of
// it was testing anything: the migrations have their own tests in
// internal/store.
//
// A copy of a file that already has the schema costs single-digit
// milliseconds, and it is the same schema by construction rather than by a
// second definition that could drift from the migrations.
//
// Each test still gets its own file. Sharing one database would make the tests
// order-dependent, which is a much worse trade than the time it saves.
var (
	templateOnce sync.Once
	templateDir  string
	templatePath string
	templateErr  error
)

func buildTemplate() {
	templateDir, templateErr = os.MkdirTemp("", "panorama-schema-")
	if templateErr != nil {
		return
	}
	templatePath = filepath.Join(templateDir, "panorama.db")

	cfg := config.Default()
	cfg.Database.DSN = "file:" + templatePath

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		templateErr = err
		return
	}
	if err := db.Migrate(ctx); err != nil {
		_ = db.Close()
		templateErr = err
		return
	}
	// Closed before it is ever copied. A handle still open could have pages
	// the file does not, and every test would start from a database missing
	// whatever had not been written back.
	templateErr = db.Close()
}

// migratedDatabaseFile returns a path to a fresh database with the schema
// already applied, inside the test's own directory.
func migratedDatabaseFile(t *testing.T) string {
	t.Helper()

	templateOnce.Do(buildTemplate)
	if templateErr != nil {
		t.Fatalf("building the schema template: %v", templateErr)
	}

	raw, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("reading the schema template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "panorama.db")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing the test database: %v", err)
	}
	return path
}

// TestMain removes the template, which outlives every individual test and so
// cannot be cleaned up by one.
func TestMain(m *testing.M) {
	code := m.Run()
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
	os.Exit(code)
}
