-- +goose Up
-- Sign-in state. See docs/configuration-and-data-model.md and docs/access.md.

-- A browser session after sign-in. The cookie holds a random token; only its
-- SHA-256 hash is stored, so reading this table does not let anyone take
-- over a session. Rows live in Postgres so that every replica accepts a
-- session, whichever replica issued it.
CREATE TABLE sessions (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash text        NOT NULL,
    user_id    uuid        NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,

    CONSTRAINT sessions_token_hash_key UNIQUE (token_hash)
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- A sign-in that has been sent to the identity provider and has not come
-- back yet. It holds what the callback must present or prove: the state
-- (hashed, as the lookup key), the nonce expected in the ID token and the
-- PKCE code verifier. A row is used once and lives a few minutes.
CREATE TABLE sign_in_flows (
    state_hash    text        PRIMARY KEY,
    nonce         text        NOT NULL,
    code_verifier text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL
);

CREATE INDEX sign_in_flows_expires_at_idx ON sign_in_flows (expires_at);

-- +goose Down
DROP TABLE sign_in_flows;
DROP TABLE sessions;
