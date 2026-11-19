-- +goose Up
-- Where each alert group is in its escalation policy. One row per group
-- that has been escalated. See docs/escalation.md.
CREATE TABLE escalations (
    alert_group_id       uuid        PRIMARY KEY REFERENCES alert_groups (id),
    escalation_policy_id uuid        NOT NULL REFERENCES escalation_policies (id),
    -- active: waiting for the next step. acknowledged: no alert of the group
    -- is firing, so nothing more is sent unless one fires again. exhausted:
    -- every step of every pass ran and nobody responded. resolved: the group
    -- is resolved.
    status               text        NOT NULL,
    -- The step that ran last in the pass in progress, counted from 0, and
    -- -1 before the first step of an escalation that starts or starts again.
    step_index           integer     NOT NULL DEFAULT -1,
    -- How many times all steps have run before the pass in progress.
    pass                 integer     NOT NULL DEFAULT 0,
    -- The timer: when the next step is due. Workers take due rows with
    -- SKIP LOCKED. Null exactly when the escalation is not active.
    next_step_at         timestamptz,
    -- The silence whose end the next step waits for, when every firing
    -- alert of the group is silenced.
    silence_id           uuid        REFERENCES silences (id),
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,

    CONSTRAINT escalations_status_check CHECK (status IN ('active', 'acknowledged', 'exhausted', 'resolved')),
    CONSTRAINT escalations_timer_check CHECK ((status = 'active') = (next_step_at IS NOT NULL)),
    CONSTRAINT escalations_position_check CHECK (step_index >= -1 AND pass >= 0)
);
CREATE INDEX escalations_due_idx ON escalations (next_step_at) WHERE status = 'active';
CREATE INDEX escalations_silence_id_idx ON escalations (silence_id) WHERE silence_id IS NOT NULL;

-- A notification to a person names them.
CREATE INDEX notifications_user_id_idx ON notifications (user_id) WHERE user_id IS NOT NULL;

-- +goose Down
DROP INDEX notifications_user_id_idx;
DROP TABLE escalations;
