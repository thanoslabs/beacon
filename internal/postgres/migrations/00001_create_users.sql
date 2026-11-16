-- +goose Up
-- Users are runtime state: a row is created the first time someone signs in.
-- See docs/configuration-and-data-model.md.
CREATE TABLE users (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email           text        NOT NULL,
    display_name    text        NOT NULL DEFAULT '',
    role            text        NOT NULL DEFAULT 'member',
    oidc_issuer     text,
    oidc_subject    text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    last_sign_in_at timestamptz,
    disabled_at     timestamptz,

    CONSTRAINT users_role_check CHECK (role IN ('admin', 'member')),
    CONSTRAINT users_email_lowercase_check CHECK (email = lower(email)),
    CONSTRAINT users_email_key UNIQUE (email),
    CONSTRAINT users_oidc_identity_key UNIQUE (oidc_issuer, oidc_subject),
    CONSTRAINT users_oidc_identity_check CHECK ((oidc_issuer IS NULL) = (oidc_subject IS NULL))
);

-- +goose Down
DROP TABLE users;
