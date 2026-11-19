-- +goose Up
-- Where each schedule is in its rotation, and the shifts that have happened.
-- The files state the intent; these tables hold the position. See
-- docs/configuration-and-data-model.md and docs/schedules.md.

-- One row per schedule that is in the configuration.
CREATE TABLE schedule_positions (
    schedule_id      uuid        PRIMARY KEY REFERENCES schedules (id),
    -- Who is on call under the rotation. Null while no member of the
    -- rotation is a user yet.
    user_id          uuid        REFERENCES users (id),
    shift_started_at timestamptz NOT NULL,
    -- The timer of the rotation: workers take due rows with SKIP LOCKED.
    next_handoff_at  timestamptz NOT NULL,
    -- The rotation in force, which may be older than the one in the files.
    definition       jsonb       NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX schedule_positions_due_idx ON schedule_positions (next_handoff_at);

-- Every shift, written when it begins and closed when it ends.
CREATE TABLE shifts (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    schedule_id uuid        NOT NULL REFERENCES schedules (id),
    user_id     uuid        NOT NULL REFERENCES users (id),
    starts_at   timestamptz NOT NULL,
    ends_at     timestamptz,
    kind        text        NOT NULL DEFAULT 'rotation',

    CONSTRAINT shifts_kind_check CHECK (kind IN ('rotation', 'override')),
    CONSTRAINT shifts_order_check CHECK (ends_at IS NULL OR ends_at >= starts_at)
);
-- A schedule has at most one rotation shift in progress.
CREATE UNIQUE INDEX shifts_open_rotation_key ON shifts (schedule_id) WHERE ends_at IS NULL AND kind = 'rotation';
CREATE INDEX shifts_schedule_idx ON shifts (schedule_id, starts_at);

-- A shift is a record of what happened. The only change it ever sees is its
-- end being set, once; the database refuses anything else, whatever the code
-- above it does.
-- +goose StatementBegin
CREATE FUNCTION shifts_are_final() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE TRIGGER shifts_final BEFORE UPDATE ON shifts FOR EACH ROW EXECUTE FUNCTION shifts_are_final();

-- +goose Down
DROP TABLE shifts;
DROP FUNCTION shifts_are_final();
DROP TABLE schedule_positions;
