-- +goose Up
-- Challenge attempts restarted after their deadline elapsed.
--
-- Before this, missing one challenge expired the whole session, so a subject
-- who was slow on the last step lost the two they had already passed. The count
-- is per session rather than per challenge: a retry is another attempt at the
-- same instruction, and the budget is what stops a challenge-response protocol
-- from becoming a guessing game with no cost for guessing wrong.
--
-- Defaulted rather than nullable. Rows written before this migration had no
-- retries by definition, and zero says that without every read having to decide
-- what NULL means.
ALTER TABLE liveness_sessions
    ADD COLUMN retries INTEGER NOT NULL DEFAULT 0;

ALTER TABLE liveness_sessions
    ADD CONSTRAINT liveness_sessions_retries_not_negative CHECK (retries >= 0);

-- +goose Down
ALTER TABLE liveness_sessions
    DROP CONSTRAINT IF EXISTS liveness_sessions_retries_not_negative;

ALTER TABLE liveness_sessions
    DROP COLUMN IF EXISTS retries;
