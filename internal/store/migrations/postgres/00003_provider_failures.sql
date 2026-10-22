-- SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- The latest request each provider refused to serve, in the provider's own
-- words, for Settings, Model providers. Kept in step with
-- migrations/sqlite/00003_provider_failures.sql, which says why it exists.
CREATE TABLE provider_failures (
    kind TEXT PRIMARY KEY CHECK (kind IN ('bedrock', 'bedrock-mantle')),
    region TEXT NOT NULL,
    model TEXT NOT NULL,
    error_kind TEXT NOT NULL,
    message TEXT NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    failed_at TIMESTAMPTZ NOT NULL
);

-- +goose Down

DROP TABLE provider_failures;
