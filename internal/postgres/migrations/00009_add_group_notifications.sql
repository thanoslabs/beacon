-- +goose Up
-- Grouping: a group holds the alerts that share the values of its group_by
-- labels, and notifications are sent for the group, not for each alert. The
-- timers that decide when live here, in Postgres, so that they survive a
-- restart and fire once when several replicas run. See
-- docs/configuration-and-data-model.md.
ALTER TABLE alert_groups
    -- The label values that define the group, for display.
    ADD COLUMN labels          jsonb       NOT NULL DEFAULT '{}',
    -- The least time between two notifications caused by the sources: the
    -- group_interval that applied when the group was opened.
    ADD COLUMN update_interval interval    NOT NULL DEFAULT '5 minutes',
    -- When the next notification is due. Null when none is.
    ADD COLUMN notify_at       timestamptz,
    -- When the last notification was queued. Null until the first.
    ADD COLUMN notified_at     timestamptz;

-- Until now every alert notified the moment it fired, so the groups that
-- exist have been notified.
UPDATE alert_groups SET notified_at = created_at;

CREATE INDEX alert_groups_notify_at_idx ON alert_groups (notify_at) WHERE notify_at IS NOT NULL;

-- +goose Down
DROP INDEX alert_groups_notify_at_idx;
ALTER TABLE alert_groups
    DROP COLUMN notified_at,
    DROP COLUMN notify_at,
    DROP COLUMN update_interval,
    DROP COLUMN labels;
