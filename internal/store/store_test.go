// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
		"cli_authorizations", "cli_grants", "service_tokens", "web_search",
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

// spentVersions are migration numbers that were issued and then withdrawn.
//
// goose records that version 12 was applied; it does not record which file did
// it. So a database that ran the deployment while a since-deleted migration
// existed will skip any later migration reusing that number - silently, with
// `migrate status` reporting the new file as applied and the tables it should
// have created simply absent. The first symptom is a query failing against a
// schema the tool calls current.
//
// A version number is append-only. Withdrawing a migration does not return its
// number to the pool, so this list only ever grows. It is empty because the
// chain was squashed into 00001_initial.sql and every database that predates
// the squash has its record rewritten by scripts/rebaseline, which leaves no
// number behind it spent.
var spentVersions = map[int]string{}

// TestNoMigrationReusesAWithdrawnVersion.
//
// The failure this prevents cost a crash-looping local deployment to find, and
// nothing else would have caught it: a fresh database has no spent numbers, so
// every other migration test passes.
func TestNoMigrationReusesAWithdrawnVersion(t *testing.T) {
	for _, driver := range []string{config.DriverSQLite, config.DriverPostgres} {
		for _, name := range migrationFilenames(t, driver) {
			digits, _, ok := strings.Cut(name, "_")
			if !ok {
				t.Errorf("%s/%s is not named <version>_<description>.sql", driver, name)
				continue
			}
			version, err := strconv.Atoi(digits)
			if err != nil {
				t.Errorf("%s/%s has no version number: %v", driver, name, err)
				continue
			}
			if withdrawn, spent := spentVersions[version]; spent {
				t.Errorf("%s/%s reuses version %d, which was spent by %s - "+
					"every database that applied it will skip this migration; renumber it",
					driver, name, version, withdrawn)
			}
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
