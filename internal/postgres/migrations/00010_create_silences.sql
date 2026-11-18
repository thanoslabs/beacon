-- +goose Up
-- Silences: runtime state that people create through the API. A silence
-- matches alerts by their labels for a period. See
-- docs/configuration-and-data-model.md.
CREATE TABLE silences (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- A list of {label, operator, value}; all must match.
    matchers         jsonb       NOT NULL,
    starts_at        timestamptz NOT NULL,
    -- Expiring a silence early sets this to the time it was expired.
    ends_at          timestamptz NOT NULL,
    -- The author: the user who created it, or the API token it was created
    -- with. Exactly one is set.
    created_by       uuid        REFERENCES users (id),
    created_by_token uuid        REFERENCES api_tokens (id),
    comment          text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT silences_period_check CHECK (ends_at >= starts_at),
    CONSTRAINT silences_author_check CHECK (num_nonnulls(created_by, created_by_token) = 1)
);

-- The silences in force, and the ones still to come, are found by their end.
CREATE INDEX silences_ends_at_idx ON silences (ends_at);

-- +goose Down
DROP TABLE silences;
