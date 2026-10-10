// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package jobs holds the background workers: work-item confirmation against
// GitHub, model discovery, price-table refresh, and the session sweep.
//
// Work runs on intervals in this process, so a deployment ships as one binary
// with no broker to operate and no database requirement beyond the one it
// already has. River - an in-process, Postgres-backed queue - is where retries,
// scheduling and fan-out belong once there is any; see jobs.go for why there is
// not yet. Background work therefore runs on SQLite as well as Postgres.
//
// Anything that would slow the request path belongs here. In particular, the
// asynchronous attribution passes operate on metadata that in-flight matching
// already extracted - they never re-read a request body, because no body was
// kept.
package jobs
