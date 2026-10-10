-- SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
-- SPDX-License-Identifier: AGPL-3.0-only

-- Moves a Postgres database migrated by the old chain (versions 1 to 27) onto
-- the single baseline, 00001_initial.sql. Run once, by an operator, with the
-- old release stopped and before the first start of a release carrying the
-- baseline. See README.md beside this file.
--
-- The baseline is the old chain's schema less two things nothing writes any
-- more: the `routed` decision and the `fallback_from` column, both left from
-- the fallback chain a route no longer has. So this does those two changes,
-- then rewrites goose's record of what ran so that version 1 is the only
-- migration applied. Everything happens in one transaction: either the
-- database is on the baseline afterwards or it is exactly as it was.
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f rebaseline.sql

BEGIN;

-- Refuse anything that is not exactly the end of the old chain. A database at
-- an earlier version is missing tables the baseline has, and one already
-- rebaselined has nothing to do; both would be left disagreeing with what
-- goose believes about them.
DO $$
DECLARE
    latest bigint;
BEGIN
    SELECT max(version_id) INTO latest FROM goose_db_version WHERE is_applied;
    IF latest IS DISTINCT FROM 27 THEN
        RAISE EXCEPTION 'rebaseline: this database is at migration %, and this script only moves a database at 27. Nothing was changed.', latest;
    END IF;
END
$$;

-- A row recorded by an older release that named a model it was moved onto.
-- It was served, which is what `allowed` says.
UPDATE requests SET decision = 'allowed' WHERE decision = 'routed';

ALTER TABLE requests DROP CONSTRAINT requests_decision_check;
ALTER TABLE requests ADD CONSTRAINT requests_decision_check
    CHECK (decision IN ('allowed', 'refused', 'blocked', 'error'));

ALTER TABLE requests DROP COLUMN fallback_from;

-- goose's record: version 0 is the row goose writes when it creates the
-- table, and version 1 is the baseline, which this database now matches.
DELETE FROM goose_db_version;
INSERT INTO goose_db_version (version_id, is_applied) VALUES (0, true), (1, true);

COMMIT;
