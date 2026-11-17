-- +goose Up
-- The remaining kinds of provisioned object, stored like sources and
-- integrations: a copy of what the configuration files define, kept so that
-- runtime rows have something stable to reference. See
-- docs/configuration-and-data-model.md.

CREATE TABLE teams (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL,
    spec        jsonb       NOT NULL DEFAULT '{}',
    spec_hash   text        NOT NULL DEFAULT '',
    provisioned boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    removed_at  timestamptz
);
CREATE UNIQUE INDEX teams_name_key ON teams (name) WHERE removed_at IS NULL;

CREATE TABLE schedules (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL,
    team_id     uuid        NOT NULL REFERENCES teams (id),
    spec        jsonb       NOT NULL DEFAULT '{}',
    spec_hash   text        NOT NULL DEFAULT '',
    provisioned boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    removed_at  timestamptz
);
CREATE UNIQUE INDEX schedules_name_key ON schedules (name) WHERE removed_at IS NULL;
CREATE INDEX schedules_team_id_idx ON schedules (team_id);

CREATE TABLE escalation_policies (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL,
    spec        jsonb       NOT NULL DEFAULT '{}',
    spec_hash   text        NOT NULL DEFAULT '',
    provisioned boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    removed_at  timestamptz
);
CREATE UNIQUE INDEX escalation_policies_name_key ON escalation_policies (name) WHERE removed_at IS NULL;

CREATE TABLE routing_rules (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL,
    -- Rules are tried in this order; the first match wins.
    position    integer     NOT NULL DEFAULT 0,
    spec        jsonb       NOT NULL DEFAULT '{}',
    spec_hash   text        NOT NULL DEFAULT '',
    provisioned boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    removed_at  timestamptz
);
CREATE UNIQUE INDEX routing_rules_name_key ON routing_rules (name) WHERE removed_at IS NULL;

-- provisioned marks a row as owned by the configuration files: the API and
-- the UI must treat it as read-only, and reconciliation manages only these
-- rows. Every row is provisioned today.
ALTER TABLE sources      ADD COLUMN provisioned boolean NOT NULL DEFAULT true;
ALTER TABLE integrations ADD COLUMN provisioned boolean NOT NULL DEFAULT true;

-- The references that the first alert migration left out, because their
-- tables did not exist yet. Nothing sets them until routing is implemented.
ALTER TABLE alerts
    ADD COLUMN routing_rule_id uuid REFERENCES routing_rules (id),
    ADD COLUMN team_id         uuid REFERENCES teams (id);
ALTER TABLE alert_groups
    ADD COLUMN routing_rule_id      uuid REFERENCES routing_rules (id),
    ADD COLUMN team_id              uuid REFERENCES teams (id),
    ADD COLUMN escalation_policy_id uuid REFERENCES escalation_policies (id);

-- +goose Down
ALTER TABLE alert_groups
    DROP COLUMN escalation_policy_id,
    DROP COLUMN team_id,
    DROP COLUMN routing_rule_id;
ALTER TABLE alerts
    DROP COLUMN team_id,
    DROP COLUMN routing_rule_id;
ALTER TABLE integrations DROP COLUMN provisioned;
ALTER TABLE sources      DROP COLUMN provisioned;
DROP TABLE routing_rules;
DROP TABLE escalation_policies;
DROP TABLE schedules;
DROP TABLE teams;
