-- +goose Up
CREATE TABLE liveness_sessions (
    id                  TEXT        PRIMARY KEY,
    nonce               TEXT        NOT NULL,
    state               TEXT        NOT NULL,

    challenges          TEXT[]      NOT NULL,
    current_index       INTEGER     NOT NULL DEFAULT 0,

    created_at          TIMESTAMPTZ NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    challenge_deadline  TIMESTAMPTZ NOT NULL,

    last_seq            BIGINT      NOT NULL DEFAULT 0,

    -- Perceptual hashes are unsigned 64-bit and Postgres has no such type, so
    -- they are stored as the same bits reinterpreted as signed. Both sides do
    -- the reinterpretation, and neither ever does arithmetic on them, so the
    -- sign is irrelevant — but a query that sorts these will look nonsensical.
    recent_hashes       BIGINT[]    NOT NULL DEFAULT '{}',
    duplicate_streak    INTEGER     NOT NULL DEFAULT 0,

    -- The identity every later key frame is measured against. Null until the
    -- first key frame arrives.
    reference_embedding REAL[],

    progress            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    failure_reason      TEXT        NOT NULL DEFAULT '',

    -- Optimistic lock. Frames from one session arrive concurrently, and
    -- without this the later write silently discards the earlier one's
    -- progress.
    version             INTEGER     NOT NULL DEFAULT 0,

    CONSTRAINT liveness_sessions_state_known
        CHECK (state IN ('PENDING', 'IN_PROGRESS', 'PASSED', 'FAILED', 'EXPIRED')),
    CONSTRAINT liveness_sessions_index_in_range
        CHECK (current_index >= 0 AND current_index <= cardinality(challenges))
);

-- The purge job scans by expiry, and it runs often enough to matter.
CREATE INDEX liveness_sessions_expires_at_idx ON liveness_sessions (expires_at);

-- Counting live sessions is a readiness signal; without this it is a full scan.
CREATE INDEX liveness_sessions_state_idx ON liveness_sessions (state)
    WHERE state IN ('PENDING', 'IN_PROGRESS');

-- +goose Down
DROP TABLE IF EXISTS liveness_sessions;
