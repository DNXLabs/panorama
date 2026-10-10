// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/dnxlabs/panorama/internal/store"
)

// sweepInterval is how often expired console sessions and abandoned sign-ins
// are removed.
const sweepInterval = time.Hour

// deviceGrace is how long an expired device authorization is kept before it is
// removed, so a CLI polling one that has just lapsed gets the answer that tells
// it what to do rather than the one that says the code was never real.
const deviceGrace = time.Hour

// Sweeper removes rows that have outlived their purpose.
//
// Housekeeping rather than security. Expiry is enforced on read, in the query
// that authenticates a session and the one that consumes a sign-in, so a row
// this has not got to yet cannot be used. What it prevents is tables growing
// forever in a deployment nobody restarts.
//
// Device authorizations need it more than the rest: a gateway token is minted
// when somebody asks for one, but an abandoned `panorama login` leaves a row
// behind every time, and the CLI retries.
type Sweeper struct {
	db  *store.DB
	log *slog.Logger
}

// NewSweeper builds a sweeper.
func NewSweeper(db *store.DB, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sweeper{db: db, log: log}
}

// Run sweeps until the context is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()

			removed, err := s.db.PurgeExpiredSessions(ctx, now)
			if err != nil {
				s.log.ErrorContext(ctx, "sweeping expired sessions", "error", err)
			} else if removed > 0 {
				s.log.DebugContext(ctx, "swept expired sessions", "rows", removed)
			}

			// Kept a while past expiry rather than removed on the dot, so that
			// a CLI still polling a flow that has just lapsed is told
			// expired_token - the answer that tells it to run login again -
			// instead of being answered as an unknown code.
			devices, err := s.db.PurgeExpiredDeviceAuthorizations(ctx, now.Add(-deviceGrace))
			if err != nil {
				s.log.ErrorContext(ctx, "sweeping device authorizations", "error", err)
			} else if devices > 0 {
				s.log.DebugContext(ctx, "swept device authorizations", "rows", devices)
			}
		}
	}
}
