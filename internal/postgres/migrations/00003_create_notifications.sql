-- +goose Up
-- The notification queue and its delivery history. See
-- docs/configuration-and-data-model.md.

-- Integrations are provisioned objects, stored like sources.
CREATE TABLE integrations (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    spec       jsonb       NOT NULL DEFAULT '{}',
    spec_hash  text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    removed_at timestamptz
);

CREATE UNIQUE INDEX integrations_name_key ON integrations (name) WHERE removed_at IS NULL;

-- One row per message to one destination. Rows with status pending are the
-- queue: a worker claims a due row by moving next_attempt_at into the future,
-- which hides it from other workers until the attempt is recorded or the
-- claim runs out.
CREATE TABLE notifications (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_group_id  uuid        NOT NULL REFERENCES alert_groups (id),
    integration_id  uuid        NOT NULL REFERENCES integrations (id),
    user_id         uuid        REFERENCES users (id),
    destination     text        NOT NULL,
    reason          text        NOT NULL,
    status          text        NOT NULL DEFAULT 'pending',
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL,
    external_ref    text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,

    CONSTRAINT notifications_reason_check CHECK (reason IN ('escalation_step', 'group_update', 'resolved')),
    CONSTRAINT notifications_status_check CHECK (status IN ('pending', 'sent', 'failed')),
    CONSTRAINT notifications_sent_at_check CHECK ((status = 'sent') = (sent_at IS NOT NULL))
);

CREATE INDEX notifications_due_idx ON notifications (next_attempt_at) WHERE status = 'pending';
CREATE INDEX notifications_target_idx ON notifications (alert_group_id, integration_id, destination);

-- Every delivery attempt. Append only.
CREATE TABLE notification_attempts (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    notification_id uuid        NOT NULL REFERENCES notifications (id),
    attempted_at    timestamptz NOT NULL,
    outcome         text        NOT NULL,
    error           text,

    CONSTRAINT notification_attempts_outcome_check CHECK (outcome IN ('success', 'retryable_error', 'permanent_error'))
);

CREATE INDEX notification_attempts_notification_id_idx ON notification_attempts (notification_id, id);

-- +goose Down
DROP TABLE notification_attempts;
DROP TABLE notifications;
DROP TABLE integrations;
