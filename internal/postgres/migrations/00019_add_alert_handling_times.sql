-- +goose Up
-- Two moments of an alert that the metrics of alert handling are measured
-- between, kept on the row so that each is observed once, whichever replica
-- makes the change and however often the alert changes afterwards. See
-- docs/metrics.md.
ALTER TABLE alerts
    -- When Beacon opened the alert. started_at is when the source says the
    -- problem began, which may be long before Beacon heard of it.
    ADD COLUMN opened_at         timestamptz,
    -- When a person first acknowledged or snoozed the alert. It is never
    -- cleared: a snooze that runs out makes the alert fire again, and the
    -- first response stays the first.
    ADD COLUMN first_response_at timestamptz;

-- The alerts that exist were opened no later than Beacon last heard of
-- them, and those that are acknowledged now have had a response.
UPDATE alerts SET opened_at = LEAST(started_at, last_received_at), first_response_at = acknowledged_at;

ALTER TABLE alerts
    ALTER COLUMN opened_at SET NOT NULL,
    ALTER COLUMN opened_at SET DEFAULT now();

-- +goose Down
ALTER TABLE alerts
    DROP COLUMN first_response_at,
    DROP COLUMN opened_at;
