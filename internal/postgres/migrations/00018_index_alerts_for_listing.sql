-- +goose Up
-- The alerts API lists alerts newest first, a page at a time, and continues
-- a list from the alert a page ended with. See docs/api.md.
CREATE INDEX alerts_started_at_idx ON alerts (started_at DESC, id DESC);
-- The filter that every view of open alerts uses.
CREATE INDEX alerts_open_started_at_idx ON alerts (started_at DESC, id DESC) WHERE status <> 'resolved';

-- +goose Down
DROP INDEX alerts_open_started_at_idx;
DROP INDEX alerts_started_at_idx;
