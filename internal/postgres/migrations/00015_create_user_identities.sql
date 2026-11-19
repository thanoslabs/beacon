-- +goose Up
-- Which account in a chat service belongs to which user. Beacon needs it to
-- reach a person there, and to know who a person acting from there is. See
-- docs/access.md.
CREATE TABLE user_identities (
    user_id        uuid        NOT NULL REFERENCES users (id),
    -- The integration through which the account is known. An identifier of
    -- a chat service means something only within one workspace or one
    -- application, and it is through that integration that Beacon talks to
    -- the person.
    integration_id uuid        NOT NULL REFERENCES integrations (id),
    provider       text        NOT NULL,
    -- The identifier of the account in the service, such as a Slack user ID.
    external_id    text        NOT NULL,
    -- How Beacon learned of it: found by the user's email address, or
    -- linked by the user themselves.
    origin         text        NOT NULL,
    created_at     timestamptz NOT NULL,

    PRIMARY KEY (user_id, integration_id),
    CONSTRAINT user_identities_account_key UNIQUE (integration_id, external_id),
    CONSTRAINT user_identities_provider_check CHECK (provider IN ('slack', 'discord')),
    CONSTRAINT user_identities_origin_check CHECK (origin IN ('email', 'self')),
    CONSTRAINT user_identities_external_id_check CHECK (external_id <> '')
);

-- When Beacon may next ask a service for the account of a user by email
-- address. A row exists only for a user who has no identity there: it keeps
-- every replica from asking again and again for someone the service does
-- not know.
CREATE TABLE identity_lookups (
    user_id        uuid        NOT NULL REFERENCES users (id),
    integration_id uuid        NOT NULL REFERENCES integrations (id),
    -- The address that was asked for. A user whose address changed is asked
    -- for again at once.
    email          text        NOT NULL,
    outcome        text        NOT NULL,
    next_check_at  timestamptz NOT NULL,

    PRIMARY KEY (user_id, integration_id),
    CONSTRAINT identity_lookups_outcome_check CHECK (outcome IN ('pending', 'not_found', 'error'))
);

-- One-time codes with which a user proves, from inside a chat service, that
-- an account there is theirs. Only the SHA-256 hash of a code is stored.
CREATE TABLE identity_link_codes (
    code_hash      text        PRIMARY KEY,
    user_id        uuid        NOT NULL REFERENCES users (id),
    integration_id uuid        NOT NULL REFERENCES integrations (id),
    created_at     timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL
);
CREATE INDEX identity_link_codes_user_idx ON identity_link_codes (user_id, integration_id);

-- +goose Down
DROP TABLE identity_link_codes;
DROP TABLE identity_lookups;
DROP TABLE user_identities;
