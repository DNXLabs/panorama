// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/dnxlabs/panorama/internal/config"
)

// migrationsFS holds one migration set per dialect. The two sets express the
// same logical schema; they are kept separate so the SQL each database runs is
// the SQL committed to the repository, with no translation layer in between.
// Their versions must stay in lockstep - TestMigrationSetsAreInLockstep
// enforces that.
//
//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationsFS embed.FS

// gooseDialect maps a configured driver to goose's dialect name.
func gooseDialect(driver string) (string, error) {
	switch driver {
	case config.DriverSQLite:
		return "sqlite3", nil
	case config.DriverPostgres:
		return "postgres", nil
	default:
		return "", fmt.Errorf("store: no migration dialect for driver %q", driver)
	}
}

// migrationDir returns the embedded directory holding this driver's migrations.
func migrationDir(driver string) string {
	return "migrations/" + driver
}

// provider builds a goose provider bound to this handle's dialect and
// migration set.
func (db *DB) provider(opts ...goose.ProviderOption) (*goose.Provider, error) {
	dialect, err := gooseDialect(db.driver)
	if err != nil {
		return nil, err
	}

	sub, err := fs.Sub(migrationsFS, migrationDir(db.driver))
	if err != nil {
		return nil, fmt.Errorf("store: locate %s migrations: %w", db.driver, err)
	}

	p, err := goose.NewProvider(goose.Dialect(dialect), db.DB, sub, opts...)
	if err != nil {
		return nil, fmt.Errorf("store: build %s migration provider: %w", db.driver, err)
	}
	return p, nil
}

// Migrate applies every pending migration. It is safe to call on a database
// that is already current, in which case it does nothing.
//
// On Postgres it holds a session advisory lock while it does, because every
// task of a service starts at once: without the lock two replicas apply the
// same migration side by side and one dies on a duplicate key in the catalogue.
// The one that waits finds the schema current. Only Migrate locks: status is
// read by /readyz, which must answer "not yet" rather than queue behind a
// migration. SQLite has one process and one connection, so nothing to race.
//
// The lock is asked for every second for up to five minutes: goose's own
// ceiling, polled more often than its five-second default, so a replica
// waiting behind a short migration starts a second late rather than five.
func (db *DB) Migrate(ctx context.Context) error {
	var opts []goose.ProviderOption
	if db.driver == config.DriverPostgres {
		locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
		if err != nil {
			return fmt.Errorf("store: build the migration lock: %w", err)
		}
		opts = append(opts, goose.WithSessionLocker(locker))
	}

	p, err := db.provider(opts...)
	if err != nil {
		return err
	}

	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate %s: %w", db.driver, err)
	}

	for _, r := range results {
		if r.Error != nil {
			return fmt.Errorf("store: migration %d (%s) failed: %w", r.Source.Version, r.Source.Path, r.Error)
		}
	}
	return nil
}

// MigrationState describes one migration and whether it has been applied.
type MigrationState struct {
	Version int64  `json:"version"`
	Source  string `json:"source"`
	Applied bool   `json:"applied"`
}

// MigrationStatus reports every known migration and its state, for the
// `panorama migrate status` command and for readiness reporting.
func (db *DB) MigrationStatus(ctx context.Context) ([]MigrationState, error) {
	p, err := db.provider()
	if err != nil {
		return nil, err
	}

	statuses, err := p.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: migration status for %s: %w", db.driver, err)
	}

	out := make([]MigrationState, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, MigrationState{
			Version: s.Source.Version,
			Source:  s.Source.Path,
			Applied: s.State == goose.StateApplied,
		})
	}
	return out, nil
}

// PendingMigrations returns the migrations that have not yet been applied.
func (db *DB) PendingMigrations(ctx context.Context) ([]MigrationState, error) {
	all, err := db.MigrationStatus(ctx)
	if err != nil {
		return nil, err
	}

	var pending []MigrationState
	for _, m := range all {
		if !m.Applied {
			pending = append(pending, m)
		}
	}
	return pending, nil
}
