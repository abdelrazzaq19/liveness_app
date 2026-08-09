package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/enrollment"
)

// FaceRepo stores and searches the face gallery.
type FaceRepo struct {
	db *DB

	// efSearch is how many candidates HNSW keeps while descending the graph.
	//
	// Set per query rather than per pool because it is the recall-versus-latency
	// dial, and a value set once at connection time would silently apply to
	// whatever ran next on a pooled connection.
	efSearch int
}

var (
	_ enrollment.Repository = (*FaceRepo)(nil)
	_ enrollment.Audit      = (*FaceRepo)(nil)
)

// NewFaceRepo wraps a pool as a face repository.
func NewFaceRepo(db *DB, efSearch int) (*FaceRepo, error) {
	if db == nil || db.Pool == nil {
		return nil, errors.New("postgres: face repository needs a pool")
	}
	if efSearch < 1 {
		return nil, fmt.Errorf("postgres: hnsw ef_search must be at least 1, got %d", efSearch)
	}
	return &FaceRepo{db: db, efSearch: efSearch}, nil
}

// Insert adds one enrolled capture together with its audit entry.
//
// One transaction, because the rule is that no template is stored without a
// record of it. Two statements outside a transaction would satisfy that rule
// almost always, and "almost always" is the same as "not" for an audit trail:
// the cases it misses are exactly the crashes somebody would later be trying to
// reconstruct.
func (r *FaceRepo) Insert(ctx context.Context, f enrollment.Face, entry enrollment.AuditEntry) error {
	if err := f.Validate(); err != nil {
		return fmt.Errorf("postgres: refusing to store face: %w", err)
	}
	if err := entry.Validate(); err != nil {
		return fmt.Errorf("postgres: refusing to store face without a usable audit entry: %w", err)
	}

	var artifact any
	if f.ArtifactKey != "" {
		artifact = f.ArtifactKey
	}

	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin insert face: %w", err)
	}
	// Rolls back unless Commit already ran, in which case it is a no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	const insertFace = `
		INSERT INTO faces (id, subject_id, embedding, session_id, artifact_key)
		VALUES ($1, $2, $3, $4, $5)`

	if _, err := tx.Exec(ctx, insertFace,
		string(f.ID), f.SubjectID, vectorLiteral(f.Embedding), f.SessionID, artifact); err != nil {
		// The id is safe to name; the subject is not, and neither is anything
		// derived from the template.
		return fmt.Errorf("postgres: insert face %s: %w", f.ID, err)
	}

	if err := insertAudit(ctx, tx, entry); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit insert face %s: %w", f.ID, err)
	}
	return nil
}

// Search returns the nearest faces by cosine similarity, most similar first.
//
// No threshold is applied here. Deciding what counts as a match belongs to the
// service, where it is one testable comparison rather than a filter buried in
// SQL that nobody can see from the outside.
func (r *FaceRepo) Search(ctx context.Context, query biometric.Embedding, topK int) ([]enrollment.Match, error) {
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: search vector: %w", err)
	}
	if topK < 1 {
		return nil, fmt.Errorf("postgres: topK must be at least 1, got %d", topK)
	}

	// Columns are named rather than selected wholesale. A SELECT * here would
	// carry the embedding of every candidate up towards a response, and the one
	// thing that must never leave this table is the template itself.
	const sql = `
		SELECT id, subject_id, 1 - (embedding <=> $1) AS similarity
		FROM faces
		ORDER BY embedding <=> $1
		LIMIT $2`

	conn, err := r.db.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: acquire connection: %w", err)
	}
	defer conn.Release()

	// ef_search is per transaction, so it cannot leak onto whatever runs next
	// on this pooled connection.
	if _, err := conn.Exec(ctx, "SET LOCAL hnsw.ef_search = "+strconv.Itoa(r.efSearch)); err != nil {
		return nil, fmt.Errorf("postgres: set hnsw.ef_search: %w", err)
	}

	rows, err := conn.Query(ctx, sql, vectorLiteral(query), topK)
	if err != nil {
		return nil, fmt.Errorf("postgres: search faces: %w", err)
	}
	defer rows.Close()

	var out []enrollment.Match
	for rows.Next() {
		var m enrollment.Match
		var id string
		if err := rows.Scan(&id, &m.SubjectID, &m.Score); err != nil {
			return nil, fmt.Errorf("postgres: scan match: %w", err)
		}
		m.FaceID = enrollment.FaceID(id)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: read matches: %w", err)
	}
	return out, nil
}

// DeleteSubject removes every face belonging to a subject.
//
// A hard delete, not a flag. A biometric template that is still readable has
// not been deleted, whatever a column says about it. The audit trail lives in
// its own table precisely so that the record of the deletion survives the thing
// deleted.
func (r *FaceRepo) DeleteSubject(ctx context.Context, subjectID string, entry enrollment.AuditEntry) (int, error) {
	if subjectID == "" {
		return 0, errors.New("postgres: subject id is required")
	}
	if err := entry.Validate(); err != nil {
		return 0, fmt.Errorf("postgres: refusing to delete without a usable audit entry: %w", err)
	}

	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: begin delete subject: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `DELETE FROM faces WHERE subject_id = $1`, subjectID)
	if err != nil {
		return 0, fmt.Errorf("postgres: delete subject: %w", err)
	}
	affected := int(tag.RowsAffected())

	// The count goes into the entry rather than being taken on trust from the
	// caller: how many templates actually went is the one number a deletion has
	// to be able to answer for afterwards.
	entry.Affected = affected

	if err := insertAudit(ctx, tx, entry); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("postgres: commit delete subject: %w", err)
	}
	return affected, nil
}

// Record writes a standalone audit entry, for actions that do not touch the
// gallery.
//
// Gallery writes are audited inside their own transaction instead. This is only
// for reads — a search leaves a trail too, because knowing who was looked for is
// part of knowing how the system was used.
func (r *FaceRepo) Record(ctx context.Context, entry enrollment.AuditEntry) error {
	if err := entry.Validate(); err != nil {
		return fmt.Errorf("postgres: audit entry: %w", err)
	}
	return insertAudit(ctx, r.db.Pool, entry)
}

// execer is what insertAudit needs: a pool or a transaction, indifferently.
//
// That indifference is the point. The same statement has to run inside the
// transaction that writes a face and on its own for a search, and having one
// function serve both is what stops the two drifting apart.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertAudit(ctx context.Context, q execer, e enrollment.AuditEntry) error {
	const query = `
		INSERT INTO face_audit (at, action, outcome, subject_id, face_id, session_id, affected)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	if _, err := q.Exec(ctx, query,
		e.At, string(e.Action), string(e.Outcome),
		nullable(e.SubjectID), nullable(e.FaceID.String()), nullable(e.SessionID),
		e.Affected,
	); err != nil {
		return fmt.Errorf("postgres: write audit entry: %w", err)
	}
	return nil
}

// nullable turns an empty string into a SQL NULL.
//
// Not every action has every field, and storing "" would make an absent value
// indistinguishable from one that was genuinely blank when somebody reads the
// trail years later.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Count reports how many faces are enrolled.
func (r *FaceRepo) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.db.Pool.QueryRow(ctx, `SELECT count(*) FROM faces`).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: count faces: %w", err)
	}
	return n, nil
}

// vectorLiteral renders an embedding in pgvector's text form.
//
// Written by hand rather than pulled in with the pgvector Go package: the
// format is "[1,2,3]" and adding a dependency for it would be more code to
// audit than the six lines it replaces.
//
// Precision matters here. Formatted with -1 precision, so each float32 round
// trips exactly rather than being quietly truncated into a slightly different
// vector from the one that was measured.
func vectorLiteral(e biometric.Embedding) string {
	var b strings.Builder
	b.Grow(len(e)*12 + 2)

	b.WriteByte('[')
	for i, v := range e {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
