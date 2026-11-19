-- +goose Up
-- Overrides of one schedule may overlap. Where they do, the one created
-- last is in force. The order of creation is kept in a column of its own,
-- because two overrides can be created within the resolution of a clock.
-- See docs/schedules.md.
ALTER TABLE schedule_overrides ADD COLUMN layer bigint GENERATED ALWAYS AS IDENTITY;
CREATE UNIQUE INDEX schedule_overrides_layer_key ON schedule_overrides (layer);

-- +goose Down
DROP INDEX schedule_overrides_layer_key;
ALTER TABLE schedule_overrides DROP COLUMN layer;
