-- +goose Up
-- pgvector is needed by the face gallery in a later migration. Creating the
-- extension here rather than there keeps the privileged operation in one place:
-- CREATE EXTENSION needs rights that the application role should not hold.
CREATE EXTENSION IF NOT EXISTS vector;

-- +goose Down
-- Deliberately not dropped. Another schema in the same database may depend on
-- it, and a down migration that breaks unrelated tables is worse than one that
-- leaves an extension behind.
SELECT 1;
