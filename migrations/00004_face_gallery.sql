-- +goose Up
-- The face gallery: one row per enrolled capture, searched by vector distance.
--
-- A subject may have several rows. Enrolling the same person twice from
-- different captures makes 1:N search more robust, and collapsing them to one
-- average vector would throw that away.
CREATE TABLE faces (
    id           TEXT        PRIMARY KEY,
    subject_id   TEXT        NOT NULL,

    -- 512 dimensions, L2-normalised on the way in. Normalisation is what makes
    -- cosine distance and inner product agree, and the repository refuses a
    -- vector that is not normalised rather than storing one that would compare
    -- wrongly against everything already here.
    embedding    VECTOR(512) NOT NULL,

    -- Which liveness session authorised this enrollment.
    --
    -- Not a foreign key: sessions are purged on a schedule and an enrollment
    -- must outlive the session that produced it. Losing the session row must
    -- not take the provenance with it, so the id is kept as a plain value.
    session_id   TEXT        NOT NULL,

    -- Object store key for the aligned crop, when one was kept. Null means no
    -- image was retained, which is the privacy-preserving default.
    artifact_key TEXT,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX faces_subject_id_idx ON faces (subject_id);

-- HNSW over cosine distance.
--
-- Cosine rather than L2 because the embeddings are normalised and the
-- thresholds everywhere else in this system are cosine similarities. Mixing the
-- two would mean a number that looks like a similarity and orders like a
-- distance.
--
-- m and ef_construction are build-time properties of the index and so live
-- here, not in configuration: changing them means rebuilding. Only ef_search is
-- a runtime knob, and the repository sets that per connection.
CREATE INDEX faces_embedding_hnsw_idx ON faces
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- +goose Down
DROP TABLE IF EXISTS faces;
