// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/store"
)

// The schema is applied once per package run and copied per test, for the same
// reason as in internal/store and internal/controlplane: twenty migrations
// against a fresh file costs about 1.7 seconds under the race detector, every
// fixture here was paying it, and none of it tests anything this package is
// about.
//
// Each test still gets its own file. Sharing one would make the tests
// order-dependent, which costs more than it saves.
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
	// Closed before it is ever copied; an open handle could hold pages the file
	// does not.
	templateErr = db.Close()
}

// migratedDatabaseFile returns a fresh database with the schema applied, in the
// test's own directory.
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

// TestMain removes the template, which outlives every individual test.
func TestMain(m *testing.M) {
	code := m.Run()
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
	os.Exit(code)
}
