// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package store owns persistent storage: connection setup and schema
// migrations for both supported databases.
//
// SQLite is the zero-dependency default and Postgres is the production target.
// They share one logical schema, expressed as a separate migration set per
// dialect rather than one set run through a translation layer - the SQL each
// database actually executes is the SQL in the repository.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	// Pure-Go SQLite. No cgo, so the binary stays a single static artifact.
	_ "modernc.org/sqlite"

	// Postgres via pgx's database/sql adapter. River, if it is ever added,
	// wants pgx as well.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/dnxlabs/panorama/internal/config"
)

// SQL driver names, which differ from the configuration's driver names.
const (
	sqlDriverSQLite   = "sqlite"
	sqlDriverPostgres = "pgx"
)

// DB is a handle to Panorama's database. It embeds *sql.DB, so callers use the
// standard library API directly; the wrapper exists to carry the dialect, which
// migrations and a few dialect-specific queries need.
type DB struct {
	*sql.DB

	driver string
}

// Driver reports the configured driver: config.DriverSQLite or
// config.DriverPostgres.
func (db *DB) Driver() string { return db.driver }

// rebind converts the `?` placeholders used throughout this package into the
// `$N` form Postgres expects. Queries are written once, in the portable form,
// rather than duplicated per dialect.
//
// It walks the query rather than using a regular expression because a `?`
// inside a string literal must not be rewritten.
func (db *DB) rebind(query string) string {
	if db.driver != config.DriverPostgres {
		return query
	}

	var (
		out    strings.Builder
		n      int
		inQuot bool
	)
	out.Grow(len(query) + 8)

	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			// Doubled quotes are an escaped quote inside a literal, not a
			// close followed by an open.
			if inQuot && i+1 < len(query) && query[i+1] == '\'' {
				out.WriteString("''")
				i++
				continue
			}
			inQuot = !inQuot
			out.WriteByte(c)
		case c == '?' && !inQuot:
			n++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(n))
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// IsSQLite reports whether this handle is backed by SQLite.
func (db *DB) IsSQLite() bool { return db.driver == config.DriverSQLite }

// Open connects to the configured database and verifies the connection.
//
// It does not run migrations; call Migrate for that, so that an operator
// running multiple replicas can migrate as a separate, single step.
func Open(ctx context.Context, cfg config.Database) (*DB, error) {
	var driverName string

	// An allowlist, not a default case: an unrecognised driver is a
	// configuration error, not something to guess at.
	switch cfg.Driver {
	case config.DriverSQLite:
		driverName = sqlDriverSQLite
	case config.DriverPostgres:
		driverName = sqlDriverPostgres
	default:
		return nil, fmt.Errorf("store: unsupported driver %q (want %s or %s)",
			cfg.Driver, config.DriverSQLite, config.DriverPostgres)
	}

	sqlDB, err := sql.Open(driverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", cfg.Driver, err)
	}

	if cfg.Driver == config.DriverSQLite {
		// SQLite permits one writer at a time. Holding the pool to a single
		// connection trades read concurrency for the absence of SQLITE_BUSY
		// retries, which is the right trade while SQLite is the evaluation
		// path and Postgres is the production one. Revisit with a split
		// reader/writer pool if SQLite ever carries real request volume.
		sqlDB.SetMaxOpenConns(1)
	} else {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
		sqlDB.SetMaxIdleConns(cfg.MaxOpenConns)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
		sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	}

	// sql.Open is lazy. Fail here, at startup, rather than on the first request.
	if err := sqlDB.PingContext(ctx); err != nil {
		// Close the handle we are about to drop; ignoring the error here would
		// hide nothing useful, but leaking the pool would.
		if cerr := sqlDB.Close(); cerr != nil {
			return nil, fmt.Errorf("store: ping %s: %w (and closing the handle failed: %w)", cfg.Driver, err, cerr)
		}
		return nil, fmt.Errorf("store: ping %s: %w", cfg.Driver, err)
	}

	return &DB{DB: sqlDB, driver: cfg.Driver}, nil
}
