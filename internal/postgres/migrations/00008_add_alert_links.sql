-- +goose Up
-- Links that a source sends with an alert: where it was generated and, for
-- Grafana, its dashboard and panel. A map of link name to URL. See
-- docs/configuration-and-data-model.md.
ALTER TABLE alerts ADD COLUMN links jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE alerts DROP COLUMN links;
