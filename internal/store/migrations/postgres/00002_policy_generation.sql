-- SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
-- SPDX-License-Identifier: AGPL-3.0-only

-- A counter every write to the policy's tables moves, so a running gateway can
-- tell that a route was saved by another process - `panorama policy apply` in a
-- one-off task, or the console on another replica - by reading one row a
-- second, rather than learning it at the next full reload half a minute later.
--
-- Moved by triggers rather than by the code that writes, because there are a
-- dozen write paths and the one that forgot would be the one whose change
-- appears to have done nothing. The tables are exactly the ones
-- store.LoadPolicy reads: model_routes, budgets, team_members, providers and
-- cost_centers. Nothing written on the request path is among them; a table
-- added to the snapshot needs its trigger here too.
--
-- Statement-level, so one statement touching three routes moves the counter
-- once. The row lock the update takes serialises policy writes against each
-- other until they commit, which is a cost paid by a screen somebody saves,
-- never by a request.

-- +goose Up

CREATE TABLE policy_generation (
    -- One row, ever.
    id INTEGER PRIMARY KEY CHECK (id = 1),
    generation BIGINT NOT NULL
);

INSERT INTO policy_generation (id, generation) VALUES (1, 0);

-- +goose StatementBegin
CREATE FUNCTION move_policy_generation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE policy_generation SET generation = generation + 1;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER model_routes_moves_policy AFTER INSERT OR UPDATE OR DELETE ON model_routes
    FOR EACH STATEMENT EXECUTE FUNCTION move_policy_generation();
CREATE TRIGGER budgets_moves_policy AFTER INSERT OR UPDATE OR DELETE ON budgets
    FOR EACH STATEMENT EXECUTE FUNCTION move_policy_generation();
CREATE TRIGGER team_members_moves_policy AFTER INSERT OR UPDATE OR DELETE ON team_members
    FOR EACH STATEMENT EXECUTE FUNCTION move_policy_generation();
CREATE TRIGGER providers_moves_policy AFTER INSERT OR UPDATE OR DELETE ON providers
    FOR EACH STATEMENT EXECUTE FUNCTION move_policy_generation();
CREATE TRIGGER cost_centers_moves_policy AFTER INSERT OR UPDATE OR DELETE ON cost_centers
    FOR EACH STATEMENT EXECUTE FUNCTION move_policy_generation();

-- +goose Down

DROP TRIGGER cost_centers_moves_policy ON cost_centers;
DROP TRIGGER providers_moves_policy ON providers;
DROP TRIGGER team_members_moves_policy ON team_members;
DROP TRIGGER budgets_moves_policy ON budgets;
DROP TRIGGER model_routes_moves_policy ON model_routes;
DROP FUNCTION move_policy_generation();
DROP TABLE policy_generation;
