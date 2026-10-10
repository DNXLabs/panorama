// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dnxlabs/panorama/internal/config"
)

// The SQLite schema is applied once per package run and copied per test.
//
// eachDialect is used dozens of times in this package and would migrate on
// every one of them. Applying the schema to a fresh file under the race
// detector, once per test, was very nearly the whole of what this package's
// tests spent their time on - and the migrations are already tested here
// directly, so applying them again for a test about token expiry proves
// nothing.
//
// Each test still gets its own file, because sharing one would make the tests
// order-dependent. The schema still comes from the real migrations rather than
// from a dump that could drift from them.
var (
	sqliteTemplateOnce sync.Once
	sqliteTemplateDir  string
	sqliteTemplatePath string
	sqliteTemplateErr  error
)

func buildSQLiteTemplate() {
	sqliteTemplateDir, sqliteTemplateErr = os.MkdirTemp("", "panorama-schema-")
	if sqliteTemplateErr != nil {
		return
	}
	sqliteTemplatePath = filepath.Join(sqliteTemplateDir, "panorama.db")

	ctx := context.Background()
	db, err := Open(ctx, config.Database{
		Driver:       config.DriverSQLite,
		DSN:          sqliteDSN(sqliteTemplatePath),
		MaxOpenConns: 1,
	})
	if err != nil {
		sqliteTemplateErr = err
		return
	}
	if err := db.Migrate(ctx); err != nil {
		_ = db.Close()
		sqliteTemplateErr = err
		return
	}
	// Closed before it is ever copied: a handle still open could hold pages the
	// file does not, and every test would start from a database missing
	// whatever had not been written back.
	sqliteTemplateErr = db.Close()
}

// sqliteDSN is the one spelling of the connection string, so the template and
// the copies cannot be opened with different pragmas.
func sqliteDSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
}

// migratedSQLiteFile copies the schema template into the test's own directory.
func migratedSQLiteFile(t *testing.T) string {
	t.Helper()

	sqliteTemplateOnce.Do(buildSQLiteTemplate)
	if sqliteTemplateErr != nil {
		t.Fatalf("building the schema template: %v", sqliteTemplateErr)
	}

	raw, err := os.ReadFile(sqliteTemplatePath)
	if err != nil {
		t.Fatalf("reading the schema template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "panorama.db")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing the test database: %v", err)
	}
	return path
}

// migratedSQLite opens a database that already has the schema.
//
// The schema is copied rather than migrated, for the reason above. openSQLite
// remains the way to get an empty one, which is what the migration tests
// themselves need.
func migratedSQLite(t *testing.T) *DB {
	t.Helper()

	db, err := Open(t.Context(), config.Database{
		Driver:       config.DriverSQLite,
		DSN:          sqliteDSN(migratedSQLiteFile(t)),
		MaxOpenConns: 1,
	})
	if err != nil {
		t.Fatalf("Open sqlite: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return db
}

// TestMain removes the template, which outlives every individual test.
func TestMain(m *testing.M) {
	code := m.Run()
	if sqliteTemplateDir != "" {
		_ = os.RemoveAll(sqliteTemplateDir)
	}
	os.Exit(code)
}
