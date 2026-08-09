-- +goose Up
-- Single-use tokens that carry a passed liveness session into an enrollment.
--
-- Stored rather than merely signed. A signature proves a token was issued here
-- and has not expired, but it cannot prove the token has not already been
-- spent, and "used exactly once" is the whole point: without it a captured
-- token enrols the attacker's face as often as they like.
CREATE TABLE liveness_tokens (
    -- The HMAC of the token, never the token itself.
    --
    -- Same reasoning as storing password hashes. A dump of this table is then
    -- useless without the signing secret, which lives in the environment and
    -- not in the database.
    token_hash  BYTEA       PRIMARY KEY,

    -- Which session earned it. Not a foreign key: sessions are purged on a
    -- schedule and a token must not outlive its own expiry anyway, but losing
    -- the session row must not invalidate a token still inside its window.
    session_id  TEXT        NOT NULL,

    issued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,

    -- Null until spent. The single-use guarantee is an atomic UPDATE against
    -- this column, so two requests racing with the same token cannot both win.
    used_at     TIMESTAMPTZ,

    CONSTRAINT liveness_tokens_expiry_after_issue CHECK (expires_at > issued_at)
);

-- The purge job scans by expiry.
CREATE INDEX liveness_tokens_expires_at_idx ON liveness_tokens (expires_at);

-- +goose Down
DROP TABLE IF EXISTS liveness_tokens;
