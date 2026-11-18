-- +goose Up
-- API tokens, with which automation authenticates. A token is shown once,
-- when it is created; only its SHA-256 hash is stored. It carries a role of
-- its own and is not tied to the user who created it. See docs/access.md.
CREATE TABLE api_tokens (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text        NOT NULL,
    token_hash   text        NOT NULL,
    role         text        NOT NULL,
    -- Null when the token was created with another token.
    created_by   uuid        REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz,
    revoked_at   timestamptz,

    CONSTRAINT api_tokens_token_hash_key UNIQUE (token_hash),
    CONSTRAINT api_tokens_role_check CHECK (role IN ('admin', 'member'))
);

-- +goose Down
DROP TABLE api_tokens;
