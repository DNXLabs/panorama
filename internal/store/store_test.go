// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dnxlabs/panorama/internal/config"
)

// testDBEnv names a Postgres DSN to run the Postgres half of these tests
// against. Without it, the Postgres cases skip: the suite must stay runnable
// with nothing but a Go toolchain.
const testDBEnv = "PANORAMA_TEST_POSTGRES_DSN"

func TestOpenRejectsUnknownDriver(t *testing.T) {
	_, err := Open(t.Context(), config.Database{Driver: "mysql", DSN: "whatever"})
	if err == nil {
		t.Fatal("an unsupported driver must be rejected, got nil error")
	}
}

func TestOpenSQLite(t *testing.T) {
	db := openSQLite(t)

	if db.Driver() != config.DriverSQLite {
		t.Errorf("Driver() = %q, want %q", db.Driver(), config.DriverSQLite)
	}
	if !db.IsSQLite() {
		t.Error("IsSQLite() = false for a SQLite handle")
	}
}

func TestOpenSQLiteFailsOnAnUnwritablePath(t *testing.T) {
	// A directory that does not exist cannot be created implicitly, so this
	// surfaces at Ping rather than at sql.Open - which is exactly the case
	// Open's eager ping exists to catch.
	dsn := "file:" + filepath.Join(t.TempDir(), "no-such-dir", "panorama.db")

	if _, err := Open(t.Context(), config.Database{Driver: config.DriverSQLite, DSN: dsn}); err == nil {
		t.Fatal("opening a database under a missing directory must fail at startup, got nil error")
	}
}

func TestMigrateSQLite(t *testing.T) {
	db := openSQLite(t)
	ctx := t.Context()

	pending, err := db.PendingMigrations(ctx)
	if err != nil {
		t.Fatalf("PendingMigrations: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("a fresh database should have pending migrations")
	}

	if merr := db.Migrate(ctx); merr != nil {
		t.Fatalf("Migrate: %v", merr)
	}

	pending, err = db.PendingMigrations(ctx)
	if err != nil {
		t.Fatalf("PendingMigrations after Migrate: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d migrations still pending after Migrate: %v", len(pending), pending)
	}

	assertSchema(t, db)
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openSQLite(t)
	ctx := t.Context()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate on an up-to-date database must be a no-op, got: %v", err)
	}
}

func TestMigrationStatusReportsAppliedState(t *testing.T) {
	db := openSQLite(t)
	ctx := t.Context()

	before, err := db.MigrationStatus(ctx)
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	for _, m := range before {
		if m.Applied {
			t.Errorf("migration %d reported applied on a fresh database", m.Version)
		}
	}

	if merr := db.Migrate(ctx); merr != nil {
		t.Fatalf("Migrate: %v", merr)
	}

	after, err := db.MigrationStatus(ctx)
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("migration count changed: %d then %d", len(before), len(after))
	}
	for _, m := range after {
		if !m.Applied {
			t.Errorf("migration %d still reported unapplied after Migrate", m.Version)
		}
	}
}

// TestMigrationSetsAreInLockstep guards the one hazard of keeping a migration
// set per dialect: the sets drifting apart, so a schema change lands on
// Postgres and not SQLite, or the same version number means two different
// things.
func TestMigrationSetsAreInLockstep(t *testing.T) {
	sqlite := migrationFilenames(t, config.DriverSQLite)
	postgres := migrationFilenames(t, config.DriverPostgres)

	if len(sqlite) == 0 {
		t.Fatal("no SQLite migrations found")
	}
	if len(sqlite) != len(postgres) {
		t.Fatalf("migration sets differ in size: sqlite has %d %v, postgres has %d %v",
			len(sqlite), sqlite, len(postgres), postgres)
	}
	for i := range sqlite {
		if sqlite[i] != postgres[i] {
			t.Errorf("migration %d differs: sqlite has %q, postgres has %q - "+
				"the two sets must stay in lockstep", i, sqlite[i], postgres[i])
		}
	}
}

func TestOpenPostgres(t *testing.T) {
	db := openPostgres(t)
	ctx := t.Context()

	if db.IsSQLite() {
		t.Error("IsSQLite() = true for a Postgres handle")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	assertSchema(t, db)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate must be a no-op, got: %v", err)
	}
}

// TestReplicasMigrateTogether is a service's tasks starting at once against a
// database with a migration pending. Without the migration lock one of them
// applies a migration the other is already applying and exits on a duplicate
// key in the catalogue, which is how two Fargate tasks met their first start.
func TestReplicasMigrateTogether(t *testing.T) {
	first := openPostgres(t)

	// The version table has to exist already, as it does on any deployment
	// that has started once. On an empty database goose's own retry while
	// creating that table staggers the replicas by a second, which hides the
	// race this test is for.
	if _, err := first.PendingMigrations(t.Context()); err != nil {
		t.Fatalf("PendingMigrations: %v", err)
	}

	const replicas = 4
	dbs := []*DB{first}
	for range replicas - 1 {
		db, err := Open(t.Context(), config.Database{
			Driver:       config.DriverPostgres,
			DSN:          os.Getenv(testDBEnv),
			MaxOpenConns: 5,
		})
		if err != nil {
			t.Fatalf("Open postgres: %v", err)
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		})
		dbs = append(dbs, db)
	}

	start := make(chan struct{})
	errs := make(chan error, replicas)
	for _, db := range dbs {
		go func() {
			<-start
			errs <- db.Migrate(t.Context())
		}()
	}
	close(start)
	for range replicas {
		if err := <-errs; err != nil {
			t.Errorf("a replica failed to migrate: %v", err)
		}
	}

	assertSchema(t, first)
}

// eachDialect runs a test body against both supported databases.
//
// It exists because the queries that differ between them are exactly the ones
// most likely to be wrong: the upsert that resolves price precedence, the
// keyset pagination that compares timestamps, and the date extraction in the
// usage summary, which has no portable spelling. Testing those on SQLite alone
// would prove nothing about the production target.
func eachDialect(t *testing.T, fn func(t *testing.T, db *DB)) {
	t.Helper()

	t.Run("sqlite", func(t *testing.T) {
		fn(t, migratedSQLite(t))
	})
	t.Run("postgres", func(t *testing.T) {
		db := openPostgres(t) // skips unless a DSN is configured
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		fn(t, db)
	})
}

// assertSchema checks that the tables the identity and audit foundation
// promises actually exist and are queryable, on whichever dialect is in play.
func assertSchema(t *testing.T, db *DB) {
	t.Helper()

	for _, table := range []string{
		"users", "teams", "team_members", "gateway_tokens", "settings", "audit_events",
		"requests", "price_table", "budgets",
		"sessions", "work_items", "attributions",
		"work_item_links", "integrations",
		"console_sessions", "oidc_flows", "oidc_group_roles",
		"provider_models", "model_routes", "providers",
		"cli_authorizations", "cli_grants", "service_tokens", "web_search", "provider_failures",
	} {
		var n int
		// The table names come from this test's own literal list.
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Errorf("table %s is not queryable after migration: %v", table, err)
			continue
		}
		if n != 0 {
			t.Errorf("table %s has %d rows on a fresh database, want 0", table, n)
		}
	}
}

func migrationFilenames(t *testing.T, driver string) []string {
	t.Helper()

	entries, err := migrationsFS.ReadDir(migrationDir(driver))
	if err != nil {
		t.Fatalf("read %s migrations: %v", driver, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func openSQLite(t *testing.T) *DB {
	t.Helper()

	dsn := sqliteDSN(filepath.Join(t.TempDir(), "panorama.db"))

	db, err := Open(t.Context(), config.Database{
		Driver:       config.DriverSQLite,
		DSN:          dsn,
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

func openPostgres(t *testing.T) *DB {
	t.Helper()

	dsn := os.Getenv(testDBEnv)
	if dsn == "" {
		t.Skipf("set %s to run the Postgres tests", testDBEnv)
	}

	db, err := Open(t.Context(), config.Database{
		Driver:       config.DriverPostgres,
		DSN:          dsn,
		MaxOpenConns: 5,
	})
	if err != nil {
		t.Fatalf("Open postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	// Each run starts from an empty schema, so a failed run cannot leave the
	// next one migrating on top of half-applied state.
	if _, err := db.ExecContext(t.Context(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	return db
}
