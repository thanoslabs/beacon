-- +goose Up
-- Alerts, their groups and their timeline. See
-- docs/configuration-and-data-model.md. Columns that reference tables which do
-- not exist yet (routing rules, teams, escalation policies) are added by the
-- migrations that create those tables.

-- Sources are provisioned objects: the row is a copy of what the configuration
-- defines, kept so that alerts have something stable to reference.
CREATE TABLE sources (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    spec       jsonb       NOT NULL DEFAULT '{}',
    spec_hash  text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    removed_at timestamptz
);

CREATE UNIQUE INDEX sources_name_key ON sources (name) WHERE removed_at IS NULL;

CREATE TABLE alert_groups (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    group_key   text        NOT NULL,
    status      text        NOT NULL DEFAULT 'open',
    created_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,

    CONSTRAINT alert_groups_status_check CHECK (status IN ('open', 'resolved'))
);

CREATE UNIQUE INDEX alert_groups_open_group_key ON alert_groups (group_key) WHERE status = 'open';

CREATE TABLE alerts (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id        uuid        NOT NULL REFERENCES sources (id),
    fingerprint      text        NOT NULL,
    status           text        NOT NULL DEFAULT 'firing',
    severity         text        NOT NULL,
    labels           jsonb       NOT NULL DEFAULT '{}',
    annotations      jsonb       NOT NULL DEFAULT '{}',
    group_id         uuid        NOT NULL REFERENCES alert_groups (id),
    service          text,
    started_at       timestamptz NOT NULL,
    last_received_at timestamptz NOT NULL,
    acknowledged_at  timestamptz,
    acknowledged_by  uuid        REFERENCES users (id),
    snoozed_until    timestamptz,
    resolved_at      timestamptz,

    CONSTRAINT alerts_status_check CHECK (status IN ('firing', 'acknowledged', 'snoozed', 'resolved')),
    CONSTRAINT alerts_severity_check CHECK (severity IN ('critical', 'warning', 'info')),
    CONSTRAINT alerts_snoozed_until_check CHECK ((status = 'snoozed') = (snoozed_until IS NOT NULL)),
    CONSTRAINT alerts_resolved_at_check CHECK ((status = 'resolved') = (resolved_at IS NOT NULL))
);

-- One open alert per source and fingerprint: repeated deliveries update the
-- same row, and a resolved alert that fires again opens a new one.
CREATE UNIQUE INDEX alerts_open_fingerprint_key ON alerts (source_id, fingerprint) WHERE status <> 'resolved';
CREATE INDEX alerts_group_id_idx ON alerts (group_id);
CREATE INDEX alerts_snoozed_until_idx ON alerts (snoozed_until) WHERE status = 'snoozed';

-- The timeline. Append only: rows are never updated or deleted.
CREATE TABLE alert_events (
    id                bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    alert_id          uuid        REFERENCES alerts (id),
    alert_group_id    uuid        NOT NULL REFERENCES alert_groups (id),
    type              text        NOT NULL,
    actor_type        text        NOT NULL,
    actor_id          uuid,
    -- Set when a person acted from a chat service. They identify the person
    -- there, so the event can be attributed to a Beacon user once identities
    -- are linked (user_identities.provider and external_id).
    actor_provider    text,
    actor_external_id text,
    occurred_at       timestamptz NOT NULL DEFAULT now(),
    data              jsonb       NOT NULL DEFAULT '{}',

    CONSTRAINT alert_events_actor_type_check CHECK (actor_type IN ('user', 'api_token', 'source', 'system')),
    CONSTRAINT alert_events_actor_external_check CHECK ((actor_provider IS NULL) = (actor_external_id IS NULL))
);

CREATE INDEX alert_events_alert_id_idx ON alert_events (alert_id, id);
CREATE INDEX alert_events_alert_group_id_idx ON alert_events (alert_group_id, id);

-- +goose Down
DROP TABLE alert_events;
DROP TABLE alerts;
DROP TABLE alert_groups;
DROP TABLE sources;
