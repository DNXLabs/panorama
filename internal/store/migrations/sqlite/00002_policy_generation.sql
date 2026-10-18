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
-- added to the snapshot needs its triggers here too.
--
-- SQLite triggers fire once per row, so one statement touching three routes
-- moves the counter by three. Only a change matters, not its size.

-- +goose Up

CREATE TABLE policy_generation (
    -- One row, ever.
    id INTEGER PRIMARY KEY CHECK (id = 1),
    generation INTEGER NOT NULL
);

INSERT INTO policy_generation (id, generation) VALUES (1, 0);

-- +goose StatementBegin
CREATE TRIGGER model_routes_insert_moves_policy AFTER INSERT ON model_routes
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER model_routes_update_moves_policy AFTER UPDATE ON model_routes
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER model_routes_delete_moves_policy AFTER DELETE ON model_routes
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER budgets_insert_moves_policy AFTER INSERT ON budgets
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER budgets_update_moves_policy AFTER UPDATE ON budgets
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER budgets_delete_moves_policy AFTER DELETE ON budgets
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER team_members_insert_moves_policy AFTER INSERT ON team_members
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER team_members_update_moves_policy AFTER UPDATE ON team_members
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER team_members_delete_moves_policy AFTER DELETE ON team_members
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER providers_insert_moves_policy AFTER INSERT ON providers
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER providers_update_moves_policy AFTER UPDATE ON providers
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER providers_delete_moves_policy AFTER DELETE ON providers
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER cost_centers_insert_moves_policy AFTER INSERT ON cost_centers
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER cost_centers_update_moves_policy AFTER UPDATE ON cost_centers
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER cost_centers_delete_moves_policy AFTER DELETE ON cost_centers
BEGIN UPDATE policy_generation SET generation = generation + 1; END;
-- +goose StatementEnd

-- +goose Down

DROP TRIGGER cost_centers_delete_moves_policy;
DROP TRIGGER cost_centers_update_moves_policy;
DROP TRIGGER cost_centers_insert_moves_policy;
DROP TRIGGER providers_delete_moves_policy;
DROP TRIGGER providers_update_moves_policy;
DROP TRIGGER providers_insert_moves_policy;
DROP TRIGGER team_members_delete_moves_policy;
DROP TRIGGER team_members_update_moves_policy;
DROP TRIGGER team_members_insert_moves_policy;
DROP TRIGGER budgets_delete_moves_policy;
DROP TRIGGER budgets_update_moves_policy;
DROP TRIGGER budgets_insert_moves_policy;
DROP TRIGGER model_routes_delete_moves_policy;
DROP TRIGGER model_routes_update_moves_policy;
DROP TRIGGER model_routes_insert_moves_policy;
DROP TABLE policy_generation;
