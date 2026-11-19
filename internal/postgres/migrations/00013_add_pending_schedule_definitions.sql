-- +goose Up
-- A changed rotation that has not taken over yet. The rotation in force
-- keeps running until it does, so a change in the files never touches the
-- shift in progress. See docs/schedules.md.
ALTER TABLE schedule_positions
    ADD COLUMN pending_definition   jsonb,
    -- When the pending rotation takes over. Null means at the next handoff.
    ADD COLUMN pending_effective_at timestamptz,
    ADD CONSTRAINT schedule_positions_pending_check CHECK (pending_definition IS NOT NULL OR pending_effective_at IS NULL);

-- +goose Down
ALTER TABLE schedule_positions
    DROP CONSTRAINT schedule_positions_pending_check,
    DROP COLUMN pending_effective_at,
    DROP COLUMN pending_definition;
