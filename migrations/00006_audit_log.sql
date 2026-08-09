-- +goose Up
-- Append-only record of everything done to the gallery.
--
-- The rule it exists to enforce is that no biometric template is ever stored
-- without a record of who stored it and on whose authority. That rule is only
-- real if the two writes cannot come apart, so the repository does both in one
-- transaction; this table is the half that survives the other being deleted.
CREATE TABLE face_audit (
    id          BIGSERIAL   PRIMARY KEY,

    at          TIMESTAMPTZ NOT NULL,
    action      TEXT        NOT NULL,

    -- Nullable because not every action has every field: a search has no face
    -- id until it finds one, and a deletion has no session.
    subject_id  TEXT,
    face_id     TEXT,
    session_id  TEXT,

    -- Outcome is the answer, not the reason. "refused" is recorded; which
    -- defence refused is not, because this table is read by more people than
    -- the logs are.
    outcome     TEXT        NOT NULL,

    -- How many rows the action affected, for deletions.
    affected    INTEGER     NOT NULL DEFAULT 0,

    CONSTRAINT face_audit_action_known
        CHECK (action IN ('ENROLL', 'SEARCH', 'DELETE')),
    CONSTRAINT face_audit_outcome_known
        CHECK (outcome IN ('OK', 'REFUSED'))
);

-- Audits are read by time and by subject, which are the two questions anyone
-- asks of them: what happened then, and what happened to this person.
CREATE INDEX face_audit_at_idx ON face_audit (at DESC);
CREATE INDEX face_audit_subject_idx ON face_audit (subject_id) WHERE subject_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS face_audit;
