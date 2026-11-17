-- +goose Up
-- Who belongs to which team. Membership is the union of two origins, and
-- each origin manages only its own rows: "config" rows follow the members
-- list of the team in the configuration files, "group" rows follow the
-- identity provider groups in a user's token at sign-in. A user is a member
-- when either row exists. See docs/configuration-and-data-model.md.
CREATE TABLE team_members (
    team_id    uuid        NOT NULL REFERENCES teams (id),
    user_id    uuid        NOT NULL REFERENCES users (id),
    origin     text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (team_id, user_id, origin),
    CONSTRAINT team_members_origin_check CHECK (origin IN ('config', 'group'))
);

CREATE INDEX team_members_user_id_idx ON team_members (user_id);

-- +goose Down
DROP TABLE team_members;
