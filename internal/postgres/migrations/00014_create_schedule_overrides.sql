-- +goose Up
-- Overrides: runtime state with which someone covers a schedule for a
-- period without a change to the configuration. See docs/schedules.md.
CREATE TABLE schedule_overrides (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    schedule_id      uuid        NOT NULL REFERENCES schedules (id),
    -- Who is on call during the period.
    user_id          uuid        NOT NULL REFERENCES users (id),
    starts_at        timestamptz NOT NULL,
    ends_at          timestamptz NOT NULL,
    -- The author: the user who created it, or the API token it was created
    -- with. Exactly one is set.
    created_by       uuid        REFERENCES users (id),
    created_by_token uuid        REFERENCES api_tokens (id),
    reason           text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL,
    -- Set when the override is removed. One that had begun ends then.
    cancelled_at     timestamptz,

    CONSTRAINT schedule_overrides_period_check CHECK (ends_at > starts_at),
    CONSTRAINT schedule_overrides_author_check CHECK (num_nonnulls(created_by, created_by_token) = 1)
);
-- The overrides of a schedule that are in force or still to come.
CREATE INDEX schedule_overrides_schedule_idx ON schedule_overrides (schedule_id, ends_at) WHERE cancelled_at IS NULL;

-- An override that begins is recorded as a shift of kind "override", beside
-- the rotation shift it covers.
ALTER TABLE shifts
    ADD COLUMN override_id uuid REFERENCES schedule_overrides (id),
    ADD CONSTRAINT shifts_override_check CHECK ((kind = 'override') = (override_id IS NOT NULL));
-- An override is recorded once.
CREATE UNIQUE INDEX shifts_override_key ON shifts (override_id) WHERE override_id IS NOT NULL;

-- The override of a shift is as final as the rest of it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION shifts_are_final() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.ends_at IS NOT NULL THEN
        RAISE EXCEPTION 'shift % is closed, and a closed shift is never changed', OLD.id;
    END IF;
    IF NEW.id <> OLD.id OR NEW.schedule_id <> OLD.schedule_id OR NEW.user_id <> OLD.user_id
        OR NEW.starts_at <> OLD.starts_at OR NEW.kind <> OLD.kind
        OR NEW.override_id IS DISTINCT FROM OLD.override_id THEN
        RAISE EXCEPTION 'only the end of shift % can be set', OLD.id;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION shifts_are_final() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.ends_at IS NOT NULL THEN
        RAISE EXCEPTION 'shift % is closed, and a closed shift is never changed', OLD.id;
    END IF;
    IF NEW.id <> OLD.id OR NEW.schedule_id <> OLD.schedule_id OR NEW.user_id <> OLD.user_id
        OR NEW.starts_at <> OLD.starts_at OR NEW.kind <> OLD.kind THEN
        RAISE EXCEPTION 'only the end of shift % can be set', OLD.id;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
DROP INDEX shifts_override_key;
ALTER TABLE shifts
    DROP CONSTRAINT shifts_override_check,
    DROP COLUMN override_id;
DROP TABLE schedule_overrides;
