-- +goose Up
-- What a group needs so that silences can keep it quiet. See
-- docs/configuration-and-data-model.md.
ALTER TABLE alert_groups
    -- The alerts that a notification about the group has shown, each with
    -- the state it was shown in: "open" or "resolved". It tells what is news,
    -- and which alerts people know of although a silence matches them now.
    ADD COLUMN notified_alerts jsonb NOT NULL DEFAULT '{}',
    -- The silence that the group's next notification is waiting for: the
    -- one, among those that keep its alerts quiet, that ends first.
    ADD COLUMN silence_id      uuid  REFERENCES silences (id);

-- The groups that exist were told about with every alert they have.
UPDATE alert_groups g SET notified_alerts = COALESCE((
    SELECT jsonb_object_agg(a.id::text, CASE WHEN a.status = 'resolved' THEN 'resolved' ELSE 'open' END)
    FROM alerts a WHERE a.group_id = g.id), '{}')
WHERE g.notified_at IS NOT NULL;

-- Expiring a silence looks for the groups that wait for it.
CREATE INDEX alert_groups_silence_id_idx ON alert_groups (silence_id) WHERE silence_id IS NOT NULL;

-- +goose Down
DROP INDEX alert_groups_silence_id_idx;
ALTER TABLE alert_groups
    DROP COLUMN silence_id,
    DROP COLUMN notified_alerts;
