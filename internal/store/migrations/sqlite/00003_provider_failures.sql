-- SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
-- SPDX-License-Identifier: AGPL-3.0-only

-- +goose Up

-- The latest request each provider refused to serve, in the provider's own
-- words, for Settings, Model providers.
--
-- Bedrock's reason for refusing a model - Anthropic's use case form not
-- submitted, a model AWS does not offer this account, an SCP - is something an
-- engineer cannot act on and must not be shown, since it names the account and
-- its roles. Without this row it reached the log and nowhere else, so the
-- administrator who could fix it saw only a generic 502 relayed by somebody
-- else. Only refusals (not found, not permitted) are kept: their text is
-- AWS's, and never quotes a request body.
--
-- One row per provider, replaced by the next refusal: it answers "why is
-- this failing now", and the audit trail already counts how often.
-- Deliberately not a column of providers, whose writes reload the policy
-- snapshot; a refused request is not a configuration change.
CREATE TABLE provider_failures (
    kind TEXT PRIMARY KEY CHECK (kind IN ('bedrock', 'bedrock-mantle')),
    -- Where and on what: a refusal is a statement about one model in one
    -- region, and the region may have changed since.
    region TEXT NOT NULL,
    model TEXT NOT NULL,
    -- The coarse class the data plane recorded: not_found or forbidden.
    error_kind TEXT NOT NULL,
    -- AWS's error code and message, truncated.
    message TEXT NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    failed_at TIMESTAMP NOT NULL
);

-- +goose Down

DROP TABLE provider_failures;
